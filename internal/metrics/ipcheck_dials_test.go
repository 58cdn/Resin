package metrics

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

func assertIPCheckDialAccounting(t *testing.T, snapshot IPCheckDialsSnapshot) {
	t.Helper()
	if snapshot.DialsStarted != snapshot.DialsCompleted+snapshot.DialsInFlight ||
		snapshot.DialsCompleted != snapshot.DialsSucceeded+snapshot.DialsFailed+snapshot.DialsCanceled {
		t.Errorf("incoherent dial snapshot: %+v", snapshot)
	}
}

func TestIPCheckDialsInitialAndClassification(t *testing.T) {
	mgr := NewManager(ManagerConfig{}) // No database or background samplers needed.
	initial := mgr.SnapshotIPCheckDials()
	if initial.Scope != "process" || initial.StartedAt.IsZero() || initial.ObservedAt.IsZero() ||
		initial.DialsStarted != 0 || initial.DialsCompleted != 0 || initial.DialsInFlight != 0 {
		t.Fatalf("initial snapshot: %+v", initial)
	}
	assertIPCheckDialAccounting(t, initial)
	cases := []struct {
		name string
		err  error
		kind string
	}{
		{"success", nil, "success"},
		{"canceled", context.Canceled, "canceled"},
		{"wrapped cancellation", fmt.Errorf("dial: %w", context.Canceled), "canceled"},
		{"deadline", context.DeadlineExceeded, "failed"},
		{"wrapped deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), "failed"},
		{"other failure", errors.New("canceled"), "failed"},
	}
	var succeeded, failed, canceled uint64
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finish := mgr.BeginIPCheckDial()
			pending := mgr.SnapshotIPCheckDials()
			if pending.DialsStarted != uint64(i+1) || pending.DialsInFlight != 1 {
				t.Fatalf("pending dial not visible: %+v", pending)
			}
			assertIPCheckDialAccounting(t, pending)
			finish(tc.err)
			finish(errors.New("later result must not replace first result"))
			switch tc.kind {
			case "success":
				succeeded++
			case "canceled":
				canceled++
			default:
				failed++
			}
			got := mgr.SnapshotIPCheckDials()
			if got.DialsCompleted != uint64(i+1) || got.DialsInFlight != 0 ||
				got.DialsSucceeded != succeeded || got.DialsFailed != failed || got.DialsCanceled != canceled {
				t.Fatalf("classification: %+v", got)
			}
			if got.StartedAt != initial.StartedAt || pending.DialsInFlight != 1 {
				t.Fatal("snapshot or manager start time changed")
			}
			assertIPCheckDialAccounting(t, got)
		})
	}
	if next := NewManager(ManagerConfig{}).SnapshotIPCheckDials(); next.DialsStarted != 0 {
		t.Fatalf("new Manager inherited old counters: %+v", next)
	}
}

func TestIPCheckDialsConcurrentSnapshotsAndDuplicateFinish(t *testing.T) {
	mgr := NewManager(ManagerConfig{})
	const workers, iterations = 24, 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				finish := mgr.BeginIPCheckDial()
				assertIPCheckDialAccounting(t, mgr.SnapshotIPCheckDials())
				switch worker % 3 {
				case 0:
					finish(nil)
				case 1:
					finish(context.Canceled)
				default:
					finish(context.DeadlineExceeded)
				}
				finish(nil)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	close(start)
observing:
	for {
		assertIPCheckDialAccounting(t, mgr.SnapshotIPCheckDials())
		select {
		case <-done:
			break observing
		default:
			runtime.Gosched()
		}
	}
	got := mgr.SnapshotIPCheckDials()
	const total = workers * iterations
	if got.DialsStarted != total || got.DialsCompleted != total || got.DialsInFlight != 0 ||
		got.DialsSucceeded != total/3 || got.DialsFailed != total/3 || got.DialsCanceled != total/3 {
		t.Fatalf("concurrent accounting: %+v", got)
	}

	finish := mgr.BeginIPCheckDial()
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); finish(context.Canceled) }()
	}
	wg.Wait()
	finish(nil)
	got = mgr.SnapshotIPCheckDials()
	if got.DialsStarted != total+1 || got.DialsCompleted != total+1 || got.DialsInFlight != 0 || got.DialsCanceled != total/3+1 {
		t.Fatalf("duplicate completion counted more than once: %+v", got)
	}
	assertIPCheckDialAccounting(t, got)
}

func TestIPCheckDialsDoNotChangeLegacyMetrics(t *testing.T) {
	mgr := NewManager(ManagerConfig{})
	mgr.collector.RecordRequest("fixture-platform", true, 123, false)
	mgr.collector.RecordTraffic(17, 29)
	mgr.collector.RecordConnection(ConnInbound, 2)
	mgr.collector.RecordConnection(ConnOutbound, 3)
	mgr.collector.RecordProbe(ProbeKindEgress)
	mgr.collector.RecordProbe(ProbeKindLatency)
	before := mgr.collector.Snapshot()
	platformsBefore := mgr.collector.PlatformSnapshots()
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		mgr.BeginIPCheckDial()(err)
	}
	for range 3 {
		assertIPCheckDialAccounting(t, mgr.SnapshotIPCheckDials())
	}
	if got := mgr.collector.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("legacy counters changed: before=%+v after=%+v", before, got)
	}
	if got := mgr.collector.PlatformSnapshots(); !reflect.DeepEqual(got, platformsBefore) {
		t.Fatalf("platform counters changed: %+v", got)
	}
	if len(mgr.pendingTasks) != 0 {
		t.Fatal("dial counters queued a history write")
	}
	for _, ring := range []*RealtimeRing{mgr.throughputRing, mgr.connectionsRing, mgr.leasesRing} {
		if _, ok := ring.Latest(); ok {
			t.Fatal("dial counters wrote a realtime sample")
		}
	}
}
