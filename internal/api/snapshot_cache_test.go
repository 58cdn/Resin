package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSnapshotClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeSnapshotClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeSnapshotClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestSnapshotCache(ttl time.Duration) (*snapshotCache, *fakeSnapshotClock) {
	clock := &fakeSnapshotClock{now: time.Unix(1_700_000_000, 0)}
	cache := newSnapshotCache(ttl)
	cache.now = clock.Now
	return cache, clock
}

func TestSnapshotCache_ReusesResponseUntilTTL(t *testing.T) {
	cache, clock := newTestSnapshotCache(3 * time.Second)
	computes := 0
	compute := func() map[string]any {
		computes++
		return map[string]any{"n": computes}
	}

	if got := cache.get("", compute); got["n"] != 1 {
		t.Fatalf("first get: got n=%v, want 1", got["n"])
	}
	clock.Advance(3*time.Second - time.Millisecond)
	if got := cache.get("", compute); got["n"] != 1 {
		t.Fatalf("get within TTL: got n=%v, want the cached 1", got["n"])
	}
	clock.Advance(time.Millisecond)
	if got := cache.get("", compute); got["n"] != 2 {
		t.Fatalf("get at TTL: got n=%v, want a new computation", got["n"])
	}
}

func TestSnapshotCache_TTLCountsFromComputeStart(t *testing.T) {
	cache, clock := newTestSnapshotCache(3 * time.Second)
	computes := 0
	compute := func() map[string]any {
		computes++
		clock.Advance(2 * time.Second) // a slow scan
		return map[string]any{"n": computes}
	}

	cache.get("", compute)
	clock.Advance(time.Second)
	if got := cache.get("", compute); got["n"] != 2 {
		t.Fatalf("get 3s after the first scan started: got n=%v, want a new computation", got["n"])
	}
}

func TestSnapshotCache_KeysAreSeparate(t *testing.T) {
	cache, _ := newTestSnapshotCache(3 * time.Second)
	computes := make(map[string]int)
	computeFor := func(key string) func() map[string]any {
		return func() map[string]any {
			computes[key]++
			return map[string]any{"key": key}
		}
	}

	for _, key := range []string{"", "platform-a", "platform-b", "", "platform-a"} {
		if got := cache.get(key, computeFor(key)); got["key"] != key {
			t.Fatalf("get(%q): got response for %v", key, got["key"])
		}
	}
	for _, key := range []string{"", "platform-a", "platform-b"} {
		if computes[key] != 1 {
			t.Fatalf("computes for %q: got %d, want 1", key, computes[key])
		}
	}
}

func TestSnapshotCache_ConcurrentMissesShareOneComputation(t *testing.T) {
	cache, _ := newTestSnapshotCache(3 * time.Second)
	var computes atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	compute := func() map[string]any {
		n := computes.Add(1)
		if n == 1 {
			close(started)
		}
		<-release
		return map[string]any{"n": n}
	}

	const callers = 8
	results := make(chan map[string]any, callers)
	go func() { results <- cache.get("", compute) }()
	<-started
	for i := 1; i < callers; i++ {
		go func() { results <- cache.get("", compute) }()
	}
	// Give the other callers time to wait on the running computation. A caller
	// that arrives after it finishes gets the stored response, so the checks
	// below hold either way.
	time.Sleep(20 * time.Millisecond)
	close(release)

	for i := 0; i < callers; i++ {
		if got := <-results; got["n"] != int32(1) {
			t.Fatalf("result %d: got n=%v, want the first computation", i, got["n"])
		}
	}
	if n := computes.Load(); n != 1 {
		t.Fatalf("computes: got %d, want 1", n)
	}
}

func TestSnapshotCache_PanicDoesNotBlockLaterCalls(t *testing.T) {
	cache, _ := newTestSnapshotCache(3 * time.Second)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("get did not propagate the panic")
			}
		}()
		cache.get("", func() map[string]any { panic("scan failed") })
	}()

	done := make(chan map[string]any, 1)
	go func() {
		done <- cache.get("", func() map[string]any { return map[string]any{"ok": true} })
	}()
	select {
	case got := <-done:
		if got["ok"] != true {
			t.Fatalf("get after panic: got %v, want a new computation", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("get after a panicking computation is still blocked")
	}
}

func TestSnapshotCache_StoreDropsExpiredEntries(t *testing.T) {
	cache, clock := newTestSnapshotCache(3 * time.Second)
	compute := func() map[string]any { return map[string]any{} }

	cache.get("deleted-platform", compute)
	clock.Advance(3 * time.Second)
	cache.get("platform-a", compute)

	if _, ok := cache.entries["deleted-platform"]; ok {
		t.Fatal("expired entry was kept")
	}
	if _, ok := cache.entries["platform-a"]; !ok || len(cache.entries) != 1 {
		t.Fatalf("entries: got %d, want only platform-a", len(cache.entries))
	}
}
