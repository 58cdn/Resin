package requestguard_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
)

type contractClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *contractClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.value }
func (c *contractClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(d)
}

func contractPolicy() requestguard.Policy {
	return requestguard.Policy{Enabled: true, QPS: 100, Burst: 64, DailyLimit: 100,
		ConsecutiveFailureThreshold: 1, OpenFor: time.Second}
}

func contractPermit(t *testing.T, c *requestguard.Controller, source requestguard.Source) *requestguard.Permit {
	t.Helper()
	p, err := c.Admit(context.Background(), source)
	if err != nil {
		t.Fatalf("expected admission: %v", err)
	}
	return p
}

func contractReject(t *testing.T, c *requestguard.Controller, source requestguard.Source, reason requestguard.Reason) {
	t.Helper()
	p, err := c.Admit(context.Background(), source)
	if p != nil {
		p.Finish(requestguard.Neutral)
		t.Fatal("rejected attempt returned a permit")
	}
	var rejection *requestguard.Rejection
	if !errors.As(err, &rejection) || rejection.Reason != reason {
		t.Fatalf("rejection = %v, want %s", err, reason)
	}
}

func TestContractOutstandingProbeSurvivesPolicyReplacement(t *testing.T) {
	clock := &contractClock{value: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	c := requestguard.New(clock.now)
	policy := contractPolicy()
	if err := c.Apply(requestguard.EgressConsistency, policy); err != nil {
		t.Fatal(err)
	}
	contractPermit(t, c, requestguard.EgressConsistency).Finish(requestguard.Failure)
	clock.advance(2 * time.Second)
	oldProbe := contractPermit(t, c, requestguard.EgressConsistency)
	policy.QPS = 50
	policy.OpenFor = 2 * time.Second
	if err := c.Apply(requestguard.EgressConsistency, policy); err != nil {
		t.Fatal(err)
	}
	clock.advance(3 * time.Second)
	contractReject(t, c, requestguard.EgressConsistency, requestguard.HalfOpenBusy)
	oldProbe.Finish(requestguard.Success)
	// Old success releases its occupancy but must not close the new generation.
	currentProbe := contractPermit(t, c, requestguard.EgressConsistency)
	contractReject(t, c, requestguard.EgressConsistency, requestguard.HalfOpenBusy)
	oldProbe.Finish(requestguard.Failure) // repeated finish cannot affect it
	currentProbe.Finish(requestguard.Failure)
	contractReject(t, c, requestguard.EgressConsistency, requestguard.CircuitOpen)
	clock.advance(2 * time.Second)
	contractPermit(t, c, requestguard.EgressConsistency).Finish(requestguard.Success)
	contractPermit(t, c, requestguard.EgressConsistency).Finish(requestguard.Neutral)
}

func TestContractDisabledCallsAndClockRollbackCannotEraseDailyConsumption(t *testing.T) {
	clock := &contractClock{value: time.Date(2026, 10, 14, 23, 59, 55, 0, time.UTC)}
	c := requestguard.New(clock.now)
	policy := contractPolicy()
	policy.DailyLimit = 2
	if err := c.Apply(requestguard.AIRegion, policy); err != nil {
		t.Fatal(err)
	}
	p := contractPermit(t, c, requestguard.AIRegion)
	p.Finish(requestguard.Neutral)
	if err := c.Apply(requestguard.AIRegion, requestguard.Policy{}); err != nil {
		t.Fatal(err)
	}
	contractPermit(t, c, requestguard.AIRegion).Finish(requestguard.Success)
	if err := c.Apply(requestguard.AIRegion, policy); err != nil {
		t.Fatal(err)
	}
	contractReject(t, c, requestguard.AIRegion, requestguard.DailyLimited)
	clock.advance(10 * time.Second)
	contractPermit(t, c, requestguard.AIRegion).Finish(requestguard.Neutral)
	clock.advance(-24 * time.Hour)
	contractPermit(t, c, requestguard.AIRegion).Finish(requestguard.Neutral)
	contractReject(t, c, requestguard.AIRegion, requestguard.DailyLimited)
	clock.advance(24 * time.Hour)
	contractReject(t, c, requestguard.AIRegion, requestguard.DailyLimited)
}

func TestContractRejectedApplyAndCancellationPreserveAdmissionRules(t *testing.T) {
	clock := &contractClock{value: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	c := requestguard.New(clock.now)
	policy := contractPolicy()
	policy.DailyLimit = 1
	if err := c.Apply(requestguard.CloudflareChallenge, policy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, err := c.Admit(ctx, requestguard.CloudflareChallenge); p != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v %v", p, err)
	}
	contractPermit(t, c, requestguard.CloudflareChallenge).Finish(requestguard.Neutral)
	invalid := policy
	invalid.Enabled = false
	invalid.QPS = 101
	if err := c.Apply(requestguard.CloudflareChallenge, invalid); err == nil {
		t.Fatal("invalid policy accepted")
	}
	contractReject(t, c, requestguard.CloudflareChallenge, requestguard.DailyLimited)
	// A different fixed source retains its independent budget and default-off policy.
	contractPermit(t, c, requestguard.EgressConsistency).Finish(requestguard.Neutral)
	if p, err := c.Admit(context.Background(), requestguard.Source("https://secret.example/token")); p != nil || err == nil {
		t.Fatal("unbounded source identity accepted")
	}
}

func TestContractClockRollbackCannotShortenNewProbeFailureCooldown(t *testing.T) {
	clock := &contractClock{value: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	c := requestguard.New(clock.now)
	if err := c.Apply(requestguard.AIRegion, contractPolicy()); err != nil {
		t.Fatal(err)
	}
	contractPermit(t, c, requestguard.AIRegion).Finish(requestguard.Failure)
	clock.advance(2 * time.Second)
	probe := contractPermit(t, c, requestguard.AIRegion)
	clock.advance(-time.Hour)
	probe.Finish(requestguard.Failure)
	clock.advance(2 * time.Second)
	// Time is still behind the already-observed probe admission. A rollback
	// must not turn its failed completion into an already-expired cooldown.
	contractReject(t, c, requestguard.AIRegion, requestguard.CircuitOpen)
	clock.advance(time.Hour)
	contractPermit(t, c, requestguard.AIRegion).Finish(requestguard.Success)
}
