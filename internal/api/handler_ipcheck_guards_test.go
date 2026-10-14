package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
)

// The embedded fixture returns fixed responses without opening a socket.
// The real router, service, builtin lifecycle and cache handle the request.
type guardedIPCheckAPIHost struct {
	*ipCheckAPIHost
	guards *requestguard.Controller
	store  ipcheck.Store
}

func (h *guardedIPCheckAPIHost) RequestGuards() *requestguard.Controller { return h.guards }
func (h *guardedIPCheckAPIHost) Capability(name string) (any, bool) {
	switch name {
	case builtin.IPQualityHostCapability:
		return h, true
	case builtin.IPQualityStoreCapability:
		return h.store, true
	default:
		return nil, false
	}
}

func TestIPCheckAPI_AllSuppressedIs200WithoutObservationAndPreservesCache(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	host := &guardedIPCheckAPIHost{
		ipCheckAPIHost: &ipCheckAPIHost{nodes: map[node.Hash]ipcheck.NodeSnapshot{
			ipCheckTestHash: {Hash: ipCheckTestHash, EgressIP: netip.MustParseAddr("8.8.8.8"), Enabled: true, Healthy: true},
		}},
		guards: requestguard.New(func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }),
		store:  cp.Engine.IPCheckStore(),
	}
	manager := plugin.NewManager(plugin.ManagerConfig{Store: cp.Engine, PluginDir: t.TempDir(), Builtins: builtin.All(), BuiltinHost: host})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Stop(context.Background()) })
	cp.Plugins = manager
	enabled := true
	cfg := json.RawMessage(`{"enabled_sources":["egress-consistency"],"source_guards":[{"source":"egress-consistency","enabled":true,"qps":100,"burst":64,"daily_limit":1,"consecutive_failure_threshold":1,"open_for":"1m"}]}`)
	if _, err := manager.Update(context.Background(), builtin.IPQualityID, plugin.Update{Enabled: &enabled, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	postPath := "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/actions/check-ip"
	getPath := "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/ip-check"
	body := `{"sources":["egress-consistency"],"force":true}`
	tpExpectStatus(t, doJSONRequest(t, srv, http.MethodPost, postPath, body, false), http.StatusUnauthorized)
	if host.viaCalls.Load() != 0 {
		t.Fatal("unauthenticated request executed a source")
	}
	first := doJSONRequest(t, srv, http.MethodPost, postPath, body, true)
	tpExpectStatus(t, first, http.StatusOK)
	observation := decodeIPCheckResult(t, first.Body.Bytes())
	if len(observation.Sources) != 1 || !observation.Sources[0].OK || observation.CheckedAt.IsZero() || host.viaCalls.Load() != 1 {
		t.Fatalf("first permitted observation missing: %s", first.Body.String())
	}
	before := doJSONRequest(t, srv, http.MethodGet, getPath, nil, true)
	tpExpectStatus(t, before, http.StatusOK)

	blocked := doJSONRequest(t, srv, http.MethodPost, postPath, body, true)
	tpExpectStatus(t, blocked, http.StatusOK)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(blocked.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, present := envelope["checked_at"]; present {
		t.Fatalf("control decision fabricated an observation time: %s", blocked.Body.String())
	}
	result := decodeIPCheckResult(t, blocked.Body.Bytes())
	if len(result.Sources) != 0 || !result.CheckedAt.IsZero() || result.Summary.EgressConsistent != nil || len(result.Summary.EgressIPsBySite) != 0 {
		t.Fatalf("suppression was published as a source observation: %s", blocked.Body.String())
	}
	if len(result.SuppressedSources) != 1 {
		t.Fatalf("suppression metadata missing: %s", blocked.Body.String())
	}
	suppressed := result.SuppressedSources[0]
	if suppressed.Source != "egress-consistency" || suppressed.Kind != ipcheck.SourceKindViaNode || suppressed.Admitted != 0 || suppressed.Suppressed != 1 || len(suppressed.Reasons) != 1 || suppressed.Reasons[0].Reason != requestguard.DailyLimited || suppressed.Reasons[0].Count != 1 {
		t.Fatalf("wrong local suppression metadata: %+v", suppressed)
	}
	if host.viaCalls.Load() != 1 || host.geoCalls.Load() != 0 {
		t.Fatal("suppressed request performed additional source I/O")
	}

	for _, reconfigure := range []bool{false, true} {
		if reconfigure {
			// Replace the Engine without changing observation inputs, then read
			// through the real Store (including its pending journal). Suppression
			// must preserve it as well as the original engine's memory cache;
			// this does not claim that the journal has been flushed to disk.
			next := json.RawMessage(`{"enabled_sources":["egress-consistency"],"source_guards":[{"source":"egress-consistency","enabled":true,"qps":99,"burst":64,"daily_limit":1,"consecutive_failure_threshold":1,"open_for":"1m"}]}`)
			if _, err := manager.Update(context.Background(), builtin.IPQualityID, plugin.Update{Config: next}); err != nil {
				t.Fatal(err)
			}
		}
		after := doJSONRequest(t, srv, http.MethodGet, getPath, nil, true)
		tpExpectStatus(t, after, http.StatusOK)
		if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
			t.Fatalf("GET changed old observation after suppression (reconfigure=%v): before=%s after=%s", reconfigure, before.Body.String(), after.Body.String())
		}
		cached := decodeIPCheckResult(t, after.Body.Bytes())
		if len(cached.SuppressedSources) != 0 || host.viaCalls.Load() != 1 {
			t.Fatal("cache read returned control metadata or performed a probe")
		}
	}
}
