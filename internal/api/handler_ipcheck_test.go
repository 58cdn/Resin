package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
	"github.com/Resinat/Resin/internal/service"
)

var ipCheckTestHash = node.HashFromRawOptions([]byte(`{"type":"direct","tag":"IP quality fixture"}`))

// All source I/O is supplied by this fixture host; these tests never contact
// the configured Cloudflare URL or any other external network service.
type ipCheckAPIHost struct {
	nodes        map[node.Hash]ipcheck.NodeSnapshot
	geoAvailable bool
	geoCalls     atomic.Int64
	viaCalls     atomic.Int64
	viaError     error
}

func (h *ipCheckAPIHost) Capability(name string) (any, bool) {
	if name == builtin.IPQualityHostCapability {
		return h, true
	}
	return nil, false
}

func (h *ipCheckAPIHost) NodeSnapshot(_ context.Context, hash node.Hash) (ipcheck.NodeSnapshot, bool) {
	n, ok := h.nodes[hash]
	return n, ok
}

func (h *ipCheckAPIHost) GeoIPLookup(_ context.Context, _ netip.Addr) (ipcheck.GeoIPInfo, bool) {
	h.geoCalls.Add(1)
	return ipcheck.GeoIPInfo{Country: "us", City: "Fixture City", ASOrg: "Fixture Network"}, h.geoAvailable
}

func (h *ipCheckAPIHost) DoViaNode(_ context.Context, _ node.Hash, _ *http.Request) (*http.Response, error) {
	h.viaCalls.Add(1)
	if h.viaError != nil {
		return nil, h.viaError
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ip=8.8.8.8\n"))}, nil
}

func newIPCheckAPIServer(t *testing.T, enabled bool) (*Server, *service.ControlPlaneService, *plugin.Manager, *ipCheckAPIHost) {
	t.Helper()
	srv, cp, _ := newControlPlaneTestServer(t)
	host := &ipCheckAPIHost{
		nodes: map[node.Hash]ipcheck.NodeSnapshot{
			ipCheckTestHash: {Hash: ipCheckTestHash, EgressIP: netip.MustParseAddr("8.8.8.8"), Enabled: true, Healthy: true},
		},
		geoAvailable: true,
	}
	mgr := plugin.NewManager(plugin.ManagerConfig{
		Store: cp.Engine, PluginDir: t.TempDir(), Builtins: builtin.All(), BuiltinHost: host,
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Stop(context.Background()) })
	cp.Plugins = mgr
	if enabled {
		if _, err := mgr.Update(context.Background(), builtin.IPQualityID, plugin.Update{Enabled: &enabled}); err != nil {
			t.Fatal(err)
		}
	}
	return srv, cp, mgr, host
}

func decodeIPCheckResult(t *testing.T, body []byte) ipcheck.Result {
	t.Helper()
	var result ipcheck.Result
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode IP check result: %v; body=%s", err, body)
	}
	return result
}

func TestIPCheckAPI_RequiresAuth(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/actions/check-ip", `{}`},
		{http.MethodGet, "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/ip-check", ""},
		{http.MethodGet, "/api/v1/ip-checks/8.8.8.8", ""},
		{http.MethodPost, "/api/v1/ip-checks/lookup", `{"ip":"8.8.8.8"}`},
		{http.MethodGet, "/api/v1/ip-checks/sources", ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rec := doJSONRequest(t, srv, tc.method, tc.path, tc.body, false)
			tpExpectError(t, rec, http.StatusUnauthorized, "UNAUTHORIZED", "")
		})
	}
}

func TestIPCheckAPI_StrictBodyAndAddressValidation(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)
	nodePath := "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/actions/check-ip"
	for _, body := range []string{"", "null", "[]", "true", `{`, `{} {}`, `{"unexpected":true}`, `{"force":"true"}`, `{"sources":"db-ip"}`, `{"sources":[1]}`} {
		t.Run("node body "+body, func(t *testing.T) {
			tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, nodePath, body, true), http.StatusBadRequest, "INVALID_ARGUMENT", "")
		})
	}
	for _, body := range []string{"", "null", "[]", `{}`, `{"ip":1}`, `{"ip":"invalid"}`, `{"ip":"127.0.0.1"}`, `{"ip":"10.0.0.1"}`, `{"ip":"fe80::1%lo0"}`, `{"ip":"8.8.8.8","extra":1}`, `{"ip":"8.8.8.8"} {}`} {
		t.Run("lookup body "+body, func(t *testing.T) {
			tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, "/api/v1/ip-checks/lookup", body, true), http.StatusBadRequest, "INVALID_ARGUMENT", "")
		})
	}
	for _, path := range []string{"/api/v1/nodes/invalid/ip-check", "/api/v1/ip-checks/invalid", "/api/v1/ip-checks/127.0.0.1"} {
		tpExpectError(t, doJSONRequest(t, srv, http.MethodGet, path, nil, true), http.StatusBadRequest, "INVALID_ARGUMENT", "")
	}
	tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/invalid/actions/check-ip", `{}`, true), http.StatusBadRequest, "INVALID_ARGUMENT", "node_hash")
}

func TestIPCheckAPI_BodyLimit(t *testing.T) {
	srv, _, _ := newControlPlaneTestServerWithBodyLimit(t, 128)
	for _, path := range []string{"/api/v1/ip-checks/lookup", "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/actions/check-ip"} {
		body := `{"sources":["` + strings.Repeat("x", 512) + `"]}`
		tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, path, body, true), http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "")
	}
}

func TestIPCheckAPI_DisabledAndMissingManager(t *testing.T) {
	for _, missingManager := range []bool{false, true} {
		srv, cp, _, _ := newIPCheckAPIServer(t, false)
		if missingManager {
			cp.Plugins = nil
		}
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/ip-checks/sources", ""},
			{http.MethodGet, "/api/v1/ip-checks/8.8.8.8", ""},
			{http.MethodGet, "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/ip-check", ""},
			{http.MethodPost, "/api/v1/ip-checks/lookup", `{"ip":"8.8.8.8"}`},
			{http.MethodPost, "/api/v1/nodes/" + ipCheckTestHash.Hex() + "/actions/check-ip", `{}`},
		} {
			tpExpectError(t, doJSONRequest(t, srv, tc.method, tc.path, tc.body, true), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "disabled or unavailable")
		}
	}
}

func TestIPCheckAPI_NotFoundNotCheckedAndNoEgress(t *testing.T) {
	srv, _, _, host := newIPCheckAPIServer(t, true)
	missingHash := node.HashFromRawOptions([]byte(`{"type":"missing"}`)).Hex()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/nodes/" + missingHash + "/ip-check", ""},
		{http.MethodPost, "/api/v1/nodes/" + missingHash + "/actions/check-ip", `{}`},
	} {
		tpExpectError(t, doJSONRequest(t, srv, tc.method, tc.path, tc.body, true), http.StatusNotFound, "NOT_FOUND", "node not found")
	}
	for _, path := range []string{"/api/v1/nodes/" + ipCheckTestHash.Hex() + "/ip-check", "/api/v1/ip-checks/8.8.8.8"} {
		tpExpectError(t, doJSONRequest(t, srv, http.MethodGet, path, nil, true), http.StatusNotFound, "NOT_FOUND", "not checked")
	}
	if host.geoCalls.Load() != 0 || host.viaCalls.Load() != 0 {
		t.Fatal("cache reads must not perform checks")
	}
	host.nodes[ipCheckTestHash] = ipcheck.NodeSnapshot{Hash: ipCheckTestHash}
	tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/"+ipCheckTestHash.Hex()+"/actions/check-ip", `{}`, true), http.StatusConflict, "CONFLICT", "egress IP")
}

func TestIPCheckAPI_ByIPCannotUseViaNodeSources(t *testing.T) {
	srv, _, _, host := newIPCheckAPIServer(t, true)
	for _, sources := range [][]string{
		{"egress-consistency"}, {"cloudflare-challenge"}, {"ai-region"},
		{"db-ip", "egress-consistency"}, {"not-a-source"}, {"db-ip", "db-ip"},
	} {
		rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/ip-checks/lookup", map[string]any{"ip": "8.8.8.8", "sources": sources}, true)
		tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "")
	}
	if host.geoCalls.Load() != 0 || host.viaCalls.Load() != 0 {
		t.Fatal("invalid source selection must be rejected before any source runs")
	}
}

func TestIPCheckAPI_LookupCacheForceAndDisable(t *testing.T) {
	srv, _, mgr, host := newIPCheckAPIServer(t, true)
	body := `{"ip":"8.8.8.8","sources":["db-ip"]}`
	for i := 0; i < 2; i++ {
		rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/ip-checks/lookup", body, true)
		tpExpectStatus(t, rec, http.StatusOK)
		result := decodeIPCheckResult(t, rec.Body.Bytes())
		if result.EgressIP != "8.8.8.8" || result.Summary.Country != "us" || len(result.Sources) != 1 || !result.Sources[0].OK {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if host.geoCalls.Load() != 1 || host.viaCalls.Load() != 0 {
		t.Fatalf("cached lookup calls: geo=%d via=%d", host.geoCalls.Load(), host.viaCalls.Load())
	}
	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/ip-checks/8.8.8.8", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/ip-checks/lookup", `{"ip":"::ffff:8.8.8.8","sources":["db-ip"],"force":true}`, true)
	tpExpectStatus(t, rec, http.StatusOK)
	if host.geoCalls.Load() != 2 || decodeIPCheckResult(t, rec.Body.Bytes()).EgressIP != "8.8.8.8" {
		t.Fatal("force must bypass valid cache and normalize IPv4-mapped addresses")
	}
	disabled := false
	if _, err := mgr.Update(context.Background(), builtin.IPQualityID, plugin.Update{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	tpExpectError(t, doJSONRequest(t, srv, http.MethodGet, "/api/v1/ip-checks/8.8.8.8", nil, true), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "")
}

func TestIPCheckAPI_NodeCheckAndUnavailableSource(t *testing.T) {
	srv, _, _, host := newIPCheckAPIServer(t, true)
	host.geoAvailable = false
	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/"+ipCheckTestHash.Hex()+"/actions/check-ip", `{"sources":["egress-consistency","db-ip"]}`, true)
	tpExpectStatus(t, rec, http.StatusOK)
	result := decodeIPCheckResult(t, rec.Body.Bytes())
	if len(result.Sources) != 2 || result.Summary.Country != "unknown" || result.Summary.ASN != nil {
		t.Fatalf("unknown GeoIP fields must not be fabricated: %+v", result)
	}
	for _, source := range result.Sources {
		if source.Source == "db-ip" && (source.OK || source.Status != "unavailable" || source.Error == "") {
			t.Fatalf("database absence must be reported as unavailable: %+v", source)
		}
	}
	if host.viaCalls.Load() == 0 || host.geoCalls.Load() != 1 {
		t.Fatal("node check must run the requested sources")
	}
	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes/"+ipCheckTestHash.Hex()+"/ip-check", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)

	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/ip-checks/lookup", `{"ip":"8.8.8.8","sources":["ippure"]}`, true)
	tpExpectStatus(t, rec, http.StatusOK)
	result = decodeIPCheckResult(t, rec.Body.Bytes())
	if len(result.Sources) != 1 || result.Sources[0].OK || result.Sources[0].Status != "unavailable" || result.Summary.FraudScore != nil {
		t.Fatalf("unverified provider must not fabricate a score: %+v", result)
	}
}

func TestIPCheckAPI_SourceStatusAndErrorRedaction(t *testing.T) {
	const fakeSecret = "fixture-key-do-not-disclose"
	const fakePath = "/private/resin/runtime/cache.db"
	t.Setenv("RESIN_IP_CHECK_IPDATA_KEY", fakeSecret)
	srv, _, _, host := newIPCheckAPIServer(t, true)
	host.viaError = errors.New("request failed: key=" + fakeSecret + "; database=" + fakePath)
	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/ip-checks/sources", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	var listing struct {
		Items []ipcheck.SourceStatus `json:"items"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Items) == 0 || listing.Total != len(listing.Items) {
		t.Fatalf("unexpected source status envelope: %s", rec.Body.String())
	}
	var sawUnavailable bool
	for _, source := range listing.Items {
		if source.Name == "ippure" {
			sawUnavailable = !source.Available && source.Reason != ""
		}
	}
	if !sawUnavailable {
		t.Fatal("unverified provider must remain visibly unavailable")
	}
	statusBody := rec.Body.String()
	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/"+ipCheckTestHash.Hex()+"/actions/check-ip", `{"sources":["egress-consistency"]}`, true)
	tpExpectStatus(t, rec, http.StatusOK)
	result := decodeIPCheckResult(t, rec.Body.Bytes())
	if len(result.Sources) != 1 || result.Sources[0].OK || result.Sources[0].Status != "error" {
		t.Fatalf("failed source must not report success: %+v", result)
	}
	for _, denied := range []string{fakeSecret, fakePath, "plugin_dir", "consistency_targets", "config_fingerprint", "RESIN_IP_CHECK_IPDATA_KEY"} {
		if strings.Contains(statusBody+rec.Body.String(), denied) {
			t.Fatalf("IP check API leaked %q", denied)
		}
	}
}
