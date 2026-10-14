package ipcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

func submitBatchTest(t *testing.T, e *Engine, id string, hashes []node.Hash, sources []string, force bool) *BatchTask {
	t.Helper()
	task, reused, err := e.SubmitBatch(context.Background(), id, hashes, sources, force)
	if err != nil || reused {
		t.Fatalf("submit reused=%v err=%v", reused, err)
	}
	return task
}

type batchDeadlineSource struct {
	name  string
	calls *atomic.Int32
}

func (s *batchDeadlineSource) Name() string      { return s.name }
func (s *batchDeadlineSource) Kind() SourceKind  { return sourceCatalog[s.name].Kind }
func (s *batchDeadlineSource) RequiresKey() bool { return false }
func (s *batchDeadlineSource) Check(ctx context.Context, _ CheckRequest) (*SourceResult, error) {
	s.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBatchDeadlineStopsQueuedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFixtureHost()
		cfg := DefaultConfig()
		cfg.EnabledSources = []string{"db-ip", "egress-consistency"}
		cfg.RequestTimeoutMS = 60000
		e := newTestEngine(t, h, nil, cfg)
		var calls atomic.Int32
		for _, name := range cfg.EnabledSources {
			e.sources[name] = &batchDeadlineSource{name, &calls}
		}
		hashes := make([]node.Hash, MaxBatchNodes)
		for i := range hashes {
			hashes[i] = testHash(byte(i + 1))
			h.nodes[hashes[i]] = netip.MustParseAddr(fmt.Sprintf("8.8.4.%d", i+1))
		}
		blocker := submitBatchTest(t, e, "deadline-first", hashes, cfg.EnabledSources, true)
		first := submitBatchTest(t, e, "deadline-running", hashes, cfg.EnabledSources, true)
		queued := submitBatchTest(t, e, "deadline-queued", hashes, cfg.EnabledSources, true)
		synctest.Wait()
		before, err := e.GetBatch(context.Background(), blocker.ID)
		if err != nil || before.Progress.Running != batchWorkerCount {
			t.Fatalf("workers not active: %+v %v", before, err)
		}
		// Advance a Go virtual clock through the actual configured deadline;
		// no wall-clock 30-minute wait or replacement production timeout.
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		completed, err := e.GetBatch(context.Background(), blocker.ID)
		if err != nil || completed.State != "completed" || completed.Progress.Completed != MaxBatchNodes {
			t.Fatal("completed task changed at deadline")
		}
		for _, id := range []string{first.ID, queued.ID} {
			done, err := e.GetBatch(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if done.State != "canceled" || done.CancelReason != "deadline" || done.Progress.Canceled == 0 || done.Progress.Running != 0 || done.Progress.Queued != 0 {
				t.Fatalf("deadline did not terminate task: err=%v state=%s reason=%s progress=%+v", err, done.State, done.CancelReason, done.Progress)
			}
			if id == queued.ID && done.Progress.Canceled != MaxBatchNodes {
				t.Fatal("queued task executed before deadline cancellation")
			}
		}
		atDeadline := calls.Load()
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != atDeadline {
			t.Fatal("queued work executed after task deadline")
		}
	})
}

func finishBatchTest(t *testing.T, e *Engine, id string) *BatchTask {
	t.Helper()
	var task *BatchTask
	waitFor(t, func() bool {
		var err error
		task, err = e.GetBatch(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return task.FinishedAt != nil
	})
	return task
}

func TestBatchValidationHasNoExecution(t *testing.T) {
	h := newFixtureHost()
	e := newTestEngine(t, h, nil, DefaultConfig())
	for _, tc := range []struct {
		id      string
		hashes  []node.Hash
		sources []string
	}{
		{"", []node.Hash{testHash(1)}, []string{"db-ip"}},
		{strings.Repeat("x", 65), []node.Hash{testHash(1)}, []string{"db-ip"}},
		{"bad/id", []node.Hash{testHash(1)}, []string{"db-ip"}},
		{"empty", nil, []string{"db-ip"}},
		{"large", make([]node.Hash, 51), []string{"db-ip"}},
		{"default-sources", []node.Hash{testHash(1)}, nil},
		{"unknown", []node.Hash{testHash(1)}, []string{"future"}},
		{"duplicate", []node.Hash{testHash(1)}, []string{"db-ip", "db-ip"}},
	} {
		if _, _, err := e.SubmitBatch(context.Background(), tc.id, tc.hashes, tc.sources, false); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.SubmitBatch(ctx, "canceled", []node.Hash{testHash(1)}, []string{"db-ip"}, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(e.batches.jobs) != 0 || h.geoCalls.Load() != 0 || h.viaCalls.Load() != 0 {
		t.Fatal("invalid/canceled batch executed")
	}
}

func TestBatchCapacityCancellationAndAdmissionLifetime(t *testing.T) {
	h := newFixtureHost()
	hashes := make([]node.Hash, MaxBatchNodes)
	for i := range hashes {
		hashes[i] = testHash(byte(i + 1))
		h.nodes[hashes[i]] = netip.MustParseAddr(fmt.Sprintf("8.8.4.%d", i+1))
	}
	h.geo = func(ctx context.Context, _ netip.Addr) (GeoIPInfo, bool) { <-ctx.Done(); return GeoIPInfo{}, false }
	e := newTestEngine(t, h, nil, DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, _, err := e.SubmitBatch(ctx, "first", hashes, []string{"db-ip"}, true)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{first.ID}
	for i := 1; i < MaxActiveBatchTasks; i++ {
		keys = append(keys, submitBatchTest(t, e, fmt.Sprintf("batch-%d", i), hashes, []string{"db-ip"}, true).ID)
	}
	waitFor(t, func() bool { return h.geoCalls.Load() == batchWorkerCount })
	cancel() // The accepted task must not inherit the HTTP request lifetime.
	current, err := e.GetBatch(context.Background(), first.ID)
	if err != nil || current.Progress.Running != batchWorkerCount || current.CancelReason != "" {
		t.Fatalf("lost accepted task: %+v %v", current, err)
	}
	if _, _, err = e.SubmitBatch(context.Background(), "overflow", hashes, []string{"db-ip"}, false); !errors.Is(err, ErrBatchCapacity) {
		t.Fatal(err)
	}
	if len(e.batches.jobs) != MaxActiveBatchTasks {
		t.Fatal("partial over-capacity admission")
	}
	if _, reused, err := e.SubmitBatch(context.Background(), "first", append([]node.Hash{hashes[0]}, hashes[:49]...), []string{"db-ip"}, true); !errors.Is(err, ErrBatchConflict) || reused {
		t.Fatalf("different set reused: %v %v", reused, err)
	}
	reversed := append([]node.Hash{}, hashes...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if task, reused, err := e.SubmitBatch(context.Background(), "first", reversed, []string{"db-ip"}, true); err != nil || !reused || task.ID != first.ID {
		t.Fatalf("idempotent at capacity: %+v %v %v", task, reused, err)
	}
	// Cancel queued tasks before freeing either running worker.
	for _, id := range keys[1:] {
		if _, err = e.CancelBatch(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = e.CancelBatch(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range keys {
		task := finishBatchTest(t, e, id)
		if task.State != "canceled" || task.Progress.Canceled != MaxBatchNodes || task.Progress.Running != 0 || task.Progress.Queued != 0 {
			t.Fatalf("incomplete cancel: %+v", task)
		}
	}
	if h.geoCalls.Load() != batchWorkerCount {
		t.Fatalf("claimed after cancellation: %d", h.geoCalls.Load())
	}
	if _, err = e.CancelBatch(context.Background(), first.ID); err != nil {
		t.Fatal("cancel is not idempotent", err)
	}
}

func TestBatchCancellationDoesNotStopSharedManualSource(t *testing.T) {
	h := newFixtureHost()
	started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.geo = func(ctx context.Context, _ netip.Addr) (GeoIPInfo, bool) {
		close(started)
		select {
		case <-release:
			return GeoIPInfo{Country: "US"}, true
		case <-ctx.Done():
			close(canceled)
			return GeoIPInfo{}, false
		}
	}
	e := newTestEngine(t, h, nil, DefaultConfig())
	manual := make(chan error, 1)
	go func() {
		_, err := e.Lookup(context.Background(), netip.MustParseAddr("8.8.8.8"), []string{"db-ip"}, false)
		manual <- err
	}()
	<-started
	task := submitBatchTest(t, e, "shared", []node.Hash{testHash(1), testHash(2)}, []string{"db-ip"}, true)
	waitFor(t, func() bool { return waiters(e) == 3 })
	if _, err := e.CancelBatch(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if done := finishBatchTest(t, e, task.ID); done.Progress.Canceled != 2 {
		t.Fatalf("bad canceled batch: %+v", done)
	}
	select {
	case <-canceled:
		t.Fatal("shared source canceled despite manual waiter")
	default:
	}
	close(release)
	if err := <-manual; err != nil {
		t.Fatal(err)
	}
	if h.geoCalls.Load() != 1 {
		t.Fatal("source singleflight bypassed")
	}
	if result, err := e.GetIP(context.Background(), netip.MustParseAddr("8.8.8.8")); err != nil || result.Summary.Country != "US" {
		t.Fatalf("completed cache lost: %+v %v", result, err)
	}
}

func TestBatchCacheForceAndRetainedRequestID(t *testing.T) {
	h := newFixtureHost()
	e := newTestEngine(t, h, nil, DefaultConfig())
	for i, force := range []bool{false, false, true} {
		task := submitBatchTest(t, e, fmt.Sprintf("force-%d", i), []node.Hash{testHash(1), testHash(1)}, []string{"db-ip"}, force)
		done := finishBatchTest(t, e, task.ID)
		if done.Progress.Total != 1 || done.Progress.Completed != 1 || done.Items[0].ObservationStatus != "all_ok" {
			t.Fatalf("bad completion %+v", done)
		}
		expected := int32(1)
		if force {
			expected = 2
		}
		if h.geoCalls.Load() != expected {
			t.Fatalf("force/cache: %d", h.geoCalls.Load())
		}
		if again, reused, err := e.SubmitBatch(context.Background(), fmt.Sprintf("force-%d", i), []node.Hash{testHash(1)}, []string{"db-ip"}, force); err != nil || !reused || again.ID != task.ID {
			t.Fatalf("retained request retry: %+v %v %v", again, reused, err)
		}
	}
}

type batchObservationSource struct {
	ok      bool
	status  string
	message string
}

func (*batchObservationSource) Name() string      { return "db-ip" }
func (*batchObservationSource) Kind() SourceKind  { return SourceKindByIP }
func (*batchObservationSource) RequiresKey() bool { return false }
func (s *batchObservationSource) Check(context.Context, CheckRequest) (*SourceResult, error) {
	return &SourceResult{OK: s.ok, Status: s.status, Error: s.message, Data: Data{Country: "private_raw_marker"}}, nil
}

func TestBatchSourceFailuresAreNotPassingResults(t *testing.T) {
	for _, tc := range []struct{ status, want string }{{"error", "all_failed"}, {"unavailable", "unavailable"}} {
		t.Run(tc.status, func(t *testing.T) {
			e := newTestEngine(t, newFixtureHost(), nil, DefaultConfig())
			e.sources["db-ip"] = &batchObservationSource{status: tc.status, message: strings.Repeat("源", 600)}
			task := submitBatchTest(t, e, "failed", []node.Hash{testHash(1)}, []string{"db-ip"}, false)
			done := finishBatchTest(t, e, task.ID)
			if done.State != "completed" || done.Items[0].ObservationStatus != tc.want || done.Items[0].Sources[0].OK {
				t.Fatalf("invented pass %+v", done)
			}
			if len(done.Items[0].Sources[0].Error) > 512 {
				t.Fatal("unbounded error")
			}
			encoded, err := json.Marshal(done)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private_raw_marker") || strings.Contains(string(encoded), "config_fingerprint") {
				t.Fatal("task copied raw result/cache metadata")
			}
		})
	}
	h := newFixtureHost()
	h.via = func(context.Context, node.Hash, *http.Request) (*http.Response, error) {
		return nil, errors.New("fixture unavailable")
	}
	e := newTestEngine(t, h, nil, DefaultConfig())
	task := submitBatchTest(t, e, "partial", []node.Hash{testHash(1)}, []string{"db-ip", "egress-consistency"}, false)
	if done := finishBatchTest(t, e, task.ID); done.Items[0].ObservationStatus != "partial_failure" {
		t.Fatalf("lost partial result: %+v", done)
	}
}

func TestBatchNodeErrorsAndChanges(t *testing.T) {
	h := newFixtureHost()
	h.nodes[testHash(2)] = netip.MustParseAddr("127.0.0.1")
	h.via = func(context.Context, node.Hash, *http.Request) (*http.Response, error) {
		h.setIP(testHash(1), "9.9.9.9")
		return echoResponse("ip=8.8.8.8\n"), nil
	}
	e := newTestEngine(t, h, nil, DefaultConfig())
	task := submitBatchTest(t, e, "nodes", []node.Hash{testHash(1), testHash(2), testHash(3)}, []string{"egress-consistency"}, false)
	done := finishBatchTest(t, e, task.ID)
	if done.Progress.Failed != 3 {
		t.Fatalf("invalid nodes not terminal: %+v", done)
	}
	for i, code := range []string{"CONFLICT", "CONFLICT", "NOT_FOUND"} {
		if done.Items[i].Error == nil || done.Items[i].Error.Code != code || len(done.Items[i].Sources) != 0 {
			t.Fatalf("item %d: %+v", i, done.Items[i])
		}
	}
}

func TestBatchRecordBoundsExpiryAndDetachedSnapshots(t *testing.T) {
	e := newTestEngine(t, newFixtureHost(), nil, DefaultConfig())
	var first, last string
	for i := 0; i < MaxBatchTaskRecords+2; i++ {
		task := submitBatchTest(t, e, fmt.Sprintf("record-%d", i), []node.Hash{testHash(1)}, []string{"db-ip"}, false)
		finishBatchTest(t, e, task.ID)
		if i == 0 {
			first = task.ID
		}
		last = task.ID
	}
	if len(e.batches.jobs) != MaxBatchTaskRecords {
		t.Fatal("record bound")
	}
	if _, err := e.GetBatch(context.Background(), first); !errors.Is(err, ErrBatchNotFound) {
		t.Fatal("old record not evicted", err)
	}
	snapshot, err := e.GetBatch(context.Background(), last)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Sources[0] = "mutated"
	snapshot.Items[0].Sources[0].Status = "mutated"
	*snapshot.FinishedAt = time.Time{}
	*snapshot.Items[0].StartedAt = time.Time{}
	again, err := e.GetBatch(context.Background(), last)
	if err != nil || again.Sources[0] != "db-ip" || again.Items[0].Sources[0].Status != "ok" || again.FinishedAt.IsZero() || again.Items[0].StartedAt.IsZero() {
		t.Fatalf("mutable snapshot: %+v %v", again, err)
	}
	e.batches.mu.Lock()
	future := time.Now().Add(batchRecordTTL + time.Minute)
	e.batches.now = func() time.Time { return future }
	e.batches.mu.Unlock()
	if _, err = e.GetBatch(context.Background(), last); !errors.Is(err, ErrBatchNotFound) {
		t.Fatal("expired record retained", err)
	}
	if len(e.batches.jobs) != 0 {
		t.Fatal("expired records retained")
	}
}

func TestBatchCloseCancelsWorkersAndQueuedItems(t *testing.T) {
	h := newFixtureHost()
	h.geo = func(ctx context.Context, _ netip.Addr) (GeoIPInfo, bool) { <-ctx.Done(); return GeoIPInfo{}, false }
	e := newTestEngine(t, h, nil, DefaultConfig())
	for i := 0; i < MaxActiveBatchTasks; i++ {
		submitBatchTest(t, e, fmt.Sprintf("stop-%d", i), []node.Hash{testHash(1), testHash(2)}, []string{"db-ip"}, true)
	}
	waitFor(t, func() bool { return waiters(e) == 2 })
	done := make(chan struct{})
	go func() { _ = e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("engine close deadlocked with batch workers")
	}
	if h.geoCalls.Load() != 1 {
		t.Fatal("queued work ran during close")
	}
	for _, job := range e.batches.jobs {
		if job.task.State != "canceled" || job.task.FinishedAt == nil {
			t.Fatalf("unfinished closed task: %+v", job.task)
		}
	}
	if _, _, err := e.SubmitBatch(context.Background(), "closed", []node.Hash{testHash(1)}, []string{"db-ip"}, false); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
