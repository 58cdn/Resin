package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Resinat/Resin/internal/metrics"
	"github.com/Resinat/Resin/internal/service"
)

const ipCheckDialsPath = "/api/v1/metrics/ip-checks/dials"

func TestIPCheckDialsHandlerSnapshotIsReadOnly(t *testing.T) {
	mgr := metrics.NewManager(metrics.ManagerConfig{})
	handler := HandleIPCheckDials(mgr)
	read := func() metrics.IPCheckDialsSnapshot {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ipCheckDialsPath, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d cache=%q body=%s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"scope", "started_at", "observed_at", "dials_started", "dials_completed", "dials_succeeded", "dials_failed", "dials_canceled", "dials_in_flight"} {
			if _, ok := fields[name]; !ok {
				t.Fatalf("missing %s in %s", name, rec.Body.String())
			}
		}
		if len(fields) != 9 {
			t.Fatalf("unexpected labels or fields: %s", rec.Body.String())
		}
		var result metrics.IPCheckDialsSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Scope != "process" || result.StartedAt.IsZero() || result.ObservedAt.IsZero() {
			t.Fatalf("invalid scope/timestamps: %+v", result)
		}
		return result
	}
	initial := read()
	if initial.DialsStarted != 0 || initial.DialsCompleted != 0 || initial.DialsInFlight != 0 {
		t.Fatalf("reading triggered work: %+v", initial)
	}
	mgr.BeginIPCheckDial()(nil)
	mgr.BeginIPCheckDial()(context.Canceled)
	mgr.BeginIPCheckDial()(context.DeadlineExceeded)
	finish := mgr.BeginIPCheckDial()
	defer finish(nil)
	before := mgr.SnapshotIPCheckDials()
	legacy := mgr.Collector().Snapshot()
	for range 3 {
		got := read()
		got.ObservedAt = before.ObservedAt
		if got != before {
			t.Fatalf("reading changed counters: want=%+v got=%+v", before, got)
		}
	}
	if !reflect.DeepEqual(legacy, mgr.Collector().Snapshot()) {
		t.Fatal("reading changed legacy metrics")
	}
}

func TestIPCheckDialsHandlerRejectsAnyQueryAndMissingManager(t *testing.T) {
	mgr := metrics.NewManager(metrics.ManagerConfig{})
	for _, suffix := range []string{"?", "?platform_id=fixture", "?from=2026-01-01", "?node_hash=", "?unknown", "?x=%zz", "?;"} {
		t.Run(suffix, func(t *testing.T) {
			rec := httptest.NewRecorder()
			HandleIPCheckDials(mgr).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ipCheckDialsPath+suffix, nil))
			assertInvalidArgumentError(t, rec)
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("error response is cacheable")
			}
		})
	}
	rec := httptest.NewRecorder()
	HandleIPCheckDials(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ipCheckDialsPath, nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing manager: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := mgr.SnapshotIPCheckDials(); got.DialsStarted != 0 {
		t.Fatalf("rejected request started a dial: %+v", got)
	}
}

func TestIPCheckDialsRouteAuthenticationAndRegistration(t *testing.T) {
	mgr := metrics.NewManager(metrics.ManagerConfig{})
	// No control plane or plugin is required to read process-local counters.
	server := NewServer(0, "fixture-admin", service.SystemInfo{}, nil, nil, nil, 1024, nil, mgr)
	for _, token := range []string{"", "Bearer wrong", "Bearer fixture-admin"} {
		req := httptest.NewRequest(http.MethodGet, ipCheckDialsPath, nil)
		req.Header.Set("Authorization", token)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if token == "Bearer fixture-admin" {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Fatalf("token=%q status=%d want=%d body=%s", token, rec.Code, want, rec.Body.String())
		}
	}
	request := func(server *Server, method string) int {
		req := httptest.NewRequest(method, ipCheckDialsPath, nil)
		req.Header.Set("Authorization", "Bearer fixture-admin")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request(server, http.MethodPost); got != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d, want 405", got)
	}
	withoutMetrics := NewServer(0, "fixture-admin", service.SystemInfo{}, nil, nil, nil, 1024, nil, nil)
	if got := request(withoutMetrics, http.MethodGet); got != http.StatusNotFound {
		t.Fatalf("nil Manager registered route: status=%d", got)
	}
	if got := mgr.SnapshotIPCheckDials(); got.DialsStarted != 0 {
		t.Fatalf("route reads started a dial: %+v", got)
	}
}
