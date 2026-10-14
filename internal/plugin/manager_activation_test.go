package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const activationTestID = "test.activation"

// This builtin models a prepared generation and an autonomous worker whose
// shutdown never calls back into Manager. All observations use the fixture lock,
// so they are safe while Update is blocked in the store with Manager.mu held.
type activationFixture struct {
	mu         sync.Mutex
	manager    *Manager
	store      *tmStore
	instances  []*activationPlugin
	violations []string
	failConfig func(next, current string) error
}

type activationPlugin struct {
	f           *activationFixture
	config      string
	configs     []string
	activations []string
	active      bool
	closed      bool
	stop        chan struct{}
	done        chan struct{}
}

type activationSnapshot struct {
	config      string
	configs     []string
	activations []string
	active      bool
	closed      bool
}

func (f *activationFixture) builtin() Builtin {
	return Builtin{
		Manifest: pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion,
			ID:            activationTestID, Name: "Activation boundary", Version: "1.0.0",
		},
		New: func() pluginsdk.Plugin {
			f.mu.Lock()
			defer f.mu.Unlock()
			p := &activationPlugin{f: f}
			f.instances = append(f.instances, p)
			return p
		},
	}
}

func (p *activationPlugin) Configure(_ context.Context, raw json.RawMessage) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	config := string(raw)
	p.configs = append(p.configs, config)
	if p.f.failConfig != nil {
		if err := p.f.failConfig(config, p.config); err != nil {
			return err
		}
	}
	p.stopLocked()
	p.config = config
	return nil
}

func (p *activationPlugin) Activate() {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.closed || p.active {
		return
	}
	// Inspect fields directly under the Manager lock held by this callback;
	// calling Get/BuiltinService here would deadlock and violate the contract.
	m := p.f.manager
	e := m.entries[activationTestID]
	stored, ok := m.stored[activationTestID]
	if e == nil || !e.settings.Enabled || string(e.config) != p.config ||
		!ok || !stored.Enabled || stored.ConfigJSON != p.config {
		p.f.violations = append(p.f.violations, "activated before entry settings committed")
	}
	if p.f.store != nil {
		row, ok := p.f.store.row(activationTestID)
		if !ok || !row.Enabled || row.ConfigJSON != p.config {
			p.f.violations = append(p.f.violations, "activated before settings persisted")
		}
	}
	p.active = true
	p.activations = append(p.activations, p.config)
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	stop, done := p.stop, p.done
	go func() {
		<-stop
		close(done)
	}()
}

func (p *activationPlugin) stopLocked() {
	if p.active {
		close(p.stop)
		<-p.done
		p.active = false
	}
}

func (p *activationPlugin) Close() error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.stopLocked()
	p.closed = true
	return nil
}

func (f *activationFixture) snapshot(t *testing.T) []activationSnapshot {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.violations) != 0 {
		t.Errorf("activation ordering violations: %v", f.violations)
	}
	out := make([]activationSnapshot, len(f.instances))
	for i, p := range f.instances {
		out[i] = activationSnapshot{
			config: p.config, active: p.active, closed: p.closed,
			configs:     append([]string(nil), p.configs...),
			activations: append([]string(nil), p.activations...),
		}
	}
	return out
}

type activationStore struct {
	*tmStore
	gate func(model.PluginSettings) error
}

func (s *activationStore) UpsertPluginSettings(row model.PluginSettings) error {
	if s.gate != nil {
		if err := s.gate(row); err != nil {
			return err
		}
	}
	return s.tmStore.UpsertPluginSettings(row)
}

func newActivationManager(t *testing.T, enabled bool, fail func(string, string) error) (*Manager, *activationFixture, *activationStore) {
	t.Helper()
	st := &activationStore{tmStore: tmNewStore(model.PluginSettings{
		ID: activationTestID, Enabled: enabled, TimeoutMs: DefaultTimeoutMs,
		ConfigJSON: `{"value":1}`,
	})}
	f := &activationFixture{store: st.tmStore, failConfig: fail}
	m := NewManager(ManagerConfig{Store: st, Builtins: []Builtin{f.builtin()}, Logf: tmLogf(t)})
	f.manager = m
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m, f, st
}

func blockActivationStore(t *testing.T, st *activationStore) (<-chan model.PluginSettings, func(error)) {
	t.Helper()
	entered, result := make(chan model.PluginSettings, 1), make(chan error, 1)
	var once sync.Once
	release := func(err error) { once.Do(func() { result <- err }) }
	st.gate = func(row model.PluginSettings) error {
		entered <- row
		return <-result
	}
	t.Cleanup(func() { release(nil) })
	return entered, release
}

func awaitActivation[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle operation did not complete (possible Manager reentry)")
		var zero T
		return zero
	}
}

func updateActivationAsync(m *Manager, update Update) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := m.Update(context.Background(), activationTestID, update)
		done <- err
	}()
	return done
}

func TestManagerBuiltinActivationWaitsForEnableCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist_failure_%t", fail), func(t *testing.T) {
			m, f, st := newActivationManager(t, false, nil)
			entered, release := blockActivationStore(t, st)
			done := updateActivationAsync(m, Update{Enabled: tmPtr(true), Config: json.RawMessage(`{"value":2}`)})
			row := awaitActivation(t, entered)
			if !row.Enabled || row.ConfigJSON != `{"value":2}` {
				t.Fatalf("pending row = %+v", row)
			}
			before := f.snapshot(t)
			if len(before) != 1 || before[0].active || before[0].closed || len(before[0].activations) != 0 {
				t.Fatalf("uncommitted enable started autonomous work: %+v", before)
			}
			var persistErr error
			if fail {
				persistErr = errors.New("store unavailable")
			}
			release(persistErr)
			if err := awaitActivation(t, done); !errors.Is(err, persistErr) {
				t.Fatalf("Update error = %v, want %v", err, persistErr)
			}
			after := f.snapshot(t)[0]
			if fail {
				if after.active || !after.closed || len(after.activations) != 0 {
					t.Fatalf("failed enable activated: %+v", after)
				}
			} else if !after.active || after.closed || !reflect.DeepEqual(after.activations, []string{`{"value":2}`}) {
				t.Fatalf("committed enable not activated: %+v", after)
			}
		})
	}
}

func TestManagerBuiltinActivationReconfigureCommitOrRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist_failure_%t", fail), func(t *testing.T) {
			m, f, st := newActivationManager(t, true, nil)
			entered, release := blockActivationStore(t, st)
			done := updateActivationAsync(m, Update{Config: json.RawMessage(`{"value":2}`)})
			awaitActivation(t, entered)
			before := f.snapshot(t)[0]
			if before.active || before.config != `{"value":2}` || !reflect.DeepEqual(before.activations, []string{`{"value":1}`}) {
				t.Fatalf("uncommitted reconfiguration activated: %+v", before)
			}
			var persistErr error
			wantConfig := `{"value":2}`
			if fail {
				persistErr = errors.New("store unavailable")
				wantConfig = `{"value":1}`
			}
			release(persistErr)
			if err := awaitActivation(t, done); !errors.Is(err, persistErr) {
				t.Fatalf("Update error = %v, want %v", err, persistErr)
			}
			after := f.snapshot(t)
			if len(after) != 1 || !after[0].active || after[0].closed || after[0].config != wantConfig ||
				!reflect.DeepEqual(after[0].activations, []string{`{"value":1}`, wantConfig}) {
				t.Fatalf("committed/restored generation not activated: %+v", after)
			}
		})
	}
}

func TestManagerBuiltinActivationDisabledValidationIsDormant(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist_failure_%t", fail), func(t *testing.T) {
			m, f, st := newActivationManager(t, false, nil)
			entered, release := blockActivationStore(t, st)
			done := updateActivationAsync(m, Update{Config: json.RawMessage(`{"value":2}`)})
			awaitActivation(t, entered)
			before := f.snapshot(t)
			if len(before) != 1 || before[0].active || !before[0].closed || len(before[0].activations) != 0 {
				t.Fatalf("disabled validation activated: %+v", before)
			}
			var persistErr error
			if fail {
				persistErr = errors.New("store unavailable")
			}
			release(persistErr)
			if err := awaitActivation(t, done); !errors.Is(err, persistErr) {
				t.Fatalf("Update error = %v, want %v", err, persistErr)
			}
			if after := f.snapshot(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("disabled instance changed after persistence: %+v", after)
			}
		})
	}
}

func TestManagerBuiltinActivationDisableKeepsCommittedWorkerUntilSaved(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist_failure_%t", fail), func(t *testing.T) {
			m, f, st := newActivationManager(t, true, nil)
			entered, release := blockActivationStore(t, st)
			// A combined config/disable update also creates a dormant validation
			// instance; it must not interrupt the still-committed active instance.
			done := updateActivationAsync(m, Update{Enabled: tmPtr(false), Config: json.RawMessage(`{"value":2}`)})
			awaitActivation(t, entered)
			before := f.snapshot(t)
			if len(before) != 2 || !before[0].active || before[0].closed ||
				!before[1].closed || len(before[1].activations) != 0 {
				t.Fatalf("disable changed committed worker before save: %+v", before)
			}
			var persistErr error
			if fail {
				persistErr = errors.New("store unavailable")
			}
			release(persistErr)
			if err := awaitActivation(t, done); !errors.Is(err, persistErr) {
				t.Fatalf("Update error = %v, want %v", err, persistErr)
			}
			after := f.snapshot(t)
			if after[0].active != fail || after[0].closed == fail ||
				!reflect.DeepEqual(after[0].activations, []string{`{"value":1}`}) || len(after[1].activations) != 0 {
				t.Fatalf("disable result violated committed activity: %+v", after)
			}
		})
	}
}

func TestManagerBuiltinActivationStartupRescanAndStop(t *testing.T) {
	first := true
	m, f, st := newActivationManager(t, true, func(_, _ string) error {
		if first {
			first = false
			return errors.New("first startup unavailable")
		}
		return nil
	})
	if got := f.snapshot(t); len(got) != 1 || !got[0].closed || len(got[0].activations) != 0 {
		t.Fatalf("failed startup activated: %+v", got)
	}
	if _, err := m.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A settings-only update must not reset an already-active schedule.
	if _, err := m.Update(context.Background(), activationTestID, Update{Priority: tmPtr(3)}); err != nil {
		t.Fatal(err)
	}
	got := f.snapshot(t)
	if len(got) != 2 || !got[1].active || !reflect.DeepEqual(got[1].activations, []string{`{"value":1}`}) || st.upsertCount() != 1 {
		t.Fatalf("rescan/start reset activation: %+v, upserts=%d", got, st.upsertCount())
	}
	stopped := make(chan struct{})
	go func() { m.Stop(context.Background()); close(stopped) }()
	awaitActivation(t, stopped)
	for _, p := range f.instances {
		p.Activate() // Close is terminal even for a stale reference.
	}
	m.Stop(context.Background())
	if _, err := m.Rescan(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Rescan after Stop = %v", err)
	}
	if _, err := m.Update(context.Background(), activationTestID, Update{Enabled: tmPtr(true)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update after Stop = %v", err)
	}
	for _, p := range f.snapshot(t) {
		if p.active || !p.closed {
			t.Fatalf("worker survived Close: %+v", p)
		}
	}
}

func TestManagerBuiltinActivationRestartsOnlyCommittedConfig(t *testing.T) {
	for _, cause := range []string{"configure_uncertain", "rollback_rejected"} {
		t.Run(cause, func(t *testing.T) {
			m, f, st := newActivationManager(t, true, func(next, current string) error {
				if cause == "configure_uncertain" && next == `{"value":2}` {
					return context.DeadlineExceeded
				}
				if cause == "rollback_rejected" && current == `{"value":2}` && next == `{"value":1}` {
					return errors.New("cannot restore on this instance")
				}
				return nil
			})
			if cause == "rollback_rejected" {
				st.setUpsertErr(errors.New("store unavailable"))
			}
			if _, err := m.Update(context.Background(), activationTestID, Update{Config: json.RawMessage(`{"value":2}`)}); err == nil {
				t.Fatal("expected failed update")
			}
			got := f.snapshot(t)
			if len(got) != 2 || !got[0].closed || got[0].active || !got[1].active || got[1].closed ||
				!reflect.DeepEqual(got[0].activations, []string{`{"value":1}`}) ||
				!reflect.DeepEqual(got[1].activations, []string{`{"value":1}`}) || st.upsertCount() != 0 {
				t.Fatalf("restart did not restore committed activation: %+v", got)
			}
		})
	}
}

func TestManagerBuiltinActivationRejectedConfigKeepsPreviousWorker(t *testing.T) {
	m, f, st := newActivationManager(t, true, func(next, _ string) error {
		if next == `{"value":2}` {
			return errors.New("invalid configuration")
		}
		return nil
	})
	if _, err := m.Update(context.Background(), activationTestID, Update{Config: json.RawMessage(`{"value":2}`)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("rejected update = %v", err)
	}
	got := f.snapshot(t)
	if len(got) != 1 || !got[0].active || got[0].closed || got[0].config != `{"value":1}` ||
		!reflect.DeepEqual(got[0].activations, []string{`{"value":1}`}) || st.upsertCount() != 0 {
		t.Fatalf("rejected config disturbed committed worker: %+v", got)
	}
}

type activationInstanceSpy struct {
	tmNoopInstance
	activations int
}

func (s *activationInstanceSpy) Activate() { s.activations++ }

func TestManagerBuiltinActivationDoesNotExtendExternalInstanceProtocol(t *testing.T) {
	m := NewManager(ManagerConfig{})
	spy := &activationInstanceSpy{}
	m.mu.Lock()
	// Even an instance with a coincidentally named method is not an internal
	// builtin plugin and must not receive the activation callback.
	m.activateBuiltin(&entry{source: SourcePackage, settings: model.PluginSettings{Enabled: true}, inst: spy})
	m.activateBuiltin(&entry{builtin: &Builtin{}, settings: model.PluginSettings{Enabled: true}, inst: spy})
	m.mu.Unlock()
	if spy.activations != 0 {
		t.Fatalf("external instance activated %d times", spy.activations)
	}
}
