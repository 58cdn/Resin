package ipcheck

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
)

// SuppressedSource describes local admission decisions, never observations.
// Any suppressed target discards this source's whole new result. Attempts may
// still have occurred; its previous cache is left untouched.
type SuppressedSource struct {
	Source     string              `json:"source"`
	Kind       SourceKind          `json:"kind"`
	Admitted   int                 `json:"admitted"`
	Suppressed int                 `json:"suppressed"`
	Reasons    []SuppressionReason `json:"reasons"`
}

type SuppressionReason struct {
	Reason  requestguard.Reason `json:"reason"`
	Count   int                 `json:"count"`
	RetryAt *time.Time          `json:"retry_at,omitempty"`
}

type attemptReport struct {
	mu         sync.Mutex
	admitted   int
	suppressed int
	reasons    map[requestguard.Reason]SuppressionReason
}

func (r *attemptReport) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.admitted++
		return
	}
	var rejection *requestguard.Rejection
	if !errors.As(err, &rejection) {
		return
	}
	if r.reasons == nil {
		r.reasons = make(map[requestguard.Reason]SuppressionReason)
	}
	r.suppressed++
	item := r.reasons[rejection.Reason]
	item.Reason = rejection.Reason
	item.Count++
	if !rejection.RetryAt.IsZero() && (item.RetryAt == nil || rejection.RetryAt.After(*item.RetryAt)) {
		item.RetryAt = timePointer(rejection.RetryAt.UTC())
	}
	r.reasons[rejection.Reason] = item
}

func (r *attemptReport) snapshot(source string) *SuppressedSource {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suppressed == 0 {
		return nil
	}
	result := &SuppressedSource{Source: source, Kind: SourceKindViaNode, Admitted: r.admitted, Suppressed: r.suppressed}
	for _, item := range r.reasons {
		item.RetryAt = copyTime(item.RetryAt)
		result.Reasons = append(result.Reasons, item)
	}
	sort.Slice(result.Reasons, func(i, j int) bool { return result.Reasons[i].Reason < result.Reasons[j].Reason })
	return result
}

func cloneSuppressed(source SuppressedSource) SuppressedSource {
	source.Reasons = append([]SuppressionReason(nil), source.Reasons...)
	for i := range source.Reasons {
		source.Reasons[i].RetryAt = copyTime(source.Reasons[i].RetryAt)
	}
	return source
}

// This internal outcome is consumed by check rather than exposed as a Go/API
// error. It lets singleflight share the same decision without charging waiters.
type suppressedExecution struct{ source SuppressedSource }

func (*suppressedExecution) Error() string { return "source locally suppressed" }

// ActivateRequestGuards commits policies only after the builtin settings save.
// The manager has quiesced the old engine and serializes access during this
// infallible activation. Construction/configuration never calls Apply.
func (e *Engine) ActivateRequestGuards() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if e.guards == nil {
		return
	}
	for _, source := range []requestguard.Source{requestguard.EgressConsistency, requestguard.CloudflareChallenge, requestguard.AIRegion} {
		policy := requestguard.Policy{}
		for _, configured := range e.config.SourceGuards {
			if configured.Source == string(source) {
				policy, _ = configured.Policy() // Validated before Engine construction.
				break
			}
		}
		_ = e.guards.Apply(source, policy)
	}
}

// Feedback ends at response headers. Body reads/parsing remain the source's
// responsibility and cannot change an already completed permit. Valid risk or
// challenge observations are independent of this opt-in protective circuit.
func guardOutcome(ctx context.Context, response *http.Response, err error) requestguard.Outcome {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return requestguard.Neutral
	}
	if err != nil {
		var hostError *HostError
		if errors.As(err, &hostError) && hostError != nil && hostError.Kind == HostTransportFailure {
			return requestguard.Failure
		}
		return requestguard.Neutral
	}
	if response == nil || response.StatusCode < 100 || response.StatusCode > 599 {
		return requestguard.Neutral
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return requestguard.Failure
	}
	return requestguard.Success
}
