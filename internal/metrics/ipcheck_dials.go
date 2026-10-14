package metrics

import (
	"context"
	"errors"
	"sync"
	"time"
)

// IPCheckDialsSnapshot contains process-local actual node dial counts since this
// Manager was created. A successful dial does not imply a successful IP check,
// HTTP response, or healthy node. No node, IP, source, or error text is retained.
type IPCheckDialsSnapshot struct {
	Scope          string    `json:"scope"`
	StartedAt      time.Time `json:"started_at"`
	ObservedAt     time.Time `json:"observed_at"`
	DialsStarted   uint64    `json:"dials_started"`
	DialsCompleted uint64    `json:"dials_completed"`
	DialsSucceeded uint64    `json:"dials_succeeded"`
	DialsFailed    uint64    `json:"dials_failed"`
	DialsCanceled  uint64    `json:"dials_canceled"`
	DialsInFlight  uint64    `json:"dials_in_flight"`
}

// This tracker is deliberately separate from Collector, bucket/history data,
// connection gauges and sampling loops. One lock keeps every snapshot coherent.
type ipCheckDialTracker struct {
	mu        sync.Mutex
	startedAt time.Time
	started   uint64
	completed uint64
	succeeded uint64
	failed    uint64
	canceled  uint64
	inFlight  uint64
}

// BeginIPCheckDial records admission to an actual node outbound dial. The caller
// must finish it with the dial's returned error. Only the first finish call takes
// effect, including when callers finish concurrently. Cancellation is distinct
// from failure; a deadline remains a failure. This does not start any work.
func (m *Manager) BeginIPCheckDial() func(error) {
	t := &m.ipCheckDials
	t.mu.Lock()
	t.started++
	t.inFlight++
	t.mu.Unlock()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			canceled := errors.Is(err, context.Canceled)
			t.mu.Lock()
			defer t.mu.Unlock()
			t.completed++
			t.inFlight--
			switch {
			case err == nil:
				t.succeeded++
			case canceled:
				t.canceled++
			default:
				t.failed++
			}
		})
	}
}

// SnapshotIPCheckDials returns detached counters without resetting them or
// touching the plugin, topology, network, persistence, or sampling lifecycle.
func (m *Manager) SnapshotIPCheckDials() IPCheckDialsSnapshot {
	t := &m.ipCheckDials
	t.mu.Lock()
	defer t.mu.Unlock()
	return IPCheckDialsSnapshot{
		Scope: "process", StartedAt: t.startedAt, ObservedAt: time.Now().UTC(),
		DialsStarted: t.started, DialsCompleted: t.completed,
		DialsSucceeded: t.succeeded, DialsFailed: t.failed, DialsCanceled: t.canceled,
		DialsInFlight: t.inFlight,
	}
}
