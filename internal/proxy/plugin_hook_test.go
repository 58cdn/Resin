package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/pkg/pluginsdk"
	M "github.com/sagernet/sing/common/metadata"
)

// tpFakeHook is a scripted RequestHook used by the plugin hook tests.
type tpFakeHook struct {
	inactive bool
	fn       func(req *pluginsdk.RequestInfo) RequestHookResult

	mu    sync.Mutex
	calls []pluginsdk.RequestInfo
	count atomic.Int32
}

func (h *tpFakeHook) Active() bool { return !h.inactive }

func (h *tpFakeHook) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) RequestHookResult {
	h.count.Add(1)
	snapshot := *req
	if req.Headers != nil {
		snapshot.Headers = make(map[string][]string, len(req.Headers))
		for k, v := range req.Headers {
			snapshot.Headers[k] = append([]string(nil), v...)
		}
	}
	h.mu.Lock()
	h.calls = append(h.calls, snapshot)
	h.mu.Unlock()
	if h.fn == nil {
		return tpEcho(req)
	}
	return h.fn(req)
}

func (h *tpFakeHook) lastCall(t *testing.T) pluginsdk.RequestInfo {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) == 0 {
		t.Fatal("hook was not called")
	}
	return h.calls[len(h.calls)-1]
}

// tpEcho is the "continue without changes" result (what plugin.Manager returns
// when no plugin changes anything).
func tpEcho(req *pluginsdk.RequestInfo) RequestHookResult {
	return RequestHookResult{Platform: req.Platform, Account: req.Account}
}

func tpRejectWith(status int, msg, pluginID string) func(*pluginsdk.RequestInfo) RequestHookResult {
	return func(req *pluginsdk.RequestInfo) RequestHookResult {
		return RequestHookResult{
			Reject:   &ProxyError{HTTPCode: status, ResinError: ErrPluginRejected.ResinError, Message: msg},
			PluginID: pluginID,
			Platform: req.Platform,
			Account:  req.Account,
		}
	}
}

func tpFailClosed(pluginID string) func(*pluginsdk.RequestInfo) RequestHookResult {
	return func(req *pluginsdk.RequestInfo) RequestHookResult {
		return RequestHookResult{Reject: ErrPluginUnavailable, PluginID: pluginID, Platform: req.Platform, Account: req.Account}
	}
}

func tpOverride(platform, account string) func(*pluginsdk.RequestInfo) RequestHookResult {
	return func(req *pluginsdk.RequestInfo) RequestHookResult {
		return RequestHookResult{Platform: platform, Account: account}
	}
}

func tpAssertPluginReject(t *testing.T, w *httptest.ResponseRecorder, status int, resinErr, pluginID, body string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status: got %d, want %d (body=%q, resinErr=%q)", w.Code, status, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	if got := w.Header().Get("X-Resin-Error"); got != resinErr {
		t.Fatalf("X-Resin-Error: got %q, want %q", got, resinErr)
	}
	if got := w.Header().Get("X-Resin-Plugin"); got != pluginID {
		t.Fatalf("X-Resin-Plugin: got %q, want %q", got, pluginID)
	}
	if got := w.Body.String(); got != body {
		t.Fatalf("body: got %q, want %q", got, body)
	}
}

func tpExpectLog(t *testing.T, emitter *mockEventEmitter) RequestLogEntry {
	t.Helper()
	select {
	case ev := <-emitter.logCh:
		return ev
	case <-time.After(time.Second):
		t.Fatal("expected request log event")
		return RequestLogEntry{}
	}
}

// tpUpstream starts an HTTP server that records the headers it receives.
func tpUpstream(t *testing.T) (*httptest.Server, <-chan http.Header) {
	t.Helper()
	seen := make(chan http.Header, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		h.Set("X-Tp-Host", r.Host)
		seen <- h
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func tpUpstreamHeaders(t *testing.T, seen <-chan http.Header) http.Header {
	t.Helper()
	select {
	case h := <-seen:
		return h
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
		return nil
	}
}

func tpNewForward(t *testing.T, hook RequestHook, emitter EventEmitter) (*ForwardProxy, *proxyE2EEnv) {
	t.Helper()
	env := newProxyE2EEnv(t)
	if emitter == nil {
		emitter = NoOpEventEmitter{}
	}
	fp := NewForwardProxy(ForwardProxyConfig{
		ProxyToken: "tok",
		Router:     env.router,
		Pool:       env.pool,
		Health:     &mockHealthRecorder{},
		Events:     emitter,
		Hooks:      hook,
	})
	return fp, env
}

func tpNewReverse(t *testing.T, hook RequestHook, emitter EventEmitter) *ReverseProxy {
	t.Helper()
	env := newProxyE2EEnv(t)
	if emitter == nil {
		emitter = NoOpEventEmitter{}
	}
	return NewReverseProxy(ReverseProxyConfig{
		ProxyToken:     "tok",
		Router:         env.router,
		Pool:           env.pool,
		PlatformLookup: env.pool,
		Health:         &mockHealthRecorder{},
		Events:         emitter,
		Hooks:          hook,
	})
}

// --- Forward proxy: plain HTTP ---

func TestPluginHook_ForwardHTTPReject(t *testing.T) {
	hook := &tpFakeHook{fn: tpRejectWith(http.StatusUnavailableForLegalReasons, "blocked by policy", "resin.access-control")}
	emitter := newMockEventEmitter()
	fp, _ := tpNewForward(t, hook, emitter)

	upstreamHit := atomic.Bool{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet, upstream.URL+"/x", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusUnavailableForLegalReasons, "PLUGIN_REJECTED", "resin.access-control", "blocked by policy")
	if upstreamHit.Load() {
		t.Fatal("rejected request must not reach upstream")
	}
	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "PLUGIN_REJECTED" || logEv.HTTPStatus != http.StatusUnavailableForLegalReasons {
		t.Fatalf("log: ResinError=%q HTTPStatus=%d", logEv.ResinError, logEv.HTTPStatus)
	}
	if logEv.NodeHash != "" {
		t.Fatalf("rejected request must not be routed, NodeHash=%q", logEv.NodeHash)
	}
}

func TestPluginHook_ForwardHTTPRejectWithoutPluginID(t *testing.T) {
	hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
		return RequestHookResult{Reject: ErrPluginRejected}
	}}
	fp, _ := tpNewForward(t, hook, nil)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusForbidden, "PLUGIN_REJECTED", "", "Request rejected by plugin")
	if _, present := w.Header()["X-Resin-Plugin"]; present {
		t.Fatal("X-Resin-Plugin should be absent when PluginID is empty")
	}
}

func TestPluginHook_ForwardHTTPFailClosed(t *testing.T) {
	hook := &tpFakeHook{fn: tpFailClosed("ext.broken")}
	emitter := newMockEventEmitter()
	fp, _ := tpNewForward(t, hook, emitter)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusServiceUnavailable, "PLUGIN_ERROR", "ext.broken", "Request plugin unavailable")
	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "PLUGIN_ERROR" || logEv.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("log: ResinError=%q HTTPStatus=%d", logEv.ResinError, logEv.HTTPStatus)
	}
}

func TestPluginHook_ForwardHTTPRequestInfo(t *testing.T) {
	hook := &tpFakeHook{}
	fp, _ := tpNewForward(t, hook, nil)
	upstream, seen := tpUpstream(t)

	req := httptest.NewRequest(http.MethodPost, upstream.URL+"/v1/items?q=1", strings.NewReader("body"))
	req.RemoteAddr = "198.51.100.23:40000"
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct-1", "tok"))
	req.Header.Set("X-Client", "c1")
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	_ = tpUpstreamHeaders(t, seen)

	info := hook.lastCall(t)
	host := strings.TrimPrefix(upstream.URL, "http://")
	if info.ProxyType != pluginsdk.ProxyTypeForward || info.IsConnect {
		t.Fatalf("ProxyType=%q IsConnect=%v", info.ProxyType, info.IsConnect)
	}
	if info.Platform != "plat" || info.Account != "acct-1" {
		t.Fatalf("Platform=%q Account=%q, want plat/acct-1", info.Platform, info.Account)
	}
	if info.ClientIP != "198.51.100.23" {
		t.Fatalf("ClientIP=%q", info.ClientIP)
	}
	if info.TargetHost != host {
		t.Fatalf("TargetHost=%q, want %q", info.TargetHost, host)
	}
	if info.Method != http.MethodPost {
		t.Fatalf("Method=%q", info.Method)
	}
	if info.URL != upstream.URL+"/v1/items?q=1" {
		t.Fatalf("URL=%q", info.URL)
	}
	if got := info.Headers["X-Client"]; len(got) != 1 || got[0] != "c1" {
		t.Fatalf("Headers[X-Client]=%v", got)
	}
	for k := range info.Headers {
		if strings.EqualFold(k, "Proxy-Authorization") {
			t.Fatalf("Proxy-Authorization must not be exposed to plugins: %v", info.Headers)
		}
	}
}

func TestPluginHook_ForwardHTTPHeaderOpsApplied(t *testing.T) {
	hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
		res := tpEcho(req)
		res.HeaderOps = []HeaderOp{
			{Name: "X-Remove-Me", Remove: true},
			{Name: "X-Added", Value: "added-" + req.Account},
			{Name: "X-Replace", Value: "new"},
			{Name: "Host", Value: "evil.example"},
			{Name: "Content-Length", Remove: true},
			{Name: "", Value: "ignored"},
			{Name: "X-Set-Then-Removed", Value: "v"},
			{Name: "X-Set-Then-Removed", Remove: true},
		}
		return res
	}}
	fp, _ := tpNewForward(t, hook, nil)
	upstream, seen := tpUpstream(t)

	req := httptest.NewRequest(http.MethodGet, upstream.URL+"/h", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	req.Header.Set("X-Remove-Me", "1")
	req.Header.Set("X-Replace", "old")
	req.Header.Set("X-Untouched", "keep")
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	h := tpUpstreamHeaders(t, seen)
	if h.Get("X-Remove-Me") != "" {
		t.Fatalf("X-Remove-Me should be removed, got %q", h.Get("X-Remove-Me"))
	}
	if h.Get("X-Added") != "added-acct" {
		t.Fatalf("X-Added=%q", h.Get("X-Added"))
	}
	if vals := h.Values("X-Replace"); len(vals) != 1 || vals[0] != "new" {
		t.Fatalf("X-Replace=%v, want [new]", vals)
	}
	if h.Get("X-Untouched") != "keep" {
		t.Fatalf("X-Untouched=%q", h.Get("X-Untouched"))
	}
	if h.Get("X-Set-Then-Removed") != "" {
		t.Fatalf("X-Set-Then-Removed should be removed, got %q", h.Get("X-Set-Then-Removed"))
	}
	if host := h.Get("X-Tp-Host"); host != strings.TrimPrefix(upstream.URL, "http://") {
		t.Fatalf("Host header must not be rewritten, upstream saw %q", host)
	}
	if h.Get("Proxy-Authorization") != "" {
		t.Fatal("Proxy-Authorization leaked upstream")
	}
}

func TestPluginHook_ForwardHTTPPlatformOverrideUsedForRouting(t *testing.T) {
	upstream, _ := tpUpstream(t)

	// Without an override, an unknown platform fails routing.
	fp, _ := tpNewForward(t, &tpFakeHook{}, nil)
	req := httptest.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("ghost.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || w.Header().Get("X-Resin-Error") != "PLATFORM_NOT_FOUND" {
		t.Fatalf("baseline: got %d %q, want 404 PLATFORM_NOT_FOUND", w.Code, w.Header().Get("X-Resin-Error"))
	}

	// Override to the real platform: routing succeeds with the new identity.
	emitter := newMockEventEmitter()
	hook := &tpFakeHook{fn: tpOverride("plat", "rewritten-acct")}
	fp, _ = tpNewForward(t, hook, emitter)
	req = httptest.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("ghost.acct", "tok"))
	w = httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("override: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	logEv := tpExpectLog(t, emitter)
	if logEv.PlatformName != "plat" {
		t.Fatalf("log PlatformName=%q, want plat", logEv.PlatformName)
	}
	if logEv.Account != "rewritten-acct" {
		t.Fatalf("log Account=%q, want rewritten-acct", logEv.Account)
	}

	// Override away from a valid platform: routing uses the override and fails.
	hook = &tpFakeHook{fn: tpOverride("missing", "acct")}
	fp, _ = tpNewForward(t, hook, nil)
	req = httptest.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w = httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || w.Header().Get("X-Resin-Error") != "PLATFORM_NOT_FOUND" {
		t.Fatalf("override to missing: got %d %q, want 404 PLATFORM_NOT_FOUND", w.Code, w.Header().Get("X-Resin-Error"))
	}
}

func TestPluginHook_ForwardInactiveHookNotCalled(t *testing.T) {
	hook := &tpFakeHook{inactive: true, fn: tpRejectWith(http.StatusForbidden, "should not happen", "x")}
	fp, _ := tpNewForward(t, hook, nil)
	upstream, seen := tpUpstream(t)

	req := httptest.NewRequest(http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	_ = tpUpstreamHeaders(t, seen)

	// CONNECT with an inactive hook reaches routing (fails with an unknown
	// platform) rather than the hook.
	req = httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
	req.Host = "example.com:443"
	req.Header.Set("Proxy-Authorization", basicAuth("ghost.acct", "tok"))
	w = httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Header().Get("X-Resin-Error") == "PLUGIN_REJECTED" {
		t.Fatal("inactive hook must not reject CONNECT")
	}
	if n := hook.count.Load(); n != 0 {
		t.Fatalf("inactive hook was called %d times", n)
	}
}

func TestPluginHook_ForwardAuthFailureSkipsHook(t *testing.T) {
	hook := &tpFakeHook{}
	fp, _ := tpNewForward(t, hook, nil)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "wrong"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("bad token should fail")
	}
	if n := hook.count.Load(); n != 0 {
		t.Fatalf("hook must run after authentication, but was called %d times", n)
	}
}

// --- Forward proxy: CONNECT ---

func TestPluginHook_ConnectReject(t *testing.T) {
	hook := &tpFakeHook{fn: tpRejectWith(http.StatusForbidden, "no tunnels", "resin.access-control")}
	emitter := newMockEventEmitter()
	fp, _ := tpNewForward(t, hook, emitter)

	req := httptest.NewRequest(http.MethodConnect, "http://secure.example.com:443", nil)
	req.Host = "secure.example.com:443"
	req.RemoteAddr = "192.0.2.77:5555"
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	req.Header.Set("X-Connect-Hint", "h")
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusForbidden, "PLUGIN_REJECTED", "resin.access-control", "no tunnels")

	info := hook.lastCall(t)
	if info.ProxyType != pluginsdk.ProxyTypeForward || !info.IsConnect {
		t.Fatalf("ProxyType=%q IsConnect=%v", info.ProxyType, info.IsConnect)
	}
	if info.TargetHost != "secure.example.com:443" {
		t.Fatalf("TargetHost=%q", info.TargetHost)
	}
	if info.Platform != "plat" || info.Account != "acct" {
		t.Fatalf("Platform=%q Account=%q", info.Platform, info.Account)
	}
	if info.ClientIP != "192.0.2.77" {
		t.Fatalf("ClientIP=%q", info.ClientIP)
	}
	if info.Method != http.MethodConnect {
		t.Fatalf("Method=%q", info.Method)
	}
	if got := info.Headers["X-Connect-Hint"]; len(got) != 1 || got[0] != "h" {
		t.Fatalf("Headers[X-Connect-Hint]=%v", got)
	}
	for k := range info.Headers {
		if strings.EqualFold(k, "Proxy-Authorization") {
			t.Fatalf("Proxy-Authorization must not be exposed to plugins: %v", info.Headers)
		}
	}

	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "PLUGIN_REJECTED" || logEv.HTTPStatus != http.StatusForbidden {
		t.Fatalf("log: ResinError=%q HTTPStatus=%d", logEv.ResinError, logEv.HTTPStatus)
	}
}

func TestPluginHook_ConnectFailClosed(t *testing.T) {
	hook := &tpFakeHook{fn: tpFailClosed("ext.timeout")}
	fp, _ := tpNewForward(t, hook, nil)

	req := httptest.NewRequest(http.MethodConnect, "http://secure.example.com:443", nil)
	req.Host = "secure.example.com:443"
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusServiceUnavailable, "PLUGIN_ERROR", "ext.timeout", "Request plugin unavailable")
}

func TestPluginHook_ConnectPlatformOverrideUsedForRouting(t *testing.T) {
	hook := &tpFakeHook{fn: tpOverride("missing", "acct")}
	fp, _ := tpNewForward(t, hook, nil)

	req := httptest.NewRequest(http.MethodConnect, "http://secure.example.com:443", nil)
	req.Host = "secure.example.com:443"
	req.Header.Set("Proxy-Authorization", basicAuth("plat.acct", "tok"))
	w := httptest.NewRecorder()
	fp.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound || w.Header().Get("X-Resin-Error") != "PLATFORM_NOT_FOUND" {
		t.Fatalf("got %d %q, want 404 PLATFORM_NOT_FOUND from overridden platform", w.Code, w.Header().Get("X-Resin-Error"))
	}
}

// --- Reverse proxy ---

func TestPluginHook_ReverseReject(t *testing.T) {
	hook := &tpFakeHook{fn: tpRejectWith(http.StatusTooManyRequests, "quota exceeded", "ext.quota")}
	emitter := newMockEventEmitter()
	rp := tpNewReverse(t, hook, emitter)

	upstreamHit := atomic.Bool{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tok/plat:acct/http/%s/api", host), nil)
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusTooManyRequests, "PLUGIN_REJECTED", "ext.quota", "quota exceeded")
	if upstreamHit.Load() {
		t.Fatal("rejected request must not reach upstream")
	}
	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "PLUGIN_REJECTED" || logEv.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("log: ResinError=%q HTTPStatus=%d", logEv.ResinError, logEv.HTTPStatus)
	}
}

func TestPluginHook_ReverseFailClosed(t *testing.T) {
	hook := &tpFakeHook{fn: tpFailClosed("ext.down")}
	rp := tpNewReverse(t, hook, nil)

	req := httptest.NewRequest(http.MethodGet, "/tok/plat:acct/http/example.com/api", nil)
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)

	tpAssertPluginReject(t, w, http.StatusServiceUnavailable, "PLUGIN_ERROR", "ext.down", "Request plugin unavailable")
}

func TestPluginHook_ReverseRequestInfoAndHeaderOps(t *testing.T) {
	hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
		res := tpEcho(req)
		res.HeaderOps = []HeaderOp{
			{Name: "X-Debug", Remove: true},
			{Name: "X-Resin-Plat", Value: req.Platform},
			{Name: "X-Resin-Acct", Value: req.Account},
			{Name: "Host", Value: "evil.example"},
			{Name: "Transfer-Encoding", Value: "chunked"},
		}
		return res
	}}
	rp := tpNewReverse(t, hook, nil)
	upstream, seen := tpUpstream(t)
	host := strings.TrimPrefix(upstream.URL, "http://")

	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/tok/plat:acct-9/http/%s/api/v1?k=v", host), strings.NewReader("x"))
	req.RemoteAddr = "203.0.113.50:1234"
	req.Header.Set("X-Debug", "1")
	req.Header.Set("X-Keep", "k")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}

	info := hook.lastCall(t)
	if info.ProxyType != pluginsdk.ProxyTypeReverse || info.IsConnect {
		t.Fatalf("ProxyType=%q IsConnect=%v", info.ProxyType, info.IsConnect)
	}
	if info.Platform != "plat" || info.Account != "acct-9" {
		t.Fatalf("Platform=%q Account=%q", info.Platform, info.Account)
	}
	if info.ClientIP != "203.0.113.50" {
		t.Fatalf("ClientIP=%q", info.ClientIP)
	}
	if info.TargetHost != host {
		t.Fatalf("TargetHost=%q, want %q", info.TargetHost, host)
	}
	if info.Method != http.MethodPut {
		t.Fatalf("Method=%q", info.Method)
	}
	if info.URL != fmt.Sprintf("http://%s/api/v1?k=v", host) {
		t.Fatalf("URL=%q", info.URL)
	}
	if got := info.Headers["X-Debug"]; len(got) != 1 || got[0] != "1" {
		t.Fatalf("Headers[X-Debug]=%v", got)
	}
	for k := range info.Headers {
		if strings.EqualFold(k, "Proxy-Authorization") {
			t.Fatalf("Proxy-Authorization must not be exposed to plugins: %v", info.Headers)
		}
	}

	h := tpUpstreamHeaders(t, seen)
	if h.Get("X-Debug") != "" {
		t.Fatalf("X-Debug should be removed upstream, got %q", h.Get("X-Debug"))
	}
	if h.Get("X-Resin-Plat") != "plat" || h.Get("X-Resin-Acct") != "acct-9" {
		t.Fatalf("set headers upstream: plat=%q acct=%q", h.Get("X-Resin-Plat"), h.Get("X-Resin-Acct"))
	}
	if h.Get("X-Keep") != "k" {
		t.Fatalf("X-Keep=%q", h.Get("X-Keep"))
	}
	if got := h.Get("X-Tp-Host"); got != host {
		t.Fatalf("Host must not be rewritten, upstream saw %q", got)
	}
	if te := h.Get("Transfer-Encoding"); te != "" {
		t.Fatalf("Transfer-Encoding must not be set by plugins, got %q", te)
	}
}

func TestPluginHook_ReversePlatformOverrideUsedForRouting(t *testing.T) {
	upstream, _ := tpUpstream(t)
	host := strings.TrimPrefix(upstream.URL, "http://")
	path := fmt.Sprintf("/tok/ghost:acct/http/%s/api", host)

	rp := tpNewReverse(t, &tpFakeHook{}, nil)
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusNotFound || w.Header().Get("X-Resin-Error") != "PLATFORM_NOT_FOUND" {
		t.Fatalf("baseline: got %d %q, want 404 PLATFORM_NOT_FOUND", w.Code, w.Header().Get("X-Resin-Error"))
	}

	emitter := newMockEventEmitter()
	rp = tpNewReverse(t, &tpFakeHook{fn: tpOverride("plat", "acct-override")}, emitter)
	w = httptest.NewRecorder()
	rp.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("override: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	logEv := tpExpectLog(t, emitter)
	if logEv.PlatformName != "plat" || logEv.Account != "acct-override" {
		t.Fatalf("log PlatformName=%q Account=%q", logEv.PlatformName, logEv.Account)
	}
}

func TestPluginHook_ReverseInactiveHookNotCalled(t *testing.T) {
	hook := &tpFakeHook{inactive: true, fn: tpRejectWith(http.StatusForbidden, "should not happen", "x")}
	rp := tpNewReverse(t, hook, nil)
	upstream, seen := tpUpstream(t)
	host := strings.TrimPrefix(upstream.URL, "http://")

	w := httptest.NewRecorder()
	rp.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tok/plat:acct/http/%s/api", host), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	_ = tpUpstreamHeaders(t, seen)
	if n := hook.count.Load(); n != 0 {
		t.Fatalf("inactive hook was called %d times", n)
	}
}

// --- SOCKS5 ---

func tpSocks5Connect(t *testing.T, inbound *Socks5Inbound, user, pass, target string) ([]byte, net.Conn, io.Reader, <-chan struct{}) {
	t.Helper()
	clientConn, reader, done := startSocks5Session(t, inbound)
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))

	writeAll(t, clientConn, []byte{socks5Version, 1, socks5MethodUserPass})
	if got := readExactly(t, reader, 2); got[1] != socks5MethodUserPass {
		t.Fatalf("selected method: got %d, want %d", got[1], socks5MethodUserPass)
	}
	writeAll(t, clientConn, socks5UserPassPacket(user, pass))
	if got := readExactly(t, reader, 2); got[1] != socks5UserPassStatusSuccess {
		t.Fatalf("auth status: got %d, want %d", got[1], socks5UserPassStatusSuccess)
	}
	writeAll(t, clientConn, socks5ConnectIPv4Packet(target))
	reply := readExactly(t, reader, 10)
	if reply[0] != socks5Version {
		t.Fatalf("reply version: got %d", reply[0])
	}
	return reply, clientConn, reader, done
}

func tpNewSocks5(t *testing.T, hook RequestHook, emitter EventEmitter) (*Socks5Inbound, *proxyE2EEnv) {
	t.Helper()
	env := newProxyE2EEnv(t)
	if emitter == nil {
		emitter = NoOpEventEmitter{}
	}
	return NewSocks5Inbound(Socks5InboundConfig{
		ProxyToken: "tok",
		Router:     env.router,
		Pool:       env.pool,
		Health:     &mockHealthRecorder{},
		Events:     emitter,
		Hooks:      hook,
	}), env
}

func TestPluginHook_Socks5RejectNotAllowed(t *testing.T) {
	hook := &tpFakeHook{fn: tpRejectWith(http.StatusForbidden, "denied", "resin.access-control")}
	emitter := newMockEventEmitter()
	inbound, _ := tpNewSocks5(t, hook, emitter)

	reply, conn, _, done := tpSocks5Connect(t, inbound, "plat.acct", "tok", "127.0.0.1:9")
	defer conn.Close()
	if reply[1] != 0x02 {
		t.Fatalf("REP: got 0x%02x, want 0x02 (connection not allowed)", reply[1])
	}
	<-done

	info := hook.lastCall(t)
	if info.ProxyType != pluginsdk.ProxyTypeSocks5 || !info.IsConnect {
		t.Fatalf("ProxyType=%q IsConnect=%v", info.ProxyType, info.IsConnect)
	}
	if info.Platform != "plat" || info.Account != "acct" {
		t.Fatalf("Platform=%q Account=%q", info.Platform, info.Account)
	}
	if info.TargetHost != "127.0.0.1:9" {
		t.Fatalf("TargetHost=%q", info.TargetHost)
	}
	if info.Headers != nil {
		t.Fatalf("SOCKS5 must not expose headers, got %v", info.Headers)
	}

	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "PLUGIN_REJECTED" {
		t.Fatalf("log ResinError=%q", logEv.ResinError)
	}
}

func TestPluginHook_Socks5PluginErrorGeneralFailure(t *testing.T) {
	hook := &tpFakeHook{fn: tpFailClosed("ext.down")}
	emitter := newMockEventEmitter()
	inbound, _ := tpNewSocks5(t, hook, emitter)

	reply, conn, _, done := tpSocks5Connect(t, inbound, "plat.acct", "tok", "127.0.0.1:9")
	defer conn.Close()
	if reply[1] != 0x01 {
		t.Fatalf("REP: got 0x%02x, want 0x01 (general failure)", reply[1])
	}
	<-done
	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "PLUGIN_ERROR" {
		t.Fatalf("log ResinError=%q", logEv.ResinError)
	}
}

func TestPluginHook_Socks5PlatformOverrideUsedForRouting(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()
	go func() {
		c, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	emitter := newMockEventEmitter()
	hook := &tpFakeHook{fn: tpOverride("plat", "socks-override")}
	inbound, env := tpNewSocks5(t, hook, emitter)
	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, targetLn.Addr().String())
	})

	reply, conn, connReader, done := tpSocks5Connect(t, inbound, "ghost.acct", "tok", targetLn.Addr().String())
	if reply[1] != socks5ReplySucceeded {
		conn.Close()
		t.Fatalf("REP: got 0x%02x, want success after platform override", reply[1])
	}
	const payload = "tp-ping"
	writeAll(t, conn, []byte(payload))
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(connReader, echo); err != nil || string(echo) != payload {
		conn.Close()
		t.Fatalf("echo: %q err=%v", echo, err)
	}
	conn.Close()
	<-done

	logEv := tpExpectLog(t, emitter)
	if logEv.PlatformName != "plat" || logEv.Account != "socks-override" {
		t.Fatalf("log PlatformName=%q Account=%q", logEv.PlatformName, logEv.Account)
	}

	// Override to a missing platform fails routing (general failure).
	hook2 := &tpFakeHook{fn: tpOverride("missing", "acct")}
	inbound2, _ := tpNewSocks5(t, hook2, nil)
	reply, conn2, _, done2 := tpSocks5Connect(t, inbound2, "plat.acct", "tok", "127.0.0.1:9")
	defer conn2.Close()
	if reply[1] != socks5ReplyGeneralFailure {
		t.Fatalf("REP: got 0x%02x, want general failure for missing platform", reply[1])
	}
	<-done2
}

func TestPluginHook_Socks5InactiveHookNotCalled(t *testing.T) {
	hook := &tpFakeHook{inactive: true, fn: tpRejectWith(http.StatusForbidden, "x", "x")}
	inbound, _ := tpNewSocks5(t, hook, nil)
	// Unknown platform: without the hook the request reaches routing and
	// fails with general failure, not "not allowed".
	reply, conn, _, done := tpSocks5Connect(t, inbound, "ghost.acct", "tok", "127.0.0.1:9")
	defer conn.Close()
	if reply[1] == 0x02 {
		t.Fatal("inactive hook must not reject")
	}
	<-done
	if n := hook.count.Load(); n != 0 {
		t.Fatalf("inactive hook was called %d times", n)
	}
}

func TestPluginHook_ReversePluginCannotExposeClientIPViaXFF(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   HeaderOp
		want string
	}{
		{name: "remove", op: HeaderOp{Name: "X-Forwarded-For", Remove: true}},
		{name: "set", op: HeaderOp{Name: "X-Forwarded-For", Value: "plugin-value"}, want: "plugin-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
				res := tpEcho(req)
				res.HeaderOps = []HeaderOp{tc.op}
				return res
			}}
			rp := tpNewReverse(t, hook, nil)
			upstream, seen := tpUpstream(t)
			host := strings.TrimPrefix(upstream.URL, "http://")
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tok/plat:acct/http/%s/xff", host), nil)
			req.RemoteAddr = "203.0.113.50:1234"
			w := httptest.NewRecorder()
			rp.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
			}
			h := tpUpstreamHeaders(t, seen)
			values := h.Values("X-Forwarded-For")
			if tc.want == "" && len(values) != 0 {
				t.Fatalf("X-Forwarded-For = %v, want absent", values)
			}
			if tc.want != "" && (len(values) != 1 || values[0] != tc.want) {
				t.Fatalf("X-Forwarded-For = %v, want [%q]", values, tc.want)
			}
			for _, value := range values {
				if strings.Contains(value, "203.0.113.50") {
					t.Fatalf("client IP leaked through X-Forwarded-For: %v", values)
				}
			}
		})
	}
}

func TestPluginHook_ReverseConnectionCannotRemovePluginHeader(t *testing.T) {
	hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
		res := tpEcho(req)
		res.HeaderOps = []HeaderOp{{Name: "X-Plugin-Header", Value: "kept"}}
		return res
	}}
	rp := tpNewReverse(t, hook, nil)
	upstream, seen := tpUpstream(t)
	host := strings.TrimPrefix(upstream.URL, "http://")
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/tok/plat:acct/http/%s/connection", host), nil)
	req.Header.Set("Connection", "X-Plugin-Header")
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q, resinErr=%q)", w.Code, w.Body.String(), w.Header().Get("X-Resin-Error"))
	}
	h := tpUpstreamHeaders(t, seen)
	if got := h.Get("X-Plugin-Header"); got != "kept" {
		t.Fatalf("plugin header was removed by Connection listing: %q", got)
	}
}

func TestPluginHook_ReversePlatformRewriteReappliesAccountPolicy(t *testing.T) {
	env := newProxyE2EEnv(t)
	alt := platform.NewPlatform("alt-id", "alt", nil, nil)
	alt.ReverseProxyEmptyAccountBehavior = string(platform.ReverseProxyEmptyAccountBehaviorFixedHeader)
	alt.ReverseProxyFixedAccountHeader = "X-Account-Id"
	alt.ReverseProxyMissAction = string(platform.ReverseProxyMissActionReject)
	env.pool.RegisterPlatform(alt)

	hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
		return RequestHookResult{Platform: "alt", Account: ""}
	}}
	rp := NewReverseProxy(ReverseProxyConfig{
		ProxyToken:     "tok",
		Router:         env.router,
		Pool:           env.pool,
		PlatformLookup: env.pool,
		Health:         &mockHealthRecorder{},
		Events:         NoOpEventEmitter{},
		Hooks:          hook,
	})
	req := httptest.NewRequest(http.MethodGet, "/tok/plat/http/example.com/path", nil)
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)
	if w.Code != ErrAccountRejected.HTTPCode || w.Header().Get("X-Resin-Error") != ErrAccountRejected.ResinError {
		t.Fatalf("status/error = %d/%q, want %d/%q", w.Code, w.Header().Get("X-Resin-Error"), ErrAccountRejected.HTTPCode, ErrAccountRejected.ResinError)
	}
	call := hook.lastCall(t)
	if call.Platform != "plat" || call.Account != "" {
		t.Fatalf("hook request identity = %q/%q, want plat/empty", call.Platform, call.Account)
	}
}

func TestPluginHook_DefaultPlatformAcrossProxyPaths(t *testing.T) {
	reject := func(req *pluginsdk.RequestInfo) RequestHookResult {
		return tpRejectWith(http.StatusForbidden, "stop after inspection", "test.default")(req)
	}

	t.Run("forward http", func(t *testing.T) {
		hook := &tpFakeHook{fn: reject}
		fp, _ := tpNewForward(t, hook, nil)
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", basicAuth("", "tok"))
		w := httptest.NewRecorder()
		fp.ServeHTTP(w, req)
		tpAssertPluginReject(t, w, http.StatusForbidden, "PLUGIN_REJECTED", "test.default", "stop after inspection")
		if got := hook.lastCall(t).Platform; got != "Default" {
			t.Fatalf("platform = %q, want Default", got)
		}
	})

	t.Run("connect", func(t *testing.T) {
		hook := &tpFakeHook{fn: reject}
		fp, _ := tpNewForward(t, hook, nil)
		req := httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
		req.Host = "example.com:443"
		req.Header.Set("Proxy-Authorization", basicAuth("", "tok"))
		w := httptest.NewRecorder()
		fp.ServeHTTP(w, req)
		tpAssertPluginReject(t, w, http.StatusForbidden, "PLUGIN_REJECTED", "test.default", "stop after inspection")
		if got := hook.lastCall(t).Platform; got != "Default" {
			t.Fatalf("platform = %q, want Default", got)
		}
	})

	t.Run("reverse", func(t *testing.T) {
		hook := &tpFakeHook{fn: reject}
		rp := tpNewReverse(t, hook, nil)
		w := httptest.NewRecorder()
		rp.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tok//http/example.com/path", nil))
		tpAssertPluginReject(t, w, http.StatusForbidden, "PLUGIN_REJECTED", "test.default", "stop after inspection")
		if got := hook.lastCall(t).Platform; got != "Default" {
			t.Fatalf("platform = %q, want Default", got)
		}
	})

	t.Run("socks5", func(t *testing.T) {
		hook := &tpFakeHook{fn: reject}
		inbound, _ := tpNewSocks5(t, hook, nil)
		reply, conn, _, done := tpSocks5Connect(t, inbound, "", "tok", "127.0.0.1:9")
		defer conn.Close()
		if reply[1] != socks5ReplyNotAllowed {
			t.Fatalf("REP = 0x%02x, want not allowed", reply[1])
		}
		<-done
		if got := hook.lastCall(t).Platform; got != "Default" {
			t.Fatalf("platform = %q, want Default", got)
		}
	})
}

func TestPluginHook_ClientCancellationDuringHookIsSilent(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	hook := &tpFakeHook{fn: func(req *pluginsdk.RequestInfo) RequestHookResult {
		close(started)
		<-release
		return RequestHookResult{Canceled: true, Platform: req.Platform, Account: req.Account}
	}}
	emitter := newMockEventEmitter()
	fp, _ := tpNewForward(t, hook, emitter)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", basicAuth("plat", "tok"))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		fp.ServeHTTP(w, req)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hook did not start")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not finish after cancellation")
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("X-Resin-Error") != "" {
		t.Fatalf("canceled response = code %d headers %v body %q", w.Code, w.Header(), w.Body.String())
	}
	logEv := tpExpectLog(t, emitter)
	if logEv.ResinError != "" || logEv.HTTPStatus != 0 || !logEv.NetOK {
		t.Fatalf("canceled request log = %+v", logEv)
	}
}

// --- helpers ---

func TestPluginHook_HookHeadersStripsProxyAuthorization(t *testing.T) {
	if hookHeaders(nil) != nil {
		t.Fatal("hookHeaders(nil) should be nil")
	}
	if hookHeaders(http.Header{}) != nil {
		t.Fatal("hookHeaders(empty) should be nil")
	}
	src := http.Header{
		"Proxy-Authorization": {"Basic abc"},
		"proxy-authorization": {"Basic lower"},
		"X-A":                 {"1", "2"},
	}
	got := hookHeaders(src)
	if len(got) != 1 {
		t.Fatalf("hookHeaders = %v, want only X-A", got)
	}
	if v := got["X-A"]; len(v) != 2 || v[0] != "1" || v[1] != "2" {
		t.Fatalf("X-A = %v", v)
	}
	// The snapshot must be a copy.
	got["X-A"][0] = "mutated"
	if src["X-A"][0] != "1" {
		t.Fatal("hookHeaders must deep-copy header values")
	}
}

func TestPluginHook_IsProtectedHookHeader(t *testing.T) {
	for _, h := range []string{"Host", "host", "Content-Length", "transfer-encoding", "Connection", "Upgrade", "TE", "Trailer"} {
		if !IsProtectedHookHeader(h) {
			t.Errorf("%q should be protected", h)
		}
	}
	for _, h := range []string{"X-Custom", "Authorization", "Cookie", "User-Agent"} {
		if IsProtectedHookHeader(h) {
			t.Errorf("%q should not be protected", h)
		}
	}
}

func TestPluginHook_ApplyHeaderOps(t *testing.T) {
	h := http.Header{"X-Old": {"1"}, "Host": {"h"}, "X-Multi": {"a", "b"}}
	applyHeaderOps(h, []HeaderOp{
		{Name: "X-Old", Remove: true},
		{Name: "X-New", Value: "n"},
		{Name: "X-Multi", Value: "c"},
		{Name: "Host", Remove: true},
		{Name: "connection", Value: "close"},
		{Name: "", Value: "x"},
	})
	if h.Get("X-Old") != "" {
		t.Fatal("X-Old should be removed")
	}
	if h.Get("X-New") != "n" {
		t.Fatalf("X-New=%q", h.Get("X-New"))
	}
	if v := h.Values("X-Multi"); len(v) != 1 || v[0] != "c" {
		t.Fatalf("X-Multi=%v, want [c]", v)
	}
	if h.Get("Host") != "h" {
		t.Fatal("protected Host must not be removed")
	}
	if h.Get("Connection") != "" {
		t.Fatal("protected Connection must not be set")
	}
}

func TestPluginHook_ProxyTypeName(t *testing.T) {
	cases := map[ProxyType]string{
		ProxyTypeForward:       pluginsdk.ProxyTypeForward,
		ProxyTypeReverse:       pluginsdk.ProxyTypeReverse,
		ProxyTypeSocks5Forward: pluginsdk.ProxyTypeSocks5,
	}
	for in, want := range cases {
		if got := ProxyTypeName(in); got != want {
			t.Errorf("ProxyTypeName(%d) = %q, want %q", in, got, want)
		}
	}
}
