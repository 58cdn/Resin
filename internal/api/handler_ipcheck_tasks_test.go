package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
)

const ipTaskSubmitPath = "/api/v1/ip-checks/actions/check-batch"

func taskRequestBody(id string, hashes []string, sources []string, force bool) string {
	encoded, _ := json.Marshal(ipCheckTaskRequest{RequestID: id, NodeHashes: hashes, Sources: sources, Force: force})
	return string(encoded)
}

func readTaskTest(t *testing.T, rec *httptest.ResponseRecorder, status int) ipcheck.BatchTask {
	t.Helper()
	tpExpectStatus(t, rec, status)
	var task ipcheck.BatchTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("task response must not be cached")
	}
	return task
}

func TestIPCheckTasksAuthAndDisabled(t *testing.T) {
	srv, _, _, host := newIPCheckAPIServer(t, false)
	valid := taskRequestBody("auth", []string{ipCheckTestHash.Hex()}, []string{"db-ip"}, false)
	for _, tc := range []struct{ method, path, body string }{{"POST", ipTaskSubmitPath, valid}, {"GET", "/api/v1/ip-checks/tasks/" + strings.Repeat("a", 32), ""}, {"POST", "/api/v1/ip-checks/tasks/" + strings.Repeat("a", 32) + "/actions/cancel", ""}} {
		tpExpectError(t, doJSONRequest(t, srv, tc.method, tc.path, tc.body, false), http.StatusUnauthorized, "UNAUTHORIZED", "")
		tpExpectError(t, doJSONRequest(t, srv, tc.method, tc.path, tc.body, true), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "")
	}
	if host.geoCalls.Load() != 0 || host.viaCalls.Load() != 0 {
		t.Fatal("disabled or unauthenticated task executed")
	}
}

func TestIPCheckTasksExactScopeValidation(t *testing.T) {
	_, cp, _, host := newIPCheckAPIServer(t, true)
	validHash := `"` + ipCheckTestHash.Hex() + `"`
	for _, body := range []string{
		"", "null", "[]", `{}`, `{} {}`, `{"request_id":"x","node_hashes":null,"sources":["db-ip"]}`,
		`{"request_id":"x","node_hashes":[` + validHash + `],"sources":[]}`,
		`{"request_id":"x","node_hashes":[` + validHash + `],"sources":["db-ip"],"force":null}`,
		`{"request_id":"x","node_hashes":[` + validHash + `],"sources":["db-ip"],"force":"true"}`,
		`{"request_id":"x","node_hashes":[` + validHash + `],"sources":["db-ip"],"force":false,"force":true}`,
		`{"request_id":"x","NODE_HASHES":[` + validHash + `],"sources":["db-ip"]}`,
		`{"request_id":"x","node_hashes":[` + validHash + `],"sources":["db-ip"],"platform_id":"all"}`,
		`{"request_id":"x","node_hashes":[` + validHash + `],"sources":["db-ip"],"subscription_id":"all"}`,
		taskRequestBody("invalid", []string{ipCheckTestHash.Hex(), "invalid"}, []string{"db-ip"}, false),
		taskRequestBody("oversized", make([]string, 51), []string{"db-ip"}, false),
		taskRequestBody("source", []string{ipCheckTestHash.Hex()}, []string{"future"}, false),
		taskRequestBody("duplicate-source", []string{ipCheckTestHash.Hex()}, []string{"db-ip", "db-ip"}, false),
		taskRequestBody("bad/id", []string{ipCheckTestHash.Hex()}, []string{"db-ip"}, false),
	} {
		rec := httptest.NewRecorder()
		HandleSubmitIPCheckBatch(cp)(rec, httptest.NewRequest("POST", ipTaskSubmitPath, strings.NewReader(body)))
		tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "")
	}
	rec := httptest.NewRecorder()
	HandleSubmitIPCheckBatch(cp)(rec, httptest.NewRequest("POST", ipTaskSubmitPath, strings.NewReader(taskRequestBody(strings.Repeat("x", 17<<10), []string{ipCheckTestHash.Hex()}, []string{"db-ip"}, false))))
	tpExpectStatus(t, rec, http.StatusRequestEntityTooLarge)
	if host.geoCalls.Load() != 0 || host.viaCalls.Load() != 0 {
		t.Fatal("invalid scope executed partial selection")
	}
}

func TestIPCheckTasksAcceptanceProgressRetryAndConflict(t *testing.T) {
	srv, cp, _, host := newIPCheckAPIServer(t, true)
	body := taskRequestBody("once", []string{strings.ToUpper(ipCheckTestHash.Hex()), ipCheckTestHash.Hex()}, []string{"db-ip"}, false)
	firstRec := doJSONRequest(t, srv, "POST", ipTaskSubmitPath, body, true)
	first := readTaskTest(t, firstRec, http.StatusAccepted)
	if first.Progress.Total != 1 || firstRec.Header().Get("Location") != "/api/v1/ip-checks/tasks/"+first.ID {
		t.Fatalf("bad acceptance %+v", first)
	}
	deadline := time.Now().Add(3 * time.Second)
	var done ipcheck.BatchTask
	for time.Now().Before(deadline) {
		current, err := cp.GetIPCheckBatch(context.Background(), first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.FinishedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	done = readTaskTest(t, doJSONRequest(t, srv, "GET", firstRec.Header().Get("Location"), "", true), http.StatusOK)
	if done.State != "completed" || done.Progress.Completed != 1 || done.Items[0].ObservationStatus != "all_ok" {
		t.Fatalf("incomplete progress %+v", done)
	}
	again := readTaskTest(t, doJSONRequest(t, srv, "POST", ipTaskSubmitPath, body, true), http.StatusOK)
	if again.ID != first.ID || host.geoCalls.Load() != 1 {
		t.Fatal("retry executed another task")
	}
	conflict := httptest.NewRecorder()
	HandleSubmitIPCheckBatch(cp)(conflict, httptest.NewRequest("POST", ipTaskSubmitPath, strings.NewReader(taskRequestBody("once", []string{ipCheckTestHash.Hex()}, []string{"db-ip"}, true))))
	tpExpectError(t, conflict, http.StatusConflict, "CONFLICT", "request_id")
	canceled := readTaskTest(t, doJSONRequest(t, srv, "POST", firstRec.Header().Get("Location")+"/actions/cancel", "", true), http.StatusOK)
	if canceled.State != "completed" {
		t.Fatal("cancel rewrote completed observations")
	}
	tpExpectError(t, doJSONRequest(t, srv, "GET", "/api/v1/ip-checks/tasks/bad", "", true), http.StatusBadRequest, "INVALID_ARGUMENT", "")
	tpExpectError(t, doJSONRequest(t, srv, "GET", "/api/v1/ip-checks/tasks/"+strings.Repeat("f", 32), "", true), http.StatusNotFound, "NOT_FOUND", "")
}

func TestIPCheckTasksRateLimitHasNoExecution(t *testing.T) {
	calls := 0
	// No refill: this assertion remains valid if Windows scheduling is slow.
	handler := boundedIPCheckTaskHandler(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
	}, 0, 2, 2)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", ipTaskSubmitPath, nil))
		if i < 2 {
			tpExpectStatus(t, rec, http.StatusAccepted)
		} else {
			tpExpectError(t, rec, http.StatusTooManyRequests, "RATE_LIMITED", "task requests")
			if rec.Header().Get("Retry-After") != "1" {
				t.Fatal("missing retry guidance")
			}
		}
	}
	if calls != 2 {
		t.Fatal("limited request reached execution")
	}
}

type taskErrorReader struct{ cancel context.CancelFunc }

func (r taskErrorReader) Read([]byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
		return 0, context.Canceled
	}
	return 0, &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.123"), Port: 12345}, Err: context.DeadlineExceeded}
}

func TestIPCheckTasksBodyTimeoutAndCancellation(t *testing.T) {
	_, cp, _, host := newIPCheckAPIServer(t, true)
	for _, cancelDuringRead := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		reader := taskErrorReader{}
		if cancelDuringRead {
			reader.cancel = cancel
		}
		rec := &batchDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		start := time.Now()
		HandleSubmitIPCheckBatch(cp)(rec, httptest.NewRequest("POST", ipTaskSubmitPath, reader).WithContext(ctx))
		cancel()
		tpExpectError(t, rec.ResponseRecorder, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "timed out")
		if strings.Contains(rec.Body.String(), "192.0.2.123") || strings.Contains(rec.Body.String(), "read tcp") {
			t.Fatal("transport details leaked")
		}
		if rec.read.Before(start.Add(5*time.Second)) || rec.read.After(time.Now().Add(5*time.Second)) || rec.write.Before(start.Add(15*time.Second)) || rec.write.After(time.Now().Add(15*time.Second)) {
			t.Fatal("missing bounded transport deadlines")
		}
	}
	if host.geoCalls.Load() != 0 || host.viaCalls.Load() != 0 {
		t.Fatal("failed body admitted a task")
	}
}

type taskBlockedWriter struct {
	*httptest.ResponseRecorder
	started chan<- struct{}
	release <-chan struct{}
}

func (w *taskBlockedWriter) Write(p []byte) (int, error) {
	w.started <- struct{}{}
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestIPCheckTasksPollingCannotOccupyCancelSlots(t *testing.T) {
	srv, cp, _, _ := newIPCheckAPIServer(t, true)
	task, _, err := cp.SubmitIPCheckBatch(context.Background(), "slots", []string{ipCheckTestHash.Hex()}, []string{"db-ip"}, false)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/ip-checks/tasks/" + task.ID
	started, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	defer close(release)
	for range 2 {
		go func() {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Authorization", "Bearer "+testAdminToken)
			srv.Handler().ServeHTTP(&taskBlockedWriter{httptest.NewRecorder(), started, release}, req)
			done <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("response did not enter write")
		}
	}
	limited := doJSONRequest(t, srv, "GET", path, "", true)
	tpExpectError(t, limited, http.StatusTooManyRequests, "RATE_LIMITED", "task requests")
	if limited.Header().Get("Retry-After") != "1" {
		t.Fatal("missing retry guidance")
	}
	readTaskTest(t, doJSONRequest(t, srv, "POST", path+"/actions/cancel", "", true), http.StatusOK)
	// Release both responses and prove the response slots are reusable.
	for range 2 {
		release <- struct{}{}
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("response slot not released")
		}
	}
	readTaskTest(t, doJSONRequest(t, srv, "GET", path, "", true), http.StatusOK)
}

func TestIPCheckTasksPluginLifecycleNoManagerDeadlock(t *testing.T) {
	for _, action := range []string{"disable", "configure", "stop"} {
		t.Run(action, func(t *testing.T) {
			_, cp, mgr, host := newIPCheckAPIServer(t, true)
			hashes := make([]string, ipcheck.MaxBatchNodes)
			for i := range hashes {
				hash := node.HashFromRawOptions([]byte(fmt.Sprintf("batch-node-%d", i)))
				hashes[i] = hash.Hex()
				host.nodes[hash] = ipcheck.NodeSnapshot{Hash: hash, EgressIP: netip.MustParseAddr("8.8.8.8"), Enabled: true, Healthy: true}
			}
			host.viaResponse = func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			task, _, err := cp.SubmitIPCheckBatch(context.Background(), "lifecycle", hashes, []string{"egress-consistency"}, true)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for host.viaCalls.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if host.viaCalls.Load() != 2 {
				t.Fatal("workers not active")
			}
			stopped := make(chan error, 1)
			go func() {
				switch action {
				case "disable":
					enabled := false
					_, err := mgr.Update(context.Background(), builtin.IPQualityID, plugin.Update{Enabled: &enabled})
					stopped <- err
				case "configure":
					_, err := mgr.Update(context.Background(), builtin.IPQualityID, plugin.Update{Config: json.RawMessage(`{"enabled_sources":["db-ip"]}`)})
					stopped <- err
				case "stop":
					mgr.Stop(context.Background())
					stopped <- nil
				}
			}()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("manager lifecycle deadlocked with batch workers")
			}
			if host.viaCalls.Load() != 2 {
				t.Fatalf("old queued items executed during %s: %d", action, host.viaCalls.Load())
			}
			if _, err := cp.GetIPCheckBatch(context.Background(), task.ID); err == nil {
				t.Fatal("old-generation task remained accessible")
			}
			if action == "configure" {
				newTask, _, err := cp.SubmitIPCheckBatch(context.Background(), "new-generation", []string{hashes[0]}, []string{"db-ip"}, false)
				if err != nil || newTask.ID == task.ID {
					t.Fatalf("new generation unavailable: %+v %v", newTask, err)
				}
			}
		})
	}
}
