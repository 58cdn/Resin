package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/proxy"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/pkg/pluginsdk"
	"golang.org/x/net/http/httpguts"
)

const configureTimeout = 10 * time.Second

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	Store SettingsStore
	// PluginDir holds installed package plugins, one directory per plugin id.
	PluginDir string
	// ExternalEnabled allows package plugins (child processes) and the
	// marketplace. When false only builtin plugins are available.
	ExternalEnabled bool
	// MarketplaceURLs are plugin index URLs (see marketplace.go).
	MarketplaceURLs []string
	ResinVersion    string
	HTTPClient      *http.Client
	Logf            func(format string, args ...any)
	// PlatformName resolves a platform ID for event payloads. Optional.
	PlatformName func(id string) string
	Builtins     []Builtin
}

// Manager owns every plugin: settings, lifecycle, the request hook chain
// and event delivery. Mutations are serialized; the proxy hot path only
// reads an immutable snapshot.
type Manager struct {
	cfg ManagerConfig

	mu       sync.Mutex
	entries  map[string]*entry
	builtins map[string]*Builtin
	stored   map[string]model.PluginSettings
	started  bool
	stopped  bool

	chain atomic.Pointer[chainSnapshot]
}

// entry is the host-side record of one plugin.
type entry struct {
	id       string
	source   string
	manifest pluginsdk.Manifest
	rawMF    []byte // package manifest bytes, used to detect changes on rescan
	dir      string
	builtin  *Builtin
	runtime  pluginsdk.RuntimeSpec
	// loadErr is set when the plugin cannot be started at all (bad manifest,
	// no runtime for this platform, ...).
	loadErr string

	settings model.PluginSettings
	config   json.RawMessage

	inst   instance
	events *eventQueue

	stateMu   sync.Mutex
	status    string
	lastError string
	// stateGen counts state changes, see setStateIf.
	stateGen uint64

	requests    atomic.Int64
	rejects     atomic.Int64
	errors      atomic.Int64
	timeouts    atomic.Int64
	latencyNs   atomic.Int64
	delivered   atomic.Int64
	eventErrors atomic.Int64
	dropped     atomic.Int64
	restarts    atomic.Int64
}

// setState records the status and returns a token for setStateIf.
func (e *entry) setState(status, lastError string) uint64 {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.status = status
	e.lastError = lastError
	e.stateGen++
	return e.stateGen
}

// setStateIf records the status unless it changed since the setState call
// that returned token, for example because the process crashed meanwhile.
func (e *entry) setStateIf(token uint64, status, lastError string) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.stateGen != token {
		return
	}
	e.status = status
	e.lastError = lastError
	e.stateGen++
}

// effectiveConfig fills in config_fields defaults and validates the result.
func (e *entry) effectiveConfig(config json.RawMessage) (json.RawMessage, error) {
	full, err := e.manifest.ApplyConfigDefaults(config)
	if err != nil {
		return nil, err
	}
	if err := e.manifest.ValidateConfig(full); err != nil {
		return nil, err
	}
	return full, nil
}

func (e *entry) state() (string, string) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.status, e.lastError
}

type chainItem struct {
	e          *entry
	inst       instance
	timeout    time.Duration
	failClosed bool
}

type eventSub struct {
	e      *entry
	types  []string
	queue  *eventQueue
	lookup map[string]bool
}

type chainSnapshot struct {
	request []chainItem
	events  []eventSub
}

// NewManager creates a manager. Call Start before use.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	m := &Manager{
		cfg:      cfg,
		entries:  make(map[string]*entry),
		builtins: make(map[string]*Builtin),
		stored:   make(map[string]model.PluginSettings),
	}
	for i := range cfg.Builtins {
		b := &cfg.Builtins[i]
		m.builtins[b.Manifest.ID] = b
	}
	m.chain.Store(&chainSnapshot{})
	return m
}

// ExternalEnabled reports whether package plugins and the marketplace are enabled.
func (m *Manager) ExternalEnabled() bool { return m.cfg.ExternalEnabled }

// PluginDir returns the package directory.
func (m *Manager) PluginDir() string { return m.cfg.PluginDir }

// Start loads settings, discovers plugins and starts the enabled ones.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	m.started = true

	if m.cfg.Store != nil {
		rows, err := m.cfg.Store.ListPluginSettings()
		if err != nil {
			return fmt.Errorf("plugin: load settings: %w", err)
		}
		for _, row := range rows {
			m.stored[row.ID] = row
		}
	}

	ids := make([]string, 0, len(m.builtins))
	for id := range m.builtins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := m.builtins[id]
		if err := b.Manifest.Validate(); err != nil {
			m.cfg.Logf("[plugin] builtin %s has an invalid manifest: %v", id, err)
			continue
		}
		e := &entry{id: id, source: SourceBuiltin, manifest: b.Manifest, builtin: b}
		m.applyStoredSettings(e)
		m.entries[id] = e
	}
	if m.cfg.ExternalEnabled {
		for _, e := range m.discoverPackages() {
			m.entries[e.id] = e
		}
	}

	var wg sync.WaitGroup
	for _, e := range m.entries {
		if !e.settings.Enabled {
			if e.loadErr != "" {
				e.setState(StatusError, e.loadErr)
			} else {
				e.setState(StatusStopped, "")
			}
			continue
		}
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			m.startEntry(ctx, e, e.config)
		}(e)
	}
	wg.Wait()
	m.rebuildChain()
	return nil
}

// Stop shuts every plugin down.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.stopped = true
	m.chain.Store(&chainSnapshot{})
	var wg sync.WaitGroup
	for _, e := range m.entries {
		if e.inst == nil {
			continue
		}
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			m.stopEntry(ctx, e)
		}(e)
	}
	wg.Wait()
}

func (m *Manager) applyStoredSettings(e *entry) {
	if row, ok := m.stored[e.id]; ok {
		e.settings = row
		e.config = json.RawMessage(row.ConfigJSON)
		if len(bytes.TrimSpace(e.config)) == 0 {
			e.config = json.RawMessage(`{}`)
		}
		return
	}
	e.settings = model.PluginSettings{
		ID:        e.id,
		Enabled:   false,
		Priority:  DefaultPriority,
		TimeoutMs: DefaultTimeoutMs,
	}
	e.config = e.manifest.DefaultConfig()
	e.settings.ConfigJSON = string(e.config)
}

// startEntry starts an instance for e with config. Caller holds m.mu.
func (m *Manager) startEntry(ctx context.Context, e *entry, config json.RawMessage) error {
	if e.loadErr != "" {
		e.setState(StatusError, e.loadErr)
		return errors.New(e.loadErr)
	}
	config, err := e.effectiveConfig(config)
	if err != nil {
		err = fmt.Errorf("invalid config: %w", err)
		m.cfg.Logf("[plugin] %s failed to start: %v", e.id, err)
		e.setState(StatusError, err.Error())
		return err
	}
	token := e.setState(StatusStarting, "")
	inst, err := m.newInstance(ctx, e, config)
	if err != nil {
		m.cfg.Logf("[plugin] %s failed to start: %v", e.id, err)
		e.setState(StatusError, err.Error())
		return err
	}
	e.inst = inst
	m.syncEventQueue(ctx, e)
	e.setStateIf(token, StatusRunning, "")
	return nil
}

// syncEventQueue starts or stops e's event worker to match the event
// subscription of its instance. Caller holds m.mu.
func (m *Manager) syncEventQueue(ctx context.Context, e *entry) {
	inst := e.inst
	subscribed := inst != nil && len(inst.Capabilities().Events) > 0
	switch {
	case subscribed && e.events == nil:
		e.events = newEventQueue(defaultEventQueueSize, defaultEventBatchSize, defaultEventFlush,
			inst.HandleEvents,
			func(n int, err error) {
				if err != nil {
					e.eventErrors.Add(1)
					m.cfg.Logf("[plugin] %s event delivery failed (%d events): %v", e.id, n, err)
					return
				}
				e.delivered.Add(int64(n))
			})
	case !subscribed && e.events != nil:
		// Pushes to a stopped queue are no-ops until the caller rebuilds
		// the chain.
		q := e.events
		e.events = nil
		q.stop(ctx)
		e.dropped.Add(q.dropped.Load())
	}
}

func (m *Manager) newInstance(ctx context.Context, e *entry, config json.RawMessage) (instance, error) {
	params := pluginsdk.RegisterParams{
		SchemaVersion: pluginsdk.SchemaVersion,
		ResinVersion:  m.cfg.ResinVersion,
		PluginID:      e.id,
		DataDir:       m.dataDir(e.id),
		Config:        config,
	}
	if e.builtin != nil {
		ctx, cancel := context.WithTimeout(ctx, configureTimeout)
		defer cancel()
		return startBuiltin(ctx, e.builtin, params)
	}
	if !m.cfg.ExternalEnabled {
		return nil, ErrExternalDisabled
	}
	spec := processSpec{
		ID:           e.id,
		Dir:          e.dir,
		DataDir:      params.DataDir,
		Runtime:      e.runtime,
		Declared:     e.manifest.Capabilities,
		ResinVersion: m.cfg.ResinVersion,
		Logf:         m.cfg.Logf,
		OnStatus:     func(status, lastError string) { e.setState(status, lastError) },
		OnRestart:    func() { e.restarts.Add(1) },
	}
	return startProcess(ctx, spec, config)
}

// stopEntry takes e out of the hot path, then stops its event worker and
// instance. Caller holds m.mu.
func (m *Manager) stopEntry(ctx context.Context, e *entry) {
	inst, q := e.inst, e.events
	e.inst, e.events = nil, nil
	// Requests must not reach an instance that is shutting down.
	m.rebuildChain()
	if q != nil {
		q.stop(ctx)
		e.dropped.Add(q.dropped.Load())
	}
	if inst != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := inst.Close(closeCtx); err != nil {
			m.cfg.Logf("[plugin] %s stop: %v", e.id, err)
		}
		cancel()
	}
	e.setState(StatusStopped, "")
}

func (m *Manager) dataDir(id string) string {
	if m.cfg.PluginDir == "" {
		return ""
	}
	return filepath.Join(m.cfg.PluginDir, ".data", id)
}

// rebuildChain publishes a new hot-path snapshot. Caller holds m.mu.
func (m *Manager) rebuildChain() {
	if m.stopped {
		return
	}
	snap := &chainSnapshot{}
	for _, e := range m.entries {
		if !e.settings.Enabled {
			continue
		}
		if e.inst == nil {
			// An enabled fail-closed hook that is not running rejects
			// requests instead of silently letting them through.
			if e.settings.FailClosed && e.manifest.Capabilities.RequestHook {
				snap.request = append(snap.request, chainItem{e: e, failClosed: true})
			}
			continue
		}
		caps := e.inst.Capabilities()
		if caps.RequestHook {
			snap.request = append(snap.request, chainItem{
				e:          e,
				inst:       e.inst,
				timeout:    time.Duration(e.settings.TimeoutMs) * time.Millisecond,
				failClosed: e.settings.FailClosed,
			})
		}
		if e.events != nil && len(caps.Events) > 0 {
			sub := eventSub{e: e, types: caps.Events, queue: e.events, lookup: make(map[string]bool, len(caps.Events))}
			for _, t := range caps.Events {
				sub.lookup[t] = true
			}
			snap.events = append(snap.events, sub)
		}
	}
	sort.Slice(snap.request, func(i, j int) bool {
		a, b := snap.request[i].e, snap.request[j].e
		if a.settings.Priority != b.settings.Priority {
			return a.settings.Priority > b.settings.Priority
		}
		return a.id < b.id
	})
	sort.Slice(snap.events, func(i, j int) bool { return snap.events[i].e.id < snap.events[j].e.id })
	m.chain.Store(snap)
}

// --- request hook (proxy.RequestHook) ---

var _ proxy.RequestHook = (*Manager)(nil)

// Active reports whether any request plugin is running.
func (m *Manager) Active() bool {
	return m != nil && len(m.chain.Load().request) > 0
}

// InspectRequest runs the request plugin chain in priority order.
func (m *Manager) InspectRequest(ctx context.Context, req *pluginsdk.RequestInfo) proxy.RequestHookResult {
	res := proxy.RequestHookResult{Platform: req.Platform, Account: req.Account}
	for _, item := range m.chain.Load().request {
		dec, err := item.call(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				// The client went away; no plugin is to blame.
				res.Canceled = true
				return res
			}
			if item.failClosed {
				res.Reject = proxy.ErrPluginUnavailable
				res.PluginID = item.e.id
				return res
			}
			continue
		}
		if dec == nil {
			continue
		}
		if dec.Action == pluginsdk.ActionReject {
			item.e.rejects.Add(1)
			res.Reject = rejectError(dec)
			res.PluginID = item.e.id
			return res
		}
		if dec.Platform != nil {
			req.Platform = *dec.Platform
			res.Platform = *dec.Platform
		}
		if dec.Account != nil {
			req.Account = *dec.Account
			res.Account = *dec.Account
		}
		res.HeaderOps = appendHeaderOps(res.HeaderOps, req, dec)
	}
	return res
}

var errPluginNotRunning = errors.New("plugin is not running")

func (it chainItem) call(ctx context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	e := it.e
	e.requests.Add(1)
	if it.inst == nil {
		e.errors.Add(1)
		return nil, errPluginNotRunning
	}
	req.TimeoutMs = it.timeout.Milliseconds()
	callCtx, cancel := context.WithTimeout(ctx, it.timeout)
	start := time.Now()
	dec, err := it.inst.Inspect(callCtx, req)
	if err == nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		// In-process plugins are not preempted; treat a late answer as a timeout.
		err = callCtx.Err()
	}
	cancel()
	e.latencyNs.Add(int64(time.Since(start)))
	if err == nil && dec != nil {
		switch dec.Action {
		case "", pluginsdk.ActionContinue, pluginsdk.ActionReject:
		default:
			err = fmt.Errorf("invalid decision action %q", dec.Action)
		}
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			// Cancelled by the caller: not the plugin's fault.
		case errors.Is(err, context.DeadlineExceeded):
			e.timeouts.Add(1)
		default:
			e.errors.Add(1)
		}
		return nil, err
	}
	return dec, nil
}

func rejectError(dec *pluginsdk.RequestDecision) *proxy.ProxyError {
	status := dec.Status
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	msg := strings.TrimSpace(dec.Message)
	if msg == "" {
		msg = proxy.ErrPluginRejected.Message
	}
	return &proxy.ProxyError{HTTPCode: status, ResinError: proxy.ErrPluginRejected.ResinError, Message: msg}
}

// appendHeaderOps converts a decision's header changes into ordered ops and
// applies them to req.Headers so later plugins see the result.
func appendHeaderOps(ops []proxy.HeaderOp, req *pluginsdk.RequestInfo, dec *pluginsdk.RequestDecision) []proxy.HeaderOp {
	if len(dec.RemoveHeaders) == 0 && len(dec.SetHeaders) == 0 {
		return ops
	}
	// No rewritable headers for this request type: the CONNECT request
	// itself never reaches the upstream, and SOCKS5 has no headers.
	if req.IsConnect || req.ProxyType == pluginsdk.ProxyTypeSocks5 {
		return ops
	}
	if req.Headers == nil {
		req.Headers = make(map[string][]string)
	}
	for _, name := range dec.RemoveHeaders {
		key := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !httpguts.ValidHeaderFieldName(key) || proxy.IsProtectedHookHeader(key) {
			continue
		}
		delete(req.Headers, key)
		ops = append(ops, proxy.HeaderOp{Name: key, Remove: true})
	}
	names := make([]string, 0, len(dec.SetHeaders))
	for name := range dec.SetHeaders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		key := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !httpguts.ValidHeaderFieldName(key) || proxy.IsProtectedHookHeader(key) || !httpguts.ValidHeaderFieldValue(dec.SetHeaders[name]) {
			continue
		}
		req.Headers[key] = []string{dec.SetHeaders[name]}
		ops = append(ops, proxy.HeaderOp{Name: key, Value: dec.SetHeaders[name]})
	}
	return ops
}

// --- events ---

// ObserveRequest is registered as proxy.ConfigAwareEventEmitter.RequestObserver.
func (m *Manager) ObserveRequest(entry proxy.RequestLogEntry) {
	if m == nil {
		return
	}
	subs := m.chain.Load().events
	if len(subs) == 0 {
		return
	}
	var ev *lazyEvent
	for i := range subs {
		if !subs[i].lookup[pluginsdk.EventRequestFinished] {
			continue
		}
		if ev == nil {
			// Only this copy escapes to the heap, so requests nobody
			// subscribed to cost no allocation.
			entry := entry
			started := time.Unix(0, entry.StartedAtNs)
			ev = newLazyEvent(pluginsdk.EventRequestFinished, started.Add(time.Duration(entry.DurationNs)), func() any {
				return requestFinishedData(entry, started)
			})
		}
		subs[i].queue.push(ev)
	}
}

// maxEventStringBytes caps free-form strings (URLs, upstream errors) copied
// into event payloads.
const maxEventStringBytes = 4096

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func requestFinishedData(e proxy.RequestLogEntry, started time.Time) pluginsdk.RequestFinishedData {
	return pluginsdk.RequestFinishedData{
		StartedAt:      started.UTC(),
		ProxyType:      proxy.ProxyTypeName(e.ProxyType),
		ClientIP:       e.ClientIP,
		PlatformID:     e.PlatformID,
		PlatformName:   e.PlatformName,
		Account:        e.Account,
		TargetHost:     e.TargetHost,
		TargetURL:      truncateUTF8(e.TargetURL, maxEventStringBytes),
		NodeHash:       e.NodeHash,
		NodeTag:        e.NodeTag,
		EgressIP:       e.EgressIP,
		DurationNs:     e.DurationNs,
		FirstByteNs:    e.FirstByteDurationNs,
		NetOK:          e.NetOK,
		HTTPMethod:     e.HTTPMethod,
		HTTPStatus:     e.HTTPStatus,
		ResinError:     e.ResinError,
		UpstreamStage:  e.UpstreamStage,
		UpstreamErrMsg: truncateUTF8(e.UpstreamErrMsg, maxEventStringBytes),
		IngressBytes:   e.IngressBytes,
		EgressBytes:    e.EgressBytes,
	}
}

// OnLeaseEvent forwards routing lease events. It is called synchronously by
// the router and must not block or take router locks.
func (m *Manager) OnLeaseEvent(e routing.LeaseEvent) {
	if m == nil {
		return
	}
	subs := m.chain.Load().events
	if len(subs) == 0 {
		return
	}
	var typ string
	switch e.Type {
	case routing.LeaseCreate:
		typ = pluginsdk.EventLeaseCreated
	case routing.LeaseReplace:
		typ = pluginsdk.EventLeaseReplaced
	case routing.LeaseRemove:
		typ = pluginsdk.EventLeaseRemoved
	case routing.LeaseExpire:
		typ = pluginsdk.EventLeaseExpired
	default:
		return
	}
	var ev *lazyEvent
	for i := range subs {
		if !subs[i].lookup[typ] {
			continue
		}
		if ev == nil {
			e := e // see ObserveRequest
			platformName := m.cfg.PlatformName
			ev = newLazyEvent(typ, time.Now(), func() any {
				data := pluginsdk.LeaseEventData{PlatformID: e.PlatformID, Account: e.Account}
				if platformName != nil {
					// Resolved on the delivery worker, never under router locks.
					data.PlatformName = platformName(e.PlatformID)
				}
				if !e.NodeHash.IsZero() {
					data.NodeHash = e.NodeHash.Hex()
				}
				if e.EgressIP.IsValid() {
					data.EgressIP = e.EgressIP.String()
				}
				if e.CreatedAtNs > 0 {
					t := time.Unix(0, e.CreatedAtNs).UTC()
					data.CreatedAt = &t
				}
				return data
			})
		}
		subs[i].queue.push(ev)
	}
}

// --- queries ---

// List returns every known plugin sorted by id.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, m.info(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns one plugin.
func (m *Manager) Get(id string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return Info{}, ErrNotFound
	}
	return m.info(e), nil
}

func (m *Manager) info(e *entry) Info {
	status, lastErr := e.state()
	if status == "" {
		status = StatusStopped
	}
	if lastErr == "" && e.loadErr != "" {
		lastErr = e.loadErr
		status = StatusError
	}
	caps := e.manifest.Capabilities
	if e.inst != nil {
		caps = e.inst.Capabilities()
	}
	stats := Stats{
		Requests:        e.requests.Load(),
		Rejects:         e.rejects.Load(),
		Errors:          e.errors.Load(),
		Timeouts:        e.timeouts.Load(),
		EventsDelivered: e.delivered.Load(),
		EventsDropped:   e.dropped.Load(),
		EventErrors:     e.eventErrors.Load(),
		Restarts:        e.restarts.Load(),
	}
	if e.events != nil {
		stats.EventsDropped += e.events.dropped.Load()
	}
	if stats.Requests > 0 {
		stats.AvgLatencyUs = e.latencyNs.Load() / stats.Requests / int64(time.Microsecond)
	}
	fields := e.manifest.ConfigFields
	if fields == nil {
		fields = []pluginsdk.ConfigField{}
	}
	config := e.config
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	info := Info{
		ID:           e.id,
		Name:         e.manifest.Name,
		Version:      e.manifest.Version,
		Description:  e.manifest.Description,
		Author:       e.manifest.Author,
		Homepage:     e.manifest.Homepage,
		License:      e.manifest.License,
		Source:       e.source,
		Capabilities: caps,
		ConfigFields: fields,
		Enabled:      e.settings.Enabled,
		Priority:     e.settings.Priority,
		TimeoutMs:    e.settings.TimeoutMs,
		FailClosed:   e.settings.FailClosed,
		Config:       config,
		Status:       status,
		LastError:    lastErr,
		Stats:        stats,
	}
	if e.settings.CreatedAtNs > 0 {
		info.CreatedAt = time.Unix(0, e.settings.CreatedAtNs).UTC().Format(time.RFC3339Nano)
	}
	if e.settings.UpdatedAtNs > 0 {
		info.UpdatedAt = time.Unix(0, e.settings.UpdatedAtNs).UTC().Format(time.RFC3339Nano)
	}
	return info
}

// --- mutations ---

// Update changes settings of one plugin, starting, reconfiguring or stopping
// it as needed. Nothing is persisted when the runtime change fails.
func (m *Manager) Update(ctx context.Context, id string, u Update) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return Info{}, fmt.Errorf("%w: plugin manager is stopped", ErrConflict)
	}
	e, ok := m.entries[id]
	if !ok {
		return Info{}, ErrNotFound
	}

	next := e.settings
	next.ID = id
	if u.Priority != nil {
		if *u.Priority < MinPriority || *u.Priority > MaxPriority {
			return Info{}, fmt.Errorf("%w: priority must be between %d and %d", ErrInvalidArgument, MinPriority, MaxPriority)
		}
		next.Priority = *u.Priority
	}
	if u.TimeoutMs != nil {
		if *u.TimeoutMs < MinTimeoutMs || *u.TimeoutMs > MaxTimeoutMs {
			return Info{}, fmt.Errorf("%w: timeout_ms must be between %d and %d", ErrInvalidArgument, MinTimeoutMs, MaxTimeoutMs)
		}
		next.TimeoutMs = *u.TimeoutMs
	}
	if u.FailClosed != nil {
		next.FailClosed = *u.FailClosed
	}
	if u.Enabled != nil {
		next.Enabled = *u.Enabled
	}
	if next.Enabled && e.loadErr != "" {
		return Info{}, fmt.Errorf("%w: cannot enable %s: %s", ErrInvalidArgument, id, e.loadErr)
	}

	// prevConfig is what a running instance is configured with.
	prevConfig := e.config
	if full, err := e.effectiveConfig(prevConfig); err == nil {
		prevConfig = full
	}
	newConfig := e.config
	if u.Config != nil || next.Enabled {
		raw := e.config
		if u.Config != nil {
			compacted, err := normalizeConfig(u.Config)
			if err != nil {
				return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
			}
			raw = compacted
		}
		// Stored the way the plugin receives it, with defaults filled in.
		full, err := e.effectiveConfig(raw)
		if err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
		newConfig = full
	}
	configChanged := u.Config != nil && !bytes.Equal(newConfig, prevConfig)
	next.ConfigJSON = string(newConfig)

	startedNew := false
	reconfigured := false
	switch {
	case next.Enabled && e.inst == nil:
		if err := m.startEntry(ctx, e, newConfig); err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrStartFailed, err)
		}
		startedNew = true
	case next.Enabled && configChanged:
		if err := m.configure(ctx, e, newConfig); err != nil {
			if configRejected(e, err) {
				return Info{}, fmt.Errorf("%w: plugin rejected config: %v", ErrInvalidArgument, err)
			}
			// The plugin may have applied the config anyway.
			m.cfg.Logf("[plugin] %s configure failed: %v; restarting it with the previous config", id, err)
			m.restartEntry(ctx, e, prevConfig)
			return Info{}, fmt.Errorf("configure %s: %v (restarted with the previous config)", id, err)
		}
		reconfigured = true
	case !next.Enabled && configChanged && e.builtin != nil:
		// Validate against a throwaway instance so bad configs are caught
		// before the plugin is enabled.
		cctx, cancel := context.WithTimeout(ctx, configureTimeout)
		probe, err := startBuiltin(cctx, e.builtin, pluginsdk.RegisterParams{
			SchemaVersion: pluginsdk.SchemaVersion, ResinVersion: m.cfg.ResinVersion,
			PluginID: id, DataDir: m.dataDir(id), Config: newConfig,
		})
		cancel()
		if err != nil {
			return Info{}, fmt.Errorf("%w: plugin rejected config: %v", ErrInvalidArgument, err)
		}
		_ = probe.Close(ctx)
	}

	now := time.Now().UnixNano()
	if next.CreatedAtNs == 0 {
		next.CreatedAtNs = now
	}
	next.UpdatedAtNs = now
	if m.cfg.Store != nil {
		if err := m.cfg.Store.UpsertPluginSettings(next); err != nil {
			switch {
			case startedNew:
				m.stopEntry(ctx, e)
			case reconfigured:
				if cerr := m.configure(ctx, e, prevConfig); cerr != nil {
					m.cfg.Logf("[plugin] %s: restoring the previous config failed: %v; restarting it", id, cerr)
					m.restartEntry(ctx, e, prevConfig)
				}
			}
			return Info{}, fmt.Errorf("persist plugin settings: %w", err)
		}
	}
	e.settings = next
	e.config = newConfig
	m.stored[id] = next
	if !next.Enabled && e.inst != nil {
		m.stopEntry(ctx, e)
	}
	if !next.Enabled {
		e.setState(StatusStopped, "")
	}
	// A builtin's event subscription can depend on its config.
	m.syncEventQueue(ctx, e)
	m.rebuildChain()
	return m.info(e), nil
}

// configure applies config to e's running instance. Caller holds m.mu.
func (m *Manager) configure(ctx context.Context, e *entry, config json.RawMessage) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), configureTimeout)
	defer cancel()
	return e.inst.Configure(cctx, config)
}

// configRejected reports whether a Configure error means the plugin refused
// the config. After other errors (timeout, crash) the outcome is unknown.
func configRejected(e *entry, err error) bool {
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) {
		return true
	}
	return e.builtin != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
}

// restartEntry restarts e with config, so that the host and the plugin
// agree on the config again. Caller holds m.mu.
func (m *Manager) restartEntry(ctx context.Context, e *entry, config json.RawMessage) {
	ctx = context.WithoutCancel(ctx)
	m.stopEntry(ctx, e)
	_ = m.startEntry(ctx, e, config) // failures are logged and shown in the status
	m.rebuildChain()
}

func normalizeConfig(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, errors.New("config must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Uninstall removes a package plugin, its data and its settings.
func (m *Manager) Uninstall(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return ErrNotFound
	}
	if e.source == SourceBuiltin {
		return fmt.Errorf("%w: builtin plugins cannot be uninstalled; disable it instead", ErrConflict)
	}
	// Settings go first: if that fails nothing has changed, and a later
	// reinstall never inherits them.
	if m.cfg.Store != nil {
		if err := m.cfg.Store.DeletePluginSettings(id); err != nil {
			return fmt.Errorf("delete plugin settings: %w", err)
		}
	}
	delete(m.stored, id)
	delete(m.entries, id)
	m.stopEntry(ctx, e)
	if e.dir != "" {
		if err := os.RemoveAll(e.dir); err != nil {
			if _, statErr := os.Stat(e.dir); statErr == nil {
				// Keep listing what is left so the removal can be retried.
				m.entries[id] = m.loadPackage(id)
				m.rebuildChain()
			}
			return fmt.Errorf("remove plugin directory: %w", err)
		}
	}
	if dd := m.dataDir(id); dd != "" {
		if err := os.RemoveAll(dd); err != nil {
			return fmt.Errorf("plugin removed, but its data directory could not be deleted: %w", err)
		}
	}
	return nil
}

// Rescan re-reads the plugin directory: new packages are added, removed ones
// are stopped, changed ones are restarted, and enabled plugins that are not
// running are retried.
func (m *Manager) Rescan(ctx context.Context) ([]Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, fmt.Errorf("%w: plugin manager is stopped", ErrConflict)
	}
	if m.cfg.ExternalEnabled {
		found := make(map[string]*entry)
		for _, e := range m.discoverPackages() {
			found[e.id] = e
		}
		// Removed and changed packages are swapped out of the map first, so
		// the chain published while they stop never includes them.
		var stale []*entry
		for id, old := range m.entries {
			if old.source != SourcePackage {
				continue
			}
			if fresh, ok := found[id]; ok && bytes.Equal(fresh.rawMF, old.rawMF) && fresh.loadErr == old.loadErr {
				delete(found, id)
				continue
			}
			stale = append(stale, old)
			delete(m.entries, id)
		}
		for id, e := range found {
			m.entries[id] = e
		}
		for _, old := range stale {
			m.stopEntry(ctx, old)
		}
	}
	for _, e := range m.entries {
		if e.settings.Enabled && e.inst == nil {
			_ = m.startEntry(ctx, e, e.config)
		}
	}
	m.rebuildChain()
	out := make([]Info, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, m.info(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// discoverPackages scans PluginDir. Caller holds m.mu.
func (m *Manager) discoverPackages() []*entry {
	if m.cfg.PluginDir == "" {
		return nil
	}
	dirEntries, err := os.ReadDir(m.cfg.PluginDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.cfg.Logf("[plugin] scan %s: %v", m.cfg.PluginDir, err)
		}
		return nil
	}
	var out []*entry
	for _, de := range dirEntries {
		name := de.Name()
		if !de.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !pluginsdk.ValidPluginID(name) {
			m.cfg.Logf("[plugin] skipping %q: directory name is not a valid plugin id", name)
			continue
		}
		if _, isBuiltin := m.builtins[name]; isBuiltin {
			m.cfg.Logf("[plugin] skipping package %q: id is reserved by a builtin plugin", name)
			continue
		}
		out = append(out, m.loadPackage(name))
	}
	return out
}

// loadPackage reads one package directory into an entry (never nil).
func (m *Manager) loadPackage(id string) *entry {
	dir := filepath.Join(m.cfg.PluginDir, id)
	e := &entry{id: id, source: SourcePackage, dir: dir}
	e.manifest = pluginsdk.Manifest{SchemaVersion: pluginsdk.SchemaVersion, ID: id, Name: id, Version: "?"}
	raw, err := os.ReadFile(filepath.Join(dir, pluginsdk.ManifestFileName))
	switch {
	case err != nil:
		e.loadErr = fmt.Sprintf("read %s: %v", pluginsdk.ManifestFileName, err)
	default:
		e.rawMF = raw
		mf, perr := pluginsdk.ParseManifest(raw)
		switch {
		case perr != nil:
			e.loadErr = perr.Error()
		case mf.ID != id:
			e.loadErr = fmt.Sprintf("manifest id %q does not match directory name %q", mf.ID, id)
		default:
			e.manifest = *mf
			rt, ok := mf.RuntimeFor(runtime.GOOS, runtime.GOARCH)
			if verr := checkMinResinVersion(mf.MinResinVersion, m.cfg.ResinVersion); verr != nil {
				e.loadErr = verr.Error()
			} else if !ok {
				e.loadErr = fmt.Sprintf("no runtime for %s-%s", runtime.GOOS, runtime.GOARCH)
			}
			e.runtime = rt
		}
	}
	m.applyStoredSettings(e)
	if e.loadErr != "" {
		e.setState(StatusError, e.loadErr)
	}
	return e
}
