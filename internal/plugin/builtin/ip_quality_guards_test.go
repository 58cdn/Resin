package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
	"github.com/Resinat/Resin/internal/plugin"
)

type guardedQualityHost struct {
	qualityTestHost
	guards *requestguard.Controller
}

func (h *guardedQualityHost) Capability(name string) (any, bool) {
	return h, name == IPQualityHostCapability
}
func (h *guardedQualityHost) RequestGuards() *requestguard.Controller { return h.guards }

func guardConfig(limit uint64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"enabled_sources":["db-ip"],"source_guards":[{"source":"egress-consistency","enabled":true,"qps":100,"burst":64,"daily_limit":%d,"consecutive_failure_threshold":1,"open_for":"1h"}]}`, limit))
}

func guardAdmission(t *testing.T, c *requestguard.Controller) {
	t.Helper()
	p, err := c.Admit(context.Background(), requestguard.EgressConsistency)
	if err != nil {
		t.Fatalf("expected admission: %v", err)
	}
	p.Finish(requestguard.Neutral)
}

func guardDailyReject(t *testing.T, c *requestguard.Controller) {
	t.Helper()
	p, err := c.Admit(context.Background(), requestguard.EgressConsistency)
	if p != nil {
		p.Finish(requestguard.Neutral)
		t.Fatal("uncommitted policy changed the controller")
	}
	var rejected *requestguard.Rejection
	if !errors.As(err, &rejected) || rejected.Reason != requestguard.DailyLimited {
		t.Fatalf("daily budget was reset or policy not retained: %v", err)
	}
}

func TestIPQualityGuardPoliciesCommitAfterStoreAndSurviveLifecycle(t *testing.T) {
	controller := requestguard.New(func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) })
	host, store := &guardedQualityHost{guards: controller}, &periodicTestStore{}
	manager := plugin.NewManager(plugin.ManagerConfig{Builtins: All(), BuiltinHost: host, Store: store})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Config: guardConfig(1)}); err != nil {
		t.Fatal(err)
	}
	// Disabled save constructs and closes a validation instance. Its policy
	// must not replace the process controller's still-disabled policy.
	guardAdmission(t, controller)
	guardAdmission(t, controller)

	startBlocked := func(update plugin.Update, result error) (<-chan error, func()) {
		t.Helper()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		store.gate = func() error { close(entered); <-release; return result }
		done := make(chan error, 1)
		go func() { _, err := manager.Update(context.Background(), IPQualityID, update); done <- err }()
		<-entered
		return done, unblock
	}
	enabled := true
	done, unblock := startBlocked(plugin.Update{Enabled: &enabled}, nil)
	defer unblock()               // release Store before deferred Manager.Stop on Fatal
	guardAdmission(t, controller) // Store has not committed the first enable.
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.gate = nil
	guardDailyReject(t, controller) // Earlier default-off admissions still count.

	// A wider replacement must remain invisible while Store blocks, and a
	// failed save must restore the old engine without erasing its daily ledger.
	done, rollback := startBlocked(plugin.Update{Config: guardConfig(100)}, errors.New("store unavailable"))
	defer rollback()
	guardDailyReject(t, controller)
	rollback()
	if err := <-done; err == nil {
		t.Fatal("failed save accepted")
	}
	store.gate = nil
	guardDailyReject(t, controller)
	if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Config: guardConfig(4)}); err != nil {
		t.Fatal(err)
	}
	guardAdmission(t, controller)
	guardDailyReject(t, controller)

	store.gate = func() error { return errors.New("store unavailable") }
	enabled = false
	if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Enabled: &enabled}); err == nil {
		t.Fatal("failed disable accepted")
	}
	guardDailyReject(t, controller)
	if _, err := manager.BuiltinService(IPQualityID); err != nil {
		t.Fatalf("failed disable removed the old service: %v", err)
	}
	store.gate = nil
	if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Config: guardConfig(5)}); err != nil {
		t.Fatal(err)
	}
	guardDailyReject(t, controller) // Disabled validation has not applied limit 5.
	enabled = true
	if _, err := manager.Update(context.Background(), IPQualityID, plugin.Update{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	guardAdmission(t, controller)
	guardDailyReject(t, controller)
	manager.Stop(context.Background())

	// Recreating the Manager within the same process also retains the host's
	// controller. Startup activates already-persisted settings, not a new budget.
	restarted := plugin.NewManager(plugin.ManagerConfig{Builtins: All(), BuiltinHost: host, Store: store})
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer restarted.Stop(context.Background())
	guardDailyReject(t, controller)
}

func TestIPQualityGuardMissingCapabilityRejectsBeforeReplacingEngine(t *testing.T) {
	p := ipQualityBuiltin().NewWithHost(qualityTestHost{})
	if err := p.Configure(context.Background(), json.RawMessage(`{"enabled_sources":["db-ip"]}`)); err != nil {
		t.Fatal(err)
	}
	builtin := p.(*ipQuality)
	old := builtin.engine.Load()
	defer builtin.Close()
	if err := p.Configure(context.Background(), guardConfig(1)); err == nil {
		t.Fatal("enabled guard without process controller was accepted")
	}
	if builtin.engine.Load() != old {
		t.Fatal("rejected configuration replaced the old engine")
	}
	// The old service remains usable after rejection; no source probe is run.
	if _, ok := builtin.BuiltinService().(*ipcheck.Engine); !ok {
		t.Fatal("rejected configuration removed the existing service")
	}
}
