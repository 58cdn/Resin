package pluginsdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test plugins ---

// tsFullPlugin implements every optional interface.
type tsFullPlugin struct {
	mu        sync.Mutex
	inits     []RegisterParams
	configs   []string
	block     string
	events    []Event
	inspected int
	shutdowns int
}

func (p *tsFullPlugin) Init(_ context.Context, params RegisterParams) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inits = append(p.inits, params)
	return nil
}

func (p *tsFullPlugin) Configure(_ context.Context, config json.RawMessage) error {
	var cfg struct {
		Block string `json:"block"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configs = append(p.configs, string(config))
	p.block = cfg.Block
	return nil
}

func (p *tsFullPlugin) InspectRequest(_ context.Context, req *RequestInfo) (*RequestDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inspected++
	if req.TargetHost == p.block {
		return Reject(451, "blocked by test"), nil
	}
	return nil, nil // nil means continue
}

func (p *tsFullPlugin) HandleEvents(_ context.Context, events []Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, events...)
	return nil
}

func (p *tsFullPlugin) Shutdown(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shutdowns++
	return nil
}

// tsConfigOnly implements only the mandatory Plugin interface.
type tsConfigOnly struct{ configured atomic.Int32 }

func (p *tsConfigOnly) Configure(context.Context, json.RawMessage) error {
	p.configured.Add(1)
	return nil
}

// tsEventsOnly implements Plugin and EventHandler.
type tsEventsOnly struct{ tsConfigOnly }

func (p *tsEventsOnly) HandleEvents(context.Context, []Event) error { return nil }

// tsInspectOnly implements Plugin and RequestInspector.
type tsInspectOnly struct{ tsConfigOnly }

func (p *tsInspectOnly) InspectRequest(context.Context, *RequestInfo) (*RequestDecision, error) {
	return Reject(403, "no"), nil
}

// --- helpers ---

func tsLine(t *testing.T, id any, method string, params any) string {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

func tsRegister(t *testing.T, id any, config any) string {
	t.Helper()
	return tsLine(t, id, MethodRegister, map[string]any{
		"schema_version": SchemaVersion,
		"resin_version":  "1.2.3",
		"plugin_id":      "acme.demo",
		"data_dir":       "/data/acme.demo",
		"config":         config,
	})
}

// tsServe runs ServeIO over the given input lines and returns every message
// written by the plugin, in output order.
func tsServe(t *testing.T, p Plugin, lines ...string) ([]Message, error) {
	t.Helper()
	return tsServeReader(t, p, strings.NewReader(strings.Join(lines, "\n")+"\n"))
}

func tsServeReader(t *testing.T, p Plugin, r io.Reader) ([]Message, error) {
	t.Helper()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- ServeIO(context.Background(), r, &out, p) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeIO did not return")
	}
	return tsParseOutput(t, out.Bytes()), err
}

func tsParseOutput(t *testing.T, data []byte) []Message {
	t.Helper()
	var msgs []Message
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var msg Message
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			t.Fatalf("plugin wrote invalid JSON line %q: %v", sc.Text(), err)
		}
		if msg.JSONRPC != "2.0" {
			t.Fatalf("response without jsonrpc 2.0: %s", sc.Text())
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func tsByID(t *testing.T, msgs []Message) map[string]Message {
	t.Helper()
	out := make(map[string]Message, len(msgs))
	for _, m := range msgs {
		key := string(m.ID)
		if _, dup := out[key]; dup && key != "null" {
			t.Fatalf("duplicate response for id %s", key)
		}
		out[key] = m
	}
	return out
}

func tsResult(t *testing.T, msg Message, v any) {
	t.Helper()
	if msg.Error != nil {
		t.Fatalf("id %s: unexpected error %+v", msg.ID, msg.Error)
	}
	if err := json.Unmarshal(msg.Result, v); err != nil {
		t.Fatalf("id %s: decode result %s: %v", msg.ID, msg.Result, err)
	}
}

func tsWantError(t *testing.T, msgs map[string]Message, id string, code int, contains string) {
	t.Helper()
	msg, ok := msgs[id]
	if !ok {
		t.Fatalf("no response for id %s", id)
	}
	if msg.Error == nil {
		t.Fatalf("id %s: expected error %d, got result %s", id, code, msg.Result)
	}
	if msg.Error.Code != code {
		t.Fatalf("id %s: error code = %d, want %d (%s)", id, msg.Error.Code, code, msg.Error.Message)
	}
	if !strings.Contains(msg.Error.Message, contains) {
		t.Fatalf("id %s: error %q does not contain %q", id, msg.Error.Message, contains)
	}
	if len(msg.Result) > 0 {
		t.Fatalf("id %s: error response also carries a result %s", id, msg.Result)
	}
}

// --- tests ---

func TestServeIOSession(t *testing.T) {
	p := &tsFullPlugin{}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	msgs, err := tsServe(t, p,
		tsRegister(t, 1, map[string]any{"block": "bad.example"}),
		"", // blank lines are ignored
		tsLine(t, 2, MethodInspectRequest, RequestInfo{ProxyType: ProxyTypeForward, Platform: "p", Account: "a", TargetHost: "bad.example"}),
		tsLine(t, 3, MethodInspectRequest, RequestInfo{ProxyType: ProxyTypeReverse, Platform: "p", Account: "a", TargetHost: "ok.example"})+"\r", // CRLF line ending
		tsLine(t, 4, MethodEventBatch, EventBatchParams{Events: []Event{
			{Type: EventLeaseCreated, Time: now, Data: json.RawMessage(`{"account":"a"}`)},
			{Type: EventRequestFinished, Time: now, Data: json.RawMessage(`{}`)},
		}}),
		// Notification: handled, but no response.
		tsLine(t, nil, MethodEventBatch, EventBatchParams{Events: []Event{{Type: EventLeaseRemoved, Time: now, Data: json.RawMessage(`{}`)}}}),
		tsLine(t, "str-id", "plugin.bogus", nil),
		`this is {not json`,
		// A response-shaped message (no method) is ignored.
		`{"jsonrpc":"2.0","id":99,"result":{}}`,
		tsLine(t, 7, MethodShutdown, nil),
		// Nothing after shutdown is processed.
		tsLine(t, 8, MethodInspectRequest, RequestInfo{TargetHost: "bad.example"}),
		tsLine(t, 9, MethodEventBatch, EventBatchParams{Events: []Event{{Type: EventLeaseExpired, Time: now}}}),
	)
	if err != nil {
		t.Fatalf("ServeIO returned %v", err)
	}
	byID := tsByID(t, msgs)
	if len(msgs) != 7 {
		t.Fatalf("got %d responses, want 7: %+v", len(msgs), msgs)
	}

	var reg RegisterResult
	tsResult(t, byID["1"], &reg)
	if reg.SchemaVersion != SchemaVersion {
		t.Fatalf("register schema_version = %d", reg.SchemaVersion)
	}
	if reg.Capabilities == nil || !reg.Capabilities.RequestHook || len(reg.Capabilities.Events) != 1 || reg.Capabilities.Events[0] != "*" {
		t.Fatalf("register capabilities = %+v", reg.Capabilities)
	}

	var dec RequestDecision
	tsResult(t, byID["2"], &dec)
	if dec.Action != ActionReject || dec.Status != 451 || dec.Message != "blocked by test" {
		t.Fatalf("inspect bad.example = %+v, want reject 451", dec)
	}
	dec = RequestDecision{}
	tsResult(t, byID["3"], &dec)
	if dec.Action != ActionContinue {
		t.Fatalf("inspect ok.example = %+v, want continue", dec)
	}

	var empty map[string]any
	tsResult(t, byID["4"], &empty)
	if len(empty) != 0 {
		t.Fatalf("event.batch result = %v, want {}", empty)
	}

	tsWantError(t, byID, `"str-id"`, CodeMethodNotFound, "plugin.bogus")
	tsWantError(t, byID, "null", CodeParseError, "parse error")

	tsResult(t, byID["7"], &empty)
	if last := msgs[len(msgs)-1]; string(last.ID) != "7" {
		t.Fatalf("shutdown response is not the last line (last id %s)", last.ID)
	}
	for _, id := range []string{"8", "9", "99"} {
		if _, ok := byID[id]; ok {
			t.Fatalf("unexpected response for id %s", id)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.inits) != 1 || p.inits[0].PluginID != "acme.demo" || p.inits[0].ResinVersion != "1.2.3" || p.inits[0].DataDir != "/data/acme.demo" {
		t.Fatalf("Init params = %+v", p.inits)
	}
	if len(p.configs) != 1 || p.configs[0] != `{"block":"bad.example"}` {
		t.Fatalf("Configure calls = %v", p.configs)
	}
	if p.inspected != 2 {
		t.Fatalf("inspected %d requests, want 2", p.inspected)
	}
	if len(p.events) != 3 || p.events[0].Type != EventLeaseCreated || p.events[1].Type != EventRequestFinished || p.events[2].Type != EventLeaseRemoved {
		t.Fatalf("events = %+v", p.events)
	}
	if !p.events[0].Time.Equal(now) || string(p.events[0].Data) != `{"account":"a"}` {
		t.Fatalf("event payload not preserved: %+v", p.events[0])
	}
	if p.shutdowns != 1 {
		t.Fatalf("Shutdown called %d times, want 1", p.shutdowns)
	}
}

func TestServeIOConfigureHotUpdate(t *testing.T) {
	p := &tsFullPlugin{}
	msgs, err := tsServe(t, p,
		tsRegister(t, 1, map[string]any{"block": "a.example"}),
		tsLine(t, 2, MethodConfigure, ConfigureParams{Config: json.RawMessage(`{"block":"b.example"}`)}),
		// configure runs inline, so this inspect sees the new config.
		tsLine(t, 3, MethodInspectRequest, RequestInfo{TargetHost: "b.example"}),
		tsLine(t, 4, MethodConfigure, map[string]any{"config": "not an object"}),
	)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	byID := tsByID(t, msgs)
	var ok map[string]any
	tsResult(t, byID["2"], &ok)
	var dec RequestDecision
	tsResult(t, byID["3"], &dec)
	if dec.Action != ActionReject {
		t.Fatalf("inspect after configure = %+v, want reject", dec)
	}
	tsWantError(t, byID, "4", CodeInternalError, "")

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.configs) != 2 || p.block != "b.example" {
		t.Fatalf("configs = %v block = %q", p.configs, p.block)
	}
	// EOF without plugin.shutdown still runs the Shutdown hook once.
	if p.shutdowns != 1 {
		t.Fatalf("Shutdown called %d times after EOF, want 1", p.shutdowns)
	}
}

func TestServeIOReportsImplementedCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		plugin     Plugin
		wantHook   bool
		wantEvents bool
		wantAction string
	}{
		{"config only", &tsConfigOnly{}, false, false, ActionContinue},
		{"events only", &tsEventsOnly{}, false, true, ActionContinue},
		{"inspect only", &tsInspectOnly{}, true, false, ActionReject},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs, err := tsServe(t, tc.plugin,
				tsRegister(t, 1, map[string]any{}),
				tsLine(t, 2, MethodInspectRequest, RequestInfo{TargetHost: "x"}),
				tsLine(t, 3, MethodEventBatch, EventBatchParams{Events: []Event{{Type: EventLeaseCreated}}}),
				tsLine(t, 4, MethodShutdown, nil),
			)
			if err != nil {
				t.Fatalf("ServeIO: %v", err)
			}
			byID := tsByID(t, msgs)
			var reg RegisterResult
			tsResult(t, byID["1"], &reg)
			if reg.Capabilities == nil {
				t.Fatal("register result has no capabilities")
			}
			if reg.Capabilities.RequestHook != tc.wantHook {
				t.Fatalf("request_hook = %v, want %v", reg.Capabilities.RequestHook, tc.wantHook)
			}
			if gotEvents := len(reg.Capabilities.Events) > 0; gotEvents != tc.wantEvents {
				t.Fatalf("events = %v, want events %v", reg.Capabilities.Events, tc.wantEvents)
			}
			var dec RequestDecision
			tsResult(t, byID["2"], &dec)
			if dec.Action != tc.wantAction {
				t.Fatalf("inspect action = %q, want %q", dec.Action, tc.wantAction)
			}
			var empty map[string]any
			tsResult(t, byID["3"], &empty)
			tsResult(t, byID["4"], &empty)
		})
	}
}

// tsErrPlugin fails in configurable ways.
type tsErrPlugin struct {
	configured atomic.Int32
}

func (p *tsErrPlugin) Configure(_ context.Context, config json.RawMessage) error {
	var cfg struct {
		Fail string `json:"fail"`
	}
	_ = json.Unmarshal(config, &cfg)
	switch cfg.Fail {
	case "plain":
		return errors.New("bad config value")
	case "rpc":
		return &RPCError{Code: -32001, Message: "custom rpc failure"}
	}
	p.configured.Add(1)
	return nil
}

func (p *tsErrPlugin) InspectRequest(_ context.Context, req *RequestInfo) (*RequestDecision, error) {
	switch req.TargetHost {
	case "err":
		return nil, fmt.Errorf("inspect failed for %s", req.TargetHost)
	case "rpc":
		return nil, &RPCError{Code: CodeInvalidParams, Message: "rpc says no"}
	case "panic":
		panic("boom")
	}
	return Continue(), nil
}

func (p *tsErrPlugin) HandleEvents(context.Context, []Event) error {
	return errors.New("event sink down")
}

func TestServeIOErrors(t *testing.T) {
	p := &tsErrPlugin{}
	msgs, err := tsServe(t, p,
		tsLine(t, 1, MethodRegister, map[string]any{"schema_version": 2, "config": map[string]any{}}),
		tsLine(t, 2, MethodRegister, "not an object"),
		tsRegister(t, 3, map[string]any{"fail": "plain"}),
		tsRegister(t, 4, map[string]any{"fail": "rpc"}),
		tsRegister(t, 5, map[string]any{}),
		tsLine(t, 6, MethodConfigure, ConfigureParams{Config: json.RawMessage(`{"fail":"plain"}`)}),
		tsLine(t, 7, MethodInspectRequest, RequestInfo{TargetHost: "err"}),
		tsLine(t, 8, MethodInspectRequest, RequestInfo{TargetHost: "rpc"}),
		tsLine(t, 9, MethodInspectRequest, RequestInfo{TargetHost: "panic"}),
		tsLine(t, 10, MethodInspectRequest, []int{1, 2}),
		tsLine(t, 11, MethodInspectRequest, RequestInfo{TargetHost: "fine"}),
		tsLine(t, 12, MethodEventBatch, EventBatchParams{Events: []Event{{Type: EventLeaseCreated}}}),
		tsLine(t, 13, MethodEventBatch, "bad"),
		tsLine(t, 14, "", nil),
		`{"jsonrpc":"2.0","id":15`,
		tsLine(t, 16, MethodShutdown, nil),
	)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	byID := tsByID(t, msgs)
	tsWantError(t, byID, "1", CodeInvalidParams, "schema_version 2")
	tsWantError(t, byID, "2", CodeInvalidParams, "invalid params")
	tsWantError(t, byID, "3", CodeInternalError, "bad config value")
	tsWantError(t, byID, "4", -32001, "custom rpc failure")
	var reg RegisterResult
	tsResult(t, byID["5"], &reg)
	tsWantError(t, byID, "6", CodeInternalError, "bad config value")
	tsWantError(t, byID, "7", CodeInternalError, "inspect failed for err")
	tsWantError(t, byID, "8", CodeInvalidParams, "rpc says no")
	tsWantError(t, byID, "9", CodeInternalError, "plugin panic: boom")
	tsWantError(t, byID, "10", CodeInvalidParams, "invalid params")
	var dec RequestDecision
	tsResult(t, byID["11"], &dec)
	if dec.Action != ActionContinue {
		t.Fatalf("inspect after panic = %+v", dec)
	}
	tsWantError(t, byID, "12", CodeInternalError, "event sink down")
	tsWantError(t, byID, "13", CodeInvalidParams, "invalid params")
	if _, ok := byID["14"]; ok {
		t.Fatal("message without method was answered")
	}
	tsWantError(t, byID, "null", CodeParseError, "parse error")
	var empty map[string]any
	tsResult(t, byID["16"], &empty)

	// Only the successful register configured the plugin (schema check runs first).
	if got := p.configured.Load(); got != 1 {
		t.Fatalf("successful Configure calls = %d, want 1", got)
	}
}

func TestServeIONilPlugin(t *testing.T) {
	if err := ServeIO(context.Background(), strings.NewReader(""), io.Discard, nil); err == nil {
		t.Fatal("ServeIO accepted a nil plugin")
	}
}

type tsFailingReader struct {
	data []byte
	err  error
}

func (r *tsFailingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestServeIOReturnsReadError(t *testing.T) {
	p := &tsFullPlugin{}
	readErr := errors.New("stdin broke")
	r := &tsFailingReader{data: []byte(tsRegister(t, 1, map[string]any{}) + "\n"), err: readErr}
	msgs, err := tsServeReader(t, p, r)
	if !errors.Is(err, readErr) {
		t.Fatalf("ServeIO error = %v, want %v", err, readErr)
	}
	if len(msgs) != 1 || string(msgs[0].ID) != "1" {
		t.Fatalf("responses = %+v", msgs)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdowns != 1 {
		t.Fatalf("Shutdown called %d times, want 1", p.shutdowns)
	}
}

// tsBarrierPlugin blocks every inspect call until n calls are in flight, so
// the test only passes when ServeIO runs inspect calls concurrently.
type tsBarrierPlugin struct {
	tsConfigOnly
	n       int32
	entered atomic.Int32
	all     chan struct{}
	once    sync.Once
}

func (p *tsBarrierPlugin) InspectRequest(ctx context.Context, req *RequestInfo) (*RequestDecision, error) {
	if p.entered.Add(1) == p.n {
		p.once.Do(func() { close(p.all) })
	}
	select {
	case <-p.all:
		return &RequestDecision{Action: ActionContinue, Account: &req.Account}, nil
	case <-time.After(3 * time.Second):
		return nil, errors.New("inspect calls were not run concurrently")
	}
}

func TestServeIOConcurrentInspect(t *testing.T) {
	const n = 16
	p := &tsBarrierPlugin{n: n, all: make(chan struct{})}
	lines := []string{tsRegister(t, 0, map[string]any{})}
	for i := 1; i <= n; i++ {
		lines = append(lines, tsLine(t, i, MethodInspectRequest, RequestInfo{Account: fmt.Sprintf("acct-%d", i)}))
	}
	msgs, err := tsServe(t, p, lines...)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	byID := tsByID(t, msgs)
	if len(byID) != n+1 {
		t.Fatalf("got %d responses, want %d", len(byID), n+1)
	}
	for i := 1; i <= n; i++ {
		var dec RequestDecision
		tsResult(t, byID[fmt.Sprint(i)], &dec)
		if dec.Account == nil || *dec.Account != fmt.Sprintf("acct-%d", i) {
			t.Fatalf("id %d: response routed to wrong request: %+v", i, dec)
		}
	}
}

// tsOrderedEvents records batch order and detects concurrent delivery.
type tsOrderedEvents struct {
	tsConfigOnly
	busy       atomic.Bool
	overlapped atomic.Bool
	mu         sync.Mutex
	seqs       []int
}

func (p *tsOrderedEvents) HandleEvents(_ context.Context, events []Event) error {
	if !p.busy.CompareAndSwap(false, true) {
		p.overlapped.Store(true)
	}
	defer p.busy.Store(false)
	time.Sleep(time.Millisecond)
	for _, ev := range events {
		var d struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		p.mu.Lock()
		p.seqs = append(p.seqs, d.Seq)
		p.mu.Unlock()
	}
	return nil
}

func TestServeIOEventBatchesInOrder(t *testing.T) {
	p := &tsOrderedEvents{}
	lines := []string{tsRegister(t, 0, map[string]any{})}
	const batches = 30
	for i := 0; i < batches; i++ {
		lines = append(lines, tsLine(t, i+1, MethodEventBatch, EventBatchParams{Events: []Event{
			{Type: EventLeaseCreated, Data: json.RawMessage(fmt.Sprintf(`{"seq":%d}`, 2*i))},
			{Type: EventLeaseRemoved, Data: json.RawMessage(fmt.Sprintf(`{"seq":%d}`, 2*i+1))},
		}}))
	}
	msgs, err := tsServe(t, p, lines...)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	if len(msgs) != batches+1 {
		t.Fatalf("got %d responses, want %d", len(msgs), batches+1)
	}
	if p.overlapped.Load() {
		t.Fatal("event batches were delivered concurrently")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seqs) != 2*batches {
		t.Fatalf("delivered %d events, want %d", len(p.seqs), 2*batches)
	}
	for i, s := range p.seqs {
		if s != i {
			t.Fatalf("events out of order: %v", p.seqs)
		}
	}
}

// tsSlowPlugin makes inspect and event calls slow and records the order of
// completion relative to Shutdown.
type tsSlowPlugin struct {
	tsConfigOnly
	mu    sync.Mutex
	order []string
}

func (p *tsSlowPlugin) record(s string) {
	p.mu.Lock()
	p.order = append(p.order, s)
	p.mu.Unlock()
}

func (p *tsSlowPlugin) InspectRequest(context.Context, *RequestInfo) (*RequestDecision, error) {
	time.Sleep(30 * time.Millisecond)
	p.record("inspect")
	return nil, nil
}

func (p *tsSlowPlugin) HandleEvents(context.Context, []Event) error {
	time.Sleep(30 * time.Millisecond)
	p.record("events")
	return nil
}

func (p *tsSlowPlugin) Shutdown(context.Context) error {
	p.record("shutdown")
	return nil
}

func TestServeIOShutdownDrainsInFlightWork(t *testing.T) {
	p := &tsSlowPlugin{}
	msgs, err := tsServe(t, p,
		tsRegister(t, 1, map[string]any{}),
		tsLine(t, 2, MethodInspectRequest, RequestInfo{}),
		tsLine(t, 3, MethodInspectRequest, RequestInfo{}),
		tsLine(t, 4, MethodEventBatch, EventBatchParams{}),
		tsLine(t, 5, MethodShutdown, nil),
	)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d responses, want 5: %+v", len(msgs), msgs)
	}
	if string(msgs[len(msgs)-1].ID) != "5" {
		t.Fatalf("shutdown was answered before in-flight work (last id %s)", msgs[len(msgs)-1].ID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.order) != 4 || p.order[3] != "shutdown" {
		t.Fatalf("completion order = %v, want shutdown last", p.order)
	}
}

// TestServeIOStreaming drives ServeIO interactively over pipes to make sure
// each response is written as soon as it is ready (not only at EOF).
func TestServeIOStreaming(t *testing.T) {
	p := &tsFullPlugin{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := ServeIO(context.Background(), inR, outW, p)
		outW.Close()
		done <- err
	}()
	lines := make(chan Message, 8)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var msg Message
			if err := json.Unmarshal(sc.Bytes(), &msg); err == nil {
				lines <- msg
			}
		}
		close(lines)
	}()
	next := func() Message {
		t.Helper()
		select {
		case msg, ok := <-lines:
			if !ok {
				t.Fatal("output closed early")
			}
			return msg
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for a response")
		}
		return Message{}
	}
	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	send(tsRegister(t, 1, map[string]any{"block": "evil.example"}))
	if msg := next(); string(msg.ID) != "1" || msg.Error != nil {
		t.Fatalf("register response = %+v", msg)
	}
	send(tsLine(t, 2, MethodInspectRequest, RequestInfo{TargetHost: "evil.example"}))
	msg := next()
	var dec RequestDecision
	tsResult(t, msg, &dec)
	if string(msg.ID) != "2" || dec.Action != ActionReject {
		t.Fatalf("inspect response = %+v / %+v", msg, dec)
	}
	send(tsLine(t, 3, "nope", nil))
	if msg := next(); msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
		t.Fatalf("unknown method response = %+v", msg)
	}

	// Closing stdin ends the loop cleanly.
	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeIO: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeIO did not return after stdin closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdowns != 1 {
		t.Fatalf("Shutdown called %d times, want 1", p.shutdowns)
	}
}
