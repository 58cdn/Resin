package ipcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

type fixtureHost struct {
	mu       sync.Mutex
	nodes    map[node.Hash]netip.Addr
	geo      func(context.Context, netip.Addr) (GeoIPInfo, bool)
	via      func(context.Context, node.Hash, *http.Request) (*http.Response, error)
	geoCalls atomic.Int32
	viaCalls atomic.Int32
	versions map[node.Hash]int64
}

func newFixtureHost() *fixtureHost {
	return &fixtureHost{nodes: map[node.Hash]netip.Addr{testHash(1): netip.MustParseAddr("8.8.8.8"), testHash(2): netip.MustParseAddr("8.8.8.8")}}
}
func (h *fixtureHost) NodeSnapshot(ctx context.Context, hash node.Hash) (NodeSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ip, ok := h.nodes[hash]
	return NodeSnapshot{Hash: hash, EgressIP: ip, EgressVersion: h.versions[hash], Healthy: true, Enabled: true}, ok
}
func (h *fixtureHost) GeoIPLookup(ctx context.Context, ip netip.Addr) (GeoIPInfo, bool) {
	h.geoCalls.Add(1)
	if h.geo != nil {
		return h.geo(ctx, ip)
	}
	return GeoIPInfo{Country: "US"}, true
}
func (h *fixtureHost) DoViaNode(ctx context.Context, hash node.Hash, req *http.Request) (*http.Response, error) {
	h.viaCalls.Add(1)
	if h.via != nil {
		return h.via(ctx, hash, req)
	}
	return echoResponse("ip=8.8.8.8\n"), nil
}
func (h *fixtureHost) setIP(hash node.Hash, ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ip == "" {
		delete(h.nodes, hash)
	} else {
		h.nodes[hash] = netip.MustParseAddr(ip)
		if h.versions == nil {
			h.versions = map[node.Hash]int64{}
		}
		h.versions[hash]++
	}
}
func testHash(value byte) node.Hash { var hash node.Hash; hash[0] = value; return hash }
func echoResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}
func newTestEngine(t *testing.T, h Host, store Store, cfg Config) *Engine {
	t.Helper()
	e, err := NewEngine(h, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}
func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func waiters(e *Engine) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	sum := 0
	for _, f := range e.flights {
		sum += f.waiters
	}
	return sum
}

type fixtureStore struct {
	mu      sync.Mutex
	rows    map[cacheKey]*SourceResult
	fail    bool
	deletes int
}

func newFixtureStore() *fixtureStore { return &fixtureStore{rows: map[cacheKey]*SourceResult{}} }
func (s *fixtureStore) Load(ctx context.Context, kind, subject, source string) (*SourceResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, errors.New("private database /secret/path")
	}
	return cloneSource(s.rows[cacheKey{kind, subject, source}]), nil
}
func (s *fixtureStore) Save(ctx context.Context, kind, subject string, result *SourceResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("private database /secret/path")
	}
	s.rows[cacheKey{kind, subject, result.Source}] = cloneSource(result)
	return nil
}
func (s *fixtureStore) DeleteNode(ctx context.Context, hash node.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	for key := range s.rows {
		if key.kind == SubjectNode && key.subject == hash.Hex() {
			delete(s.rows, key)
		}
	}
	return nil
}

func TestEngineCacheForceExpiryAndIsolation(t *testing.T) {
	h := newFixtureHost()
	store := newFixtureStore()
	e := newTestEngine(t, h, store, DefaultConfig())
	ctx := context.Background()
	first, err := e.CheckNode(ctx, testHash(1), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.Country != "US" || first.Summary.RiskLevel != "unknown" || first.Summary.FraudScore != nil || first.Summary.EgressConsistent != nil {
		t.Fatalf("invented summary: %+v", first.Summary)
	}
	if _, err = e.CheckNode(ctx, testHash(1), nil, false); err != nil {
		t.Fatal(err)
	}
	if h.geoCalls.Load() != 1 || h.viaCalls.Load() != 1 {
		t.Fatal("cache miss")
	}
	if _, err = e.CheckNode(ctx, testHash(2), nil, false); err != nil {
		t.Fatal(err)
	}
	if h.geoCalls.Load() != 1 || h.viaCalls.Load() != 2 {
		t.Fatal("by-IP must share, via-node must isolate")
	}
	if _, err = e.CheckNode(ctx, testHash(1), nil, true); err != nil {
		t.Fatal(err)
	}
	if h.geoCalls.Load() != 2 || h.viaCalls.Load() != 3 {
		t.Fatal("force did not bypass cache")
	}
	// Mutating returned structures must never mutate the cache.
	first.Sources[0].Data.Country = "MUTATED"
	first.Sources[1].Data.EgressIPsBySite["bad"] = "127.0.0.1"
	got, err := e.GetNode(ctx, testHash(1))
	if err != nil || got.Summary.Country != "US" {
		t.Fatalf("cache mutated: %+v %v", got, err)
	}
	for _, source := range got.Sources {
		if _, ok := source.Data.EgressIPsBySite["bad"]; ok {
			t.Fatal("map aliased")
		}
	}
	e.mu.Lock()
	for _, element := range e.cache {
		entry := element.Value.(cacheEntry)
		entry.result.ExpiresAt = time.Now().Add(-time.Second)
	}
	e.mu.Unlock()
	store.mu.Lock()
	for _, result := range store.rows {
		result.ExpiresAt = time.Now().Add(-time.Second)
	}
	store.mu.Unlock()
	if _, err = e.GetIP(ctx, netip.MustParseAddr("8.8.8.8")); !errors.Is(err, ErrNotChecked) {
		t.Fatalf("expired get: %v", err)
	}
	if _, err = e.Lookup(ctx, netip.MustParseAddr("8.8.8.8"), nil, false); err != nil {
		t.Fatal(err)
	}
	if h.geoCalls.Load() != 3 {
		t.Fatal("expired result reused")
	}
}

func TestPersistentReuseConfigInvalidationAndDisabledSource(t *testing.T) {
	h := newFixtureHost()
	store := newFixtureStore()
	cfg := DefaultConfig()
	e := newTestEngine(t, h, store, cfg)
	if _, err := e.CheckNode(context.Background(), testHash(1), nil, false); err != nil {
		t.Fatal(err)
	}
	_ = e.Close()
	reused := newTestEngine(t, h, store, cfg)
	if _, err := reused.CheckNode(context.Background(), testHash(1), nil, false); err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 1 || h.geoCalls.Load() != 1 {
		t.Fatal("persisted cache not reused")
	}
	cfg.ConsistencyTargets = []string{"https://example.com/new-trace"}
	changed := newTestEngine(t, h, store, cfg)
	if _, err := changed.CheckNode(context.Background(), testHash(1), []string{"egress-consistency"}, false); err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 2 {
		t.Fatal("old target configuration cache reused")
	}
	cfg.EnabledSources = []string{"db-ip"}
	disabled := newTestEngine(t, h, store, cfg)
	got, err := disabled.CheckNode(context.Background(), testHash(1), []string{"egress-consistency"}, false)
	if err != nil || len(got.Sources) != 1 || got.Sources[0].Status != "unavailable" || got.Sources[0].OK {
		t.Fatalf("disabled source revived: %+v %v", got, err)
	}
	if h.viaCalls.Load() != 2 {
		t.Fatal("disabled source performed network I/O")
	}
}

func TestSingleflightForceSharedIPAndCancellation(t *testing.T) {
	h := newFixtureHost()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.geo = func(ctx context.Context, ip netip.Addr) (GeoIPInfo, bool) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return GeoIPInfo{Country: "US"}, true
		case <-ctx.Done():
			return GeoIPInfo{}, false
		}
	}
	e := newTestEngine(t, h, nil, DefaultConfig())
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	errorsCh := make(chan error, 3)
	go func() { _, err := e.CheckNode(ctx1, testHash(1), []string{"db-ip"}, false); errorsCh <- err }()
	<-started
	go func() {
		_, err := e.CheckNode(context.Background(), testHash(2), []string{"db-ip"}, true)
		errorsCh <- err
	}()
	go func() {
		_, err := e.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"), []string{"db-ip"}, true)
		errorsCh <- err
	}()
	waitFor(t, func() bool { return waiters(e) == 3 })
	cancel1()
	if err := <-errorsCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller: %v", err)
	}
	waitFor(t, func() bool { return waiters(e) == 2 })
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if h.geoCalls.Load() != 1 {
		t.Fatalf("singleflight count=%d", h.geoCalls.Load())
	}
}

func TestLastWaiterCancellationAndClose(t *testing.T) {
	h := newFixtureHost()
	started := make(chan struct{}, 4)
	stopped := make(chan struct{}, 4)
	h.via = func(ctx context.Context, hash node.Hash, req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return nil, ctx.Err()
	}
	e := newTestEngine(t, h, nil, DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.CheckNode(ctx, testHash(1), []string{"egress-consistency"}, false); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("last waiter did not cancel work")
	}
	go func() {
		_, err := e.CheckNode(context.Background(), testHash(1), []string{"egress-consistency"}, false)
		done <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() { _ = e.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel work")
	}
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("close result: %v", err)
	}
	if _, err := e.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"), nil, false); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed lookup: %v", err)
	}
}

func TestEgressChangeAndDeletionInvalidatesOnlyNode(t *testing.T) {
	h := newFixtureHost()
	store := newFixtureStore()
	e := newTestEngine(t, h, store, DefaultConfig())
	ctx := context.Background()
	if _, err := e.CheckNode(ctx, testHash(1), nil, false); err != nil {
		t.Fatal(err)
	}
	h.setIP(testHash(1), "1.1.1.1")
	if _, err := e.GetNode(ctx, testHash(1)); !errors.Is(err, ErrNotChecked) {
		t.Fatalf("old egress reused: %v", err)
	}
	if _, err := e.GetIP(ctx, netip.MustParseAddr("8.8.8.8")); err != nil {
		t.Fatalf("by-IP was invalidated: %v", err)
	}
	if _, err := e.CheckNode(ctx, testHash(1), []string{"egress-consistency"}, false); err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 2 {
		t.Fatal("did not refetch after egress change")
	}
	h.setIP(testHash(1), "")
	if _, err := e.GetNode(ctx, testHash(1)); !errors.Is(err, ErrNodeNotFound) {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for key := range store.rows {
		if key.kind == SubjectNode {
			t.Fatal("node result remains after deletion")
		}
	}
}

func TestChangedEgressCannotPublishInflightResult(t *testing.T) {
	h := newFixtureHost()
	started := make(chan struct{})
	release := make(chan struct{})
	h.via = func(ctx context.Context, hash node.Hash, req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			return echoResponse("8.8.8.8"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	store := newFixtureStore()
	e := newTestEngine(t, h, store, DefaultConfig())
	done := make(chan error, 1)
	go func() {
		_, err := e.CheckNode(context.Background(), testHash(1), []string{"egress-consistency"}, false)
		done <- err
	}()
	<-started
	h.setIP(testHash(1), "1.1.1.1")
	close(release)
	if err := <-done; !errors.Is(err, ErrNodeChanged) {
		t.Fatalf("stale in-flight result: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.rows) != 0 {
		t.Fatal("stale result persisted")
	}
}

func TestSourceFailureIsolationUnknownAndUnavailable(t *testing.T) {
	h := newFixtureHost()
	h.via = func(context.Context, node.Hash, *http.Request) (*http.Response, error) {
		return nil, errors.New("secret-token /srv/private/db.sqlite https://internal.example/")
	}
	e := newTestEngine(t, h, &fixtureStore{fail: true}, DefaultConfig())
	got, err := e.CheckNode(context.Background(), testHash(1), []string{"egress-consistency", "db-ip", "ippure", "ipdata"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Country != "US" || got.Summary.FraudScore != nil || got.Summary.ASN != nil || got.Summary.IPOrigin != "unknown" || got.Summary.EgressConsistent != nil {
		t.Fatalf("wrong summary: %+v", got.Summary)
	}
	for _, source := range got.Sources {
		if strings.Contains(fmt.Sprintf("%+v", source), "secret-token") || strings.Contains(source.Error, "/srv") {
			t.Fatal("internal error leaked")
		}
		if source.Source == "ippure" || source.Source == "ipdata" {
			if source.Status != "unavailable" || source.OK {
				t.Fatal("unsupported source claimed success")
			}
		}
	}
	h2 := newFixtureHost()
	h2.geo = func(context.Context, netip.Addr) (GeoIPInfo, bool) { return GeoIPInfo{}, false }
	e2 := newTestEngine(t, h2, nil, DefaultConfig())
	unavailable, err := e2.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"), nil, false)
	if err != nil || unavailable.Sources[0].Status != "unavailable" || unavailable.Summary.Country != "unknown" {
		t.Fatalf("missing database: %+v %v", unavailable, err)
	}
}

func TestConcurrencyAndMemoryBounds(t *testing.T) {
	h := newFixtureHost()
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	h.geo = func(ctx context.Context, ip netip.Addr) (GeoIPInfo, bool) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return GeoIPInfo{Country: "US"}, true
	}
	cfg := DefaultConfig()
	cfg.MaxConcurrentChecks = 2
	cfg.MaxCacheEntries = 2
	e := newTestEngine(t, h, nil, cfg)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 1; i <= 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.Lookup(context.Background(), netip.MustParseAddr(fmt.Sprintf("8.8.8.%d", i)), nil, false)
			errs <- err
		}(i)
	}
	waitFor(t, func() bool { return active.Load() == 2 })
	time.Sleep(10 * time.Millisecond)
	e.mu.Lock()
	if len(e.flights) > 8 {
		t.Fatal("pending flight bound exceeded")
	}
	e.mu.Unlock()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	if maxActive.Load() > 2 {
		t.Fatal("execution bound exceeded")
	}
	e.mu.Lock()
	if len(e.cache) > 2 {
		t.Fatal("cache bound exceeded")
	}
	e.mu.Unlock()
	for i := 3; i < 100; i++ {
		_, _ = e.GetNode(context.Background(), testHash(byte(i)))
	}
	e.mu.Lock()
	if len(e.nodes) > 2 {
		t.Fatal("missing nodes grew state")
	}
	e.mu.Unlock()
	for i := 3; i < 10; i++ {
		h.setIP(testHash(byte(i)), "8.8.8.8")
		_, _ = e.GetNode(context.Background(), testHash(byte(i)))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.nodes) > 2 {
		t.Fatal("node state bound exceeded")
	}
}

func TestValidationAndNotChecked(t *testing.T) {
	e := newTestEngine(t, newFixtureHost(), nil, DefaultConfig())
	ctx := context.Background()
	if _, err := e.GetIP(ctx, netip.MustParseAddr("8.8.8.8")); !errors.Is(err, ErrNotChecked) {
		t.Fatal(err)
	}
	for _, ip := range []string{"127.0.0.1", "192.168.1.1", "203.0.113.1"} {
		if _, err := e.Lookup(ctx, netip.MustParseAddr(ip), nil, false); err == nil {
			t.Fatal("private lookup allowed")
		}
		if _, err := e.GetIP(ctx, netip.MustParseAddr(ip)); err == nil {
			t.Fatal("private cache read allowed")
		}
	}
	for _, sources := range [][]string{{"egress-consistency"}, {"missing"}, {"db-ip", "db-ip"}} {
		if _, err := e.Lookup(ctx, netip.MustParseAddr("8.8.8.8"), sources, false); err == nil {
			t.Fatalf("accepted %v", sources)
		}
	}
	if _, err := e.CheckNode(ctx, testHash(99), nil, false); !errors.Is(err, ErrNodeNotFound) {
		t.Fatal(err)
	}
}

func TestEgressVersionInvalidatesRoundTripAndRestart(t *testing.T) {
	h := newFixtureHost()
	store := newFixtureStore()
	e := newTestEngine(t, h, store, DefaultConfig())
	ctx := context.Background()
	if _, err := e.CheckNode(ctx, testHash(1), []string{"egress-consistency"}, false); err != nil {
		t.Fatal(err)
	}
	h.setIP(testHash(1), "1.1.1.1")
	h.setIP(testHash(1), "8.8.8.8")
	if _, err := e.GetNode(ctx, testHash(1)); !errors.Is(err, ErrNotChecked) {
		t.Fatalf("A-B-A reused old observation: %v", err)
	}
	if _, err := e.CheckNode(ctx, testHash(1), []string{"egress-consistency"}, false); err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 2 {
		t.Fatal("A-B-A did not refetch")
	}
	_ = e.Close()
	h.setIP(testHash(1), "1.1.1.1")
	h.setIP(testHash(1), "8.8.8.8")
	restarted := newTestEngine(t, h, store, DefaultConfig())
	if _, err := restarted.GetNode(ctx, testHash(1)); !errors.Is(err, ErrNotChecked) {
		t.Fatalf("restart reused old epoch: %v", err)
	}
	if _, err := restarted.CheckNode(ctx, testHash(1), []string{"egress-consistency"}, false); err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 3 {
		t.Fatal("restart did not refetch")
	}
}

func TestUnknownNodeReadsNeverQueuePersistenceWrites(t *testing.T) {
	h := newFixtureHost()
	store := newFixtureStore()
	e := newTestEngine(t, h, store, DefaultConfig())
	for i := 3; i < 250; i++ {
		if _, err := e.GetNode(context.Background(), testHash(byte(i))); !errors.Is(err, ErrNodeNotFound) {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	count := len(e.nodes)
	e.mu.Unlock()
	store.mu.Lock()
	defer store.mu.Unlock()
	if count != 0 || store.deletes != 0 || len(store.rows) != 0 {
		t.Fatalf("missing-node reads allocated state: %d nodes, %d writes", count, store.deletes)
	}
}

func TestTimeoutReportsSourceErrorAndOtherSourceSurvives(t *testing.T) {
	h := newFixtureHost()
	h.via = func(ctx context.Context, _ node.Hash, _ *http.Request) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cfg := DefaultConfig()
	cfg.RequestTimeoutMS = 100
	e := newTestEngine(t, h, nil, cfg)
	result, err := e.CheckNode(context.Background(), testHash(1), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Country != "US" {
		t.Fatal("timeout suppressed independent source")
	}
	for _, source := range result.Sources {
		if source.Source == "egress-consistency" && (source.OK || source.Status != "error" || len(source.Data.TargetErrors) != 1) {
			t.Fatalf("bad timeout result: %+v", source)
		}
	}
}

func TestAggregatePriorityMaxFraudUnionAndUnknown(t *testing.T) {
	low, high := 25.0, 75.0
	asn := uint32(123)
	sources := []SourceResult{
		{Source: "second", OK: true, Data: Data{Country: "CA", IPType: "datacenter", IPOrigin: "broadcast", FraudScore: &high, AbuseFlags: []string{"vpn", "spam"}, ASN: &asn}},
		{Source: "first", OK: true, Data: Data{Country: "US", IPType: "residential", FraudScore: &low, AbuseFlags: []string{"spam"}}},
		{Source: "failed", OK: false, Data: Data{City: "Must not trust", IPType: "mobile"}},
	}
	result := aggregate(netip.MustParseAddr("8.8.8.8"), sources, []string{"first", "second"})
	if result.Summary.Country != "US" || result.Summary.IPType != "residential" || result.Summary.IPOrigin != "broadcast" || result.Summary.FraudScore == nil || *result.Summary.FraudScore != 75 || result.Summary.City != "unknown" || len(result.Summary.AbuseFlags) != 2 || result.Summary.RiskLevel != "unknown" {
		t.Fatalf("bad aggregation: %+v", result.Summary)
	}
	*result.Summary.ASN = 999
	if *sources[1].Data.ASN == 999 {
		t.Fatal("summary aliased source")
	}
}
