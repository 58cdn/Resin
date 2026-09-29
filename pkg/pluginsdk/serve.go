package pluginsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Plugin is implemented by every plugin. Configure receives the initial
// config (from plugin.register) and every later hot update. When it returns
// an error the plugin must keep using its previous config. Its context
// expires after 9 seconds.
type Plugin interface {
	Configure(ctx context.Context, config json.RawMessage) error
}

// RequestInspector is implemented by plugins with the request_hook
// capability. It is called concurrently for many requests and must be fast:
// the context is cancelled when the per-plugin timeout configured by the
// admin passes, and at most 1024 calls run at a time.
type RequestInspector interface {
	InspectRequest(ctx context.Context, req *RequestInfo) (*RequestDecision, error)
}

// EventHandler is implemented by plugins that subscribe to events. Batches
// are delivered one at a time, in order. The context of each call expires
// after 9 seconds.
type EventHandler interface {
	HandleEvents(ctx context.Context, events []Event) error
}

// Initializer is optionally implemented to receive host information before
// the first Configure call.
type Initializer interface {
	Init(ctx context.Context, params RegisterParams) error
}

// Shutdowner is optionally implemented to release resources on exit. Its
// context expires after 2 seconds.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

const (
	// maxLineBytes bounds a single protocol line. Longer lines are skipped
	// and answered with an error.
	maxLineBytes = 16 << 20
	// maxConcurrentInspects bounds concurrent InspectRequest calls. Calls
	// beyond it are answered with an error.
	maxConcurrentInspects = 1024
	// eventQueueSize and controlQueueSize bound the calls waiting for the
	// event worker and the register/configure worker. When a queue is full
	// the call is answered with an error instead of blocking the reader.
	eventQueueSize   = 64
	controlQueueSize = 16

	// Deadlines of plugin callbacks. They are a little shorter than the
	// host's own timeouts, so the host receives the error. Inspect calls use
	// RequestInfo.TimeoutMs, capped at maxInspectTimeout.
	maxInspectTimeout = 60 * time.Second
	controlTimeout    = 9 * time.Second
	eventBatchTimeout = 9 * time.Second

	// When the session ends, accepted calls get drainTimeout to finish.
	// Then their contexts are cancelled and they get cancelGrace more;
	// calls still running after that are abandoned.
	drainTimeout        = time.Second
	cancelGrace         = 500 * time.Millisecond
	shutdownHookTimeout = 2 * time.Second
)

var errLineTooLong = fmt.Errorf("pluginsdk: protocol line exceeds %d bytes", maxLineBytes)

// Serve runs the plugin protocol on stdin/stdout until the host closes stdin
// or sends plugin.shutdown. Use log/fmt on stderr for diagnostics; stdout
// is reserved for the protocol.
func Serve(p Plugin) error {
	return ServeIO(context.Background(), os.Stdin, os.Stdout, p)
}

// call is one request read from the host.
type call struct {
	msg Message
	// after is closed once every register/configure call read before this
	// call has been handled.
	after <-chan struct{}
	// done is closed when a register/configure call has been handled.
	done chan struct{}
}

// ServeIO is Serve with explicit streams (useful for tests).
//
// The reader never waits for plugin code. register and configure calls run
// one at a time in order, event batches run one at a time in order, and
// inspect calls run concurrently. Calls read after a register or configure
// start once it has been handled, so they observe the new config.
func ServeIO(ctx context.Context, r io.Reader, w io.Writer, p Plugin) error {
	if p == nil {
		return errors.New("pluginsdk: nil plugin")
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()

	s := &server{
		plugin:       p,
		enc:          json.NewEncoder(w),
		inspectSlots: make(chan struct{}, maxConcurrentInspects),
	}
	control := make(chan call, controlQueueSize)
	events := make(chan call, eventQueueSize)
	var work sync.WaitGroup
	work.Add(2)
	go func() {
		defer work.Done()
		for c := range control {
			s.handle(workCtx, c)
			close(c.done)
		}
	}()
	go func() {
		defer work.Done()
		for c := range events {
			s.handle(workCtx, c)
		}
	}()

	configured := closedChan
	var (
		shutdownMsg *Message
		retErr      error
	)
	reader := bufio.NewReaderSize(r, 64<<10)
loop:
	for {
		line, err := readLine(reader)
		if errors.Is(err, errLineTooLong) {
			s.reply(nil, nil, &RPCError{Code: CodeInvalidRequest, Message: err.Error()})
			continue
		}
		if len(line) > 0 {
			var msg Message
			if uerr := json.Unmarshal(line, &msg); uerr != nil {
				s.reply(nil, nil, &RPCError{Code: CodeParseError, Message: "parse error: " + uerr.Error()})
			} else {
				c := call{msg: msg, after: configured}
				switch msg.Method {
				case "":
					// Responses to host calls are not used in this protocol version.
				case MethodRegister, MethodConfigure:
					c.done = make(chan struct{})
					select {
					case control <- c:
						configured = c.done
					default:
						s.reply(msg.ID, nil, &RPCError{Code: CodeInternalError, Message: "plugin busy: too many pending configure calls"})
					}
				case MethodInspectRequest:
					if !s.acquireInspect() {
						s.reply(msg.ID, nil, &RPCError{Code: CodeInternalError, Message: "plugin busy: too many concurrent inspect calls"})
						break
					}
					work.Add(1)
					go func() {
						defer work.Done()
						defer s.releaseInspect()
						s.handle(workCtx, c)
					}()
				case MethodEventBatch:
					select {
					case events <- c:
					default:
						s.reply(msg.ID, nil, &RPCError{Code: CodeInternalError, Message: "plugin busy: event queue full"})
					}
				case MethodShutdown:
					shutdownMsg = &msg
					break loop
				default:
					s.reply(msg.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + msg.Method})
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				retErr = err
			}
			break
		}
	}

	// Let accepted calls finish before shutting down, but not forever: the
	// host kills a plugin that does not exit shortly after shutdown.
	close(control)
	close(events)
	drained := make(chan struct{})
	go func() {
		work.Wait()
		close(drained)
	}()
	if !waitClosed(drained, drainTimeout) {
		cancelWork()
		waitClosed(drained, cancelGrace)
	}
	s.shutdown(ctx)
	if shutdownMsg != nil {
		s.reply(shutdownMsg.ID, struct{}{}, nil)
	}
	s.close()
	return retErr
}

var closedChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

func waitClosed(ch <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// readLine reads one protocol line. A line longer than maxLineBytes is
// consumed and reported as errLineTooLong; the stream stays usable.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if len(buf)+len(chunk) > maxLineBytes {
			for isPrefix && err == nil {
				_, isPrefix, err = r.ReadLine()
			}
			if err != nil {
				return nil, err
			}
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		if err != nil || !isPrefix {
			return buf, err
		}
	}
}

type server struct {
	plugin       Plugin
	inspectSlots chan struct{}

	mu     sync.Mutex
	enc    *json.Encoder
	closed bool // set after the final response; later replies are dropped
}

func (s *server) acquireInspect() bool {
	select {
	case s.inspectSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *server) releaseInspect() { <-s.inspectSlots }

func (s *server) reply(id json.RawMessage, result any, rpcErr *RPCError) {
	if len(id) == 0 {
		// Notifications do not get responses.
		if rpcErr == nil {
			return
		}
		id = json.RawMessage("null")
	}
	msg := Message{JSONRPC: "2.0", ID: id, Error: rpcErr}
	if rpcErr == nil {
		data, err := json.Marshal(result)
		if err != nil {
			msg.Error = &RPCError{Code: CodeInternalError, Message: "marshal result: " + err.Error()}
		} else {
			msg.Result = data
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_ = s.enc.Encode(msg)
}

func (s *server) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *server) handle(ctx context.Context, c call) {
	defer func() {
		if r := recover(); r != nil {
			s.reply(c.msg.ID, nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("plugin panic: %v", r)})
		}
	}()
	result, err := s.dispatch(ctx, c)
	if err != nil {
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			rpcErr = &RPCError{Code: CodeInternalError, Message: err.Error()}
		}
		s.reply(c.msg.ID, nil, rpcErr)
		return
	}
	s.reply(c.msg.ID, result, nil)
}

func (s *server) dispatch(ctx context.Context, c call) (any, error) {
	msg := c.msg
	switch msg.Method {
	case MethodRegister:
		ctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		var params RegisterParams
		if err := decodeParams(msg.Params, &params); err != nil {
			return nil, err
		}
		if params.SchemaVersion != SchemaVersion {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unsupported schema_version %d", params.SchemaVersion)}
		}
		if init, ok := s.plugin.(Initializer); ok {
			if err := init.Init(ctx, params); err != nil {
				return nil, err
			}
		}
		if err := s.plugin.Configure(ctx, params.Config); err != nil {
			return nil, err
		}
		caps := &Capabilities{}
		if _, ok := s.plugin.(RequestInspector); ok {
			caps.RequestHook = true
		}
		if _, ok := s.plugin.(EventHandler); ok {
			caps.Events = []string{"*"}
		}
		return RegisterResult{SchemaVersion: SchemaVersion, Capabilities: caps}, nil

	case MethodConfigure:
		ctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		var params ConfigureParams
		if err := decodeParams(msg.Params, &params); err != nil {
			return nil, err
		}
		if err := s.plugin.Configure(ctx, params.Config); err != nil {
			return nil, err
		}
		return struct{}{}, nil

	case MethodInspectRequest:
		inspector, ok := s.plugin.(RequestInspector)
		if !ok {
			return Continue(), nil
		}
		var req RequestInfo
		if err := decodeParams(msg.Params, &req); err != nil {
			return nil, err
		}
		timeout := maxInspectTimeout
		if req.TimeoutMs > 0 && req.TimeoutMs < maxInspectTimeout.Milliseconds() {
			timeout = time.Duration(req.TimeoutMs) * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := waitConfigured(ctx, c.after); err != nil {
			return nil, err
		}
		decision, err := inspector.InspectRequest(ctx, &req)
		if err != nil {
			return nil, err
		}
		if decision == nil {
			decision = Continue()
		}
		return decision, nil

	case MethodEventBatch:
		handler, ok := s.plugin.(EventHandler)
		if !ok {
			return struct{}{}, nil
		}
		var params EventBatchParams
		if err := decodeParams(msg.Params, &params); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, eventBatchTimeout)
		defer cancel()
		if err := waitConfigured(ctx, c.after); err != nil {
			return nil, err
		}
		if err := handler.HandleEvents(ctx, params.Events); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + msg.Method}
}

// waitConfigured waits until the register/configure calls read before the
// current call have been handled.
func waitConfigured(ctx context.Context, after <-chan struct{}) error {
	select {
	case <-after:
		return nil
	default:
	}
	select {
	case <-after:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown runs the Shutdowner hook once, waiting at most
// shutdownHookTimeout for it.
func (s *server) shutdown(ctx context.Context) {
	sd, ok := s.plugin.(Shutdowner)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownHookTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		_ = sd.Shutdown(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return nil
}
