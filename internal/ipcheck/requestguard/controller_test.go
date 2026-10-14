package requestguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type manualClock struct {
	mu       sync.Mutex
	value    time.Time
	block    bool
	captured chan struct{}
	release  chan struct{}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	now := c.value
	block, captured, release := c.block, c.captured, c.release
	c.block = false
	c.mu.Unlock()
	if block {
		close(captured)
		<-release
	}
	return now
}

func (c *manualClock) Set(now time.Time) {
	c.mu.Lock()
	c.value = now
	c.mu.Unlock()
}

func (c *manualClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.value = c.value.Add(duration)
	c.mu.Unlock()
}

func (c *manualClock) BlockNext(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	c.mu.Lock()
	c.block, c.captured, c.release = true, make(chan struct{}), make(chan struct{})
	captured, release := c.captured, c.release
	c.mu.Unlock()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return captured, unblock
}

func testController() (*Controller, *manualClock, time.Time) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	clock := &manualClock{value: now}
	return New(clock.Now), clock, now
}

func apply(t *testing.T, c *Controller, p Policy) {
	t.Helper()
	if err := c.Apply(EgressConsistency, p); err != nil {
		t.Fatal(err)
	}
}

func admit(t *testing.T, c *Controller) *Permit {
	t.Helper()
	p, err := c.Admit(context.Background(), EgressConsistency)
	if err != nil || p == nil {
		t.Fatalf("admission = %v, %v", p, err)
	}
	return p
}

func rejection(t *testing.T, c *Controller, reason Reason) *Rejection {
	t.Helper()
	p, err := c.Admit(context.Background(), EgressConsistency)
	if p != nil {
		p.Finish(Neutral)
		t.Fatalf("expected %s, received permit", reason)
	}
	var rejected *Rejection
	if !errors.As(err, &rejected) || rejected.Reason != reason {
		t.Fatalf("expected %s, got %v", reason, err)
	}
	return rejected
}

func TestTokenBucketExactBoundariesRollbackAndLongIdle(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.QPS, p.Burst = 3, 2
	apply(t, c, p)
	admit(t, c).Finish(Neutral)
	admit(t, c).Finish(Neutral)
	first := start.Add(333_333_334 * time.Nanosecond)
	if got := rejection(t, c, RateLimited).RetryAt; !got.Equal(first) {
		t.Fatalf("retry = %v, want %v", got, first)
	}
	clock.Set(first.Add(-time.Nanosecond))
	rejection(t, c, RateLimited)
	clock.Set(first)
	admit(t, c).Finish(Neutral)
	second := start.Add(666_666_667 * time.Nanosecond)
	clock.Set(start.Add(-time.Hour))
	if got := rejection(t, c, RateLimited).RetryAt; !got.Equal(second) {
		t.Fatalf("rollback changed refill boundary to %v", got)
	}
	clock.Set(second.Add(-time.Nanosecond))
	rejection(t, c, RateLimited)
	clock.Set(second)
	admit(t, c).Finish(Neutral)
	// A huge jump must cap before multiplication; it cannot overflow refill or
	// accumulate more than Burst. The UTC ledger legitimately enters a new day.
	clock.Set(start.AddDate(2000, 0, 0))
	admit(t, c).Finish(Neutral)
	admit(t, c).Finish(Neutral)
	rejection(t, c, RateLimited)
}

func TestApplyDoesNotRefillOrReinitializeSpentBurst(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.QPS, p.Burst = 1, 2
	apply(t, c, p)
	admit(t, c).Finish(Neutral)
	admit(t, c).Finish(Neutral)
	apply(t, c, p)
	rejection(t, c, RateLimited)
	apply(t, c, Policy{})
	admit(t, c).Finish(Neutral) // Enforcement off; ledger and bucket still debit.
	apply(t, c, p)
	rejection(t, c, RateLimited)
	p.QPS, p.Burst = 2, 4
	apply(t, c, p) // Increasing capacity does not grant tokens.
	rejection(t, c, RateLimited)
	clock.Set(start.Add(500*time.Millisecond - 1))
	rejection(t, c, RateLimited)
	clock.Set(start.Add(500 * time.Millisecond))
	admit(t, c).Finish(Neutral)
	clock.Set(start.Add(10 * time.Second))
	p.QPS, p.Burst = 1, 1
	apply(t, c, p) // Refill at the old rate, then clamp to the new capacity.
	admit(t, c).Finish(Neutral)
	rejection(t, c, RateLimited)
}

func TestCancellationBeforeAdmissionAndAfterCharge(t *testing.T) {
	c, clock, _ := testController()
	p := validPolicy()
	p.DailyLimit = 1
	apply(t, c, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if permit, err := c.Admit(ctx, EgressConsistency); permit != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled admission: %v, %v", permit, err)
	}
	// Cancellation after the first check but while sampling the clock is also
	// observed before any ledger/circuit state is touched under the mutex.
	ctx, cancel = context.WithCancel(context.Background())
	captured, release := clock.BlockNext(t)
	done := make(chan error, 1)
	go func() { _, err := c.Admit(ctx, EgressConsistency); done <- err }()
	<-captured
	cancel()
	release()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation before commit: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	permit, err := c.Admit(ctx, EgressConsistency)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	permit.Finish(Neutral)
	permit.Finish(Success)
	rejection(t, c, DailyLimited)
}

func TestUTCAdmissionDayCompletionAndSaturation(t *testing.T) {
	c, clock, _ := testController()
	// A non-UTC presentation must still use the UTC midnight boundary.
	start := time.Date(2026, 10, 15, 7, 59, 59, 0, time.FixedZone("UTC+8", 8*3600))
	clock.Set(start)
	p := validPolicy()
	p.DailyLimit, p.Burst = 1, 2
	apply(t, c, p)
	previousDay := admit(t, c)
	boundary := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if got := rejection(t, c, DailyLimited).RetryAt; !got.Equal(boundary) {
		t.Fatalf("UTC reset = %v", got)
	}
	clock.Set(boundary)
	admit(t, c).Finish(Neutral)
	previousDay.Finish(Neutral) // Completion must not debit/refund the new day.
	rejection(t, c, DailyLimited)
	clock.Set(boundary.Add(-2 * time.Hour))
	if got := rejection(t, c, DailyLimited).RetryAt; !got.Equal(boundary.AddDate(0, 0, 1)) {
		t.Fatalf("clock rollback reset quota: %v", got)
	}
	// Saturation is unreachable in normal bounded operation, but disabled
	// admission must not wrap a long-lived ledger back to zero.
	apply(t, c, Policy{})
	c.mu.Lock()
	c.sources[0].dayUsed = ^uint64(0)
	c.mu.Unlock()
	admit(t, c).Finish(Neutral)
	apply(t, c, p)
	rejection(t, c, DailyLimited)
}

func TestCircuitFeedbackDeduplicationAndStaleCompletions(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.QPS, p.Burst, p.ConsecutiveFailureThreshold = 100, 64, 2
	p.OpenFor = time.Second
	apply(t, c, p)
	first, neutral, second, oldSuccess, oldFailure := admit(t, c), admit(t, c), admit(t, c), admit(t, c), admit(t, c)
	first.Finish(Failure)
	first.Finish(Success)        // Duplicate must neither heal nor count again.
	neutral.Finish(Outcome(255)) // Unknown outcomes are neutral, not success.
	second.Finish(Failure)
	clock.Set(start.Add(500 * time.Millisecond))
	oldSuccess.Finish(Success)
	oldFailure.Finish(Failure)
	if got := rejection(t, c, CircuitOpen).RetryAt; !got.Equal(start.Add(time.Second)) {
		t.Fatalf("old completion changed cooldown: %v", got)
	}
	clock.Set(start.Add(time.Second))
	probe := admit(t, c)
	if got := rejection(t, c, HalfOpenBusy).RetryAt; !got.IsZero() {
		t.Fatalf("unfinished probe has invented retry time: %v", got)
	}
	probe.Finish(Neutral)
	// Neutral releases occupancy without inventing another cooldown or healing.
	nextProbe := admit(t, c)
	rejection(t, c, HalfOpenBusy)
	nextProbe.Finish(Success)
	admit(t, c).Finish(Neutral)
}

func TestConcurrentAdmissionAndUniqueHalfOpen(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.QPS, p.Burst, p.ConsecutiveFailureThreshold = 100, 64, 1
	p.OpenFor = time.Second
	apply(t, c, p)
	admit(t, c).Finish(Failure)
	clock.Set(start.Add(time.Second))
	const callers = 32
	gate := make(chan struct{})
	results := make(chan struct {
		permit *Permit
		err    error
	}, callers)
	for range callers {
		go func() {
			<-gate
			permit, err := c.Admit(context.Background(), EgressConsistency)
			results <- struct {
				permit *Permit
				err    error
			}{permit, err}
		}()
	}
	close(gate)
	var probe *Permit
	for range callers {
		result := <-results
		if result.err == nil {
			if result.permit == nil || probe != nil {
				t.Fatal("more than one concurrent half-open permit")
			}
			probe = result.permit
		} else {
			var rejected *Rejection
			if !errors.As(result.err, &rejected) || rejected.Reason != HalfOpenBusy {
				t.Fatalf("unexpected concurrent rejection: %v", result.err)
			}
		}
	}
	if probe == nil {
		t.Fatal("no half-open permit")
	}
	var finishers sync.WaitGroup
	for range callers {
		finishers.Add(1)
		go func() { defer finishers.Done(); probe.Finish(Failure) }()
	}
	finishers.Wait()
	if got := rejection(t, c, CircuitOpen).RetryAt; !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("duplicate Finish changed cooldown: %v", got)
	}
}

func TestConcurrentBurstAndDailyLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		burst  int
		daily  uint64
		want   int
		reason Reason
	}{
		{"burst", 7, 1000, 7, RateLimited},
		{"daily", 64, 17, 17, DailyLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := testController()
			p := validPolicy()
			p.QPS, p.Burst, p.DailyLimit = 100, tc.burst, tc.daily
			apply(t, c, p)
			const callers = 64
			gate := make(chan struct{})
			results := make(chan error, callers)
			for range callers {
				go func() {
					<-gate
					permit, err := c.Admit(context.Background(), EgressConsistency)
					if permit != nil {
						permit.Finish(Neutral)
					}
					results <- err
				}()
			}
			close(gate)
			allowed := 0
			for range callers {
				err := <-results
				if err == nil {
					allowed++
					continue
				}
				var rejected *Rejection
				if !errors.As(err, &rejected) || rejected.Reason != tc.reason {
					t.Fatalf("unexpected rejection: %v", err)
				}
			}
			if allowed != tc.want {
				t.Fatalf("concurrent admissions = %d, want %d", allowed, tc.want)
			}
		})
	}
}

func TestRejectedGateDoesNotSpendOtherBudgetsOrClaimProbe(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.QPS, p.Burst, p.DailyLimit, p.ConsecutiveFailureThreshold = 1, 1, 2, 1
	p.OpenFor = time.Second
	apply(t, c, p)
	admit(t, c).Finish(Neutral)
	for range 10 {
		rejection(t, c, RateLimited)
	}
	clock.Set(start.Add(time.Second))
	admit(t, c).Finish(Failure) // Rate rejections did not consume the second unit.
	clock.Set(start.Add(2 * time.Second))
	rejection(t, c, DailyLimited) // Expired OPEN must not claim a hidden probe.
	p.DailyLimit = 3
	apply(t, c, p)
	probe := admit(t, c)
	probe.Finish(Success)
	rejection(t, c, DailyLimited)
}

func TestDelayedClockSampleCannotShortenNewCooldown(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.Burst, p.ConsecutiveFailureThreshold, p.OpenFor = 4, 1, 10*time.Second
	apply(t, c, p)
	old := admit(t, c)
	clock.Set(start.Add(time.Second))
	captured, release := clock.BlockNext(t)
	done := make(chan struct{})
	go func() { old.Finish(Failure); close(done) }()
	<-captured // Finish sampled t+1 but has not entered the state mutex yet.
	clock.Set(start.Add(30 * time.Second))
	admit(t, c).Finish(Neutral) // A later event is committed first.
	release()
	<-done
	if got := rejection(t, c, CircuitOpen).RetryAt; !got.Equal(start.Add(40 * time.Second)) {
		t.Fatalf("stale sample shortened full cooldown: %v", got)
	}
}

func TestHalfOpenFailureClockRollbackKeepsFullCooldown(t *testing.T) {
	c, clock, start := testController()
	p := validPolicy()
	p.ConsecutiveFailureThreshold, p.OpenFor = 1, 10*time.Second
	apply(t, c, p)
	admit(t, c).Finish(Failure)
	clock.Set(start.Add(10 * time.Second))
	probe := admit(t, c)
	clock.Set(start.Add(time.Second))
	probe.Finish(Failure)
	if got := rejection(t, c, CircuitOpen).RetryAt; !got.Equal(start.Add(20 * time.Second)) {
		t.Fatalf("rollback shortened half-open failure cooldown: %v", got)
	}
}

func TestDefaultDisabledLedgerSurvivesFirstEnableWithoutFailureFeedback(t *testing.T) {
	c, _, _ := testController()
	old := admit(t, c)
	admit(t, c).Finish(Failure) // Enforcement was never enabled.
	p := validPolicy()
	p.DailyLimit, p.ConsecutiveFailureThreshold = 3, 1
	apply(t, c, p)
	admit(t, c).Finish(Neutral) // Only one daily unit remains, despite first enable.
	rejection(t, c, DailyLimited)
	old.Finish(Failure) // A default-off admission cannot trip the new policy.
	p.DailyLimit = 4
	apply(t, c, p)
	admit(t, c).Finish(Neutral)
	rejection(t, c, DailyLimited)
}

func TestConcurrentApplyAdmitAndFinish(t *testing.T) {
	c, clock, _ := testController()
	sources := []Source{EgressConsistency, CloudflareChallenge, AIRegion}
	const workers = 9
	gate := make(chan struct{})
	errorsFound := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			for iteration := range 32 {
				clock.Advance(100 * time.Millisecond)
				source := sources[(worker+iteration)%len(sources)]
				if worker%3 == 0 {
					p := validPolicy()
					p.Enabled = iteration%3 != 0
					p.QPS, p.Burst, p.OpenFor = 25+iteration, 2+iteration%3, time.Second
					if err := c.Apply(source, p); err != nil {
						errorsFound <- err
						return
					}
					continue
				}
				permit, err := c.Admit(context.Background(), source)
				if err != nil {
					var rejected *Rejection
					if !errors.As(err, &rejected) {
						errorsFound <- err
						return
					}
					continue
				}
				permit.Finish(Outcome(iteration % 3))
				permit.Finish(Failure)
			}
		}()
	}
	close(gate)
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("mixed concurrent lifecycle: %v", err)
	}
}
