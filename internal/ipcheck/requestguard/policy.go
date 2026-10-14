// Package requestguard provides an in-memory HTTP-attempt admission controller.
// It is not connected to any production request path or configuration. Its
// budget counts admitted attempts, not sent requests, dials or provider billing.
package requestguard

import (
	"errors"
	"fmt"
	"time"
)

type Source string

const (
	EgressConsistency   Source = "egress-consistency"
	CloudflareChallenge Source = "cloudflare-challenge"
	AIRegion            Source = "ai-region"
)

var (
	ErrUnknownSource = errors.New("unknown request guard source")
	ErrInvalidPolicy = errors.New("invalid request guard policy")
)

// Policy controls one source. The zero value disables enforcement. A nonzero
// policy must specify all limits, even when Enabled is false. Disabling does not
// erase the source's ledger, token balance or existing circuit cooldown.
type Policy struct {
	Enabled                     bool
	QPS                         int
	Burst                       int
	DailyLimit                  uint64
	ConsecutiveFailureThreshold int
	OpenFor                     time.Duration
}

func (p Policy) Validate() error {
	if p == (Policy{}) {
		return nil
	}
	if p.QPS < 1 || p.QPS > 100 {
		return fmt.Errorf("%w: QPS must be between 1 and 100", ErrInvalidPolicy)
	}
	if p.Burst < 1 || p.Burst > 64 {
		return fmt.Errorf("%w: Burst must be between 1 and 64", ErrInvalidPolicy)
	}
	if p.DailyLimit < 1 || p.DailyLimit > 1_000_000 {
		return fmt.Errorf("%w: DailyLimit must be between 1 and 1000000", ErrInvalidPolicy)
	}
	if p.ConsecutiveFailureThreshold < 1 || p.ConsecutiveFailureThreshold > 100 {
		return fmt.Errorf("%w: ConsecutiveFailureThreshold must be between 1 and 100", ErrInvalidPolicy)
	}
	if p.OpenFor < time.Second || p.OpenFor > time.Hour {
		return fmt.Errorf("%w: OpenFor must be between 1s and 1h", ErrInvalidPolicy)
	}
	return nil
}

func sourceIndex(source Source) (int, error) {
	switch source {
	case EgressConsistency:
		return 0, nil
	case CloudflareChallenge:
		return 1, nil
	case AIRegion:
		return 2, nil
	default:
		return 0, ErrUnknownSource
	}
}

type Reason string

const (
	RateLimited  Reason = "source_rate"
	DailyLimited Reason = "source_daily_budget"
	CircuitOpen  Reason = "source_circuit_open"
	HalfOpenBusy Reason = "source_half_open_busy"
)

// Rejection describes local suppression, not a remote HTTP failure. RetryAt is
// the earliest suggested retry, not guaranteed admission. It is zero when an
// unfinished half-open attempt prevents predicting when admission may resume.
type Rejection struct {
	Reason  Reason
	RetryAt time.Time
}

func (r *Rejection) Error() string { return string(r.Reason) }

// Outcome describes only circuit feedback. Every admitted attempt consumes its
// budget permanently, including Neutral. HTTP/error classification belongs to a
// future adapter, not this controller. Unknown values are treated as Neutral.
type Outcome uint8

const (
	Neutral Outcome = iota
	Success
	Failure
)
