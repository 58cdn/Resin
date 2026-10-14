package ipcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

func backgroundConfig() Config {
	cfg := DefaultConfig()
	cfg.BackgroundEnabled = true
	cfg.BackgroundNodes = []string{testHash(1).Hex(), testHash(2).Hex()}
	cfg.BackgroundSources = []string{"egress-consistency"}
	cfg.BackgroundInterval = "5m"
	return cfg
}

func TestBackgroundConfigExplicitBounds(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			edit func(*Config)
		}{
			{"bad_hash", func(c *Config) { c.BackgroundNodes = []string{"bad"} }},
			{"duplicate_hash", func(c *Config) { c.BackgroundNodes = []string{strings.Repeat("ab", 16), strings.Repeat("AB", 16)} }},
			{"too_many_nodes", func(c *Config) {
				for i := 3; i <= 51; i++ {
					c.BackgroundNodes = append(c.BackgroundNodes, testHash(byte(i)).Hex())
				}
			}},
			{"short_interval", func(c *Config) { c.BackgroundInterval = "4m59s" }},
			{"long_interval", func(c *Config) { c.BackgroundInterval = "24h1s" }},
			{"invalid_interval", func(c *Config) { c.BackgroundInterval = "daily" }},
			{"duplicate_source", func(c *Config) { c.BackgroundSources = []string{"db-ip", "db-ip"} }},
			{"unknown_source", func(c *Config) { c.BackgroundSources = []string{"unknown"} }},
			{"keyed_source", func(c *Config) { c.EnabledSources = []string{"ipdata"}; c.BackgroundSources = []string{"ipdata"} }},
			{"disabled_source", func(c *Config) { c.EnabledSources = []string{"db-ip"} }},
			{"unconfigured_optional", func(c *Config) {
				c.EnabledSources = []string{"cloudflare-challenge"}
				c.BackgroundSources = []string{"cloudflare-challenge"}
			}},
		} {
			t.Run(fmt.Sprintf("%s_enabled_%t", tc.name, enabled), func(t *testing.T) {
				cfg := backgroundConfig()
				cfg.BackgroundEnabled = enabled
				tc.edit(&cfg)
				if err := cfg.Validate(); err == nil {
					t.Fatal("invalid schedule accepted")
				}
			})
		}
	}
	for _, field := range []string{"nodes", "sources", "interval"} {
		cfg := backgroundConfig()
		switch field {
		case "nodes":
			cfg.BackgroundNodes = nil
		case "sources":
			cfg.BackgroundSources = nil
		case "interval":
			cfg.BackgroundInterval = ""
		}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	for _, raw := range []string{`{"background_nodes":null}`, `{"background_sources":null}`, `{"background_interval":null}`, `{"background_enabled":false,"BACKGROUND_ENABLED":true}`} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("ambiguous config accepted: %s", raw)
		}
	}
	cfg := backgroundConfig()
	cfg.BackgroundInterval = "24h"
	cfg.EnabledSources = []string{"db-ip", "cloudflare-challenge", "ai-region"}
	cfg.BackgroundSources = append([]string(nil), cfg.EnabledSources...)
	cfg.CloudflareChallengeURLs = []string{"https://example.com/"}
	cfg.AIRegionTargets = map[string]AIRegionTargetConfig{"chatgpt": {URL: "https://example.com/region"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundActivationDelayIdempotenceAndCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFixtureHost()
		cfg := backgroundConfig()
		cfg.NodeResultTTL = "11m"
		e := newTestEngine(t, h, nil, cfg)
		defer e.Close()
		// Caller mutation cannot change the prepared schedule.
		cfg.BackgroundNodes[0] = testHash(9).Hex()
		cfg.BackgroundSources[0] = "db-ip"
		time.Sleep(20 * time.Minute)
		if h.viaCalls.Load() != 0 {
			t.Fatal("construction started background work")
		}
		e.ActivateBackground()
		e.ActivateBackground()
		synctest.Wait()
		time.Sleep(5*time.Minute - time.Nanosecond)
		if h.viaCalls.Load() != 0 {
			t.Fatal("checked before first interval")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if h.viaCalls.Load() != 2 || h.geoCalls.Load() != 0 {
			t.Fatal("wrong explicit nodes/sources or duplicate worker")
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if h.viaCalls.Load() != 2 {
			t.Fatal("valid cache was refreshed")
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if h.viaCalls.Load() != 4 {
			t.Fatal("expired cache was not refreshed")
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		e.ActivateBackground()
		time.Sleep(time.Hour)
		if h.viaCalls.Load() != 4 {
			t.Fatal("closed engine reactivated")
		}
	})
}

type backgroundEligibilityHost struct{ *fixtureHost }

func (h backgroundEligibilityHost) NodeSnapshot(ctx context.Context, hash node.Hash) (NodeSnapshot, bool) {
	s, exists := h.fixtureHost.NodeSnapshot(ctx, hash)
	if hash == testHash(1) {
		s.Healthy = false
	}
	if hash == testHash(2) {
		s.Enabled = false
	}
	return s, exists
}

func TestBackgroundSkipsIneligibleNodesAndDefaultOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFixtureHost()
		h.nodes[testHash(3)] = netip.MustParseAddr("127.0.0.1")
		h.nodes[testHash(5)] = netip.MustParseAddr("8.8.8.8")
		cfg := backgroundConfig()
		cfg.BackgroundNodes = nil
		for i := 1; i <= 5; i++ {
			cfg.BackgroundNodes = append(cfg.BackgroundNodes, testHash(byte(i)).Hex())
		}
		cfg.BackgroundEnabled = false
		off := newTestEngine(t, h, nil, cfg)
		defer off.Close()
		off.ActivateBackground()
		time.Sleep(time.Hour)
		if h.viaCalls.Load() != 0 {
			t.Fatal("disabled scheduler ran")
		}
		cfg.BackgroundEnabled = true
		e := newTestEngine(t, backgroundEligibilityHost{h}, nil, cfg)
		defer e.Close()
		e.ActivateBackground()
		synctest.Wait()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if h.viaCalls.Load() != 1 {
			t.Fatalf("ineligible checks executed: %d", h.viaCalls.Load())
		}
	})
}

func TestBackgroundFairCursorAndFixedDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFixtureHost()
		cfg := backgroundConfig()
		cfg.RequestTimeoutMS = 59000
		cfg.BackgroundNodes = nil
		for i := 1; i <= 8; i++ {
			cfg.BackgroundNodes = append(cfg.BackgroundNodes, testHash(byte(i)).Hex())
			h.nodes[testHash(byte(i))] = netip.MustParseAddr("8.8.8.8")
		}
		var order []node.Hash
		var orderMu sync.Mutex
		snapshotOrder := func() []node.Hash {
			orderMu.Lock()
			defer orderMu.Unlock()
			return append([]node.Hash(nil), order...)
		}
		h.via = func(ctx context.Context, hash node.Hash, _ *http.Request) (*http.Response, error) {
			orderMu.Lock()
			order = append(order, hash)
			orderMu.Unlock()
			<-ctx.Done()
			return nil, ctx.Err()
		}
		e := newTestEngine(t, h, nil, cfg)
		defer e.Close()
		e.ActivateBackground()
		synctest.Wait()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := snapshotOrder(); len(got) != 6 {
			t.Fatalf("cycle exceeded budget: %v", got)
		}
		time.Sleep(5*time.Minute - time.Nanosecond)
		synctest.Wait()
		if len(snapshotOrder()) != 6 {
			t.Fatal("cycle overlapped or caught up")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := snapshotOrder(); len(got) != 7 || got[6] != testHash(7) {
			t.Fatalf("cursor starved later nodes: %v", got)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if waiters(e) != 0 {
			t.Fatal("Close left background check running")
		}
	})
}

func TestBackgroundSharesManualFlightAndCancelsOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFixtureHost()
		cfg := backgroundConfig()
		cfg.BackgroundNodes = cfg.BackgroundNodes[:1]
		cfg.RequestTimeoutMS = 60000
		h.via = func(ctx context.Context, _ node.Hash, _ *http.Request) (*http.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		e := newTestEngine(t, h, nil, cfg)
		defer e.Close()
		e.ActivateBackground()
		synctest.Wait()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		done := make(chan error, 1)
		go func() {
			_, err := e.CheckNode(context.Background(), testHash(1), cfg.BackgroundSources, true)
			done <- err
		}()
		synctest.Wait()
		if h.viaCalls.Load() != 1 || waiters(e) != 2 {
			t.Fatal("manual force did not share scheduled flight")
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatal("closed check returned success")
		}
		time.Sleep(time.Hour)
		if h.viaCalls.Load() != 1 {
			t.Fatal("worker survived close")
		}
	})
}

func TestBackgroundFingerprintPreservesLegacyPersistentCache(t *testing.T) {
	// Exact default config serialized by the pre-scheduler engine after cloning.
	legacy := `{"enabled_sources":["egress-consistency","db-ip"],"ip_result_ttl":"24h","node_result_ttl":"6h","request_timeout_ms":10000,"consistency_targets":["https://cloudflare.com/cdn-cgi/trace"],"cloudflare_challenge_urls":null,"ai_region_targets":{},"background_enabled":false,"max_concurrent_checks":8,"max_cache_entries":4096,"source_priority":["db-ip","egress-consistency"]}`
	digest := sha256.Sum256([]byte(legacy))
	h := newFixtureHost()
	store := newFixtureStore()
	base := newTestEngine(t, h, store, DefaultConfig())
	if base.fingerprint != hex.EncodeToString(digest[:]) {
		t.Fatal("legacy fingerprint changed")
	}
	if _, err := base.CheckNode(context.Background(), testHash(1), nil, false); err != nil {
		t.Fatal(err)
	}
	cfg := backgroundConfig()
	next := newTestEngine(t, h, store, cfg)
	if _, err := next.CheckNode(context.Background(), testHash(1), nil, false); err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 1 || h.geoCalls.Load() != 1 {
		t.Fatal("schedule-only config invalidated persisted observation")
	}
	cfg.ConsistencyTargets = []string{"https://example.com/ip"}
	changed := newTestEngine(t, h, store, cfg)
	if changed.fingerprint == next.fingerprint {
		t.Fatal("observation target change did not invalidate fingerprint")
	}
}

func TestBackgroundConcurrentActivateClose(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newFixtureHost()
		e := newTestEngine(t, h, nil, backgroundConfig())
		var wg sync.WaitGroup
		for j := 0; j < 10; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); e.ActivateBackground(); _ = e.Close() }()
		}
		wg.Wait()
		if h.viaCalls.Load() != 0 {
			t.Fatal("concurrent activation made an immediate check")
		}
	}
}
