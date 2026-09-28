package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

type tpWebhookCapture struct {
	mu       sync.Mutex
	requests []tpCapturedRequest
}

type tpCapturedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func (c *tpWebhookCapture) all() []tpCapturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tpCapturedRequest(nil), c.requests...)
}

func tpWebhookServer(t *testing.T, status int) (*httptest.Server, *tpWebhookCapture) {
	t.Helper()
	capture := &tpWebhookCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.mu.Lock()
		capture.requests = append(capture.requests, tpCapturedRequest{
			method: r.Method,
			path:   r.URL.Path,
			header: r.Header.Clone(),
			body:   body,
		})
		capture.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, capture
}

func tpWebhookHandler(t *testing.T, config string) pluginsdk.EventHandler {
	t.Helper()
	p := tpConfigure(t, WebhookID, config)
	h, ok := p.(pluginsdk.EventHandler)
	if !ok {
		t.Fatalf("%T does not implement EventHandler", p)
	}
	return h
}

func tpEvents() []pluginsdk.Event {
	ts := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	return []pluginsdk.Event{
		{Type: pluginsdk.EventRequestFinished, Time: ts, Data: json.RawMessage(`{"platform":"p","account":"a"}`)},
		{Type: pluginsdk.EventLeaseCreated, Time: ts.Add(time.Second), Data: json.RawMessage(`{"account":"a"}`)},
		{Type: pluginsdk.EventLeaseExpired, Time: ts.Add(2 * time.Second), Data: json.RawMessage(`{"account":"b"}`)},
	}
}

type tpWebhookBody struct {
	Source string            `json:"source"`
	Events []pluginsdk.Event `json:"events"`
}

func tpDecodeWebhookBody(t *testing.T, raw []byte) tpWebhookBody {
	t.Helper()
	var body tpWebhookBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode webhook body %q: %v", raw, err)
	}
	return body
}

func TestWebhook_PostsEventBatch(t *testing.T) {
	srv, capture := tpWebhookServer(t, http.StatusOK)
	cfg := fmt.Sprintf(`{"url":%q,"headers":{"Authorization":"Bearer secret","X-Custom":"custom-value"}}`, srv.URL+"/hook")
	h := tpWebhookHandler(t, cfg)

	events := tpEvents()
	if err := h.HandleEvents(context.Background(), events); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}

	reqs := capture.all()
	if len(reqs) != 1 {
		t.Fatalf("server received %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", got.method)
	}
	if got.path != "/hook" {
		t.Fatalf("path = %s, want /hook", got.path)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if ua := got.header.Get("User-Agent"); ua != "Resin-Webhook/1" {
		t.Fatalf("User-Agent = %q", ua)
	}
	if v := got.header.Get("Authorization"); v != "Bearer secret" {
		t.Fatalf("Authorization = %q", v)
	}
	if v := got.header.Get("X-Custom"); v != "custom-value" {
		t.Fatalf("X-Custom = %q", v)
	}

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &generic); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	if len(generic) != 2 {
		t.Fatalf("body keys = %v, want exactly source and events", generic)
	}
	body := tpDecodeWebhookBody(t, got.body)
	if body.Source != "resin" {
		t.Fatalf("source = %q, want resin", body.Source)
	}
	if len(body.Events) != len(events) {
		t.Fatalf("events len = %d, want %d", len(body.Events), len(events))
	}
	for i := range events {
		if body.Events[i].Type != events[i].Type || !body.Events[i].Time.Equal(events[i].Time) {
			t.Fatalf("event %d = %+v, want %+v", i, body.Events[i], events[i])
		}
		if string(body.Events[i].Data) != string(events[i].Data) {
			t.Fatalf("event %d data = %s, want %s", i, body.Events[i].Data, events[i].Data)
		}
	}
}

func TestWebhook_FiltersEvents(t *testing.T) {
	srv, capture := tpWebhookServer(t, http.StatusOK)
	h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q,"events":["lease.*"]}`, srv.URL))

	if err := h.HandleEvents(context.Background(), tpEvents()); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}
	reqs := capture.all()
	if len(reqs) != 1 {
		t.Fatalf("server received %d requests, want 1", len(reqs))
	}
	body := tpDecodeWebhookBody(t, reqs[0].body)
	if len(body.Events) != 2 {
		t.Fatalf("events = %+v, want only the 2 lease events", body.Events)
	}
	for _, ev := range body.Events {
		if ev.Type == pluginsdk.EventRequestFinished {
			t.Fatalf("request.finished should be filtered out: %+v", body.Events)
		}
	}

	// Nothing selected -> no HTTP request, no error.
	onlyRequests := []pluginsdk.Event{{Type: pluginsdk.EventRequestFinished, Time: time.Now()}}
	if err := h.HandleEvents(context.Background(), onlyRequests); err != nil {
		t.Fatalf("HandleEvents(no selected): %v", err)
	}
	if err := h.HandleEvents(context.Background(), nil); err != nil {
		t.Fatalf("HandleEvents(nil): %v", err)
	}
	if n := len(capture.all()); n != 1 {
		t.Fatalf("server received %d requests, want still 1", n)
	}
}

func TestWebhook_ExactEventTypes(t *testing.T) {
	srv, capture := tpWebhookServer(t, http.StatusOK)
	h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q,"events":["request.finished","lease.expired"]}`, srv.URL))
	if err := h.HandleEvents(context.Background(), tpEvents()); err != nil {
		t.Fatalf("HandleEvents: %v", err)
	}
	reqs := capture.all()
	if len(reqs) != 1 {
		t.Fatalf("server received %d requests, want 1", len(reqs))
	}
	body := tpDecodeWebhookBody(t, reqs[0].body)
	if len(body.Events) != 2 || body.Events[0].Type != pluginsdk.EventRequestFinished || body.Events[1].Type != pluginsdk.EventLeaseExpired {
		t.Fatalf("events = %+v", body.Events)
	}
}

func TestWebhook_UnconfiguredIsNoop(t *testing.T) {
	p := tpFindBuiltin(t, WebhookID).New()
	h := p.(pluginsdk.EventHandler)
	if err := h.HandleEvents(context.Background(), tpEvents()); err != nil {
		t.Fatalf("unconfigured HandleEvents should be a no-op, got %v", err)
	}
}

func TestWebhook_Non2xxReturnsError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusMovedPermanently} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv, capture := tpWebhookServer(t, status)
			h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q}`, srv.URL))
			err := h.HandleEvents(context.Background(), tpEvents())
			tpAssertErrContains(t, err, fmt.Sprintf("webhook returned HTTP %d", status))
			if n := len(capture.all()); n != 1 {
				t.Fatalf("server received %d requests, want 1", n)
			}
		})
	}
	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv, _ := tpWebhookServer(t, status)
			h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q}`, srv.URL))
			if err := h.HandleEvents(context.Background(), tpEvents()); err != nil {
				t.Fatalf("status %d should succeed, got %v", status, err)
			}
		})
	}
}

func TestWebhook_ConnectionErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q,"timeout_ms":2000}`, url))
	if err := h.HandleEvents(context.Background(), tpEvents()); err == nil {
		t.Fatal("expected error for closed server")
	}
}

func TestWebhook_InvalidConfig(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"missing url", `{}`, "url must be an absolute http(s) URL"},
		{"empty url", `{"url":"  "}`, "url must be an absolute http(s) URL"},
		{"relative url", `{"url":"/hook"}`, "url must be an absolute http(s) URL"},
		{"no scheme", `{"url":"hooks.example.com/x"}`, "url must be an absolute http(s) URL"},
		{"ftp scheme", `{"url":"ftp://hooks.example.com/x"}`, "url must be an absolute http(s) URL"},
		{"no host", `{"url":"http://"}`, "url must be an absolute http(s) URL"},
		{"unparseable", `{"url":"http://[::1"}`, "url must be an absolute http(s) URL"},
		{"url wrong type", `{"url":5}`, "invalid config"},
		{"blank header name", `{"url":"https://h.example.com","headers":{" ":"v"}}`, "headers: invalid header"},
		{"header value newline", `{"url":"https://h.example.com","headers":{"X-A":"a\nb"}}`, "headers: invalid header"},
		{"header name newline", `{"url":"https://h.example.com","headers":{"X-A\r\nX-B":"v"}}`, "headers: invalid header"},
		{"headers wrong type", `{"url":"https://h.example.com","headers":["x"]}`, "invalid config"},
		{"unknown event", `{"url":"https://h.example.com","events":["foo.bar"]}`, `events: unknown event type "foo.bar"`},
		{"unknown lease event", `{"url":"https://h.example.com","events":["lease.bogus"]}`, `events: unknown event type "lease.bogus"`},
		{"timeout too large", `{"url":"https://h.example.com","timeout_ms":60001}`, "timeout_ms must be at most 60000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpAssertErrContains(t, tpConfigureErr(t, WebhookID, tc.config), tc.want)
		})
	}

	valid := []string{
		`{"url":"http://h.example.com"}`,
		`{"url":"  https://h.example.com/path?q=1  "}`,
		`{"url":"https://h.example.com","events":[]}`,
		`{"url":"https://h.example.com","events":["*"]}`,
		`{"url":"https://h.example.com","events":["request.*","lease.*"]}`,
		`{"url":"https://h.example.com","timeout_ms":0}`,
		`{"url":"https://h.example.com","timeout_ms":60000}`,
		`{"url":"https://h.example.com","headers":{}}`,
	}
	for _, cfg := range valid {
		if err := tpConfigureErr(t, WebhookID, cfg); err != nil {
			t.Errorf("Configure(%s): unexpected error %v", cfg, err)
		}
	}
}

func TestWebhook_TimeoutRespected(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close

	h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q,"timeout_ms":100}`, srv.URL))
	start := time.Now()
	err := h.HandleEvents(context.Background(), tpEvents())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("HandleEvents took %v, timeout_ms=100 not respected", elapsed)
	}
}

func TestWebhook_ParentContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	h := tpWebhookHandler(t, fmt.Sprintf(`{"url":%q,"timeout_ms":30000}`, srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.HandleEvents(ctx, tpEvents()); err == nil {
		t.Fatal("expected error when parent context expires")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("HandleEvents took %v, parent context not respected", elapsed)
	}
}
