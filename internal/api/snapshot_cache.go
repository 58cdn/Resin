package api

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// snapshotCacheTTL is how long a snapshot response is reused. Snapshot
// endpoints scan the whole node pool, or a platform's view, on every request,
// and the WebUI polls them every 5 seconds per open page. A TTL below the poll
// interval keeps each page's data as fresh as before, while other pages, tabs
// and viewers, and refetches on window focus or remount, share a computation.
const snapshotCacheTTL = 3 * time.Second

// snapshotCache reuses snapshot responses per key for a short TTL, and
// concurrent misses for the same key share a single computation. Handlers
// validate the request, including that the platform exists, before calling
// get, so errors are never cached and a deleted platform gets 404 at once.
type snapshotCache struct {
	ttl   time.Duration
	now   func() time.Time
	group singleflight.Group

	mu      sync.Mutex
	entries map[string]snapshotCacheEntry
}

type snapshotCacheEntry struct {
	resp      map[string]any
	expiresAt time.Time
}

func newSnapshotCache(ttl time.Duration) *snapshotCache {
	return &snapshotCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]snapshotCacheEntry),
	}
}

// get returns the response cached under key, calling compute when there is
// none or it has expired. The TTL counts from when compute started. The
// returned map is shared between callers and must not be modified.
func (c *snapshotCache) get(key string, compute func() map[string]any) map[string]any {
	if resp, ok := c.lookup(key); ok {
		return resp
	}
	v, _, _ := c.group.Do(key, func() (any, error) {
		// Another caller may have stored a response after the lookup above.
		if resp, ok := c.lookup(key); ok {
			return resp, nil
		}
		start := c.now()
		resp := compute()
		c.store(key, resp, start.Add(c.ttl))
		return resp, nil
	})
	return v.(map[string]any)
}

func (c *snapshotCache) lookup(key string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !c.now().Before(entry.expiresAt) {
		return nil, false
	}
	return entry.resp, true
}

// store also drops expired entries, such as those of deleted platforms, so the
// map only holds recently requested keys.
func (c *snapshotCache) store(key string, resp map[string]any, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = snapshotCacheEntry{resp: resp, expiresAt: expiresAt}
}
