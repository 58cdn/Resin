package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Resinat/Resin/internal/metrics"
)

// countingRuntimeStats counts calls to the stats methods that scan nodes.
type countingRuntimeStats struct {
	testRuntimeStatsProvider
	calls map[string]int
}

func newCountingRuntimeStats(platforms ...string) *countingRuntimeStats {
	platformSet := make(map[string]struct{}, len(platforms))
	for _, id := range platforms {
		platformSet[id] = struct{}{}
	}
	return &countingRuntimeStats{
		testRuntimeStatsProvider: testRuntimeStatsProvider{
			testPlatformStats: testPlatformStats{
				platforms:            platformSet,
				totalNodes:           20,
				healthyNodes:         15,
				egressIPCount:        6,
				healthyEgressIPCount: 4,
			},
			testNodeLatencyData: testNodeLatencyProvider{
				global:   []float64{50, 150},
				platform: map[string][]float64{"platform-a": {80}},
			},
		},
		calls: make(map[string]int),
	}
}

func (s *countingRuntimeStats) HealthyNodes() int {
	s.calls["HealthyNodes"]++
	return s.testRuntimeStatsProvider.HealthyNodes()
}

func (s *countingRuntimeStats) EgressIPCount() int {
	s.calls["EgressIPCount"]++
	return s.testRuntimeStatsProvider.EgressIPCount()
}

func (s *countingRuntimeStats) UniqueHealthyEgressIPCount() int {
	s.calls["UniqueHealthyEgressIPCount"]++
	return s.testRuntimeStatsProvider.UniqueHealthyEgressIPCount()
}

func (s *countingRuntimeStats) PlatformEgressIPCount(platformID string) (int, bool) {
	s.calls["PlatformEgressIPCount/"+platformID]++
	return s.testRuntimeStatsProvider.PlatformEgressIPCount(platformID)
}

func (s *countingRuntimeStats) CollectNodeEWMAs(platformID string) []float64 {
	s.calls["CollectNodeEWMAs/"+platformID]++
	return s.testRuntimeStatsProvider.CollectNodeEWMAs(platformID)
}

func newCountingMetricsManager(t *testing.T, stats *countingRuntimeStats) *metrics.Manager {
	t.Helper()

	repo, err := metrics.NewMetricsRepo(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("NewMetricsRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	return metrics.NewManager(metrics.ManagerConfig{Repo: repo, RuntimeStats: stats})
}

func serveSnapshot(t *testing.T, handler http.Handler, target string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body=%s", target, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func assertCallCounts(t *testing.T, calls map[string]int, want map[string]int) {
	t.Helper()

	for name, n := range want {
		if calls[name] != n {
			t.Fatalf("%s calls: got %d, want %d (all calls: %v)", name, calls[name], n, calls)
		}
	}
}

func TestSnapshotNodePool_ReusesCachedResponse(t *testing.T) {
	stats := newCountingRuntimeStats()
	handler := HandleSnapshotNodePool(newCountingMetricsManager(t, stats))

	first := serveSnapshot(t, handler, "/api/v1/metrics/snapshots/node-pool")
	second := serveSnapshot(t, handler, "/api/v1/metrics/snapshots/node-pool")
	if second != first {
		t.Fatalf("second response differs from the cached one:\nfirst:  %s\nsecond: %s", first, second)
	}
	assertCallCounts(t, stats.calls, map[string]int{
		"HealthyNodes":               1,
		"EgressIPCount":              1,
		"UniqueHealthyEgressIPCount": 1,
	})
}

func TestSnapshotPlatformNodePool_CachesPerPlatform(t *testing.T) {
	stats := newCountingRuntimeStats("platform-a", "platform-b")
	handler := HandleSnapshotPlatformNodePool(newCountingMetricsManager(t, stats))
	targetA := "/api/v1/metrics/snapshots/platform-node-pool?platform_id=platform-a"
	targetB := "/api/v1/metrics/snapshots/platform-node-pool?platform_id=platform-b"

	firstA := serveSnapshot(t, handler, targetA)
	serveSnapshot(t, handler, targetB)
	if secondA := serveSnapshot(t, handler, targetA); secondA != firstA {
		t.Fatalf("second platform-a response differs from the cached one:\nfirst:  %s\nsecond: %s", firstA, secondA)
	}
	assertCallCounts(t, stats.calls, map[string]int{
		"PlatformEgressIPCount/platform-a": 1,
		"PlatformEgressIPCount/platform-b": 1,
	})

	// The platform check runs before the cache, so a deleted platform is not
	// served from it.
	delete(stats.platforms, "platform-a")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, targetA, nil))
	assertNotFoundError(t, rec)
}

func TestSnapshotNodeLatencyDistribution_CachesPerScope(t *testing.T) {
	stats := newCountingRuntimeStats("platform-a")
	handler := HandleSnapshotNodeLatencyDistribution(newCountingMetricsManager(t, stats))

	cases := []struct {
		target      string
		scope       string
		platformID  any
		sampleCount float64
	}{
		{"/api/v1/metrics/snapshots/node-latency-distribution", "global", nil, 2},
		{"/api/v1/metrics/snapshots/node-latency-distribution?platform_id=platform-a", "platform", "platform-a", 1},
	}
	for range 2 {
		for _, tc := range cases {
			var body map[string]any
			if err := json.Unmarshal([]byte(serveSnapshot(t, handler, tc.target)), &body); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if body["scope"] != tc.scope || body["platform_id"] != tc.platformID || body["sample_count"] != tc.sampleCount {
				t.Fatalf("GET %s: got scope=%v platform_id=%v sample_count=%v, want %v %v %v",
					tc.target, body["scope"], body["platform_id"], body["sample_count"], tc.scope, tc.platformID, tc.sampleCount)
			}
		}
	}
	assertCallCounts(t, stats.calls, map[string]int{
		"CollectNodeEWMAs/":           1,
		"CollectNodeEWMAs/platform-a": 1,
	})
}
