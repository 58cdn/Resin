package requestguard

import (
	"context"
	"sync"
	"time"
)

const tokenUnits uint64 = 1_000_000_000

type circuitState uint8

const (
	closed circuitState = iota
	open
	halfOpen
)

type sourceState struct {
	policy       Policy
	policyEpoch  uint64
	breakerEpoch uint64
	timeSet      bool
	lastObserved time.Time

	daySet   bool
	dayStart time.Time
	dayUsed  uint64

	// One token is tokenUnits integers, avoiding float rounding at boundaries.
	// The bucket is filled only on the first enabled policy. While disabled it
	// retains the last nonzero rate/capacity and accounts for allowed attempts.
	bucketInitialized bool
	rate              int
	capacity          uint64
	tokens            uint64
	refilledAt        time.Time

	circuit   circuitState
	failures  int
	openUntil time.Time
	// A probe remains occupied across policy changes until its own Finish.
	// Circuit epochs alone cannot protect against a still-running old probe.
	probe *Permit
}

// Controller owns exactly three source slots for its lifetime. Share this
// object across engine/config generations; constructing another one represents
// a new process ledger, with no persistence or coordination between processes.
// All methods are concurrency-safe. No method performs I/O or starts workers.
type Controller struct {
	mu      sync.Mutex
	now     func() time.Time
	sources [3]sourceState
}

// New creates a default-disabled controller. now must be concurrency-safe and
// nonblocking; nil uses time.Now. It is always called outside the state mutex.
func New(now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	return &Controller{now: now}
}

// Apply validates before changing anything, then atomically commits one source
// policy. Callers integrating settings persistence must validate the whole
// configuration first and Apply only after it is committed. Equal policies are
// a no-op. Elapsed time may refill tokens normally; changing policy never grants
// a fresh burst or resets daily usage. Existing open deadlines are retained.
func (c *Controller) Apply(source Source, policy Policy) error {
	index, err := sourceIndex(source)
	if err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.sources[index]
	now = s.observeTime(now)
	if s.policy == policy {
		return nil
	}
	s.refill(now)
	if !s.bucketInitialized && policy.Enabled {
		s.bucketInitialized = true
		s.tokens = uint64(policy.Burst) * tokenUnits
		s.refilledAt = now
	}
	if s.bucketInitialized && policy.QPS != 0 {
		s.rate = policy.QPS
		s.capacity = uint64(policy.Burst) * tokenUnits
		if s.tokens > s.capacity {
			s.tokens = s.capacity
		}
	}
	s.policy = policy
	s.policyEpoch++
	if s.circuit == halfOpen {
		// Keep the actual probe token occupied. Its old outcome cannot mutate
		// this policy, but its eventual Finish must release its own token.
		s.circuit = open
		s.breakerEpoch++
	}
	if policy.Enabled && s.circuit == closed && s.failures >= policy.ConsecutiveFailureThreshold {
		s.trip(now)
	}
	return nil
}

// Permit represents one already-charged admission. Finish must be called even
// when the caller never sends a request or panics (normally using defer). There
// is deliberately no refund or automatic expiry. Concurrent/repeated Finish
// calls apply only the first outcome.
type Permit struct {
	once         sync.Once
	controller   *Controller
	index        int
	policyEpoch  uint64
	breakerEpoch uint64
	feedback     bool
	isProbe      bool
}

// Admit never waits for capacity or schedules a retry. Canceled contexts consume
// nothing if observed before the admission decision. Cancellation after that
// decision does not undo the admission. Even disabled policies keep their UTC
// ledger and, after the first enable, debit their retained token bucket.
func (c *Controller) Admit(ctx context.Context, source Source) (*Permit, error) {
	index, err := sourceIndex(source)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &c.sources[index]
	now = s.observeTime(now)
	s.advanceDay(now)
	s.refill(now)
	if s.policy.Enabled {
		if s.dayUsed >= s.policy.DailyLimit {
			return nil, &Rejection{Reason: DailyLimited, RetryAt: s.dayStart.AddDate(0, 0, 1)}
		}
		if s.probe != nil {
			return nil, &Rejection{Reason: HalfOpenBusy}
		}
		if s.circuit == open && now.Before(s.openUntil) {
			return nil, &Rejection{Reason: CircuitOpen, RetryAt: s.openUntil}
		}
		if s.tokens < tokenUnits {
			missing := tokenUnits - s.tokens
			nanos := (missing + uint64(s.rate) - 1) / uint64(s.rate)
			return nil, &Rejection{Reason: RateLimited, RetryAt: s.refilledAt.Add(time.Duration(nanos))}
		}
	}
	if s.dayUsed != ^uint64(0) {
		s.dayUsed++
	}
	if s.tokens >= tokenUnits {
		s.tokens -= tokenUnits
	} else {
		s.tokens = 0
	}
	p := &Permit{controller: c, index: index, policyEpoch: s.policyEpoch, feedback: s.policy.Enabled}
	if s.policy.Enabled && s.circuit == open {
		s.circuit = halfOpen
		s.breakerEpoch++
		s.probe = p
		p.isProbe = true
	}
	p.breakerEpoch = s.breakerEpoch
	return p, nil
}

func (p *Permit) Finish(outcome Outcome) {
	p.once.Do(func() {
		c := p.controller
		now := c.now()
		c.mu.Lock()
		defer c.mu.Unlock()
		s := &c.sources[p.index]
		now = s.observeTime(now)
		if s.probe == p {
			s.probe = nil
		}
		if !p.feedback || !s.policy.Enabled || p.policyEpoch != s.policyEpoch || p.breakerEpoch != s.breakerEpoch {
			return
		}
		if p.isProbe && s.circuit == halfOpen {
			switch outcome {
			case Success:
				s.circuit, s.failures, s.openUntil = closed, 0, time.Time{}
				s.breakerEpoch++
			case Failure:
				s.trip(now)
			default:
				// Neutral releases occupancy but does not heal, count a failure,
				// refund admission or extend the previous cooldown.
				s.circuit = open
				s.breakerEpoch++
			}
			return
		}
		if s.circuit != closed {
			return
		}
		switch outcome {
		case Success:
			s.failures = 0
		case Failure:
			s.failures++
			if s.failures >= s.policy.ConsecutiveFailureThreshold {
				s.trip(now)
			}
		}
	})
}

func (s *sourceState) trip(now time.Time) {
	s.circuit = open
	s.openUntil = now.Add(s.policy.OpenFor)
	s.breakerEpoch++
}

// A sampled clock can be stale by the time an operation gets the lock. Clamp
// every source event, not just refill/day rollover, so a delayed completion or
// clock rollback cannot shorten a new circuit cooldown.
func (s *sourceState) observeTime(now time.Time) time.Time {
	if !s.timeSet || now.After(s.lastObserved) {
		s.timeSet, s.lastObserved = true, now
	}
	return s.lastObserved
}

func (s *sourceState) advanceDay(now time.Time) {
	utc := now.UTC()
	day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	if !s.daySet || day.After(s.dayStart) {
		s.daySet, s.dayStart, s.dayUsed = true, day, 0
	}
}

func (s *sourceState) refill(now time.Time) {
	if !s.bucketInitialized || !now.After(s.refilledAt) {
		return
	}
	missing := s.capacity - s.tokens
	// Compare before multiplying: even a very large forward clock jump cannot
	// overflow. refilledAt never moves backwards, avoiding duplicate refill.
	toFull := (missing + uint64(s.rate) - 1) / uint64(s.rate)
	elapsed := now.Sub(s.refilledAt)
	if uint64(elapsed) >= toFull {
		s.tokens = s.capacity
	} else {
		s.tokens += uint64(elapsed) * uint64(s.rate)
	}
	s.refilledAt = now
}
