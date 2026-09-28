package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
	"github.com/Resinat/Resin/internal/service"
)

// tpAttachPluginManager wires a real plugin.Manager (builtins only, backed by
// the test server's state engine) into cp.
func tpAttachPluginManager(t *testing.T, cp *service.ControlPlaneService, external bool) *plugin.Manager {
	t.Helper()
	mgr := plugin.NewManager(plugin.ManagerConfig{
		Store:           cp.Engine,
		PluginDir:       t.TempDir(),
		ExternalEnabled: external,
		Builtins:        builtin.All(),
		Logf:            func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("plugin manager start: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(context.Background()) })
	cp.Plugins = mgr
	return mgr
}

func tpPluginServer(t *testing.T, external bool) (*Server, *service.ControlPlaneService, *plugin.Manager) {
	t.Helper()
	srv, cp, _ := newControlPlaneTestServer(t)
	mgr := tpAttachPluginManager(t, cp, external)
	return srv, cp, mgr
}

func tpDecodeInfo(t *testing.T, rec *httptest.ResponseRecorder) plugin.Info {
	t.Helper()
	var info plugin.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode plugin info: %v body=%q", err, rec.Body.String())
	}
	return info
}

func tpExpectStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, status, rec.Body.String())
	}
}

func tpExpectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, msgContains string) {
	t.Helper()
	tpExpectStatus(t, rec, status)
	var er ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("unmarshal error response: %v body=%q", err, rec.Body.String())
	}
	if er.Error.Code != code {
		t.Fatalf("error code = %q, want %q, body=%s", er.Error.Code, code, rec.Body.String())
	}
	if !strings.Contains(er.Error.Message, msgContains) {
		t.Fatalf("error message = %q, want it to contain %q", er.Error.Message, msgContains)
	}
}

func tpItemIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items is %T, want array: %v", body["items"], body)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item is %T, want object", it)
		}
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestPluginAPI_ListEnvelope(t *testing.T) {
	srv, _, _ := tpPluginServer(t, false)

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body := decodeJSONMap(t, rec)
	for _, key := range []string{"items", "total", "limit", "offset"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("list envelope missing %q: %v", key, body)
		}
	}
	wantIDs := []string{builtin.AccessControlID, builtin.HeaderRewriteID, builtin.WebhookID}
	if got := tpItemIDs(t, body); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("item ids = %v, want %v", got, wantIDs)
	}
	if body["total"] != float64(3) || body["limit"] != float64(defaultPageLimit) || body["offset"] != float64(0) {
		t.Fatalf("envelope = total %v limit %v offset %v", body["total"], body["limit"], body["offset"])
	}

	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins?limit=1&offset=1", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body = decodeJSONMap(t, rec)
	if got := tpItemIDs(t, body); !reflect.DeepEqual(got, []string{builtin.HeaderRewriteID}) {
		t.Fatalf("paged item ids = %v", got)
	}
	if body["total"] != float64(3) || body["limit"] != float64(1) || body["offset"] != float64(1) {
		t.Fatalf("paged envelope = total %v limit %v offset %v", body["total"], body["limit"], body["offset"])
	}

	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins?offset=10", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body = decodeJSONMap(t, rec)
	if got := tpItemIDs(t, body); len(got) != 0 || body["total"] != float64(3) {
		t.Fatalf("offset past end = items %v total %v", got, body["total"])
	}

	for _, q := range []string{"limit=-1", "limit=abc", "offset=-1"} {
		rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins?"+q, nil, true)
		tpExpectStatus(t, rec, http.StatusBadRequest)
		assertErrorCode(t, rec, "INVALID_ARGUMENT")
	}
}

func TestPluginAPI_GetPlugin(t *testing.T) {
	srv, _, _ := tpPluginServer(t, false)

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins/"+builtin.AccessControlID, nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body := decodeJSONMap(t, rec)
	for _, key := range []string{
		"id", "name", "version", "source", "capabilities", "config_fields", "enabled",
		"priority", "timeout_ms", "fail_closed", "config", "status", "last_error", "stats",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("plugin object missing %q: %v", key, body)
		}
	}
	info := tpDecodeInfo(t, rec)
	if info.ID != builtin.AccessControlID || info.Source != plugin.SourceBuiltin || info.Enabled ||
		info.Status != plugin.StatusStopped || info.TimeoutMs != plugin.DefaultTimeoutMs || info.Priority != plugin.DefaultPriority {
		t.Fatalf("builtin info = %+v", info)
	}
	if !info.Capabilities.RequestHook || len(info.ConfigFields) == 0 {
		t.Fatalf("capabilities/config_fields not exposed: %+v / %+v", info.Capabilities, info.ConfigFields)
	}

	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins/no.such.plugin", nil, true)
	tpExpectError(t, rec, http.StatusNotFound, "NOT_FOUND", "plugin not found")
}

func TestPluginAPI_PatchValidation(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		body  string
		want  int
		code  string
		msgIn string
	}{
		{"invalid json", builtin.AccessControlID, `{`, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON"},
		{"empty body", builtin.AccessControlID, ``, http.StatusBadRequest, "INVALID_ARGUMENT", ""},
		{"empty patch", builtin.AccessControlID, `{}`, http.StatusBadRequest, "INVALID_ARGUMENT", "empty patch"},
		{"unknown field", builtin.AccessControlID, `{"name":"x"}`, http.StatusBadRequest, "INVALID_ARGUMENT", "field is not patchable: name"},
		{"read-only status", builtin.AccessControlID, `{"status":"running"}`, http.StatusBadRequest, "INVALID_ARGUMENT", "field is not patchable: status"},
		{"null value", builtin.AccessControlID, `{"priority":null}`, http.StatusBadRequest, "INVALID_ARGUMENT", `null value not allowed for field: "priority"`},
		{"wrong type bool", builtin.AccessControlID, `{"enabled":"yes"}`, http.StatusBadRequest, "INVALID_ARGUMENT", "enabled: must be a boolean"},
		{"wrong type int", builtin.AccessControlID, `{"timeout_ms":"100"}`, http.StatusBadRequest, "INVALID_ARGUMENT", "timeout_ms: must be an integer"},
		{"config not object", builtin.AccessControlID, `{"config":[1]}`, http.StatusBadRequest, "INVALID_ARGUMENT", "config: must be an object"},
		{"priority out of range", builtin.AccessControlID, `{"priority":20000}`, http.StatusBadRequest, "INVALID_ARGUMENT", "priority must be between -10000 and 10000"},
		{"timeout out of range", builtin.AccessControlID, `{"timeout_ms":1}`, http.StatusBadRequest, "INVALID_ARGUMENT", "timeout_ms must be between 10 and 60000"},
		{"config field wrong type", builtin.AccessControlID, `{"config":{"reject_status":"x"}}`, http.StatusBadRequest, "INVALID_ARGUMENT", "config.reject_status: must be an integer"},
		{"plugin rejects config", builtin.AccessControlID, `{"config":{"rules":[{"action":"block"}]}}`, http.StatusBadRequest, "INVALID_ARGUMENT", "plugin rejected config"},
		{"enable fails to start", builtin.WebhookID, `{"enabled":true}`, http.StatusBadRequest, "INVALID_ARGUMENT", "plugin failed to start"},
		{"unknown plugin", "no.such.plugin", `{"enabled":true}`, http.StatusNotFound, "NOT_FOUND", "plugin not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, cp, _ := tpPluginServer(t, false)
			rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/plugins/"+tc.id, tc.body, true)
			tpExpectError(t, rec, tc.want, tc.code, tc.msgIn)

			rows, err := cp.Engine.ListPluginSettings()
			if err != nil {
				t.Fatalf("ListPluginSettings: %v", err)
			}
			if len(rows) != 0 {
				t.Fatalf("failed patch must not persist settings, got %+v", rows)
			}
		})
	}
}

func TestPluginAPI_PatchSuccessReturnsFullObject(t *testing.T) {
	srv, cp, mgr := tpPluginServer(t, false)

	rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/plugins/"+builtin.AccessControlID, `{
		"enabled": true,
		"priority": -3,
		"timeout_ms": 300,
		"fail_closed": true,
		"config": {"default_action": "deny", "reject_status": 451, "reject_message": "blocked"}
	}`, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body := decodeJSONMap(t, rec)
	for _, key := range []string{"id", "name", "source", "capabilities", "config_fields", "stats", "created_at", "updated_at"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("PATCH response is not the full object, missing %q: %v", key, body)
		}
	}
	info := tpDecodeInfo(t, rec)
	if info.ID != builtin.AccessControlID || !info.Enabled || info.Priority != -3 || info.TimeoutMs != 300 || !info.FailClosed {
		t.Fatalf("patched info = %+v", info)
	}
	if info.Status != plugin.StatusRunning {
		t.Fatalf("status = %q (last_error %q), want running", info.Status, info.LastError)
	}
	var cfg map[string]any
	if err := json.Unmarshal(info.Config, &cfg); err != nil {
		t.Fatalf("config %s: %v", info.Config, err)
	}
	if cfg["default_action"] != "deny" || cfg["reject_status"] != float64(451) || cfg["reject_message"] != "blocked" {
		t.Fatalf("config = %v", cfg)
	}

	// GET reflects the change.
	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins/"+builtin.AccessControlID, nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	got := tpDecodeInfo(t, rec)
	if !got.Enabled || got.Priority != -3 || got.TimeoutMs != 300 || !got.FailClosed || string(got.Config) != string(info.Config) {
		t.Fatalf("GET after PATCH = %+v", got)
	}

	// Settings were persisted through the real state engine.
	rows, err := cp.Engine.ListPluginSettings()
	if err != nil {
		t.Fatalf("ListPluginSettings: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != builtin.AccessControlID || !rows[0].Enabled || rows[0].Priority != -3 ||
		rows[0].TimeoutMs != 300 || !rows[0].FailClosed || rows[0].ConfigJSON != string(info.Config) {
		t.Fatalf("persisted rows = %+v", rows)
	}
	if !mgr.Active() {
		t.Fatal("request hook should be active after enabling access-control")
	}

	// Partial patch keeps the other fields.
	rec = doJSONRequest(t, srv, http.MethodPatch, "/api/v1/plugins/"+builtin.AccessControlID, `{"enabled":false}`, true)
	tpExpectStatus(t, rec, http.StatusOK)
	info = tpDecodeInfo(t, rec)
	if info.Enabled || info.Status != plugin.StatusStopped || info.Priority != -3 || info.TimeoutMs != 300 || !info.FailClosed {
		t.Fatalf("disabled info = %+v", info)
	}
	if mgr.Active() {
		t.Fatal("request hook should be inactive after disabling")
	}
}

func TestPluginAPI_DeletePlugin(t *testing.T) {
	srv, _, _ := tpPluginServer(t, false)

	rec := doJSONRequest(t, srv, http.MethodDelete, "/api/v1/plugins/"+builtin.AccessControlID, nil, true)
	tpExpectError(t, rec, http.StatusConflict, "CONFLICT", "builtin plugins cannot be uninstalled")

	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins/"+builtin.AccessControlID, nil, true)
	tpExpectStatus(t, rec, http.StatusOK)

	rec = doJSONRequest(t, srv, http.MethodDelete, "/api/v1/plugins/no.such.plugin", nil, true)
	tpExpectError(t, rec, http.StatusNotFound, "NOT_FOUND", "plugin not found")
}

func TestPluginAPI_Rescan(t *testing.T) {
	srv, _, _ := tpPluginServer(t, false)

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugins/actions/rescan", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body := decodeJSONMap(t, rec)
	if len(body) != 2 {
		t.Fatalf("rescan body keys = %v, want exactly items and total", body)
	}
	if body["total"] != float64(3) {
		t.Fatalf("total = %v, want 3", body["total"])
	}
	wantIDs := []string{builtin.AccessControlID, builtin.HeaderRewriteID, builtin.WebhookID}
	if got := tpItemIDs(t, body); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("rescan ids = %v, want %v", got, wantIDs)
	}
}

func TestPluginAPI_ExternalDisabledConflict(t *testing.T) {
	srv, _, _ := tpPluginServer(t, false)
	want := plugin.ErrExternalDisabled.Error()

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugin-marketplace", nil, true)
	tpExpectError(t, rec, http.StatusConflict, "CONFLICT", want)

	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugin-marketplace/some.plugin/actions/install", nil, true)
	tpExpectError(t, rec, http.StatusConflict, "CONFLICT", want)

	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugins/actions/upload", []byte("PK\x03\x04garbage"), true)
	tpExpectError(t, rec, http.StatusConflict, "CONFLICT", want)

	// An empty package is rejected before the external check.
	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugins/actions/upload", []byte{}, true)
	tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "empty plugin package")
}

func TestPluginAPI_ExternalEnabled(t *testing.T) {
	srv, _, _ := tpPluginServer(t, true)

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugin-marketplace", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body := decodeJSONMap(t, rec)
	for _, key := range []string{"sources", "plugins", "errors"} {
		arr, ok := body[key].([]any)
		if !ok || len(arr) != 0 {
			t.Fatalf("marketplace %q = %#v, want empty array", key, body[key])
		}
	}

	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugin-marketplace/missing.plugin/actions/install", nil, true)
	tpExpectError(t, rec, http.StatusNotFound, "NOT_FOUND", "missing.plugin is not listed by any marketplace")

	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugins/actions/upload", []byte("definitely not an archive"), true)
	tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "unsupported package format")
}

func TestPluginAPI_UploadBypassesAPIBodyLimit(t *testing.T) {
	const apiLimit = 1024
	big := []byte(strings.Repeat("x", 4*apiLimit))

	for _, external := range []bool{false, true} {
		name := "external disabled"
		if external {
			name = "external enabled"
		}
		t.Run(name, func(t *testing.T) {
			srv, cp, _ := newControlPlaneTestServerWithBodyLimit(t, apiLimit)
			tpAttachPluginManager(t, cp, external)

			// Sanity: the regular API limit is enforced on other plugin routes.
			bigPatch := `{"config":{"reject_message":"` + strings.Repeat("m", 2*apiLimit) + `"}}`
			rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/plugins/"+builtin.AccessControlID, bigPatch, true)
			tpExpectStatus(t, rec, http.StatusRequestEntityTooLarge)

			rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/plugins/actions/upload", big, true)
			if external {
				tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "unsupported package format")
			} else {
				tpExpectError(t, rec, http.StatusConflict, "CONFLICT", plugin.ErrExternalDisabled.Error())
			}
		})
	}
}

func TestPluginAPI_RequiresAuth(t *testing.T) {
	srv, _, _ := tpPluginServer(t, true)
	routes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/plugins", nil},
		{http.MethodGet, "/api/v1/plugins/" + builtin.AccessControlID, nil},
		{http.MethodPatch, "/api/v1/plugins/" + builtin.AccessControlID, `{"enabled":true}`},
		{http.MethodDelete, "/api/v1/plugins/" + builtin.AccessControlID, nil},
		{http.MethodPost, "/api/v1/plugins/actions/rescan", nil},
		{http.MethodPost, "/api/v1/plugins/actions/upload", []byte("data")},
		{http.MethodGet, "/api/v1/plugin-marketplace", nil},
		{http.MethodPost, "/api/v1/plugin-marketplace/x/actions/install", nil},
	}
	for _, rt := range routes {
		rec := doJSONRequest(t, srv, rt.method, rt.path, rt.body, false)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated: status %d, want 401, body=%s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
		assertErrorCode(t, rec, "UNAUTHORIZED")
	}

	// Nothing was changed by the rejected PATCH.
	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/plugins/"+builtin.AccessControlID, nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	if tpDecodeInfo(t, rec).Enabled {
		t.Fatal("unauthenticated PATCH must not enable the plugin")
	}
}

func TestPluginAPI_NilManagerConflict(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)
	const want = "plugin system is not available"

	requests := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/plugins", nil},
		{http.MethodGet, "/api/v1/plugins/" + builtin.AccessControlID, nil},
		{http.MethodPatch, "/api/v1/plugins/" + builtin.AccessControlID, `{"enabled":true}`},
		{http.MethodDelete, "/api/v1/plugins/" + builtin.AccessControlID, nil},
		{http.MethodPost, "/api/v1/plugins/actions/rescan", nil},
		{http.MethodPost, "/api/v1/plugins/actions/upload", []byte("data")},
		{http.MethodGet, "/api/v1/plugin-marketplace", nil},
		{http.MethodPost, "/api/v1/plugin-marketplace/x/actions/install", nil},
	}
	for _, rq := range requests {
		rec := doJSONRequest(t, srv, rq.method, rq.path, rq.body, true)
		tpExpectError(t, rec, http.StatusConflict, "CONFLICT", want)
	}
}

func TestPluginAPI_EnvConfigShowsPluginSettings(t *testing.T) {
	srv, cp, _ := tpPluginServer(t, true)
	cp.EnvCfg.PluginDir = "/var/lib/resin/plugins"
	cp.EnvCfg.ExternalPluginsEnabled = true
	cp.EnvCfg.PluginMarketplaceURLs = []string{
		"https://user:pass@a.example.com/i.json?token=secret",
		"http://b.example.com/index.json",
		"http://[::1",
	}

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/system/config/env", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body := decodeJSONMap(t, rec)
	if body["plugin_dir"] != "/var/lib/resin/plugins" {
		t.Fatalf("plugin_dir = %v", body["plugin_dir"])
	}
	if body["external_plugins_enabled"] != true {
		t.Fatalf("external_plugins_enabled = %v", body["external_plugins_enabled"])
	}
	urls, ok := body["plugin_marketplace_urls"].([]any)
	if !ok {
		t.Fatalf("plugin_marketplace_urls = %#v", body["plugin_marketplace_urls"])
	}
	want := []any{"https://a.example.com/i.json", "http://b.example.com/index.json", "<invalid url>"}
	if !reflect.DeepEqual(urls, want) {
		t.Fatalf("plugin_marketplace_urls = %#v, want %#v", urls, want)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"user:pass", "pass@", "token=", "secret"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("env response leaks %q: %s", secret, raw)
		}
	}

	// Defaults: disabled external plugins and an empty URL list render as
	// false and [] (not null).
	cp.EnvCfg.ExternalPluginsEnabled = false
	cp.EnvCfg.PluginMarketplaceURLs = nil
	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/system/config/env", nil, true)
	tpExpectStatus(t, rec, http.StatusOK)
	body = decodeJSONMap(t, rec)
	if body["external_plugins_enabled"] != false {
		t.Fatalf("external_plugins_enabled = %v", body["external_plugins_enabled"])
	}
	if arr, ok := body["plugin_marketplace_urls"].([]any); !ok || len(arr) != 0 {
		t.Fatalf("plugin_marketplace_urls = %#v, want []", body["plugin_marketplace_urls"])
	}
}
