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
)

// Plugin is implemented by every plugin. Configure receives the initial
// config (from plugin.register) and every later hot update. When it returns
// an error the plugin must keep using its previous config.
type Plugin interface {
	Configure(ctx context.Context, config json.RawMessage) error
}

// RequestInspector is implemented by plugins with the request_hook
// capability. It is called concurrently for many requests and must be fast:
// the host applies the per-plugin timeout configured by the admin.
type RequestInspector interface {
	InspectRequest(ctx context.Context, req *RequestInfo) (*RequestDecision, error)
}

// EventHandler is implemented by plugins that subscribe to events. Batches
// are delivered one at a time, in order.
type EventHandler interface {
	HandleEvents(ctx context.Context, events []Event) error
}

// Initializer is optionally implemented to receive host information before
// the first Configure call.
type Initializer interface {
	Init(ctx context.Context, params RegisterParams) error
}

// Shutdowner is optionally implemented to release resources on exit.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// maxLineBytes bounds a single protocol line.
const maxLineBytes = 16 << 20

// Serve runs the plugin protocol on stdin/stdout until the host closes stdin
// or sends plugin.shutdown. Use log/fmt on stderr for diagnostics; stdout
// is reserved for the protocol.
func Serve(p Plugin) error {
	return ServeIO(context.Background(), os.Stdin, os.Stdout, p)
}

// ServeIO is Serve with explicit streams (useful for tests).
func ServeIO(ctx context.Context, r io.Reader, w io.Writer, p Plugin) error {
	if p == nil {
		return errors.New("pluginsdk: nil plugin")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &server{plugin: p, enc: json.NewEncoder(w)}
	events := make(chan Message, 64)
	var eventsWG sync.WaitGroup
	eventsWG.Add(1)
	go func() {
		defer eventsWG.Done()
		for msg := range events {
			s.handle(ctx, msg)
		}
	}()

	var (
		shutdownMsg *Message
		retErr      error
	)
	reader := bufio.NewReaderSize(r, 64<<10)
loop:
	for {
		line, err := readLine(reader)
		if len(line) > 0 {
			var msg Message
			if uerr := json.Unmarshal(line, &msg); uerr != nil {
				s.reply(nil, nil, &RPCError{Code: CodeParseError, Message: "parse error: " + uerr.Error()})
			} else {
				switch msg.Method {
				case "":
					// Responses to host calls are not used in this protocol version.
				case MethodInspectRequest:
					s.inflight.Add(1)
					go func() {
						defer s.inflight.Done()
						s.handle(ctx, msg)
					}()
				case MethodEventBatch:
					events <- msg
				case MethodShutdown:
					shutdownMsg = &msg
					break loop
				default:
					// register/configure run inline so they are ordered with
					// respect to subsequent requests.
					s.handle(ctx, msg)
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

	// Drain queued work before shutting down so no accepted call is lost.
	close(events)
	eventsWG.Wait()
	s.inflight.Wait()
	if shutdownMsg != nil {
		s.handle(ctx, *shutdownMsg)
	} else {
		s.shutdown(ctx)
	}
	return retErr
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		buf = append(buf, chunk...)
		if len(buf) > maxLineBytes {
			return nil, fmt.Errorf("pluginsdk: protocol line exceeds %d bytes", maxLineBytes)
		}
		if err != nil || !isPrefix {
			return buf, err
		}
	}
}

type server struct {
	plugin   Plugin
	mu       sync.Mutex
	enc      *json.Encoder
	inflight sync.WaitGroup
	shutOnce sync.Once
}

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
	_ = s.enc.Encode(msg)
}

func (s *server) handle(ctx context.Context, msg Message) {
	defer func() {
		if r := recover(); r != nil {
			s.reply(msg.ID, nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("plugin panic: %v", r)})
		}
	}()
	result, err := s.dispatch(ctx, msg)
	if err != nil {
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			rpcErr = &RPCError{Code: CodeInternalError, Message: err.Error()}
		}
		s.reply(msg.ID, nil, rpcErr)
		return
	}
	s.reply(msg.ID, result, nil)
}

func (s *server) dispatch(ctx context.Context, msg Message) (any, error) {
	switch msg.Method {
	case MethodRegister:
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
		if err := handler.HandleEvents(ctx, params.Events); err != nil {
			return nil, err
		}
		return struct{}{}, nil

	case MethodShutdown:
		s.shutdown(ctx)
		return struct{}{}, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + msg.Method}
}

func (s *server) shutdown(ctx context.Context) {
	s.shutOnce.Do(func() {
		if sd, ok := s.plugin.(Shutdowner); ok {
			_ = sd.Shutdown(ctx)
		}
	})
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
