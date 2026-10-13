package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"testing"

	"github.com/Resinat/Resin/internal/ipcheck"
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
		t.Fatal("background accepted")
	}
}
