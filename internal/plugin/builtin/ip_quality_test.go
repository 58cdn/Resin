package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin"
)

type qualityTestHost struct{}

func (qualityTestHost) Capability(name string) (any, bool) {
	if name == IPQualityHostCapability {
		return qualityTestHost{}, true
	}
	return nil, false
}
func (qualityTestHost) NodeSnapshot(context.Context, node.Hash) (ipcheck.NodeSnapshot, bool) {
	return ipcheck.NodeSnapshot{}, false
}
func (qualityTestHost) DoViaNode(context.Context, node.Hash, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected network")
}
func (qualityTestHost) GeoIPLookup(context.Context, netip.Addr) (ipcheck.GeoIPInfo, bool) {
	return ipcheck.GeoIPInfo{}, false
}

func TestIPQualityManagerLifecycle(t *testing.T) {
	ctx := context.Background()
	manager := plugin.NewManager(plugin.ManagerConfig{Builtins: All(), BuiltinHost: qualityTestHost{}})
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)
	if _, err := manager.BuiltinService(IPQualityID); !errors.Is(err, plugin.ErrConflict) {
		t.Fatalf("disabled service: %v", err)
	}
	enabled := true
	if _, err := manager.Update(ctx, IPQualityID, plugin.Update{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	svc, err := manager.BuiltinService(IPQualityID)
	if err != nil {
		t.Fatal(err)
	}
	old := svc.(*ipcheck.Engine)
	cfg := json.RawMessage(`{"enabled_sources":["db-ip"]}`)
	if _, err := manager.Update(ctx, IPQualityID, plugin.Update{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Lookup(ctx, netip.MustParseAddr("8.8.8.8"), nil, false); !errors.Is(err, ipcheck.ErrClosed) {
		t.Fatalf("old service: %v", err)
	}
	svc, err = manager.BuiltinService(IPQualityID)
	if err != nil {
		t.Fatal(err)
	}
	current := svc.(*ipcheck.Engine)
	manager.Stop(ctx)
	if _, err := manager.BuiltinService(IPQualityID); !errors.Is(err, plugin.ErrConflict) {
		t.Fatalf("stopped service: %v", err)
	}
	if _, err := current.Lookup(ctx, netip.MustParseAddr("8.8.8.8"), nil, false); !errors.Is(err, ipcheck.ErrClosed) {
		t.Fatalf("closed engine: %v", err)
	}
}

func TestIPQualityMissingHostAndBadConfig(t *testing.T) {
	p := ipQualityBuiltin().NewWithHost(nil)
	if err := p.Configure(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing host accepted")
	}
	p = ipQualityBuiltin().NewWithHost(qualityTestHost{})
	if err := p.Configure(context.Background(), json.RawMessage(`{"background_enabled":true}`)); err == nil {
		t.Fatal("incomplete background configuration accepted")
	}
}

type periodicTestHost struct{ calls atomic.Int32 }

func (h *periodicTestHost) Capability(name string) (any, bool) {
	return h, name == IPQualityHostCapability
}
func (h *periodicTestHost) NodeSnapshot(_ context.Context, hash node.Hash) (ipcheck.NodeSnapshot, bool) {
	return ipcheck.NodeSnapshot{Hash: hash, EgressIP: netip.MustParseAddr("8.8.8.8"), Healthy: true, Enabled: true}, true
}
func (h *periodicTestHost) DoViaNode(context.Context, node.Hash, *http.Request) (*http.Response, error) {
	panic("offline periodic test must never dial")
}
func (h *periodicTestHost) GeoIPLookup(context.Context, netip.Addr) (ipcheck.GeoIPInfo, bool) {
	h.calls.Add(1)
	return ipcheck.GeoIPInfo{Country: "US"}, true
}

type periodicTestStore struct {
	mu   sync.Mutex
	row  model.PluginSettings
	gate func() error
}

func (s *periodicTestStore) ListPluginSettings() ([]model.PluginSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.ID == "" {
		return nil, nil
	}
	return []model.PluginSettings{s.row}, nil
}
func (s *periodicTestStore) UpsertPluginSettings(row model.PluginSettings) error {
	if s.gate != nil {
		if err := s.gate(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row = row
	return nil
}
func (s *periodicTestStore) DeletePluginSettings(string) error { return nil }

func TestIPQualityPeriodicCommittedLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host, store := &periodicTestHost{}, &periodicTestStore{}
		manager := plugin.NewManager(plugin.ManagerConfig{Builtins: All(), BuiltinHost: host, Store: store})
		if err := manager.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer manager.Stop(context.Background())
		cfg := json.RawMessage(`{"enabled_sources":["db-ip"],"ip_result_ttl":"1s","background_enabled":true,"background_nodes":["01000000000000000000000000000000"],"background_sources":["db-ip"],"background_interval":"5m"}`)
		if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Config: cfg}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Hour)
		if host.calls.Load() != 0 {
			t.Fatal("disabled config validation scheduled work")
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseStore := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseStore() // release Store before deferred Stop, even on Fatal
		store.gate = func() error { close(entered); <-release; return nil }
		done := make(chan error, 1)
		enabled := true
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		go func() { _, err := manager.Update(ctx, IPQualityID, plugin.Update{Enabled: &enabled}); done <- err }()
		<-entered
		synctest.Wait()
		time.Sleep(15 * time.Minute)
		if host.calls.Load() != 0 {
			t.Fatal("uncommitted plugin scheduled work")
		}
		releaseStore()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		store.gate = nil
		synctest.Wait()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if host.calls.Load() != 1 {
			t.Fatal("committed schedule failed or inherited canceled request")
		}
		store.gate = func() error { return errors.New("store unavailable") }
		next := json.RawMessage(`{"enabled_sources":["db-ip"],"ip_result_ttl":"1s","background_enabled":true,"background_nodes":["01000000000000000000000000000000"],"background_sources":["db-ip"],"background_interval":"10m"}`)
		if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Config: next}); err == nil {
			t.Fatal("failed persistence accepted")
		}
		synctest.Wait()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if host.calls.Load() != 2 {
			t.Fatal("rollback did not restore old interval")
		}
		store.gate = nil
		if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Config: next}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if host.calls.Load() != 2 {
			t.Fatal("old schedule survived replacement")
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if host.calls.Load() != 3 {
			t.Fatal("new committed interval did not run")
		}
		enabled = false
		if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Enabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Hour)
		if host.calls.Load() != 3 {
			t.Fatal("disabled schedule survived")
		}
		enabled = true
		if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Enabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		manager.Stop(context.Background())
		time.Sleep(time.Hour)
		if host.calls.Load() != 3 {
			t.Fatal("stopped manager scheduled work")
		}
	})
}
