package ipcheck

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

var ErrBusy = errors.New("too many IP checks in progress")

type cacheKey struct{ kind, subject, source string }
type cacheEntry struct {
	key    cacheKey
	result *SourceResult
}
type nodeState struct {
	ip            string
	generation    uint64
	egressVersion int64
}
type flight struct {
	key        cacheKey
	request    CheckRequest
	generation uint64
	done       chan struct{}
	cancel     context.CancelFunc
	waiters    int
	result     *SourceResult
	err        error
}

// Engine is a bounded, cold-path engine with per-subject/source singleflight.
// Requests share work independently of any one caller's lifetime; work is
// canceled when the last waiter leaves or the plugin stops. No scheduler runs.
type Engine struct {
	host           Host
	store          Store
	config         Config
	sources        map[string]Source
	enabled        map[string]bool
	ctx            context.Context
	cancel         context.CancelFunc
	sem            chan struct{}
	mu             sync.Mutex
	closed         bool
	flights        map[cacheKey]*flight
	nodes          map[node.Hash]nodeState
	cache          map[cacheKey]*list.Element
	lru            *list.List
	workers        sync.WaitGroup
	closedDone     chan struct{}
	fingerprint    string
	nextGeneration uint64
}

func NewEngine(host Host, store Store, cfg Config) (*Engine, error) {
	if host == nil {
		return nil, invalid("IP quality host is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.EnabledSources = append([]string(nil), cfg.EnabledSources...)
	cfg.ConsistencyTargets = append([]string(nil), cfg.ConsistencyTargets...)
	cfg.SourcePriority = append([]string(nil), cfg.SourcePriority...)
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{host: host, store: store, config: cfg, ctx: ctx, cancel: cancel, sem: make(chan struct{}, cfg.MaxConcurrentChecks), sources: map[string]Source{}, enabled: map[string]bool{}, flights: map[cacheKey]*flight{}, nodes: map[node.Hash]nodeState{}, cache: map[cacheKey]*list.Element{}, lru: list.New(), closedDone: make(chan struct{})}
	encoded, _ := json.Marshal(cfg)
	digest := sha256.Sum256(encoded)
	e.fingerprint = hex.EncodeToString(digest[:])
	e.sources["egress-consistency"] = &consistencySource{targets: cfg.ConsistencyTargets}
	e.sources["db-ip"] = &geoIPSource{host: host}
	for _, name := range cfg.EnabledSources {
		e.enabled[name] = true
	}
	return e, nil
}

func (e *Engine) Sources() []SourceStatus {
	statuses := make([]SourceStatus, 0, len(sourceCatalog))
	for _, known := range sourceCatalog {
		status := known
		status.Enabled = e.enabled[known.Name] && status.Available
		if status.Available && !status.Enabled {
			status.Reason = "disabled by configuration"
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func (e *Engine) CheckNode(ctx context.Context, hash node.Hash, names []string, force bool) (*Result, error) {
	selected, err := e.selectSources(names, false)
	if err != nil {
		return nil, err
	}
	snapshot, generation, err := e.prepareNode(ctx, hash)
	if err != nil {
		return nil, err
	}
	result, err := e.check(ctx, CheckRequest{NodeHash: hash, EgressIP: snapshot.EgressIP.Unmap(), EgressVersion: snapshot.EgressVersion}, generation, selected, force)
	if err != nil {
		return nil, err
	}
	if err = e.checkNodeCurrent(ctx, hash, snapshot.EgressIP, snapshot.EgressVersion); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Engine) Lookup(ctx context.Context, ip netip.Addr, names []string, force bool) (*Result, error) {
	if err := validateIP(ip); err != nil {
		return nil, err
	}
	selected, err := e.selectSources(names, true)
	if err != nil {
		return nil, err
	}
	return e.check(ctx, CheckRequest{EgressIP: ip.Unmap()}, 0, selected, force)
}

func (e *Engine) GetNode(ctx context.Context, hash node.Hash) (*Result, error) {
	snapshot, generation, err := e.prepareNode(ctx, hash)
	if err != nil {
		return nil, err
	}
	result, err := e.get(ctx, CheckRequest{NodeHash: hash, EgressIP: snapshot.EgressIP.Unmap(), EgressVersion: snapshot.EgressVersion}, generation, false)
	if err != nil {
		return nil, err
	}
	if err = e.checkNodeCurrent(ctx, hash, snapshot.EgressIP, snapshot.EgressVersion); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Engine) GetIP(ctx context.Context, ip netip.Addr) (*Result, error) {
	if err := validateIP(ip); err != nil {
		return nil, err
	}
	return e.get(ctx, CheckRequest{EgressIP: ip.Unmap()}, 0, true)
}

func (e *Engine) selectSources(names []string, byIP bool) ([]string, error) {
	if len(names) == 0 {
		names = make([]string, 0, len(e.config.EnabledSources))
		for _, name := range e.config.EnabledSources {
			if !byIP || sourceCatalog[name].Kind == SourceKindByIP {
				names = append(names, name)
			}
		}
	}
	if len(names) > len(sourceCatalog) {
		return nil, invalid("too many sources")
	}
	seen := map[string]bool{}
	selected := make([]string, 0, len(names))
	for _, name := range names {
		source, ok := sourceCatalog[name]
		if !ok {
			return nil, invalid("unknown source: " + name)
		}
		if byIP && source.Kind == SourceKindViaNode {
			return nil, invalid("via-node sources cannot be used for IP lookup")
		}
		if seen[name] {
			return nil, invalid("duplicate source: " + name)
		}
		seen[name] = true
		selected = append(selected, name)
	}
	return selected, nil
}

func (e *Engine) prepareNode(ctx context.Context, hash node.Hash) (NodeSnapshot, uint64, error) {
	if err := ctx.Err(); err != nil {
		return NodeSnapshot{}, 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return NodeSnapshot{}, 0, ErrClosed
	}
	snapshot, ok := e.host.NodeSnapshot(ctx, hash)
	if !ok {
		e.invalidateNodeLocked(hash)
		return NodeSnapshot{}, 0, ErrNodeNotFound
	}
	if !IsPublicIP(snapshot.EgressIP) {
		e.invalidateNodeLocked(hash)
		return NodeSnapshot{}, 0, ErrNoEgressIP
	}
	ip := snapshot.EgressIP.Unmap().String()
	state, exists := e.nodes[hash]
	if exists && (state.ip != ip || state.egressVersion != snapshot.EgressVersion) {
		e.invalidateNodeLocked(hash)
		exists = false
	}
	if !exists {
		if len(e.nodes) >= e.config.MaxCacheEntries {
			evicted := false
			for candidate := range e.nodes {
				active := false
				for key := range e.flights {
					if key.kind == SubjectNode && key.subject == candidate.Hex() {
						active = true
						break
					}
				}
				if !active {
					delete(e.nodes, candidate)
					evicted = true
					break
				}
			}
			if !evicted {
				return NodeSnapshot{}, 0, ErrBusy
			}
		}
		e.nextGeneration++
		state = nodeState{generation: e.nextGeneration}
	}
	state.ip = ip
	state.egressVersion = snapshot.EgressVersion
	e.nodes[hash] = state
	return snapshot, state.generation, nil
}

func (e *Engine) checkNodeCurrent(ctx context.Context, hash node.Hash, ip netip.Addr, version int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot, ok := e.host.NodeSnapshot(ctx, hash)
	if !ok {
		e.InvalidateNode(hash)
		return ErrNodeNotFound
	}
	if snapshot.EgressIP.Unmap() != ip.Unmap() || snapshot.EgressVersion != version {
		e.InvalidateNode(hash)
		return ErrNodeChanged
	}
	return nil
}

// InvalidateNode clears persisted and in-memory via-node observations and
// cancels in-flight work. By-IP results remain reusable until their own TTL.
func (e *Engine) InvalidateNode(hash node.Hash) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.invalidateNodeLocked(hash)
	}
}

func (e *Engine) invalidateNodeLocked(hash node.Hash) {
	_, known := e.nodes[hash]
	delete(e.nodes, hash)
	subject := hash.Hex()
	for key, element := range e.cache {
		if key.kind == SubjectNode && key.subject == subject {
			known = true
			e.lru.Remove(element)
			delete(e.cache, key)
		}
	}
	for key, f := range e.flights {
		if key.kind == SubjectNode && key.subject == subject {
			known = true
			f.cancel()
			delete(e.flights, key)
		}
	}
	// Mutation is serialized with completion saves, so an old generation cannot
	// recreate a result after invalidation. Store adapters enqueue these writes.
	if known && e.store != nil {
		ctx, cancel := context.WithTimeout(e.ctx, e.timeout())
		defer cancel()
		_ = e.store.DeleteNode(ctx, hash)
	}
}

func (e *Engine) check(ctx context.Context, request CheckRequest, generation uint64, names []string, force bool) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	type outcome struct {
		result *SourceResult
		err    error
	}
	outcomes := make(chan outcome, len(names))
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, name := range names {
		go func(name string) {
			result, err := e.checkSource(workCtx, request, generation, name, force)
			outcomes <- outcome{result, err}
		}(name)
	}
	results := make([]SourceResult, 0, len(names))
	for range names {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case item := <-outcomes:
			if item.err != nil {
				return nil, item.err
			}
			results = append(results, *item.result)
		}
	}
	return aggregate(request.EgressIP, results, e.config.SourcePriority), nil
}

func (e *Engine) checkSource(ctx context.Context, request CheckRequest, generation uint64, name string, force bool) (*SourceResult, error) {
	key := keyFor(request, name)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, ErrClosed
	}
	if key.kind == SubjectNode && !e.generationCurrent(request, generation) {
		e.mu.Unlock()
		return nil, ErrNodeChanged
	}
	if !e.enabled[name] || e.sources[name] == nil {
		status := sourceCatalog[name]
		reason := status.Reason
		if e.sources[name] != nil {
			reason = "source is disabled by configuration"
		}
		f := &flight{key: key, request: request}
		result := e.stamp(f, &SourceResult{Status: "unavailable", Error: reason})
		e.mu.Unlock()
		return result, nil
	}
	// In-flight work takes precedence over cache, including for force requests.
	if f, ok := e.flights[key]; ok {
		f.waiters++
		e.mu.Unlock()
		return e.await(ctx, f)
	}
	if !force {
		if result := e.memoryLocked(key, request.EgressIP, request.EgressVersion); result != nil {
			e.mu.Unlock()
			return result, nil
		}
	}
	// Bound both executing and queued flights. Each check fans out over a fixed,
	// small source catalog; the source itself has at most MaxTargets requests.
	if len(e.flights) >= e.config.MaxConcurrentChecks*4 {
		e.mu.Unlock()
		return nil, ErrBusy
	}
	workCtx, cancel := context.WithTimeout(e.ctx, e.timeout())
	f := &flight{key: key, request: request, generation: generation, done: make(chan struct{}), cancel: cancel, waiters: 1}
	e.flights[key] = f
	e.workers.Add(1)
	e.mu.Unlock()
	go e.run(workCtx, f, force)
	return e.await(ctx, f)
}

func (e *Engine) await(ctx context.Context, f *flight) (*SourceResult, error) {
	select {
	case <-ctx.Done():
		e.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
			if e.flights[f.key] == f {
				delete(e.flights, f.key)
			}
		}
		e.mu.Unlock()
		return nil, ctx.Err()
	case <-f.done:
		return cloneSource(f.result), f.err
	}
}

func (e *Engine) run(ctx context.Context, f *flight, force bool) {
	defer e.workers.Done()
	defer f.cancel()
	var result *SourceResult
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
		if !force && e.store != nil {
			loaded, err := e.store.Load(ctx, f.key.kind, f.key.subject, f.key.source)
			if err == nil && e.validResult(loaded, f.key, f.request.EgressIP, f.request.EgressVersion, time.Now()) {
				result = cloneSource(loaded)
			}
		}
		if result == nil {
			result = e.execute(ctx, f)
		}
	case <-ctx.Done():
		result = e.failure(f, ctx.Err())
	}
	// Verify mutable node state again after external work and before saving.
	var nodeErr error
	if f.key.kind == SubjectNode {
		snapshot, ok := e.host.NodeSnapshot(e.ctx, f.request.NodeHash)
		if !ok {
			nodeErr = ErrNodeNotFound
		} else if snapshot.EgressIP.Unmap() != f.request.EgressIP || snapshot.EgressVersion != f.request.EgressVersion {
			nodeErr = ErrNodeChanged
		}
	}
	e.mu.Lock()
	switch {
	case e.closed:
		f.err = ErrClosed
	case f.waiters == 0:
		f.err = context.Canceled
	case nodeErr != nil:
		f.err = nodeErr
	case f.key.kind == SubjectNode && !e.generationCurrent(f.request, f.generation):
		f.err = ErrNodeChanged
	default:
		f.result = cloneSource(result)
		e.putMemoryLocked(f.key, result)
		if e.store != nil {
			saveCtx, cancel := context.WithTimeout(e.ctx, e.timeout())
			_ = e.store.Save(saveCtx, f.key.kind, f.key.subject, cloneSource(result))
			cancel()
		}
	}
	if e.flights[f.key] == f {
		delete(e.flights, f.key)
	}
	close(f.done)
	e.mu.Unlock()
}

func (e *Engine) execute(ctx context.Context, f *flight) *SourceResult {
	status := sourceCatalog[f.key.source]
	result := &SourceResult{Status: "unavailable", Error: status.Reason}
	source := e.sources[f.key.source]
	if source != nil && e.enabled[f.key.source] {
		request := f.request
		request.HTTP = &http.Client{Transport: viaNodeTransport{host: e.host, hash: request}, Timeout: e.timeout(), CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		var err error
		result, err = source.Check(ctx, request)
		if err != nil || result == nil {
			return e.failure(f, err)
		}
	} else if source != nil {
		result.Error = "source is disabled by configuration"
	}
	return e.stamp(f, result)
}

func (e *Engine) failure(f *flight, err error) *SourceResult {
	return e.stamp(f, &SourceResult{Status: "error", Error: publicSourceError(err)})
}
func (e *Engine) stamp(f *flight, result *SourceResult) *SourceResult {
	result.ConfigFingerprint = e.fingerprint
	if f.key.kind == SubjectNode {
		result.EgressVersion = f.request.EgressVersion
	}
	result.Source = f.key.source
	result.Kind = sourceCatalog[f.key.source].Kind
	result.EgressIP = f.request.EgressIP.String()
	result.CheckedAt = time.Now().UTC()
	ttl, _ := time.ParseDuration(e.config.IPResultTTL)
	if result.Kind == SourceKindViaNode {
		ttl, _ = time.ParseDuration(e.config.NodeResultTTL)
	}
	// Failure data is useful, but should not suppress retries for an entire day.
	if !result.OK && ttl > time.Minute {
		ttl = time.Minute
	}
	result.ExpiresAt = result.CheckedAt.Add(ttl)
	return result
}

func (e *Engine) get(ctx context.Context, request CheckRequest, generation uint64, byIP bool) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results := []SourceResult{}
	for _, status := range e.Sources() {
		if !status.Enabled {
			continue
		}
		if byIP && status.Kind != SourceKindByIP {
			continue
		}
		key := keyFor(request, status.Name)
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return nil, ErrClosed
		}
		if key.kind == SubjectNode && !e.generationCurrent(request, generation) {
			e.mu.Unlock()
			return nil, ErrNodeChanged
		}
		result := e.memoryLocked(key, request.EgressIP, request.EgressVersion)
		if result == nil && e.store != nil {
			loadCtx, cancel := context.WithTimeout(ctx, e.timeout())
			loaded, err := e.store.Load(loadCtx, key.kind, key.subject, key.source)
			cancel()
			if err == nil && e.validResult(loaded, key, request.EgressIP, request.EgressVersion, time.Now()) {
				result = cloneSource(loaded)
				e.putMemoryLocked(key, result)
			}
		}
		e.mu.Unlock()
		if result != nil {
			results = append(results, *result)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, ErrNotChecked
	}
	return aggregate(request.EgressIP, results, e.config.SourcePriority), nil
}

func keyFor(request CheckRequest, name string) cacheKey {
	if sourceCatalog[name].Kind == SourceKindViaNode {
		return cacheKey{SubjectNode, request.NodeHash.Hex(), name}
	}
	return cacheKey{SubjectIP, request.EgressIP.String(), name}
}
func (e *Engine) generationCurrent(request CheckRequest, generation uint64) bool {
	state, exists := e.nodes[request.NodeHash]
	return exists && state.generation == generation && state.ip == request.EgressIP.String() && state.egressVersion == request.EgressVersion
}
func (e *Engine) validResult(result *SourceResult, key cacheKey, ip netip.Addr, version int64, now time.Time) bool {
	if result == nil || result.ConfigFingerprint != e.fingerprint || result.Source != key.source || result.Kind != sourceCatalog[key.source].Kind || result.CheckedAt.IsZero() || !result.ExpiresAt.After(now) {
		return false
	}
	return result.EgressIP == ip.String() && (key.kind != SubjectNode || result.EgressVersion == version)
}
func (e *Engine) memoryLocked(key cacheKey, ip netip.Addr, version int64) *SourceResult {
	element, ok := e.cache[key]
	if !ok {
		return nil
	}
	entry := element.Value.(cacheEntry)
	if !e.validResult(entry.result, key, ip, version, time.Now()) {
		e.lru.Remove(element)
		delete(e.cache, key)
		return nil
	}
	e.lru.MoveToFront(element)
	return cloneSource(entry.result)
}
func (e *Engine) putMemoryLocked(key cacheKey, result *SourceResult) {
	if old := e.cache[key]; old != nil {
		e.lru.Remove(old)
	}
	e.cache[key] = e.lru.PushFront(cacheEntry{key, cloneSource(result)})
	for len(e.cache) > e.config.MaxCacheEntries {
		old := e.lru.Back()
		delete(e.cache, old.Value.(cacheEntry).key)
		e.lru.Remove(old)
	}
}
func (e *Engine) timeout() time.Duration {
	return time.Duration(e.config.RequestTimeoutMS) * time.Millisecond
}

// Close is idempotent and waits for all source workers. Host/Store callbacks
// must honor context cancellation. Concurrent Close callers wait for shutdown.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		done := e.closedDone
		e.mu.Unlock()
		<-done
		return nil
	}
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	e.workers.Wait()
	e.mu.Lock()
	e.cache = map[cacheKey]*list.Element{}
	e.lru.Init()
	close(e.closedDone)
	e.mu.Unlock()
	return nil
}
