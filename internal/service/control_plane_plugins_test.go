package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// tpMemStore is an in-memory plugin.SettingsStore.
type tpMemStore struct {
	mu        sync.Mutex
	rows      map[string]model.PluginSettings
	upsertErr error
	upserts   int
}

func newTPMemStore() *tpMemStore {
	return &tpMemStore{rows: make(map[string]model.PluginSettings)}
}

func (s *tpMemStore) ListPluginSettings() ([]model.PluginSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.PluginSettings, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *tpMemStore) UpsertPluginSettings(row model.PluginSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.rows[row.ID] = row
	return nil
}

func (s *tpMemStore) DeletePluginSettings(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

func (s *tpMemStore) get(id string) (model.PluginSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

func tpNewPluginManager(t *testing.T, store *tpMemStore, external bool) *plugin.Manager {
	t.Helper()
	mgr := plugin.NewManager(plugin.ManagerConfig{
		Store:           store,
		PluginDir:       t.TempDir(),
		ExternalEnabled: external,
		Builtins:        builtin.All(),
		Logf:            func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(context.Background()) })
	return mgr
}

func tpNewPluginService(t *testing.T, external bool) (*ControlPlaneService, *plugin.Manager, *tpMemStore) {
	t.Helper()
	store := newTPMemStore()
	mgr := tpNewPluginManager(t, store, external)
	return &ControlPlaneService{Plugins: mgr}, mgr, store
}

func tpAssertServiceErr(t *testing.T, err error, code, msgContains string) *ServiceError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error containing %q, got nil", code, msgContains)
	}
	var se *ServiceError
	if !errors.As(err, &se) {
		t.Fatalf("error %T (%v) is not a *ServiceError", err, err)
	}
	if se.Code != code {
		t.Fatalf("code = %s, want %s (message %q)", se.Code, code, se.Message)
	}
	if !strings.Contains(se.Message, msgContains) {
		t.Fatalf("message %q does not contain %q", se.Message, msgContains)
	}
	return se
}

func TestPluginService_NilManagerConflict(t *testing.T) {
	s := &ControlPlaneService{}
	ctx := context.Background()
	const want = "plugin system is not available"

	_, err := s.ListPlugins()
	tpAssertServiceErr(t, err, "CONFLICT", want)
	_, err = s.GetPlugin(builtin.AccessControlID)
	tpAssertServiceErr(t, err, "CONFLICT", want)
	_, err = s.PatchPlugin(ctx, builtin.AccessControlID, json.RawMessage(`{"enabled":true}`))
	tpAssertServiceErr(t, err, "CONFLICT", want)
	tpAssertServiceErr(t, s.UninstallPlugin(ctx, "x"), "CONFLICT", want)
	_, err = s.RescanPlugins(ctx)
	tpAssertServiceErr(t, err, "CONFLICT", want)
	_, err = s.PluginMarketplace(ctx)
	tpAssertServiceErr(t, err, "CONFLICT", want)
	_, err = s.InstallMarketplacePlugin(ctx, "x")
	tpAssertServiceErr(t, err, "CONFLICT", want)
	_, err = s.UploadPlugin(ctx, []byte("data"))
	tpAssertServiceErr(t, err, "CONFLICT", want)
}

func TestPluginService_ListAndGet(t *testing.T) {
	s, _, _ := tpNewPluginService(t, false)

	infos, err := s.ListPlugins()
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	wantIDs := []string{builtin.AccessControlID, builtin.HeaderRewriteID, builtin.WebhookID}
	if len(infos) != len(wantIDs) {
		t.Fatalf("ListPlugins returned %d items, want %d", len(infos), len(wantIDs))
	}
	for i, info := range infos {
		if info.ID != wantIDs[i] {
			t.Fatalf("infos[%d].ID = %q, want %q (sorted)", i, info.ID, wantIDs[i])
		}
		if info.Source != plugin.SourceBuiltin || info.Enabled || info.Status != plugin.StatusStopped {
			t.Fatalf("infos[%d] = source %q enabled %v status %q", i, info.Source, info.Enabled, info.Status)
		}
		if info.TimeoutMs != plugin.DefaultTimeoutMs || info.Priority != plugin.DefaultPriority {
			t.Fatalf("infos[%d] defaults: timeout %d priority %d", i, info.TimeoutMs, info.Priority)
		}
	}

	info, err := s.GetPlugin(builtin.HeaderRewriteID)
	if err != nil {
		t.Fatalf("GetPlugin: %v", err)
	}
	if info.ID != builtin.HeaderRewriteID || info.Name == "" {
		t.Fatalf("GetPlugin = %+v", info)
	}

	_, err = s.GetPlugin("no.such.plugin")
	se := tpAssertServiceErr(t, err, "NOT_FOUND", "plugin not found")
	if se.Message != "plugin not found" {
		t.Fatalf("message = %q", se.Message)
	}
}

func TestPluginService_PatchValidation(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		patch string
		code  string
		want  string
	}{
		{"invalid json", builtin.AccessControlID, `{`, "INVALID_ARGUMENT", "invalid JSON"},
		{"not an object", builtin.AccessControlID, `[1]`, "INVALID_ARGUMENT", ""},
		{"unknown field", builtin.AccessControlID, `{"name":"x"}`, "INVALID_ARGUMENT", "field is not patchable: name"},
		{"read-only status", builtin.AccessControlID, `{"status":"running"}`, "INVALID_ARGUMENT", "field is not patchable: status"},
		{"null enabled", builtin.AccessControlID, `{"enabled":null}`, "INVALID_ARGUMENT", `null value not allowed for field: "enabled"`},
		{"null config", builtin.AccessControlID, `{"config":null}`, "INVALID_ARGUMENT", `null value not allowed for field: "config"`},
		{"enabled string", builtin.AccessControlID, `{"enabled":"true"}`, "INVALID_ARGUMENT", "enabled: must be a boolean"},
		{"fail_closed number", builtin.AccessControlID, `{"fail_closed":1}`, "INVALID_ARGUMENT", "fail_closed: must be a boolean"},
		{"priority float", builtin.AccessControlID, `{"priority":1.5}`, "INVALID_ARGUMENT", "priority: must be an integer"},
		{"priority string", builtin.AccessControlID, `{"priority":"1"}`, "INVALID_ARGUMENT", "priority: must be an integer"},
		{"timeout bool", builtin.AccessControlID, `{"timeout_ms":true}`, "INVALID_ARGUMENT", "timeout_ms: must be an integer"},
		{"config array", builtin.AccessControlID, `{"config":[]}`, "INVALID_ARGUMENT", "config: must be an object"},
		{"config string", builtin.AccessControlID, `{"config":"{}"}`, "INVALID_ARGUMENT", "config: must be an object"},
		{"priority too low", builtin.AccessControlID, `{"priority":-10001}`, "INVALID_ARGUMENT", "priority must be between -10000 and 10000"},
		{"priority too high", builtin.AccessControlID, `{"priority":10001}`, "INVALID_ARGUMENT", "priority must be between -10000 and 10000"},
		{"timeout too low", builtin.AccessControlID, `{"timeout_ms":9}`, "INVALID_ARGUMENT", "timeout_ms must be between 10 and 60000"},
		{"timeout too high", builtin.AccessControlID, `{"timeout_ms":60001}`, "INVALID_ARGUMENT", "timeout_ms must be between 10 and 60000"},
		{"config field wrong type", builtin.AccessControlID, `{"config":{"reject_status":"x"}}`, "INVALID_ARGUMENT", "config.reject_status: must be an integer"},
		{"config missing required", builtin.WebhookID, `{"config":{}}`, "INVALID_ARGUMENT", "config.url: required"},
		{"builtin probe rejects config while disabled", builtin.AccessControlID, `{"config":{"rules":[{"action":"block"}]}}`, "INVALID_ARGUMENT", "plugin rejected config"},
		{"enable fails to start", builtin.WebhookID, `{"enabled":true}`, "INVALID_ARGUMENT", "plugin failed to start"},
		{"unknown plugin", "no.such.plugin", `{"enabled":true}`, "NOT_FOUND", "plugin not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, store := tpNewPluginService(t, false)
			before, _ := s.GetPlugin(builtin.AccessControlID)

			_, err := s.PatchPlugin(context.Background(), tc.id, json.RawMessage(tc.patch))
			se := tpAssertServiceErr(t, err, tc.code, tc.want)
			if tc.code == "INVALID_ARGUMENT" && strings.HasPrefix(se.Message, "invalid argument: ") {
				t.Fatalf("sentinel prefix should be trimmed: %q", se.Message)
			}
			if store.upserts != 0 {
				t.Fatalf("failed patch must not persist, got %d upserts", store.upserts)
			}
			after, _ := s.GetPlugin(builtin.AccessControlID)
			if string(after.Config) != string(before.Config) || after.Enabled != before.Enabled ||
				after.Priority != before.Priority || after.TimeoutMs != before.TimeoutMs {
				t.Fatalf("failed patch changed state: before %+v after %+v", before, after)
			}
		})
	}
}

func TestPluginService_PatchSuccess(t *testing.T) {
	s, mgr, store := tpNewPluginService(t, false)
	ctx := context.Background()

	info, err := s.PatchPlugin(ctx, builtin.AccessControlID, json.RawMessage(`{
		"enabled": true,
		"priority": 5,
		"timeout_ms": 250,
		"fail_closed": true,
		"config": {"default_action": "deny", "reject_status": 451, "reject_message": "nope"}
	}`))
	if err != nil {
		t.Fatalf("PatchPlugin: %v", err)
	}
	if info.ID != builtin.AccessControlID || !info.Enabled || info.Priority != 5 || info.TimeoutMs != 250 || !info.FailClosed {
		t.Fatalf("patched info = %+v", info)
	}
	if info.Status != plugin.StatusRunning {
		t.Fatalf("status = %q (last_error %q), want running", info.Status, info.LastError)
	}
	var cfg map[string]any
	if err := json.Unmarshal(info.Config, &cfg); err != nil {
		t.Fatalf("config %s: %v", info.Config, err)
	}
	if cfg["default_action"] != "deny" || cfg["reject_status"] != float64(451) || cfg["reject_message"] != "nope" {
		t.Fatalf("config = %v", cfg)
	}
	if info.CreatedAt == "" || info.UpdatedAt == "" {
		t.Fatalf("timestamps missing: created %q updated %q", info.CreatedAt, info.UpdatedAt)
	}

	row, ok := store.get(builtin.AccessControlID)
	if !ok {
		t.Fatal("settings were not persisted")
	}
	if !row.Enabled || row.Priority != 5 || row.TimeoutMs != 250 || !row.FailClosed || row.ConfigJSON != string(info.Config) {
		t.Fatalf("persisted row = %+v", row)
	}

	// The running chain uses the new config.
	res := mgr.InspectRequest(ctx, &pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeForward, Platform: "p"})
	if res.Reject == nil || res.Reject.HTTPCode != 451 || res.Reject.Message != "nope" || res.PluginID != builtin.AccessControlID {
		t.Fatalf("InspectRequest = %+v (reject %+v)", res, res.Reject)
	}

	// A partial patch only touches the given fields.
	info, err = s.PatchPlugin(ctx, builtin.AccessControlID, json.RawMessage(`{"enabled":false}`))
	if err != nil {
		t.Fatalf("PatchPlugin disable: %v", err)
	}
	if info.Enabled || info.Status != plugin.StatusStopped || info.Priority != 5 || info.TimeoutMs != 250 || !info.FailClosed {
		t.Fatalf("disabled info = %+v", info)
	}
	if !strings.Contains(string(info.Config), `"reject_status":451`) {
		t.Fatalf("config should be kept on disable: %s", info.Config)
	}
	if mgr.Active() {
		t.Fatal("no request hook should be active after disable")
	}

	// Empty patches are rejected like every other constrained merge patch.
	_, err = s.PatchPlugin(ctx, builtin.AccessControlID, json.RawMessage(`{}`))
	tpAssertServiceErr(t, err, "INVALID_ARGUMENT", "empty patch")
}

func TestPluginService_PatchConfigPreservesNumberPrecision(t *testing.T) {
	s, _, _ := tpNewPluginService(t, false)
	// header-rewrite rules is a JSON field; big integers inside must survive
	// verbatim (no float64 round-trip).
	patch := `{"config":{"rules":[{"set":{"X-Big":"v"},"client_cidrs":[]}],"extra":12345678901234567890}}`
	info, err := s.PatchPlugin(context.Background(), builtin.HeaderRewriteID, json.RawMessage(patch))
	if err != nil {
		t.Fatalf("PatchPlugin: %v", err)
	}
	if !strings.Contains(string(info.Config), "12345678901234567890") {
		t.Fatalf("config lost number precision: %s", info.Config)
	}
}

func TestPluginService_PatchPersistFailure(t *testing.T) {
	s, mgr, store := tpNewPluginService(t, false)
	store.upsertErr = errors.New("disk full")

	_, err := s.PatchPlugin(context.Background(), builtin.AccessControlID, json.RawMessage(`{"enabled":true,"config":{"default_action":"deny"}}`))
	se := tpAssertServiceErr(t, err, "INTERNAL", "plugin operation failed: persist plugin settings: disk full")
	if !errors.Is(se, store.upsertErr) {
		t.Fatalf("INTERNAL error should wrap the cause, got %v", se.Err)
	}
	info, _ := s.GetPlugin(builtin.AccessControlID)
	if info.Enabled || info.Status == plugin.StatusRunning {
		t.Fatalf("failed persist must leave plugin disabled, got %+v", info)
	}
	if mgr.Active() {
		t.Fatal("failed persist must not activate the request hook")
	}
}

func TestPluginService_UninstallBuiltinConflict(t *testing.T) {
	s, _, _ := tpNewPluginService(t, false)
	err := s.UninstallPlugin(context.Background(), builtin.AccessControlID)
	se := tpAssertServiceErr(t, err, "CONFLICT", "builtin plugins cannot be uninstalled; disable it instead")
	if strings.HasPrefix(se.Message, "conflict: ") {
		t.Fatalf("sentinel prefix should be trimmed: %q", se.Message)
	}
	if _, err := s.GetPlugin(builtin.AccessControlID); err != nil {
		t.Fatalf("builtin must still exist: %v", err)
	}

	tpAssertServiceErr(t, s.UninstallPlugin(context.Background(), "no.such.plugin"), "NOT_FOUND", "plugin not found")
}

func TestPluginService_ExternalDisabled(t *testing.T) {
	s, _, _ := tpNewPluginService(t, false)
	ctx := context.Background()
	want := plugin.ErrExternalDisabled.Error()

	_, err := s.PluginMarketplace(ctx)
	se := tpAssertServiceErr(t, err, "CONFLICT", want)
	if se.Message != want {
		t.Fatalf("message = %q, want full %q", se.Message, want)
	}
	_, err = s.InstallMarketplacePlugin(ctx, "some.plugin")
	tpAssertServiceErr(t, err, "CONFLICT", want)
	_, err = s.UploadPlugin(ctx, []byte("PK\x03\x04garbage"))
	tpAssertServiceErr(t, err, "CONFLICT", want)

	// Empty upload is rejected before reaching the manager.
	_, err = s.UploadPlugin(ctx, nil)
	tpAssertServiceErr(t, err, "INVALID_ARGUMENT", "empty plugin package")
}

func TestPluginService_ExternalEnabledErrors(t *testing.T) {
	s, _, _ := tpNewPluginService(t, true)
	ctx := context.Background()

	listing, err := s.PluginMarketplace(ctx)
	if err != nil {
		t.Fatalf("PluginMarketplace with no sources: %v", err)
	}
	if listing.Sources == nil || listing.Plugins == nil || listing.Errors == nil ||
		len(listing.Sources)+len(listing.Plugins)+len(listing.Errors) != 0 {
		t.Fatalf("listing = %+v, want empty non-nil slices", listing)
	}

	_, err = s.InstallMarketplacePlugin(ctx, "missing.plugin")
	se := tpAssertServiceErr(t, err, "NOT_FOUND", "missing.plugin is not listed by any marketplace")
	if strings.HasPrefix(se.Message, "plugin not found: ") {
		t.Fatalf("sentinel prefix should be trimmed: %q", se.Message)
	}

	_, err = s.UploadPlugin(ctx, []byte("definitely not an archive"))
	se = tpAssertServiceErr(t, err, "INVALID_ARGUMENT", "unsupported package format")
	if strings.HasPrefix(se.Message, "invalid argument: ") {
		t.Fatalf("sentinel prefix should be trimmed: %q", se.Message)
	}
}

func TestPluginService_Rescan(t *testing.T) {
	s, _, _ := tpNewPluginService(t, false)
	infos, err := s.RescanPlugins(context.Background())
	if err != nil {
		t.Fatalf("RescanPlugins: %v", err)
	}
	if len(infos) != 3 || infos[0].ID != builtin.AccessControlID || infos[2].ID != builtin.WebhookID {
		ids := make([]string, len(infos))
		for i := range infos {
			ids[i] = infos[i].ID
		}
		t.Fatalf("RescanPlugins ids = %v", ids)
	}
}

func TestPluginService_StoppedManagerConflict(t *testing.T) {
	s, mgr, _ := tpNewPluginService(t, false)
	mgr.Stop(context.Background())

	_, err := s.PatchPlugin(context.Background(), builtin.AccessControlID, json.RawMessage(`{"enabled":true}`))
	se := tpAssertServiceErr(t, err, "CONFLICT", "plugin manager is stopped")
	if se.Message != "plugin manager is stopped" {
		t.Fatalf("message = %q", se.Message)
	}
	_, err = s.RescanPlugins(context.Background())
	tpAssertServiceErr(t, err, "CONFLICT", "plugin manager is stopped")
}

func TestPluginService_SettingsRestoredOnRestart(t *testing.T) {
	store := newTPMemStore()
	mgr := tpNewPluginManager(t, store, false)
	s := &ControlPlaneService{Plugins: mgr}
	if _, err := s.PatchPlugin(context.Background(), builtin.AccessControlID, json.RawMessage(`{"enabled":true,"priority":-7,"config":{"default_action":"deny"}}`)); err != nil {
		t.Fatalf("PatchPlugin: %v", err)
	}
	mgr.Stop(context.Background())

	s2 := &ControlPlaneService{Plugins: tpNewPluginManager(t, store, false)}
	info, err := s2.GetPlugin(builtin.AccessControlID)
	if err != nil {
		t.Fatalf("GetPlugin: %v", err)
	}
	if !info.Enabled || info.Priority != -7 || info.Status != plugin.StatusRunning || !strings.Contains(string(info.Config), `"default_action":"deny"`) {
		t.Fatalf("restored info = %+v", info)
	}
}

func TestPluginServiceError_Mapping(t *testing.T) {
	cause := errors.New("disk full")
	cases := []struct {
		name string
		err  error
		code string
		msg  string
	}{
		{"not found bare", plugin.ErrNotFound, "NOT_FOUND", "plugin not found"},
		{"not found wrapped", fmt.Errorf("%w: x is not listed", plugin.ErrNotFound), "NOT_FOUND", "x is not listed"},
		{"invalid bare", plugin.ErrInvalidArgument, "INVALID_ARGUMENT", "invalid argument"},
		{"invalid wrapped", fmt.Errorf("%w: bad priority", plugin.ErrInvalidArgument), "INVALID_ARGUMENT", "bad priority"},
		{"invalid wrapped empty detail", fmt.Errorf("%w: ", plugin.ErrInvalidArgument), "INVALID_ARGUMENT", "invalid argument: "},
		{"invalid outer wrap keeps text", fmt.Errorf("outer: %w", fmt.Errorf("%w: inner", plugin.ErrInvalidArgument)), "INVALID_ARGUMENT", "outer: invalid argument: inner"},
		{"start failed keeps full text", fmt.Errorf("%w: boom", plugin.ErrStartFailed), "INVALID_ARGUMENT", "plugin failed to start: boom"},
		{"external disabled", plugin.ErrExternalDisabled, "CONFLICT", plugin.ErrExternalDisabled.Error()},
		{"external disabled wrapped", fmt.Errorf("install: %w", plugin.ErrExternalDisabled), "CONFLICT", "install: " + plugin.ErrExternalDisabled.Error()},
		{"conflict wrapped", fmt.Errorf("%w: busy", plugin.ErrConflict), "CONFLICT", "busy"},
		{"conflict bare", plugin.ErrConflict, "CONFLICT", "conflict"},
		{"other", cause, "INTERNAL", "plugin operation failed: disk full"},
		{"other wrapped", fmt.Errorf("persist: %w", cause), "INTERNAL", "plugin operation failed: persist: disk full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := pluginServiceError(tc.err)
			if se == nil {
				t.Fatal("nil ServiceError")
			}
			if se.Code != tc.code || se.Message != tc.msg {
				t.Fatalf("pluginServiceError(%v) = {%s %q}, want {%s %q}", tc.err, se.Code, se.Message, tc.code, tc.msg)
			}
			if tc.code == "INTERNAL" && !errors.Is(se, cause) {
				t.Fatalf("INTERNAL should wrap the cause, Err=%v", se.Err)
			}
		})
	}
}
