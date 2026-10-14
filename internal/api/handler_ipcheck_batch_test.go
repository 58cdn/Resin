package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/plugin/builtin"
	"github.com/Resinat/Resin/internal/service"
	"golang.org/x/time/rate"
)

const ipCheckBatchPath = "/api/v1/ip-checks/cache/nodes"

type nodeIPCheckCacheFunc func(context.Context, string) (*ipcheck.Result, error)

func (f nodeIPCheckCacheFunc) GetNodeIPCheck(ctx context.Context, hash string) (*ipcheck.Result, error) {
	return f(ctx, hash)
}

func batchRequest(ctx context.Context, hashes ...string) *http.Request {
	body, _ := json.Marshal(ipCheckBatchRequest{NodeHashes: hashes})
	return httptest.NewRequest(http.MethodPost, ipCheckBatchPath, strings.NewReader(string(body))).WithContext(ctx)
}

func readBatch(t *testing.T, rec *httptest.ResponseRecorder) ipCheckBatchResponse {
	t.Helper()
	tpExpectStatus(t, rec, http.StatusOK)
	var response ipCheckBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestIPCheckBatch_ValidationBeforeReads(t *testing.T) {
	oversized, _ := json.Marshal(ipCheckBatchRequest{NodeHashes: make([]string, 51)})
	for _, body := range []string{
		"", "null", "[]", `{}`, `{"node_hashes":null}`, `{"node_hashes":[]}`,
		`{"node_hashes":"bad"}`, `{"node_hashes":[1]}`, `{"node_hashes":["bad"]}`,
		`{"node_hashes":["` + ipCheckTestHash.Hex() + `","bad"]}`,
		`{"node_hashes":["` + ipCheckTestHash.Hex() + `"],"force":true}`,
		`{"node_hashes":["bad"],"node_hashes":["` + ipCheckTestHash.Hex() + `"]}`,
		`{"NODE_HASHES":["` + ipCheckTestHash.Hex() + `"]}`,
		`{"node_hashes":["bad"],"NODE_HASHES":["` + ipCheckTestHash.Hex() + `"]}`,
		`{"node_hashes":["` + ipCheckTestHash.Hex() + `"]} {}`, string(oversized),
	} {
		t.Run(body, func(t *testing.T) {
			reader := nodeIPCheckCacheFunc(func(context.Context, string) (*ipcheck.Result, error) {
				t.Fatal("invalid selection reached cache")
				return nil, nil
			})
			rec := httptest.NewRecorder()
			newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))(rec, httptest.NewRequest(http.MethodPost, ipCheckBatchPath, strings.NewReader(body)))
			tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "")
		})
	}
	rec := httptest.NewRecorder()
	newNodeIPCheckBatchHandler(nil, rate.NewLimiter(rate.Inf, 1))(rec, httptest.NewRequest(http.MethodPost, ipCheckBatchPath, strings.NewReader(`{"node_hashes":["`+strings.Repeat("a", ipCheckBatchMaxBodyBytes)+`"]}`)))
	tpExpectError(t, rec, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "")
}

func TestIPCheckBatch_MaxNodesBeforeDedup(t *testing.T) {
	hashes := make([]string, ipCheckBatchMaxNodes)
	for i := range hashes {
		hashes[i] = ipCheckTestHash.Hex()
	}
	calls := 0
	reader := nodeIPCheckCacheFunc(func(context.Context, string) (*ipcheck.Result, error) { calls++; return &ipcheck.Result{}, nil })
	handler := newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))
	rec := httptest.NewRecorder()
	handler(rec, batchRequest(context.Background(), hashes...))
	if response := readBatch(t, rec); response.Total != 1 || calls != 1 {
		t.Fatalf("50 duplicate nodes: %+v calls=%d", response, calls)
	}
	hashes = append(hashes, ipCheckTestHash.Hex())
	rec = httptest.NewRecorder()
	handler(rec, batchRequest(context.Background(), hashes...))
	tpExpectError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "")
	if calls != 1 {
		t.Fatal("51 repeated entries bypassed the original-list limit")
	}
}

type batchDeadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (r *batchDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	r.read = deadline
	return nil
}
func (r *batchDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.write = deadline
	return nil
}

func TestIPCheckBatch_TransportDeadlinesAndSlowBody(t *testing.T) {
	reader := nodeIPCheckCacheFunc(func(context.Context, string) (*ipcheck.Result, error) { return &ipcheck.Result{}, nil })
	handler := newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))
	rec := &batchDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	handler(rec, batchRequest(context.Background(), ipCheckTestHash.Hex()))
	if rec.read.Before(start) || rec.read.After(time.Now().Add(ipCheckBatchTimeout)) || rec.write.Before(rec.read) || rec.write.After(time.Now().Add(3*ipCheckBatchTimeout)) {
		t.Fatalf("missing/unbounded transport deadlines: read=%v write=%v", rec.read, rec.write)
	}
	// Exercise the real net/http body deadline, using only a temporary loopback
	// listener. A context timeout alone cannot interrupt a stalled Body.Read.
	srv := httptest.NewServer(handler)
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * ipCheckBatchTimeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("POST /api/v1/ip-checks/cache/nodes HTTP/1.1\r\nHost: fixture\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{\"node_hashes\":[")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("slow request status=%d", response.StatusCode)
	}
	// The timed-out request must release its slot for a subsequent request.
	next := httptest.NewRecorder()
	handler(next, batchRequest(context.Background(), ipCheckTestHash.Hex()))
	tpExpectStatus(t, next, http.StatusOK)
}

func TestIPCheckBatch_OrderDedupEvidenceAndErrors(t *testing.T) {
	hashes := []string{strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)}
	zero, inconsistent := 0.0, false
	result := &ipcheck.Result{
		Summary: ipcheck.Summary{RiskLevel: "unknown", FraudScore: &zero, EgressConsistent: &inconsistent},
		Sources: []ipcheck.SourceResult{{Source: "fixture", Status: "error", Error: "partial source failure", ConfigFingerprint: "private-fingerprint", Data: ipcheck.Data{TargetErrors: map[string]string{"fixture": "unavailable"}}}},
	}
	var visited []string
	reader := nodeIPCheckCacheFunc(func(ctx context.Context, hash string) (*ipcheck.Result, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > ipCheckBatchTimeout {
			t.Fatal("cache execution must have a bounded deadline")
		}
		visited = append(visited, hash)
		switch hash {
		case hashes[0]:
			return result, nil
		case hashes[1]:
			return nil, &service.ServiceError{Code: "NOT_FOUND", Message: "IP quality not checked: no unexpired results"}
		case hashes[2]:
			return nil, &service.ServiceError{Code: "CONFLICT", Message: "node egress IP changed during check; retry"}
		default:
			return nil, errors.New("private path and fixture secret")
		}
	})
	rec := httptest.NewRecorder()
	newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))(rec, batchRequest(context.Background(), strings.ToUpper(hashes[0]), hashes[1], hashes[0], hashes[2], hashes[3]))
	response := readBatch(t, rec)
	if response.Total != 4 || len(response.Items) != 4 || !reflect.DeepEqual(visited, hashes) {
		t.Fatalf("order/dedup: response=%+v visited=%v", response, visited)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "config_fingerprint") {
		t.Fatalf("private data or caching policy: %s", rec.Body.String())
	}
	cached := decodeIPCheckResult(t, response.Items[0].Result)
	if cached.Summary.RiskLevel != "unknown" || cached.Summary.FraudScore == nil || *cached.Summary.FraudScore != 0 || cached.Summary.EgressConsistent == nil || *cached.Summary.EgressConsistent || cached.Summary.ASN != nil || cached.Sources[0].Error != "partial source failure" || cached.Sources[0].Data.TargetErrors["fixture"] != "unavailable" {
		t.Fatalf("lost evidence or unknown/zero/false: %+v", cached)
	}
	if result.Sources[0].ConfigFingerprint != "private-fingerprint" {
		t.Fatal("response redaction mutated cache data")
	}
	for i, code := range []string{"NOT_FOUND", "CONFLICT", "INTERNAL"} {
		item := response.Items[i+1]
		if item.NodeHash != hashes[i+1] || item.Error == nil || item.Error.Code != code || len(item.Result) != 0 {
			t.Fatalf("item failure: %+v", item)
		}
	}
}

func TestIPCheckBatch_BoundedResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *ipcheck.Result
		code   string
	}{
		{"large", &ipcheck.Result{Sources: []ipcheck.SourceResult{{Data: ipcheck.Data{Attribution: strings.Repeat("x", ipCheckBatchMaxResultBytes)}}}}, "RESULT_TOO_LARGE"},
		{"unencodable", &ipcheck.Result{Summary: ipcheck.Summary{FraudScore: func() *float64 { n := math.NaN(); return &n }()}}, "INTERNAL"},
		{"nil", nil, "INTERNAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			reader := nodeIPCheckCacheFunc(func(context.Context, string) (*ipcheck.Result, error) { return tc.result, nil })
			newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))(rec, batchRequest(context.Background(), ipCheckTestHash.Hex()))
			item := readBatch(t, rec).Items[0]
			if item.Error == nil || item.Error.Code != tc.code || len(item.Result) != 0 || rec.Body.Len() > 512 {
				t.Fatalf("bounded failure: %s", rec.Body.String())
			}
		})
	}
}

func TestIPCheckBatch_CancelAndUnavailableDiscardPartial(t *testing.T) {
	for _, cancelRequest := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		reader := nodeIPCheckCacheFunc(func(context.Context, string) (*ipcheck.Result, error) {
			calls++
			if calls == 1 {
				return &ipcheck.Result{}, nil
			}
			if cancelRequest {
				cancel()
				return nil, context.Canceled
			}
			return nil, &service.ServiceError{Code: "SERVICE_UNAVAILABLE", Message: "private runtime details"}
		})
		rec := httptest.NewRecorder()
		newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))(rec, batchRequest(ctx, strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)))
		cancel()
		tpExpectError(t, rec, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "")
		if calls != 2 || strings.Contains(rec.Body.String(), "items") || strings.Contains(rec.Body.String(), "private") {
			t.Fatalf("partial work escaped or continued: calls=%d body=%s", calls, rec.Body.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	newNodeIPCheckBatchHandler(nil, rate.NewLimiter(rate.Inf, 1))(rec, batchRequest(ctx, ipCheckTestHash.Hex()))
	tpExpectError(t, rec, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "canceled")
}

func TestIPCheckBatch_RateLimitAndConcurrencyRelease(t *testing.T) {
	reader := nodeIPCheckCacheFunc(func(context.Context, string) (*ipcheck.Result, error) { return &ipcheck.Result{}, nil })
	// No refill makes the admission test deterministic without sleeping.
	handler := newNodeIPCheckBatchHandler(reader, rate.NewLimiter(0, 2))
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler(rec, batchRequest(context.Background(), ipCheckTestHash.Hex()))
		if i < 2 {
			tpExpectStatus(t, rec, http.StatusOK)
		} else {
			tpExpectError(t, rec, http.StatusTooManyRequests, "RATE_LIMITED", "")
			if rec.Header().Get("Retry-After") != "1" {
				t.Fatal("missing retry hint")
			}
		}
	}
	started := make(chan struct{}, 2)
	var calls atomic.Int64
	reader = nodeIPCheckCacheFunc(func(ctx context.Context, _ string) (*ipcheck.Result, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	handler = newNodeIPCheckBatchHandler(reader, rate.NewLimiter(rate.Inf, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := httptest.NewRecorder()
			handler(rec, batchRequest(ctx, ipCheckTestHash.Hex()))
			done <- rec
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("cache call did not start")
		}
	}
	rec := httptest.NewRecorder()
	handler(rec, batchRequest(context.Background(), ipCheckTestHash.Hex()))
	tpExpectError(t, rec, http.StatusTooManyRequests, "RATE_LIMITED", "")
	if calls.Load() != 2 {
		t.Fatal("overload reached the cache")
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case rec := <-done:
			tpExpectError(t, rec, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "canceled")
		case <-time.After(2 * time.Second):
			t.Fatal("canceled batch did not exit")
		}
	}
	// A short deadline proves the slot was released and the reader sees it.
	ctx, cancelDeadline := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelDeadline()
	rec = httptest.NewRecorder()
	handler(rec, batchRequest(ctx, ipCheckTestHash.Hex()))
	tpExpectError(t, rec, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "timed out")
	if calls.Load() != 3 {
		t.Fatal("canceled requests leaked admission slots")
	}
}

func TestIPCheckBatchAPI_AuthAndDisabled(t *testing.T) {
	srv, _, _, _ := newIPCheckAPIServer(t, false)
	body := ipCheckBatchRequest{NodeHashes: []string{ipCheckTestHash.Hex()}}
	tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, ipCheckBatchPath, body, false), http.StatusUnauthorized, "UNAUTHORIZED", "")
	tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, ipCheckBatchPath, body, true), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "disabled")
	smallServer, _, _ := newControlPlaneTestServerWithBodyLimit(t, 32)
	tpExpectError(t, doJSONRequest(t, smallServer, http.MethodPost, ipCheckBatchPath, body, true), http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "")
}

func TestIPCheckBatchAPI_CacheOnlyAndEgressInvalidation(t *testing.T) {
	srv, cp, _, host := newIPCheckAPIServer(t, true)
	// This host supplies every response in memory. The addresses are fixture
	// values and no HTTP client or real node is contacted to seed the cache.
	if _, err := cp.CheckNodeIP(context.Background(), ipCheckTestHash.Hex(), []string{"egress-consistency"}, false); err != nil {
		t.Fatal(err)
	}
	initialVia := host.viaCalls.Load()
	missing := node.HashFromRawOptions([]byte(`{"type":"missing"}`))
	noEgress := node.HashFromRawOptions([]byte(`{"type":"no-egress"}`))
	uncached := node.HashFromRawOptions([]byte(`{"type":"uncached"}`))
	host.nodes[noEgress] = ipcheck.NodeSnapshot{Hash: noEgress}
	host.nodes[uncached] = ipcheck.NodeSnapshot{Hash: uncached, EgressIP: netip.MustParseAddr("1.1.1.1")}
	rec := doJSONRequest(t, srv, http.MethodPost, ipCheckBatchPath, ipCheckBatchRequest{NodeHashes: []string{ipCheckTestHash.Hex(), missing.Hex(), noEgress.Hex(), uncached.Hex()}}, true)
	response := readBatch(t, rec)
	if response.Total != 4 || len(response.Items) != 4 {
		t.Fatalf("unexpected response: %+v", response)
	}
	result := decodeIPCheckResult(t, response.Items[0].Result)
	if len(result.Sources) != 1 || result.Summary.RiskLevel != "unknown" || result.Summary.EgressConsistent != nil || result.Summary.FraudScore != nil || result.Sources[0].ConfigFingerprint != "" {
		t.Fatalf("invalid evidence: %+v", result)
	}
	for i, code := range []string{"NOT_FOUND", "CONFLICT", "NOT_FOUND"} {
		if response.Items[i+1].Error == nil || response.Items[i+1].Error.Code != code {
			t.Fatalf("item failure: %+v", response.Items[i+1])
		}
	}
	snapshot := host.nodes[ipCheckTestHash]
	snapshot.EgressVersion++ // Same IP, newer successful egress observation.
	host.nodes[ipCheckTestHash] = snapshot
	rec = doJSONRequest(t, srv, http.MethodPost, ipCheckBatchPath, ipCheckBatchRequest{NodeHashes: []string{ipCheckTestHash.Hex()}}, true)
	item := readBatch(t, rec).Items[0]
	if item.Error == nil || item.Error.Code != "NOT_FOUND" || len(item.Result) != 0 {
		t.Fatalf("old egress evidence reused: %+v", item)
	}
	if host.viaCalls.Load() != initialVia || host.geoCalls.Load() != 0 {
		t.Fatal("batch read started a source check")
	}
}

func TestIPCheckBatchAPI_ConfigReplacementAndDisable(t *testing.T) {
	srv, cp, mgr, host := newIPCheckAPIServer(t, true)
	if _, err := cp.CheckNodeIP(context.Background(), ipCheckTestHash.Hex(), []string{"db-ip"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Update(context.Background(), builtin.IPQualityID, plugin.Update{Config: json.RawMessage(`{"enabled_sources":["egress-consistency"]}`)}); err != nil {
		t.Fatal(err)
	}
	body := ipCheckBatchRequest{NodeHashes: []string{ipCheckTestHash.Hex()}}
	item := readBatch(t, doJSONRequest(t, srv, http.MethodPost, ipCheckBatchPath, body, true)).Items[0]
	if item.Error == nil || item.Error.Code != "NOT_FOUND" {
		t.Fatalf("old configuration evidence reused: %+v", item)
	}
	disabled := false
	if _, err := mgr.Update(context.Background(), builtin.IPQualityID, plugin.Update{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	tpExpectError(t, doJSONRequest(t, srv, http.MethodPost, ipCheckBatchPath, body, true), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "disabled")
	if host.geoCalls.Load() != 1 || host.viaCalls.Load() != 0 {
		t.Fatal("batch read ran sources after reconfiguration")
	}
}
