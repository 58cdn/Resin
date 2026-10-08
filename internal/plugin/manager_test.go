package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/proxy"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// --- test helpers (prefix tm) ---

func tmPtr[T any](v T) *T { return &v }

type tmNoopInstance struct {
	closeFn func(context.Context) error
}

func (i *tmNoopInstance) Capabilities() pluginsdk.Capabilities {
	return pluginsdk.Capabilities{RequestHook: true}
}
func (i *tmNoopInstance) Configure(context.Context, json.RawMessage) error { return nil }
func (i *tmNoopInstance) Inspect(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	return nil, nil
}
func (i *tmNoopInstance) HandleEvents(context.Context, []pluginsdk.Event) error { return nil }
func (i *tmNoopInstance) Close(ctx context.Context) error {
	if i.closeFn != nil {
		return i.closeFn(ctx)
	}
	return nil
}

// tmStore is an in-memory SettingsStore.
type tmStore struct {
	mu        sync.Mutex
	rows      map[string]model.PluginSettings
	upsertErr error
	listErr   error
	upserts   int
	deletes   []string
}

func tmNewStore(rows ...model.PluginSettings) *tmStore {
	s := &tmStore{rows: make(map[string]model.PluginSettings)}
	for _, r := range rows {
		s.rows[r.ID] = r
	}
	return s
}

func (s *tmStore) ListPluginSettings() ([]model.PluginSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]model.PluginSettings, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *tmStore) UpsertPluginSettings(row model.PluginSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts++
	s.rows[row.ID] = row
	return nil
}

func (s *tmStore) DeletePluginSettings(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, id)
	delete(s.rows, id)
	return nil
}

func (s *tmStore) setUpsertErr(err error) {
	s.mu.Lock()
	s.upsertErr = err
	s.mu.Unlock()
}

func (s *tmStore) row(id string) (model.PluginSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

func (s *tmStore) upsertCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upserts
}

// tmSpec is the shared, observable state behind every instance of one fake
// builtin plugin.
type tmSpec struct {
	mu        sync.Mutex
	news      int
	shutdowns int
	configs   []string
	events    []pluginsdk.Event
	batches   []int

	configure func(cfg json.RawMessage) error
	inspect   func(ctx context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error)
	handle    func(ctx context.Context, events []pluginsdk.Event) error
}

func (s *tmSpec) setInspect(fn func(ctx context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error)) {
	s.mu.Lock()
	s.inspect = fn
	s.mu.Unlock()
}

func (s *tmSpec) counts() (news, shutdowns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.news, s.shutdowns
}

func (s *tmSpec) configCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.configs...)
}

func (s *tmSpec) received() []pluginsdk.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pluginsdk.Event(nil), s.events...)
}

// tmPlugin implements every optional plugin interface; the manifest
// capabilities decide what the host actually uses.
type tmPlugin struct{ s *tmSpec }

func (p *tmPlugin) Configure(_ context.Context, cfg json.RawMessage) error {
	p.s.mu.Lock()
	p.s.configs = append(p.s.configs, string(cfg))
	fn := p.s.configure
	p.s.mu.Unlock()
	if fn != nil {
		return fn(cfg)
	}
	return nil
}

func (p *tmPlugin) InspectRequest(ctx context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	p.s.mu.Lock()
	fn := p.s.inspect
	p.s.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx, req)
}

func (p *tmPlugin) HandleEvents(ctx context.Context, events []pluginsdk.Event) error {
	p.s.mu.Lock()
	p.s.events = append(p.s.events, events...)
	p.s.batches = append(p.s.batches, len(events))
	fn := p.s.handle
	p.s.mu.Unlock()
	if fn != nil {
		return fn(ctx, events)
	}
	return nil
}

func (p *tmPlugin) Shutdown(context.Context) error {
	p.s.mu.Lock()
	p.s.shutdowns++
	p.s.mu.Unlock()
	return nil
}

func tmBuiltin(id string, caps pluginsdk.Capabilities, s *tmSpec, fields ...pluginsdk.ConfigField) Builtin {
	return Builtin{
		Manifest: pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion,
			ID:            id,
			Name:          "Test " + id,
			Version:       "1.0.0",
			Capabilities:  caps,
			ConfigFields:  fields,
		},
		New: func() pluginsdk.Plugin {
			s.mu.Lock()
			s.news++
			s.mu.Unlock()
			return &tmPlugin{s: s}
		},
	}
}

func tmHook(id string, s *tmSpec, fields ...pluginsdk.ConfigField) Builtin {
	return tmBuiltin(id, pluginsdk.Capabilities{RequestHook: true}, s, fields...)
}

func tmEventBuiltin(id string, s *tmSpec, events ...string) Builtin {
	return tmBuiltin(id, pluginsdk.Capabilities{Events: events}, s)
}

// tmLogf returns a Logf that forwards to t.Logf until the test ends, so late
// log lines from background goroutines never panic.
func tmLogf(t *testing.T) func(string, ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() {
		mu.Lock()
		done = true
		mu.Unlock()
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}

func tmStartManager(t *testing.T, cfg ManagerConfig) *Manager {
	t.Helper()
	if cfg.Logf == nil {
		cfg.Logf = tmLogf(t)
	}
	if cfg.ResinVersion == "" {
		cfg.ResinVersion = "1.0.0"
	}
	m := NewManager(cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m
}

func tmUpdate(t *testing.T, m *Manager, id string, u Update) Info {
	t.Helper()
	info, err := m.Update(context.Background(), id, u)
	if err != nil {
		t.Fatalf("Update(%s): %v", id, err)
	}
	return info
}

func tmEnable(t *testing.T, m *Manager, id string, u Update) Info {
	t.Helper()
	u.Enabled = tmPtr(true)
	info := tmUpdate(t, m, id, u)
	if info.Status != StatusRunning {
		t.Fatalf("%s status after enable = %q (%s), want running", id, info.Status, info.LastError)
	}
	return info
}

func tmGet(t *testing.T, m *Manager, id string) Info {
	t.Helper()
	info, err := m.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return info
}

func tmRequest() *pluginsdk.RequestInfo {
	return &pluginsdk.RequestInfo{
		ProxyType:  pluginsdk.ProxyTypeReverse,
		ClientIP:   "10.0.0.1",
		Platform:   "plat",
		Account:    "acct",
		TargetHost: "example.com",
		Method:     "GET",
		URL:        "https://example.com/x",
		Headers:    map[string][]string{"User-Agent": {"test"}},
	}
}

func tmInspect(m *Manager) proxy.RequestHookResult {
	return m.InspectRequest(context.Background(), tmRequest())
}

// tmRecorder records the order in which plugins were called.
type tmRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *tmRecorder) add(id string) {
	r.mu.Lock()
	r.calls = append(r.calls, id)
	r.mu.Unlock()
}

func (r *tmRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func tmRecording(r *tmRecorder, id string, dec *pluginsdk.RequestDecision) func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	return func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		r.add(id)
		return dec, nil
	}
}

func tmWaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- defaults and stored settings ---

func TestManagerDefaults(t *testing.T) {
	sa, sb := &tmSpec{}, &tmSpec{}
	st := tmNewStore()
	m := tmStartManager(t, ManagerConfig{
		Store: st,
		Builtins: []Builtin{
			tmHook("test.b", sb),
			tmHook("test.a", sa,
				pluginsdk.ConfigField{Name: "greeting", Type: pluginsdk.FieldString, Default: json.RawMessage(`"hi"`)},
				pluginsdk.ConfigField{Name: "count", Type: pluginsdk.FieldInteger, Default: json.RawMessage(`3`)},
				pluginsdk.ConfigField{Name: "note", Type: pluginsdk.FieldString},
			),
		},
	})

	info := tmGet(t, m, "test.a")
	if info.Enabled || info.Priority != DefaultPriority || info.TimeoutMs != DefaultTimeoutMs || info.FailClosed {
		t.Fatalf("defaults = enabled:%v priority:%d timeout:%d fail_closed:%v", info.Enabled, info.Priority, info.TimeoutMs, info.FailClosed)
	}
	if DefaultPriority != 0 || DefaultTimeoutMs != 1000 {
		t.Fatalf("unexpected default constants %d/%d", DefaultPriority, DefaultTimeoutMs)
	}
	if got, want := string(info.Config), `{"count":3,"greeting":"hi"}`; got != want {
		t.Fatalf("config = %s, want %s", got, want)
	}
	if info.Status != StatusStopped || info.Source != SourceBuiltin || info.LastError != "" {
		t.Fatalf("status/source = %q/%q/%q", info.Status, info.Source, info.LastError)
	}
	if !info.Capabilities.RequestHook || len(info.ConfigFields) != 3 {
		t.Fatalf("caps/fields = %+v / %d", info.Capabilities, len(info.ConfigFields))
	}
	if info.Name != "Test test.a" || info.Version != "1.0.0" {
		t.Fatalf("name/version = %q/%q", info.Name, info.Version)
	}

	infoB := tmGet(t, m, "test.b")
	if string(infoB.Config) != `{}` || infoB.ConfigFields == nil || len(infoB.ConfigFields) != 0 {
		t.Fatalf("test.b config/fields = %s / %#v", infoB.Config, infoB.ConfigFields)
	}

	if m.Active() {
		t.Fatal("Active() = true with nothing enabled")
	}
	if news, _ := sa.counts(); news != 0 {
		t.Fatalf("disabled plugin was instantiated %d times", news)
	}
	if st.upsertCount() != 0 {
		t.Fatalf("Start persisted %d rows, want 0", st.upsertCount())
	}
	list := m.List()
	if len(list) != 2 || list[0].ID != "test.a" || list[1].ID != "test.b" {
		t.Fatalf("List = %+v", list)
	}
	res := tmInspect(m)
	if res.Reject != nil || res.Platform != "plat" || res.Account != "acct" || len(res.HeaderOps) != 0 {
		t.Fatalf("empty chain result = %+v", res)
	}
}

func TestManagerSkipsInvalidBuiltinManifest(t *testing.T) {
	bad := tmHook("Not Valid", &tmSpec{})
	good := tmHook("test.a", &tmSpec{})
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{bad, good}})
	if _, err := m.Get("Not Valid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(invalid builtin) err = %v, want ErrNotFound", err)
	}
	if list := m.List(); len(list) != 1 || list[0].ID != "test.a" {
		t.Fatalf("List = %+v", list)
	}
}

func TestManagerStartAppliesStoredSettings(t *testing.T) {
	sa, sb := &tmSpec{}, &tmSpec{}
	rec := &tmRecorder{}
	sa.inspect = tmRecording(rec, "test.a", nil)
	st := tmNewStore(
		model.PluginSettings{ID: "test.a", Enabled: true, Priority: 7, TimeoutMs: 250, FailClosed: true,
			ConfigJSON: `{"greeting":"yo"}`, CreatedAtNs: 1_700_000_000_000_000_000, UpdatedAtNs: 1_700_000_000_000_000_001},
		model.PluginSettings{ID: "test.b", Priority: -3, TimeoutMs: 500},
		model.PluginSettings{ID: "test.unknown", Enabled: true},
	)
	m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", sa), tmHook("test.b", sb)}})

	a := tmGet(t, m, "test.a")
	if !a.Enabled || a.Priority != 7 || a.TimeoutMs != 250 || !a.FailClosed || a.Status != StatusRunning {
		t.Fatalf("test.a = %+v", a)
	}
	if string(a.Config) != `{"greeting":"yo"}` {
		t.Fatalf("test.a config = %s", a.Config)
	}
	if a.CreatedAt == "" || a.UpdatedAt == "" {
		t.Fatalf("timestamps not reported: %q %q", a.CreatedAt, a.UpdatedAt)
	}
	if got := sa.configCalls(); len(got) != 1 || got[0] != `{"greeting":"yo"}` {
		t.Fatalf("test.a Configure calls = %q", got)
	}

	b := tmGet(t, m, "test.b")
	if b.Enabled || b.Priority != -3 || b.TimeoutMs != 500 || string(b.Config) != `{}` || b.Status != StatusStopped {
		t.Fatalf("test.b = %+v", b)
	}
	if _, err := m.Get("test.unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stored row without plugin should not create an entry, err = %v", err)
	}
	if !m.Active() {
		t.Fatal("Active() = false with an enabled request plugin")
	}
	tmInspect(m)
	if got := rec.take(); !reflect.DeepEqual(got, []string{"test.a"}) {
		t.Fatalf("calls = %v", got)
	}

	// Start is idempotent.
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if news, _ := sa.counts(); news != 1 {
		t.Fatalf("instances after second Start = %d, want 1", news)
	}
}

func TestManagerStartStoreError(t *testing.T) {
	st := tmNewStore()
	st.listErr = errors.New("db locked")
	m := NewManager(ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", &tmSpec{})}, Logf: tmLogf(t)})
	err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "db locked") {
		t.Fatalf("Start err = %v, want store error", err)
	}
	m.Stop(context.Background())
}

// --- Update ---

func TestManagerUpdateEnableDisablePersists(t *testing.T) {
	s := &tmSpec{}
	st := tmNewStore()
	m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{
		tmHook("test.a", s, pluginsdk.ConfigField{Name: "greeting", Type: pluginsdk.FieldString, Default: json.RawMessage(`"hi"`)}),
	}})

	info := tmEnable(t, m, "test.a", Update{})
	if !info.Enabled || !m.Active() || info.CreatedAt == "" || info.UpdatedAt == "" {
		t.Fatalf("after enable: %+v active=%v", info, m.Active())
	}
	row, ok := st.row("test.a")
	if !ok || !row.Enabled || row.Priority != 0 || row.TimeoutMs != 1000 || row.ConfigJSON != `{"greeting":"hi"}` {
		t.Fatalf("persisted row = %+v (ok=%v)", row, ok)
	}
	if row.CreatedAtNs == 0 || row.UpdatedAtNs < row.CreatedAtNs {
		t.Fatalf("row timestamps = %d/%d", row.CreatedAtNs, row.UpdatedAtNs)
	}
	created := row.CreatedAtNs
	if got := s.configCalls(); len(got) != 1 || got[0] != `{"greeting":"hi"}` {
		t.Fatalf("initial Configure calls = %q", got)
	}

	// Settings-only change on a running plugin: no restart, no reconfigure.
	info = tmUpdate(t, m, "test.a", Update{Priority: tmPtr(5), TimeoutMs: tmPtr(200), FailClosed: tmPtr(true)})
	if info.Priority != 5 || info.TimeoutMs != 200 || !info.FailClosed || !info.Enabled {
		t.Fatalf("after settings update: %+v", info)
	}
	if news, _ := s.counts(); news != 1 {
		t.Fatalf("plugin restarted on settings change: %d instances", news)
	}
	// Same config again is not a change.
	tmUpdate(t, m, "test.a", Update{Config: json.RawMessage(` {"greeting": "hi"} `)})
	if got := s.configCalls(); len(got) != 1 {
		t.Fatalf("unchanged config triggered Configure: %q", got)
	}

	info = tmUpdate(t, m, "test.a", Update{Enabled: tmPtr(false)})
	if info.Enabled || info.Status != StatusStopped || m.Active() {
		t.Fatalf("after disable: %+v active=%v", info, m.Active())
	}
	if _, shutdowns := s.counts(); shutdowns != 1 {
		t.Fatalf("shutdowns after disable = %d, want 1", shutdowns)
	}
	row, _ = st.row("test.a")
	if row.Enabled || row.Priority != 5 || row.CreatedAtNs != created {
		t.Fatalf("row after disable = %+v (created %d)", row, created)
	}

	tmEnable(t, m, "test.a", Update{})
	if news, _ := s.counts(); news != 2 {
		t.Fatalf("instances after re-enable = %d, want 2", news)
	}
}

func TestManagerUpdateRangeValidation(t *testing.T) {
	cases := []struct {
		name    string
		u       Update
		wantErr bool
	}{
		{"priority below min", Update{Priority: tmPtr(MinPriority - 1)}, true},
		{"priority above max", Update{Priority: tmPtr(MaxPriority + 1)}, true},
		{"priority min", Update{Priority: tmPtr(MinPriority)}, false},
		{"priority max", Update{Priority: tmPtr(MaxPriority)}, false},
		{"timeout below min", Update{TimeoutMs: tmPtr(MinTimeoutMs - 1)}, true},
		{"timeout zero", Update{TimeoutMs: tmPtr(0)}, true},
		{"timeout above max", Update{TimeoutMs: tmPtr(MaxTimeoutMs + 1)}, true},
		{"timeout min", Update{TimeoutMs: tmPtr(MinTimeoutMs)}, false},
		{"timeout max", Update{TimeoutMs: tmPtr(MaxTimeoutMs)}, false},
		{"bad priority with good timeout", Update{Priority: tmPtr(MaxPriority + 1), TimeoutMs: tmPtr(500)}, true},
		{"good priority with bad timeout", Update{Priority: tmPtr(3), TimeoutMs: tmPtr(MaxTimeoutMs + 1)}, true},
	}
	if MinPriority != -10000 || MaxPriority != 10000 || MinTimeoutMs != 10 || MaxTimeoutMs != 60000 {
		t.Fatalf("unexpected limits %d..%d, %d..%d", MinPriority, MaxPriority, MinTimeoutMs, MaxTimeoutMs)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tmNewStore()
			m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", &tmSpec{})}})
			info, err := m.Update(context.Background(), "test.a", tc.u)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Update: %v", err)
				}
				if tc.u.Priority != nil && info.Priority != *tc.u.Priority {
					t.Fatalf("priority = %d", info.Priority)
				}
				if tc.u.TimeoutMs != nil && info.TimeoutMs != *tc.u.TimeoutMs {
					t.Fatalf("timeout = %d", info.TimeoutMs)
				}
				return
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
			got := tmGet(t, m, "test.a")
			if got.Priority != DefaultPriority || got.TimeoutMs != DefaultTimeoutMs {
				t.Fatalf("settings changed after rejected update: %+v", got)
			}
			if st.upsertCount() != 0 {
				t.Fatal("rejected update was persisted")
			}
		})
	}
}

func TestManagerUpdateConfigValidation(t *testing.T) {
	fields := []pluginsdk.ConfigField{
		{Name: "name", Type: pluginsdk.FieldString, Required: true, Default: json.RawMessage(`"x"`)},
		{Name: "count", Type: pluginsdk.FieldInteger},
		{Name: "mode", Type: pluginsdk.FieldEnum, EnumValues: []string{"a", "b"}},
	}
	invalid := []struct{ name, config string }{
		{"array", `[]`},
		{"string", `"str"`},
		{"number", `42`},
		{"malformed", `{"name":`},
		{"wrong field type", `{"name":"x","count":1.5}`},
		{"bad enum", `{"name":"x","mode":"c"}`},
		{"missing required", `{"name":"x","count":1}`},
		{"required null", `{"name":"x","required_no_default":null}`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			s := &tmSpec{}
			st := tmNewStore()
			fieldsForCase := fields
			if tc.name == "missing required" || tc.name == "required null" {
				fieldsForCase = append(append([]pluginsdk.ConfigField(nil), fields...),
					pluginsdk.ConfigField{Name: "required_no_default", Type: pluginsdk.FieldString, Required: true})
			}
			m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s, fieldsForCase...)}})
			_, err := m.Update(context.Background(), "test.a", Update{Config: json.RawMessage(tc.config)})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
			if got := tmGet(t, m, "test.a"); string(got.Config) != `{"name":"x"}` {
				t.Fatalf("config changed to %s", got.Config)
			}
			if st.upsertCount() != 0 {
				t.Fatal("invalid config was persisted")
			}
			if news, _ := s.counts(); news != 0 {
				t.Fatal("invalid config reached the plugin")
			}
		})
	}

	t.Run("required field default is applied", func(t *testing.T) {
		s := &tmSpec{}
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.defaults", s,
			pluginsdk.ConfigField{Name: "required_default", Type: pluginsdk.FieldString, Required: true, Default: json.RawMessage(`"fallback"`)},
		)}})
		info := tmUpdate(t, m, "test.defaults", Update{Config: json.RawMessage(`{}`)})
		if string(info.Config) != `{"required_default":"fallback"}` {
			t.Fatalf("config = %s, want default-filled config", info.Config)
		}
		if row, _ := st.row("test.defaults"); row.ConfigJSON != `{"required_default":"fallback"}` {
			t.Fatalf("stored config = %s, want default-filled config", row.ConfigJSON)
		}
	})

	t.Run("valid config is compacted and probed", func(t *testing.T) {
		s := &tmSpec{}
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s, fields...)}})
		info := tmUpdate(t, m, "test.a", Update{Config: json.RawMessage(" { \"name\" : \"y\", \"extra\": [1, 2] }\n")})
		want := `{"name":"y","extra":[1,2]}`
		if string(info.Config) != want {
			t.Fatalf("config = %s, want %s", info.Config, want)
		}
		if row, _ := st.row("test.a"); row.ConfigJSON != want || row.Enabled {
			t.Fatalf("row = %+v", row)
		}
		// A disabled builtin validates the config with a throwaway instance.
		news, shutdowns := s.counts()
		if news != 1 || shutdowns != 1 {
			t.Fatalf("probe instances = %d, closed = %d; want 1/1", news, shutdowns)
		}
		if calls := s.configCalls(); len(calls) != 1 || calls[0] != want {
			t.Fatalf("probe Configure calls = %q", calls)
		}
	})

	t.Run("null and empty become empty object", func(t *testing.T) {
		m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.b", &tmSpec{})}})
		tmUpdate(t, m, "test.b", Update{Config: json.RawMessage(`{"k":1}`)})
		for _, raw := range []string{`null`, `  `, ``} {
			info := tmUpdate(t, m, "test.b", Update{Config: json.RawMessage(raw)})
			if string(info.Config) != `{}` {
				t.Fatalf("config %q -> %s, want {}", raw, info.Config)
			}
			tmUpdate(t, m, "test.b", Update{Config: json.RawMessage(`{"k":1}`)})
		}
	})
}

func TestManagerUpdateConfigRejectedByPlugin(t *testing.T) {
	newSpec := func() *tmSpec {
		return &tmSpec{configure: func(cfg json.RawMessage) error {
			if strings.Contains(string(cfg), "bad") {
				return errors.New("nope, bad config")
			}
			return nil
		}}
	}

	t.Run("disabled probe", func(t *testing.T) {
		s := newSpec()
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s)}})
		_, err := m.Update(context.Background(), "test.a", Update{Config: json.RawMessage(`{"bad":true}`)})
		if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("err = %v, want ErrInvalidArgument from plugin", err)
		}
		if got := tmGet(t, m, "test.a"); string(got.Config) != `{}` || got.Enabled {
			t.Fatalf("state changed: %+v", got)
		}
		if st.upsertCount() != 0 {
			t.Fatal("rejected config persisted")
		}
		if news, shutdowns := s.counts(); news != 1 || shutdowns != 1 {
			t.Fatalf("probe instances %d closed %d, want 1/1", news, shutdowns)
		}
	})

	t.Run("running hot configure", func(t *testing.T) {
		s := newSpec()
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s)}})
		tmEnable(t, m, "test.a", Update{Config: json.RawMessage(`{"v":1}`)})
		upserts := st.upsertCount()

		_, err := m.Update(context.Background(), "test.a", Update{Config: json.RawMessage(`{"bad":true}`), Priority: tmPtr(9)})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("err = %v, want ErrInvalidArgument", err)
		}
		got := tmGet(t, m, "test.a")
		if string(got.Config) != `{"v":1}` || got.Priority != 0 || got.Status != StatusRunning || !m.Active() {
			t.Fatalf("state after rejected hot config: %+v", got)
		}
		if st.upsertCount() != upserts {
			t.Fatal("rejected hot config persisted")
		}

		info := tmUpdate(t, m, "test.a", Update{Config: json.RawMessage(`{"v":2}`)})
		if string(info.Config) != `{"v":2}` {
			t.Fatalf("config = %s", info.Config)
		}
		if news, _ := s.counts(); news != 1 {
			t.Fatalf("hot configure restarted the plugin (%d instances)", news)
		}
		calls := s.configCalls()
		if calls[len(calls)-1] != `{"v":2}` {
			t.Fatalf("Configure calls = %q", calls)
		}
		if row, _ := st.row("test.a"); row.ConfigJSON != `{"v":2}` {
			t.Fatalf("row config = %s", row.ConfigJSON)
		}
	})

	t.Run("enable with rejected config", func(t *testing.T) {
		s := newSpec()
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s)}})
		_, err := m.Update(context.Background(), "test.a", Update{Enabled: tmPtr(true), Config: json.RawMessage(`{"bad":1}`)})
		if !errors.Is(err, ErrStartFailed) || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("err = %v, want ErrStartFailed", err)
		}
		got := tmGet(t, m, "test.a")
		if got.Enabled || got.Status != StatusError || !strings.Contains(got.LastError, "nope") || string(got.Config) != `{}` {
			t.Fatalf("state after failed enable: %+v", got)
		}
		if m.Active() || st.upsertCount() != 0 {
			t.Fatalf("failed enable left active=%v upserts=%d", m.Active(), st.upsertCount())
		}
		if news, shutdowns := s.counts(); news != 1 || shutdowns != 1 {
			t.Fatalf("failed instance not closed: news=%d shutdowns=%d", news, shutdowns)
		}
	})
}

func TestManagerUpdateBuiltinStartPanics(t *testing.T) {
	cases := map[string]Builtin{
		"panic in New": {
			Manifest: tmHook("test.a", &tmSpec{}).Manifest,
			New:      func() pluginsdk.Plugin { panic("constructor exploded") },
		},
		"nil plugin": {
			Manifest: tmHook("test.a", &tmSpec{}).Manifest,
			New:      func() pluginsdk.Plugin { return nil },
		},
		"panic in Configure": tmHook("test.a", &tmSpec{configure: func(json.RawMessage) error { panic("configure exploded") }}),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{b}})
			_, err := m.Update(context.Background(), "test.a", Update{Enabled: tmPtr(true)})
			if !errors.Is(err, ErrStartFailed) {
				t.Fatalf("err = %v, want ErrStartFailed", err)
			}
			if got := tmGet(t, m, "test.a"); got.Status != StatusError || got.Enabled {
				t.Fatalf("state = %+v", got)
			}
		})
	}
}

func TestManagerFailClosedStartupFailurePublishesPlaceholder(t *testing.T) {
	manifest := tmHook("test.a", &tmSpec{}).Manifest
	store := tmNewStore(model.PluginSettings{ID: "test.a", Enabled: true, FailClosed: true, ConfigJSON: `{}`})
	m := tmStartManager(t, ManagerConfig{
		Store: store,
		Builtins: []Builtin{{
			Manifest: manifest,
			New:      func() pluginsdk.Plugin { panic("startup exploded") },
		}},
	})
	res := tmInspect(m)
	if res.Reject == nil || res.Reject.HTTPCode != http.StatusServiceUnavailable || res.Reject.ResinError != "PLUGIN_ERROR" || res.PluginID != "test.a" {
		t.Fatalf("startup failure result = %+v, want fail-closed 503 placeholder", res)
	}
	if got := tmGet(t, m, "test.a"); !got.Enabled || got.Status != StatusError {
		t.Fatalf("plugin status = %+v, want enabled error", got)
	}
}

func TestManagerUpdatePersistFailureRollsBack(t *testing.T) {
	diskFull := errors.New("disk full")

	t.Run("enable", func(t *testing.T) {
		s := &tmSpec{}
		rec := &tmRecorder{}
		s.inspect = tmRecording(rec, "test.a", nil)
		st := tmNewStore()
		st.setUpsertErr(diskFull)
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s)}})
		_, err := m.Update(context.Background(), "test.a", Update{Enabled: tmPtr(true)})
		if !errors.Is(err, diskFull) {
			t.Fatalf("err = %v, want store error", err)
		}
		got := tmGet(t, m, "test.a")
		if got.Enabled || got.Status != StatusStopped {
			t.Fatalf("state after failed persist: %+v", got)
		}
		if news, shutdowns := s.counts(); news != 1 || shutdowns != 1 {
			t.Fatalf("started instance not stopped: news=%d shutdowns=%d", news, shutdowns)
		}
		if m.Active() {
			t.Fatal("plugin active after rollback")
		}
		tmInspect(m)
		if calls := rec.take(); len(calls) != 0 {
			t.Fatalf("rolled back plugin called: %v", calls)
		}
	})

	t.Run("reconfigure", func(t *testing.T) {
		s := &tmSpec{}
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s)}})
		tmEnable(t, m, "test.a", Update{Config: json.RawMessage(`{"v":1}`)})
		st.setUpsertErr(diskFull)
		_, err := m.Update(context.Background(), "test.a", Update{Config: json.RawMessage(`{"v":2}`)})
		if !errors.Is(err, diskFull) {
			t.Fatalf("err = %v, want store error", err)
		}
		if got := tmGet(t, m, "test.a"); string(got.Config) != `{"v":1}` || got.Status != StatusRunning {
			t.Fatalf("state after failed persist: %+v", got)
		}
		calls := s.configCalls()
		if want := []string{`{"v":1}`, `{"v":2}`, `{"v":1}`}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("Configure calls = %q, want %q (previous config restored)", calls, want)
		}
		if row, _ := st.row("test.a"); row.ConfigJSON != `{"v":1}` {
			t.Fatalf("row = %+v", row)
		}
	})

	t.Run("disable", func(t *testing.T) {
		s := &tmSpec{}
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", s)}})
		tmEnable(t, m, "test.a", Update{})
		st.setUpsertErr(diskFull)
		if _, err := m.Update(context.Background(), "test.a", Update{Enabled: tmPtr(false)}); !errors.Is(err, diskFull) {
			t.Fatalf("err = %v, want store error", err)
		}
		if got := tmGet(t, m, "test.a"); !got.Enabled || got.Status != StatusRunning || !m.Active() {
			t.Fatalf("state after failed disable: %+v", got)
		}
		if _, shutdowns := s.counts(); shutdowns != 0 {
			t.Fatal("plugin stopped although disable was not persisted")
		}
	})

	t.Run("priority", func(t *testing.T) {
		st := tmNewStore()
		m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", &tmSpec{})}})
		tmEnable(t, m, "test.a", Update{})
		st.setUpsertErr(diskFull)
		if _, err := m.Update(context.Background(), "test.a", Update{Priority: tmPtr(5), TimeoutMs: tmPtr(50)}); !errors.Is(err, diskFull) {
			t.Fatalf("err = %v", err)
		}
		if got := tmGet(t, m, "test.a"); got.Priority != 0 || got.TimeoutMs != DefaultTimeoutMs {
			t.Fatalf("settings changed: %+v", got)
		}
	})
}

func TestManagerUnknownPlugin(t *testing.T) {
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", &tmSpec{})}})
	if _, err := m.Update(context.Background(), "test.missing", Update{Enabled: tmPtr(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update err = %v", err)
	}
	if _, err := m.Get("test.missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v", err)
	}
	if err := m.Uninstall(context.Background(), "test.missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Uninstall err = %v", err)
	}
}

func TestManagerUninstallBuiltin(t *testing.T) {
	st := tmNewStore()
	m := tmStartManager(t, ManagerConfig{Store: st, Builtins: []Builtin{tmHook("test.a", &tmSpec{})}})
	tmEnable(t, m, "test.a", Update{})
	if err := m.Uninstall(context.Background(), "test.a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Uninstall builtin err = %v, want ErrConflict", err)
	}
	if got := tmGet(t, m, "test.a"); !got.Enabled || got.Status != StatusRunning || !m.Active() {
		t.Fatalf("builtin affected by refused uninstall: %+v", got)
	}
	if _, ok := st.row("test.a"); !ok || len(st.deletes) != 0 {
		t.Fatal("settings of builtin deleted")
	}
}

// --- request chain ---

func TestManagerChainOrder(t *testing.T) {
	rec := &tmRecorder{}
	ids := []string{"test.d", "test.b", "test.e", "test.c", "test.a"}
	var builtins []Builtin
	for _, id := range ids {
		s := &tmSpec{}
		s.inspect = tmRecording(rec, id, pluginsdk.Continue())
		builtins = append(builtins, tmHook(id, s))
	}
	m := tmStartManager(t, ManagerConfig{Builtins: builtins})
	tmEnable(t, m, "test.a", Update{Priority: tmPtr(10)})
	tmEnable(t, m, "test.b", Update{Priority: tmPtr(10)})
	tmEnable(t, m, "test.c", Update{Priority: tmPtr(20)})
	tmEnable(t, m, "test.d", Update{Priority: tmPtr(-5)})
	tmUpdate(t, m, "test.e", Update{Priority: tmPtr(100)}) // disabled

	tmInspect(m)
	if got, want := rec.take(), []string{"test.c", "test.a", "test.b", "test.d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	tmUpdate(t, m, "test.d", Update{Priority: tmPtr(50)})
	tmUpdate(t, m, "test.a", Update{Priority: tmPtr(-10000)})
	tmInspect(m)
	if got, want := rec.take(), []string{"test.d", "test.c", "test.b", "test.a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order after priority change = %v, want %v", got, want)
	}
}

func TestManagerChainRewritesIdentity(t *testing.T) {
	type seen struct{ platform, account string }
	var mu sync.Mutex
	var seenB, seenC seen
	sa := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return &pluginsdk.RequestDecision{Platform: tmPtr("plat2"), Account: tmPtr("acct2")}, nil
	}}
	sb := &tmSpec{inspect: func(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		mu.Lock()
		seenB = seen{req.Platform, req.Account}
		mu.Unlock()
		return &pluginsdk.RequestDecision{Action: pluginsdk.ActionContinue, Account: tmPtr("acct3")}, nil
	}}
	sc := &tmSpec{inspect: func(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		mu.Lock()
		seenC = seen{req.Platform, req.Account}
		mu.Unlock()
		return nil, nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", sa), tmHook("test.b", sb), tmHook("test.c", sc)}})
	tmEnable(t, m, "test.a", Update{Priority: tmPtr(10)})
	tmEnable(t, m, "test.b", Update{Priority: tmPtr(5)})
	tmEnable(t, m, "test.c", Update{Priority: tmPtr(0)})

	req := tmRequest()
	res := m.InspectRequest(context.Background(), req)
	if res.Reject != nil || res.PluginID != "" {
		t.Fatalf("unexpected reject: %+v", res)
	}
	if res.Platform != "plat2" || res.Account != "acct3" {
		t.Fatalf("result identity = %q/%q, want plat2/acct3", res.Platform, res.Account)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenB != (seen{"plat2", "acct2"}) {
		t.Fatalf("test.b saw %+v, want plat2/acct2", seenB)
	}
	if seenC != (seen{"plat2", "acct3"}) {
		t.Fatalf("test.c saw %+v, want plat2/acct3", seenC)
	}
	if req.Platform != "plat2" || req.Account != "acct3" {
		t.Fatalf("req not updated: %q/%q", req.Platform, req.Account)
	}
}

func TestManagerChainFirstRejectWins(t *testing.T) {
	rec := &tmRecorder{}
	sa := &tmSpec{inspect: tmRecording(rec, "test.a", &pluginsdk.RequestDecision{SetHeaders: map[string]string{"X-A": "1"}})}
	sb := &tmSpec{inspect: tmRecording(rec, "test.b", pluginsdk.Reject(429, " slow down "))}
	sc := &tmSpec{inspect: tmRecording(rec, "test.c", pluginsdk.Reject(401, "other"))}
	sd := &tmSpec{inspect: tmRecording(rec, "test.d", nil)}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", sa), tmHook("test.b", sb), tmHook("test.c", sc), tmHook("test.d", sd)}})
	tmEnable(t, m, "test.a", Update{Priority: tmPtr(30)})
	tmEnable(t, m, "test.b", Update{Priority: tmPtr(20)})
	tmEnable(t, m, "test.c", Update{Priority: tmPtr(10)})
	tmEnable(t, m, "test.d", Update{Priority: tmPtr(0)})

	res := tmInspect(m)
	if res.Reject == nil {
		t.Fatal("request not rejected")
	}
	if res.Reject.HTTPCode != 429 || res.Reject.ResinError != "PLUGIN_REJECTED" || res.Reject.Message != "slow down" {
		t.Fatalf("reject = %+v", res.Reject)
	}
	if res.PluginID != "test.b" {
		t.Fatalf("PluginID = %q, want test.b", res.PluginID)
	}
	if got := rec.take(); !reflect.DeepEqual(got, []string{"test.a", "test.b"}) {
		t.Fatalf("calls = %v, later plugins must not run", got)
	}
	a, b, c := tmGet(t, m, "test.a").Stats, tmGet(t, m, "test.b").Stats, tmGet(t, m, "test.c").Stats
	if a.Requests != 1 || a.Rejects != 0 || b.Requests != 1 || b.Rejects != 1 || c.Requests != 0 {
		t.Fatalf("stats a=%+v b=%+v c=%+v", a, b, c)
	}
}

func TestManagerRejectStatusAndMessage(t *testing.T) {
	var dec atomic.Pointer[pluginsdk.RequestDecision]
	s := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return dec.Load(), nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", s)}})
	tmEnable(t, m, "test.a", Update{})

	cases := []struct {
		status     int
		message    string
		wantStatus int
		wantMsg    string
	}{
		{0, "denied", 403, "denied"},
		{-1, "denied", 403, "denied"},
		{200, "denied", 403, "denied"},
		{399, "denied", 403, "denied"},
		{400, "denied", 400, "denied"},
		{451, "denied", 451, "denied"},
		{599, "denied", 599, "denied"},
		{600, "denied", 403, "denied"},
		{429, "", 429, proxy.ErrPluginRejected.Message},
		{429, "  \n ", 429, proxy.ErrPluginRejected.Message},
		{429, "  padded \n", 429, "padded"},
	}
	for _, tc := range cases {
		dec.Store(&pluginsdk.RequestDecision{Action: pluginsdk.ActionReject, Status: tc.status, Message: tc.message})
		res := tmInspect(m)
		if res.Reject == nil {
			t.Fatalf("status %d: not rejected", tc.status)
		}
		if res.Reject.HTTPCode != tc.wantStatus || res.Reject.Message != tc.wantMsg || res.Reject.ResinError != proxy.ErrPluginRejected.ResinError {
			t.Fatalf("status %d msg %q: reject = %+v, want %d %q", tc.status, tc.message, res.Reject, tc.wantStatus, tc.wantMsg)
		}
		if res.Reject == proxy.ErrPluginRejected {
			t.Fatal("shared ErrPluginRejected returned; it must not be mutated by callers")
		}
		if res.PluginID != "test.a" {
			t.Fatalf("PluginID = %q", res.PluginID)
		}
	}
	// A continue decision carrying a status is not a reject.
	dec.Store(&pluginsdk.RequestDecision{Status: 403, Message: "ignored"})
	if res := tmInspect(m); res.Reject != nil || res.PluginID != "" {
		t.Fatalf("continue with status rejected: %+v", res)
	}
}

func TestManagerHeaderOps(t *testing.T) {
	var mu sync.Mutex
	var seenByB map[string][]string
	sa := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return &pluginsdk.RequestDecision{
			RemoveHeaders: []string{"x-old", "host", "Content-Length", "  ", "transfer-encoding"},
			SetHeaders: map[string]string{
				"x-new":             "v1",
				"x-another":         "a",
				" x-spaced ":        "s",
				"":                  "empty",
				"connection":        "close",
				"Upgrade":           "websocket",
				"TE":                "trailers",
				"trailer":           "X",
				"Content-Length":    "5",
				"Host":              "evil.example",
				"Transfer-Encoding": "chunked",
				"X-CRLF":            "a\r\nInjected: 1",
				"X-LF":              "a\nb",
				"X-CR":              "a\rb",
				"X-NUL":             "a\x00b",
			},
		}, nil
	}}
	sb := &tmSpec{inspect: func(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		cp := make(map[string][]string, len(req.Headers))
		for k, v := range req.Headers {
			cp[k] = append([]string(nil), v...)
		}
		mu.Lock()
		seenByB = cp
		mu.Unlock()
		return &pluginsdk.RequestDecision{
			RemoveHeaders: []string{"X-Keep", "X-New"},
			SetHeaders:    map[string]string{"X-New": "v2"},
		}, nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", sa), tmHook("test.b", sb)}})
	tmEnable(t, m, "test.a", Update{Priority: tmPtr(10)})
	tmEnable(t, m, "test.b", Update{Priority: tmPtr(0)})

	req := tmRequest()
	req.Headers = map[string][]string{
		"X-Old":  {"1"},
		"X-Keep": {"k"},
		"Host":   {"example.com"},
	}
	res := m.InspectRequest(context.Background(), req)
	if res.Reject != nil {
		t.Fatalf("unexpected reject %+v", res.Reject)
	}
	wantOps := []proxy.HeaderOp{
		{Name: "X-Old", Remove: true},
		{Name: "X-Spaced", Value: "s"},
		{Name: "X-Another", Value: "a"},
		{Name: "X-New", Value: "v1"},
		{Name: "X-Keep", Remove: true},
		{Name: "X-New", Remove: true},
		{Name: "X-New", Value: "v2"},
	}
	if !reflect.DeepEqual(res.HeaderOps, wantOps) {
		t.Fatalf("header ops =\n%+v\nwant\n%+v", res.HeaderOps, wantOps)
	}
	wantSeen := map[string][]string{
		"Host":      {"example.com"},
		"X-Keep":    {"k"},
		"X-Spaced":  {"s"},
		"X-Another": {"a"},
		"X-New":     {"v1"},
	}
	mu.Lock()
	gotSeen := seenByB
	mu.Unlock()
	if !reflect.DeepEqual(gotSeen, wantSeen) {
		t.Fatalf("test.b saw headers %v, want %v", gotSeen, wantSeen)
	}
	wantFinal := map[string][]string{
		"Host":      {"example.com"},
		"X-Spaced":  {"s"},
		"X-Another": {"a"},
		"X-New":     {"v2"},
	}
	if !reflect.DeepEqual(req.Headers, wantFinal) {
		t.Fatalf("final req headers %v, want %v", req.Headers, wantFinal)
	}
	for _, name := range []string{"Host", "Content-Length", "Transfer-Encoding", "Connection", "Upgrade", "TE", "Trailer"} {
		if !proxy.IsProtectedHookHeader(name) {
			t.Fatalf("%s should be protected", name)
		}
	}
}

func TestManagerHeaderOpsWithoutHeaders(t *testing.T) {
	s := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return &pluginsdk.RequestDecision{SetHeaders: map[string]string{"X-A": "1"}, RemoveHeaders: []string{"X-B"}}, nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", s)}})
	tmEnable(t, m, "test.a", Update{})

	cases := []struct {
		name    string
		req     *pluginsdk.RequestInfo
		wantOps int
	}{
		{"connect with headers", &pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeForward, IsConnect: true, Headers: map[string][]string{"X-Client": {"v"}}}, 0},
		{"connect", &pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeForward, IsConnect: true}, 0},
		{"socks5", &pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeSocks5, IsConnect: true}, 0},
		{"reverse without headers", &pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeReverse}, 2},
		{"forward without headers", &pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeForward}, 2},
	}
	for _, tc := range cases {
		res := m.InspectRequest(context.Background(), tc.req)
		if len(res.HeaderOps) != tc.wantOps {
			t.Fatalf("%s: ops = %+v, want %d", tc.name, res.HeaderOps, tc.wantOps)
		}
		if tc.wantOps == 0 && tc.req.Headers != nil && tc.name != "connect with headers" {
			t.Fatalf("%s: headers map created: %v", tc.name, tc.req.Headers)
		}
		if tc.wantOps > 0 && !reflect.DeepEqual(tc.req.Headers, map[string][]string{"X-A": {"1"}}) {
			t.Fatalf("%s: headers = %v", tc.name, tc.req.Headers)
		}
	}
}

// Header ops net/http would refuse (making the upstream RoundTrip fail) are
// dropped like protected headers.
func TestManagerHeaderOpsRejectsInvalidNamesAndValues(t *testing.T) {
	s := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return &pluginsdk.RequestDecision{
			SetHeaders: map[string]string{
				"Bad Name": "v",
				"X:Colon":  "v",
				"X(Paren)": "v",
				"X-Ünï":    "v",
				"X-Ctl":    "a\x01b",
				"X-Del":    "a\x7fb",
				"X-Tab":    "a\tb",
			},
			RemoveHeaders: []string{"Bad Name", "X:Colon", "X-Gone"},
		}, nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", s)}})
	tmEnable(t, m, "test.a", Update{})
	req := tmRequest()
	req.Headers = map[string][]string{"Bad Name": {"x"}, "X-Gone": {"y"}}
	res := m.InspectRequest(context.Background(), req)
	wantOps := []proxy.HeaderOp{
		{Name: "X-Gone", Remove: true},
		{Name: "X-Tab", Value: "a\tb"},
	}
	if !reflect.DeepEqual(res.HeaderOps, wantOps) {
		t.Fatalf("header ops = %+v, want %+v", res.HeaderOps, wantOps)
	}
	wantHeaders := map[string][]string{"Bad Name": {"x"}, "X-Tab": {"a\tb"}}
	if !reflect.DeepEqual(req.Headers, wantHeaders) {
		t.Fatalf("headers = %v, want %v", req.Headers, wantHeaders)
	}
}

func TestManagerHeaderOpsProxyAuthorization(t *testing.T) {
	s := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return &pluginsdk.RequestDecision{
			SetHeaders:    map[string]string{"proxy-authorization": "Basic Zm9vOmJhcg=="},
			RemoveHeaders: []string{"Proxy-Authorization"},
		}, nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", s)}})
	tmEnable(t, m, "test.a", Update{})
	if !proxy.IsProtectedHookHeader("Proxy-Authorization") {
		t.Errorf("Proxy-Authorization is not a protected hook header")
	}
	if res := tmInspect(m); len(res.HeaderOps) != 0 {
		t.Fatalf("plugin changed Proxy-Authorization: %+v", res.HeaderOps)
	}
}

// --- errors, timeouts, panics ---

func TestManagerPluginFailures(t *testing.T) {
	boom := func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return nil, errors.New("boom")
	}
	panics := func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		panic("kaboom")
	}
	badAction := func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		return &pluginsdk.RequestDecision{Action: "allow", Platform: tmPtr("hijacked")}, nil
	}
	cases := []struct {
		name    string
		inspect func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error)
	}{
		{"error", boom},
		{"panic", panics},
		{"invalid action", badAction},
	}
	for _, tc := range cases {
		for _, failClosed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail_closed=%v", tc.name, failClosed), func(t *testing.T) {
				rec := &tmRecorder{}
				sa := &tmSpec{inspect: tc.inspect}
				sb := &tmSpec{inspect: func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
					rec.add("test.b")
					return &pluginsdk.RequestDecision{Account: tmPtr("from-b")}, nil
				}}
				m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", sa), tmHook("test.b", sb)}})
				tmEnable(t, m, "test.a", Update{Priority: tmPtr(10), FailClosed: tmPtr(failClosed)})
				tmEnable(t, m, "test.b", Update{})

				res := tmInspect(m)
				if failClosed {
					if res.Reject != proxy.ErrPluginUnavailable {
						t.Fatalf("reject = %+v, want ErrPluginUnavailable", res.Reject)
					}
					if res.Reject.HTTPCode != 503 || res.Reject.ResinError != "PLUGIN_ERROR" || res.PluginID != "test.a" {
						t.Fatalf("fail-closed result = %+v / %q", res.Reject, res.PluginID)
					}
					if calls := rec.take(); len(calls) != 0 {
						t.Fatalf("later plugin ran after fail-closed error: %v", calls)
					}
				} else {
					if res.Reject != nil || res.PluginID != "" {
						t.Fatalf("fail-open plugin rejected: %+v", res)
					}
					if res.Platform != "plat" || res.Account != "from-b" {
						t.Fatalf("identity = %q/%q; failed plugin must be skipped", res.Platform, res.Account)
					}
					if calls := rec.take(); !reflect.DeepEqual(calls, []string{"test.b"}) {
						t.Fatalf("calls = %v", calls)
					}
				}
				st := tmGet(t, m, "test.a").Stats
				if st.Requests != 1 || st.Errors != 1 || st.Timeouts != 0 || st.Rejects != 0 {
					t.Fatalf("stats = %+v", st)
				}
				if got := tmGet(t, m, "test.a"); got.Status != StatusRunning {
					t.Fatalf("plugin status after failure = %q", got.Status)
				}
			})
		}
	}
}

func TestManagerTimeouts(t *testing.T) {
	lateReject := func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		time.Sleep(80 * time.Millisecond) // ignores ctx: in-process plugins are not preempted
		return pluginsdk.Reject(418, "too late"), nil
	}
	honorsCtx := func(ctx context.Context, _ *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, nil
		}
	}
	cases := []struct {
		name       string
		inspect    func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error)
		failClosed bool
	}{
		{"late answer fail-open", lateReject, false},
		{"late answer fail-closed", lateReject, true},
		{"ctx deadline fail-open", honorsCtx, false},
		{"ctx deadline fail-closed", honorsCtx, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", &tmSpec{inspect: tc.inspect})}})
			tmEnable(t, m, "test.a", Update{TimeoutMs: tmPtr(20), FailClosed: tmPtr(tc.failClosed)})
			start := time.Now()
			res := tmInspect(m)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("InspectRequest took %v", elapsed)
			}
			if tc.failClosed {
				if res.Reject != proxy.ErrPluginUnavailable || res.PluginID != "test.a" {
					t.Fatalf("result = %+v", res)
				}
			} else if res.Reject != nil {
				t.Fatalf("late/timed out decision applied: %+v", res.Reject)
			}
			st := tmGet(t, m, "test.a").Stats
			if st.Requests != 1 || st.Timeouts != 1 || st.Errors != 0 || st.Rejects != 0 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}

	t.Run("canceled parent context is not counted as an error", func(t *testing.T) {
		m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", &tmSpec{inspect: honorsCtx})}})
		tmEnable(t, m, "test.a", Update{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := m.InspectRequest(ctx, tmRequest())
		if !res.Canceled || res.Reject != nil {
			t.Fatalf("result = %+v, want canceled without rejection", res)
		}
		if st := tmGet(t, m, "test.a").Stats; st.Errors != 0 || st.Timeouts != 0 {
			t.Fatalf("stats = %+v, want no errors or timeouts", st)
		}
	})

	t.Run("timeout update applies to chain", func(t *testing.T) {
		slow := func(context.Context, *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
			time.Sleep(40 * time.Millisecond)
			return pluginsdk.Reject(418, "slow"), nil
		}
		m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", &tmSpec{inspect: slow})}})
		tmEnable(t, m, "test.a", Update{})
		if res := tmInspect(m); res.Reject == nil || res.Reject.HTTPCode != 418 {
			t.Fatalf("with default timeout: %+v", res)
		}
		tmUpdate(t, m, "test.a", Update{TimeoutMs: tmPtr(10)})
		if res := tmInspect(m); res.Reject != nil {
			t.Fatalf("with 10ms timeout: %+v", res.Reject)
		}
		if st := tmGet(t, m, "test.a").Stats; st.Requests != 2 || st.Rejects != 1 || st.Timeouts != 1 {
			t.Fatalf("stats = %+v", st)
		}
	})
}

func TestManagerStats(t *testing.T) {
	s := &tmSpec{inspect: func(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
		switch req.Account {
		case "rej":
			return pluginsdk.Reject(0, ""), nil
		case "err":
			return nil, errors.New("fail")
		case "slow":
			time.Sleep(60 * time.Millisecond)
			return nil, nil
		}
		return pluginsdk.Continue(), nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", s)}})
	tmEnable(t, m, "test.a", Update{TimeoutMs: tmPtr(20)})
	for _, acct := range []string{"ok", "ok", "rej", "err", "slow", "ok"} {
		req := tmRequest()
		req.Account = acct
		m.InspectRequest(context.Background(), req)
	}
	st := tmGet(t, m, "test.a").Stats
	if st.Requests != 6 || st.Rejects != 1 || st.Errors != 1 || st.Timeouts != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if st.AvgLatencyUs <= 0 {
		t.Fatalf("avg latency = %d, want > 0 after a 60ms call", st.AvgLatencyUs)
	}
	// Disabling keeps the counters (they are per entry, not per instance).
	tmUpdate(t, m, "test.a", Update{Enabled: tmPtr(false)})
	if st2 := tmGet(t, m, "test.a").Stats; st2.Requests != 6 || st2.Rejects != 1 {
		t.Fatalf("stats after disable = %+v", st2)
	}
}

func TestManagerDisableRemovesFromChain(t *testing.T) {
	rec := &tmRecorder{}
	sa := &tmSpec{inspect: tmRecording(rec, "test.a", nil)}
	sb := &tmSpec{inspect: tmRecording(rec, "test.b", nil)}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmHook("test.a", sa), tmHook("test.b", sb)}})
	tmEnable(t, m, "test.a", Update{Priority: tmPtr(10)})
	tmEnable(t, m, "test.b", Update{})
	tmInspect(m)
	if got := rec.take(); !reflect.DeepEqual(got, []string{"test.a", "test.b"}) {
		t.Fatalf("calls = %v", got)
	}

	tmUpdate(t, m, "test.a", Update{Enabled: tmPtr(false)})
	tmInspect(m)
	if got := rec.take(); !reflect.DeepEqual(got, []string{"test.b"}) {
		t.Fatalf("calls after disabling test.a = %v", got)
	}
	if !m.Active() {
		t.Fatal("Active() = false with test.b enabled")
	}
	if got := tmGet(t, m, "test.a"); got.Status != StatusStopped || got.Enabled {
		t.Fatalf("test.a = %+v", got)
	}

	tmUpdate(t, m, "test.b", Update{Enabled: tmPtr(false)})
	if m.Active() {
		t.Fatal("Active() = true with everything disabled")
	}
	tmInspect(m)
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("disabled plugins called: %v", got)
	}

	tmEnable(t, m, "test.a", Update{})
	tmInspect(m)
	if got := rec.take(); !reflect.DeepEqual(got, []string{"test.a"}) {
		t.Fatalf("calls after re-enable = %v", got)
	}
	if news, shutdowns := sa.counts(); news != 2 || shutdowns != 1 {
		t.Fatalf("test.a instances=%d shutdowns=%d, want 2/1", news, shutdowns)
	}
}

func TestManagerStop(t *testing.T) {
	rec := &tmRecorder{}
	sa := &tmSpec{inspect: tmRecording(rec, "test.a", nil)}
	sev := &tmSpec{}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{
		tmHook("test.a", sa),
		tmEventBuiltin("test.ev", sev, pluginsdk.EventLeaseCreated),
	}})
	tmEnable(t, m, "test.a", Update{})
	tmEnable(t, m, "test.ev", Update{})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate, PlatformID: "p", Account: "a"})

	m.Stop(context.Background())

	if got := sev.received(); len(got) != 1 || got[0].Type != pluginsdk.EventLeaseCreated {
		t.Fatalf("queued event not flushed on Stop: %+v", got)
	}
	if _, shutdowns := sa.counts(); shutdowns != 1 {
		t.Fatalf("test.a shutdowns = %d", shutdowns)
	}
	if m.Active() {
		t.Fatal("Active() after Stop")
	}
	tmInspect(m)
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("plugins called after Stop: %v", got)
	}
	if _, err := m.Update(context.Background(), "test.a", Update{Enabled: tmPtr(true)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update after Stop err = %v, want ErrConflict", err)
	}
	if _, err := m.Rescan(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Rescan after Stop err = %v, want ErrConflict", err)
	}
	if got := tmGet(t, m, "test.a"); got.Status != StatusStopped {
		t.Fatalf("status after Stop = %q", got.Status)
	}
	m.Stop(context.Background()) // idempotent
	if _, shutdowns := sa.counts(); shutdowns != 1 {
		t.Fatalf("second Stop closed again: %d", shutdowns)
	}
}

func TestManagerNilSafe(t *testing.T) {
	var m *Manager
	if m.Active() {
		t.Fatal("nil manager active")
	}
	m.ObserveRequest(proxy.RequestLogEntry{})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate})
}

// --- events ---

func TestManagerEventsDelivery(t *testing.T) {
	sev, slc, shook, serr := &tmSpec{}, &tmSpec{}, &tmSpec{}, &tmSpec{}
	serr.handle = func(context.Context, []pluginsdk.Event) error { return errors.New("sink down") }
	m := tmStartManager(t, ManagerConfig{
		PlatformName: func(id string) string { return "name-" + id },
		Builtins: []Builtin{
			tmEventBuiltin("test.ev", sev, pluginsdk.EventRequestFinished, "lease.*"),
			tmEventBuiltin("test.lc", slc, pluginsdk.EventLeaseCreated),
			tmEventBuiltin("test.err", serr, "*"),
			tmHook("test.hook", shook),
		},
	})
	for _, id := range []string{"test.ev", "test.lc", "test.err", "test.hook"} {
		tmEnable(t, m, id, Update{})
	}
	if got := tmGet(t, m, "test.ev").Capabilities.Events; !reflect.DeepEqual(got, []string{
		pluginsdk.EventRequestFinished, pluginsdk.EventLeaseCreated, pluginsdk.EventLeaseReplaced,
		pluginsdk.EventLeaseRemoved, pluginsdk.EventLeaseExpired,
	}) {
		t.Fatalf("test.ev effective events = %v", got)
	}
	if got := tmGet(t, m, "test.hook").Capabilities.Events; len(got) != 0 {
		t.Fatalf("test.hook events = %v", got)
	}

	started := time.Unix(1_700_000_000, 123_000_000)
	m.ObserveRequest(proxy.RequestLogEntry{
		StartedAtNs:         started.UnixNano(),
		ProxyType:           proxy.ProxyTypeReverse,
		ClientIP:            "10.0.0.1",
		PlatformID:          "p1",
		PlatformName:        "Plat One",
		Account:             "acct",
		TargetHost:          "api.example.com",
		TargetURL:           "https://api.example.com/v1",
		NodeHash:            "nodehash",
		NodeTag:             "sub/tag",
		EgressIP:            "1.2.3.4",
		DurationNs:          int64(1500 * time.Millisecond),
		FirstByteDurationNs: int64(100 * time.Millisecond),
		NetOK:               true,
		HTTPMethod:          "POST",
		HTTPStatus:          201,
		ResinError:          "",
		UpstreamStage:       "",
		IngressBytes:        10,
		EgressBytes:         20,
		ReqHeaders:          []byte("Authorization: secret-header"),
		ReqBody:             []byte("secret-body"),
	})
	hash := node.HashFromRawOptions([]byte(`{"type":"direct"}`))
	createdNs := time.Unix(1_600_000_000, 0).UnixNano()
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate, PlatformID: "p1", Account: "acct", NodeHash: hash, EgressIP: netip.MustParseAddr("5.6.7.8"), CreatedAtNs: createdNs})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseTouch, PlatformID: "p1", Account: "acct"})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseReplace, PlatformID: "p1", Account: "acct", NodeHash: hash, CreatedAtNs: createdNs})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseRemove, PlatformID: "p1", Account: "acct", CreatedAtNs: createdNs})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseExpire, PlatformID: "p2", Account: "other"})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseEventType(99), PlatformID: "p1"})

	// Disabling stops the queue, which flushes everything queued so far.
	for _, id := range []string{"test.ev", "test.lc", "test.err", "test.hook"} {
		tmUpdate(t, m, id, Update{Enabled: tmPtr(false)})
	}

	evs := sev.received()
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	wantTypes := []string{pluginsdk.EventRequestFinished, pluginsdk.EventLeaseCreated, pluginsdk.EventLeaseReplaced, pluginsdk.EventLeaseRemoved, pluginsdk.EventLeaseExpired}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Fatalf("test.ev event types = %v, want %v", types, wantTypes)
	}

	if !evs[0].Time.Equal(started.Add(1500 * time.Millisecond)) {
		t.Fatalf("request.finished time = %v", evs[0].Time)
	}
	if strings.Contains(string(evs[0].Data), "secret") {
		t.Fatalf("request.finished leaks headers/body: %s", evs[0].Data)
	}
	var rf pluginsdk.RequestFinishedData
	if err := json.Unmarshal(evs[0].Data, &rf); err != nil {
		t.Fatal(err)
	}
	wantRF := pluginsdk.RequestFinishedData{
		StartedAt: started.UTC(), ProxyType: pluginsdk.ProxyTypeReverse, ClientIP: "10.0.0.1",
		PlatformID: "p1", PlatformName: "Plat One", Account: "acct", TargetHost: "api.example.com",
		TargetURL: "https://api.example.com/v1", NodeHash: "nodehash", NodeTag: "sub/tag", EgressIP: "1.2.3.4",
		DurationNs: int64(1500 * time.Millisecond), FirstByteNs: int64(100 * time.Millisecond), NetOK: true,
		HTTPMethod: "POST", HTTPStatus: 201, IngressBytes: 10, EgressBytes: 20,
	}
	if !rf.StartedAt.Equal(wantRF.StartedAt) {
		t.Fatalf("started_at = %v, want %v", rf.StartedAt, wantRF.StartedAt)
	}
	rf.StartedAt = wantRF.StartedAt
	if rf != wantRF {
		t.Fatalf("request.finished data =\n%+v\nwant\n%+v", rf, wantRF)
	}

	var created, removed, expired pluginsdk.LeaseEventData
	for i, dst := range map[int]*pluginsdk.LeaseEventData{1: &created, 3: &removed, 4: &expired} {
		if err := json.Unmarshal(evs[i].Data, dst); err != nil {
			t.Fatal(err)
		}
	}
	if created.PlatformID != "p1" || created.PlatformName != "name-p1" || created.Account != "acct" ||
		created.NodeHash != hash.Hex() || created.EgressIP != "5.6.7.8" || created.CreatedAt == nil ||
		!created.CreatedAt.Equal(time.Unix(0, createdNs).UTC()) {
		t.Fatalf("lease.created data = %+v", created)
	}
	if removed.CreatedAt == nil || !removed.CreatedAt.Equal(time.Unix(0, createdNs)) {
		t.Fatalf("lease.removed created_at = %v", removed.CreatedAt)
	}
	if expired.PlatformID != "p2" || expired.PlatformName != "name-p2" || expired.NodeHash != "" || expired.EgressIP != "" {
		t.Fatalf("lease.expired data = %+v", expired)
	}

	if got := slc.received(); len(got) != 1 || got[0].Type != pluginsdk.EventLeaseCreated {
		t.Fatalf("test.lc received %+v, want only lease.created", got)
	}
	if got := shook.received(); len(got) != 0 {
		t.Fatalf("plugin without event capability received %+v", got)
	}

	if st := tmGet(t, m, "test.ev").Stats; st.EventsDelivered != 5 || st.EventErrors != 0 || st.EventsDropped != 0 {
		t.Fatalf("test.ev stats = %+v", st)
	}
	if st := tmGet(t, m, "test.lc").Stats; st.EventsDelivered != 1 {
		t.Fatalf("test.lc stats = %+v", st)
	}
	if st := tmGet(t, m, "test.err").Stats; st.EventsDelivered != 0 || st.EventErrors != 1 {
		t.Fatalf("test.err stats = %+v (want one failed batch)", st)
	}

	// Events for disabled plugins go nowhere.
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate, PlatformID: "p1"})
	if got := len(sev.received()); got != 5 {
		t.Fatalf("disabled plugin received events: %d", got)
	}
}

func TestManagerEventsFlushPeriodically(t *testing.T) {
	s := &tmSpec{}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmEventBuiltin("test.ev", s, "lease.*")}})
	tmEnable(t, m, "test.ev", Update{})
	m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate, PlatformID: "p", Account: "a"})
	// defaultEventFlush is 1s; allow generous slack.
	tmWaitFor(t, 3*time.Second, "periodic event flush", func() bool {
		return tmGet(t, m, "test.ev").Stats.EventsDelivered == 1
	})
	if got := s.received(); len(got) != 1 || got[0].Type != pluginsdk.EventLeaseCreated {
		t.Fatalf("received %+v", got)
	}
}

func TestManagerEventsDroppedWhenQueueFull(t *testing.T) {
	release := make(chan struct{})
	var entered atomic.Bool
	s := &tmSpec{handle: func(ctx context.Context, _ []pluginsdk.Event) error {
		entered.Store(true)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	m := tmStartManager(t, ManagerConfig{Builtins: []Builtin{tmEventBuiltin("test.ev", s, pluginsdk.EventLeaseCreated)}})
	tmEnable(t, m, "test.ev", Update{})

	const total = defaultEventQueueSize + defaultEventBatchSize + 1000
	for i := 0; i < total; i++ {
		m.OnLeaseEvent(routing.LeaseEvent{Type: routing.LeaseCreate, PlatformID: "p", Account: fmt.Sprint(i)})
	}
	live := tmGet(t, m, "test.ev").Stats
	if live.EventsDropped == 0 {
		close(release)
		t.Fatalf("no drops reported while the queue is full: %+v", live)
	}
	close(release)
	tmUpdate(t, m, "test.ev", Update{Enabled: tmPtr(false)})

	st := tmGet(t, m, "test.ev").Stats
	if st.EventsDropped < live.EventsDropped || st.EventsDelivered+st.EventsDropped != total {
		t.Fatalf("delivered %d + dropped %d != pushed %d (live drops %d)", st.EventsDelivered, st.EventsDropped, total, live.EventsDropped)
	}
	if got := int64(len(s.received())); got != st.EventsDelivered {
		t.Fatalf("plugin received %d events, stats say %d", got, st.EventsDelivered)
	}
}

// --- eventQueue / lazyEvent units ---

func tmLazy(typ string) *lazyEvent {
	return newLazyEvent(typ, time.Unix(0, 0), func() any { return map[string]string{"t": typ} })
}

type tmSink struct {
	mu      sync.Mutex
	batches [][]string
	results []int
	errs    int
}

func (s *tmSink) deliver(_ context.Context, events []pluginsdk.Event) error {
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	s.mu.Lock()
	s.batches = append(s.batches, types)
	s.mu.Unlock()
	return nil
}

func (s *tmSink) onResult(n int, err error) {
	s.mu.Lock()
	s.results = append(s.results, n)
	if err != nil {
		s.errs++
	}
	s.mu.Unlock()
}

func (s *tmSink) snapshot() ([][]string, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.batches...), append([]int(nil), s.results...)
}

func TestEventQueueBatchesAndFlushesOnStop(t *testing.T) {
	sink := &tmSink{}
	q := newEventQueue(16, 3, time.Hour, sink.deliver, sink.onResult)
	for i := 0; i < 7; i++ {
		q.push(tmLazy(fmt.Sprintf("e%d", i)))
	}
	tmWaitFor(t, 2*time.Second, "two full batches", func() bool {
		_, results := sink.snapshot()
		return len(results) == 2
	})
	q.stop(context.Background())
	batches, results := sink.snapshot()
	wantBatches := [][]string{{"e0", "e1", "e2"}, {"e3", "e4", "e5"}, {"e6"}}
	if !reflect.DeepEqual(batches, wantBatches) || !reflect.DeepEqual(results, []int{3, 3, 1}) {
		t.Fatalf("batches = %v results = %v", batches, results)
	}
	// Pushing after stop is a silent no-op.
	q.push(tmLazy("late"))
	q.stop(context.Background())
	if batches, _ := sink.snapshot(); len(batches) != 3 || q.dropped.Load() != 0 {
		t.Fatalf("push after stop delivered or dropped: %v dropped=%d", batches, q.dropped.Load())
	}
}

func TestEventQueueFlushInterval(t *testing.T) {
	sink := &tmSink{}
	q := newEventQueue(16, 100, 10*time.Millisecond, sink.deliver, sink.onResult)
	defer q.stop(context.Background())
	q.push(tmLazy("a"))
	q.push(tmLazy("b"))
	tmWaitFor(t, 2*time.Second, "timer flush", func() bool {
		batches, _ := sink.snapshot()
		n := 0
		for _, b := range batches {
			n += len(b)
		}
		return n == 2
	})
}

func TestEventQueueDropsWhenFull(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var delivered []string
	var mu sync.Mutex
	deliver := func(ctx context.Context, events []pluginsdk.Event) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		for _, ev := range events {
			delivered = append(delivered, ev.Type)
		}
		mu.Unlock()
		return nil
	}
	q := newEventQueue(2, 1, time.Hour, deliver, nil)
	q.push(tmLazy("e0"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start delivering")
	}
	// The worker is blocked in deliver: two events fit in the buffer.
	for _, typ := range []string{"e1", "e2", "e3", "e4", "e5"} {
		q.push(tmLazy(typ))
	}
	if got := q.dropped.Load(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
	close(release)
	q.stop(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(delivered, []string{"e0", "e1", "e2"}) {
		t.Fatalf("delivered = %v", delivered)
	}
}

func TestEventQueueStopBoundedByContext(t *testing.T) {
	entered := make(chan struct{}, 1)
	deliver := func(ctx context.Context, _ []pluginsdk.Event) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	q := newEventQueue(4, 1, time.Hour, deliver, nil)
	q.push(tmLazy("e0"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start delivering")
	}
	q.push(tmLazy("e1"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	q.stop(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stop waited %v despite a 50ms context", elapsed)
	}
	// stop cancelled the delivery in progress and waited for the worker, so
	// the plugin can be closed right away.
	select {
	case <-q.doneCh:
	default:
		t.Fatal("stop returned before the worker exited")
	}
	// e0 failed with the cancelled delivery; e1 was never sent.
	if got := q.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

func TestEventQueueCapsBatchBytes(t *testing.T) {
	sink := &tmSink{}
	q := newEventQueue(16, 100, time.Hour, sink.deliver, sink.onResult)
	// Two of these fit in one batch, three do not.
	big := strings.Repeat("x", maxEventBatchBytes*2/5)
	for i := 0; i < 5; i++ {
		q.push(newLazyEvent(fmt.Sprintf("e%d", i), time.Unix(0, 0), func() any { return big }))
	}
	q.stop(context.Background())
	if _, results := sink.snapshot(); !reflect.DeepEqual(results, []int{2, 2, 1}) {
		t.Fatalf("batch sizes = %v, want [2 2 1]", results)
	}
}

func TestEventQueueReportsDeliveryErrors(t *testing.T) {
	sink := &tmSink{}
	q := newEventQueue(4, 10, time.Hour, func(context.Context, []pluginsdk.Event) error {
		return errors.New("fail")
	}, sink.onResult)
	q.push(tmLazy("a"))
	q.push(tmLazy("b"))
	q.stop(context.Background())
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.errs != 1 || !reflect.DeepEqual(sink.results, []int{2}) {
		t.Fatalf("results = %v errs = %d", sink.results, sink.errs)
	}
}

func TestLazyEventMarshalsOnce(t *testing.T) {
	var builds atomic.Int32
	at := time.Unix(1_700_000_000, 0)
	ev := newLazyEvent("x.y", at, func() any {
		builds.Add(1)
		return map[string]int{"n": 1}
	})
	a, b := ev.event(), ev.event()
	if builds.Load() != 1 {
		t.Fatalf("build called %d times", builds.Load())
	}
	if a.Type != "x.y" || !a.Time.Equal(at) || string(a.Data) != `{"n":1}` || string(b.Data) != string(a.Data) {
		t.Fatalf("events = %+v / %+v", a, b)
	}
	bad := newLazyEvent("bad", at, func() any { return make(chan int) })
	if got := string(bad.event().Data); got != `{}` {
		t.Fatalf("unmarshalable payload -> %s, want {}", got)
	}
}

// --- package discovery (no process is started) ---

func tmWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tmManifestJSON(t *testing.T, mf pluginsdk.Manifest) string {
	t.Helper()
	data, err := json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestManagerPackageLoadErrors(t *testing.T) {
	dir := t.TempDir()
	base := func(id string) pluginsdk.Manifest {
		return pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion, ID: id, Name: id, Version: "1.0.0",
			Capabilities: pluginsdk.Capabilities{RequestHook: true},
			Runtimes:     map[string]pluginsdk.RuntimeSpec{"any": {Command: []string{"bin/plugin"}}},
		}
	}
	tmWriteFile(t, filepath.Join(dir, "pkg.badjson", pluginsdk.ManifestFileName), `{not json`)
	tmWriteFile(t, filepath.Join(dir, "pkg.mismatch", pluginsdk.ManifestFileName), tmManifestJSON(t, base("pkg.other")))
	noRuntime := base("pkg.noruntime")
	noRuntime.Runtimes = map[string]pluginsdk.RuntimeSpec{"plan9-mips": {Command: []string{"bin/plugin"}}}
	tmWriteFile(t, filepath.Join(dir, "pkg.noruntime", pluginsdk.ManifestFileName), tmManifestJSON(t, noRuntime))
	newer := base("pkg.newer")
	newer.MinResinVersion = "99.0.0"
	tmWriteFile(t, filepath.Join(dir, "pkg.newer", pluginsdk.ManifestFileName), tmManifestJSON(t, newer))
	if err := os.MkdirAll(filepath.Join(dir, "pkg.nomanifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Skipped directories.
	tmWriteFile(t, filepath.Join(dir, "Invalid_Name", pluginsdk.ManifestFileName), tmManifestJSON(t, base("Invalid_Name")))
	tmWriteFile(t, filepath.Join(dir, ".hidden", pluginsdk.ManifestFileName), tmManifestJSON(t, base("hidden")))
	tmWriteFile(t, filepath.Join(dir, "test.a", pluginsdk.ManifestFileName), tmManifestJSON(t, base("test.a")))
	tmWriteFile(t, filepath.Join(dir, "notadir"), "x")

	st := tmNewStore(model.PluginSettings{ID: "pkg.badjson", Priority: 3, TimeoutMs: 1000, ConfigJSON: `{}`})
	m := tmStartManager(t, ManagerConfig{Store: st, PluginDir: dir, ExternalEnabled: true, Builtins: []Builtin{tmHook("test.a", &tmSpec{})}})

	var ids []string
	for _, info := range m.List() {
		ids = append(ids, info.ID)
	}
	wantIDs := []string{"pkg.badjson", "pkg.mismatch", "pkg.newer", "pkg.nomanifest", "pkg.noruntime", "test.a"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("ids = %v, want %v", ids, wantIDs)
	}
	if got := tmGet(t, m, "test.a"); got.Source != SourceBuiltin {
		t.Fatalf("package shadowed builtin: %+v", got)
	}
	wantErr := map[string]string{
		"pkg.badjson":    "invalid manifest",
		"pkg.mismatch":   "does not match",
		"pkg.noruntime":  "no runtime for",
		"pkg.newer":      "99.0.0",
		"pkg.nomanifest": pluginsdk.ManifestFileName,
	}
	for id, sub := range wantErr {
		info := tmGet(t, m, id)
		if info.Source != SourcePackage || info.Enabled || info.Status != StatusError || !strings.Contains(info.LastError, sub) {
			t.Fatalf("%s: source=%q status=%q last_error=%q, want error containing %q", id, info.Source, info.Status, info.LastError, sub)
		}
		if _, err := m.Update(context.Background(), id, Update{Enabled: tmPtr(true)}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: enable err = %v, want ErrInvalidArgument", id, err)
		}
	}
	if got := tmGet(t, m, "pkg.badjson"); got.Priority != 3 {
		t.Fatalf("stored settings not applied to package: %+v", got)
	}

	// Uninstall removes the package directory, its data dir and settings.
	dataDir := filepath.Join(dir, ".data", "pkg.badjson")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(context.Background(), "pkg.badjson"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	for _, p := range []string{filepath.Join(dir, "pkg.badjson"), dataDir} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after uninstall (err=%v)", p, err)
		}
	}
	if _, ok := st.row("pkg.badjson"); ok || !reflect.DeepEqual(st.deletes, []string{"pkg.badjson"}) {
		t.Fatalf("settings not deleted: deletes=%v", st.deletes)
	}
	if _, err := m.Get("pkg.badjson"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after uninstall err = %v", err)
	}
}

func TestManagerUninstallRemovesEntryBeforeStoppingInstance(t *testing.T) {
	dir := t.TempDir()
	manifest := pluginsdk.Manifest{
		SchemaVersion: pluginsdk.SchemaVersion,
		ID:            "pkg.running",
		Name:          "running",
		Version:       "1.0.0",
		Capabilities:  pluginsdk.Capabilities{RequestHook: true},
		Runtimes:      map[string]pluginsdk.RuntimeSpec{"any": {Command: []string{"bin/plugin"}}},
	}
	tmWriteFile(t, filepath.Join(dir, manifest.ID, pluginsdk.ManifestFileName), tmManifestJSON(t, manifest))
	m := tmStartManager(t, ManagerConfig{PluginDir: dir, ExternalEnabled: true})
	e := m.entries[manifest.ID]
	if e == nil {
		t.Fatalf("package entry %q was not discovered", manifest.ID)
	}
	closed := false
	e.settings.Enabled = true
	e.inst = &tmNoopInstance{closeFn: func(context.Context) error {
		closed = true
		if _, ok := m.entries[manifest.ID]; ok {
			t.Errorf("plugin entry still present while instance is stopping")
		}
		return nil
	}}
	m.rebuildChain()

	if err := m.Uninstall(context.Background(), manifest.ID); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !closed {
		t.Fatal("running instance was not closed")
	}
	if _, err := m.Get(manifest.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after uninstall = %v, want ErrNotFound", err)
	}
	if _, err := m.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan after uninstall: %v", err)
	}
	if _, err := m.Get(manifest.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plugin reappeared after uninstall: %v", err)
	}
}

func TestManagerPackageLoadErrorStatusAfterRescan(t *testing.T) {
	dir := t.TempDir()
	m := tmStartManager(t, ManagerConfig{PluginDir: dir, ExternalEnabled: true})
	tmWriteFile(t, filepath.Join(dir, "pkg.broken", pluginsdk.ManifestFileName), `{not json`)
	if _, err := m.Rescan(context.Background()); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	info := tmGet(t, m, "pkg.broken")
	if info.Status != StatusError || !strings.Contains(info.LastError, "invalid manifest") {
		t.Fatalf("status=%q last_error=%q, want error", info.Status, info.LastError)
	}
}

func TestManagerPackageLoadErrorStatusAfterStart(t *testing.T) {
	dir := t.TempDir()
	tmWriteFile(t, filepath.Join(dir, "pkg.broken", pluginsdk.ManifestFileName), `{not json`)
	m := tmStartManager(t, ManagerConfig{PluginDir: dir, ExternalEnabled: true})
	info := tmGet(t, m, "pkg.broken")
	if info.Status != StatusError || !strings.Contains(info.LastError, "invalid manifest") {
		t.Fatalf("status=%q last_error=%q, want error", info.Status, info.LastError)
	}
}

func TestManagerExternalDisabledIgnoresPackages(t *testing.T) {
	dir := t.TempDir()
	mf := pluginsdk.Manifest{
		SchemaVersion: pluginsdk.SchemaVersion, ID: "pkg.ok", Name: "ok", Version: "1.0.0",
		Runtimes: map[string]pluginsdk.RuntimeSpec{"any": {Command: []string{"bin/plugin"}}},
	}
	tmWriteFile(t, filepath.Join(dir, "pkg.ok", pluginsdk.ManifestFileName), tmManifestJSON(t, mf))
	m := tmStartManager(t, ManagerConfig{PluginDir: dir, ExternalEnabled: false})
	if _, err := m.Get("pkg.ok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("package visible with external plugins disabled: %v", err)
	}
	if m.ExternalEnabled() || m.PluginDir() != dir {
		t.Fatalf("ExternalEnabled=%v PluginDir=%q", m.ExternalEnabled(), m.PluginDir())
	}
}
