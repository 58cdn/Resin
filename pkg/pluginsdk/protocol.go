// Package pluginsdk defines the Resin plugin protocol and a small helper for
// writing external plugins in Go.
//
// An external plugin is a child process started by Resin. Resin talks to it
// with JSON-RPC 2.0 messages, one JSON object per line, over the plugin's
// stdin (host -> plugin) and stdout (plugin -> host). Anything the plugin
// writes to stderr is forwarded to the Resin log.
//
// This package only depends on the Go standard library so that plugin
// authors can import it without pulling in Resin internals.
package pluginsdk

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the plugin protocol / manifest schema version implemented
// by this package.
const SchemaVersion = 1

// JSON-RPC method names used by the host.
const (
	// MethodRegister is the first call after the process starts. Params are
	// RegisterParams, result is RegisterResult. The plugin must apply the
	// initial config before replying.
	MethodRegister = "plugin.register"
	// MethodConfigure hot-applies a new config. Params are ConfigureParams.
	// When it returns an error the plugin must keep its previous config.
	MethodConfigure = "plugin.configure"
	// MethodInspectRequest asks the plugin to inspect one proxy request.
	// Params are RequestInfo, result is RequestDecision.
	MethodInspectRequest = "request.inspect"
	// MethodEventBatch delivers a batch of events. Params are EventBatchParams.
	MethodEventBatch = "event.batch"
	// MethodShutdown asks the plugin to exit gracefully.
	MethodShutdown = "plugin.shutdown"
)

// Event types a plugin can subscribe to in its manifest capabilities.
// Subscriptions may also use "*" (everything) or a "prefix.*" wildcard such
// as "lease.*".
const (
	EventRequestFinished = "request.finished"
	EventLeaseCreated    = "lease.created"
	EventLeaseReplaced   = "lease.replaced"
	EventLeaseRemoved    = "lease.removed"
	EventLeaseExpired    = "lease.expired"
)

// KnownEventTypes lists every event type emitted by this protocol version.
var KnownEventTypes = []string{
	EventRequestFinished,
	EventLeaseCreated,
	EventLeaseReplaced,
	EventLeaseRemoved,
	EventLeaseExpired,
}

// Proxy types reported in RequestInfo.ProxyType / RequestFinishedData.ProxyType.
const (
	ProxyTypeForward = "forward"
	ProxyTypeReverse = "reverse"
	ProxyTypeSocks5  = "socks5"
)

// Decision actions.
const (
	ActionContinue = "continue"
	ActionReject   = "reject"
)

// Capabilities declares what a plugin participates in.
type Capabilities struct {
	// RequestHook enables request.inspect calls on the proxy hot path.
	RequestHook bool `json:"request_hook,omitempty"`
	// Events lists subscribed event types ("*" and "prefix.*" allowed).
	Events []string `json:"events,omitempty"`
}

// RegisterParams is sent with plugin.register.
type RegisterParams struct {
	SchemaVersion int             `json:"schema_version"`
	ResinVersion  string          `json:"resin_version"`
	PluginID      string          `json:"plugin_id"`
	DataDir       string          `json:"data_dir,omitempty"`
	Config        json.RawMessage `json:"config"`
}

// RegisterResult is returned from plugin.register.
type RegisterResult struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name,omitempty"`
	Version       string `json:"version,omitempty"`
	// Capabilities, when present, narrows the manifest capabilities for this
	// process (for example a plugin that only implements events). The host
	// never grants more than the manifest declares.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// ConfigureParams is sent with plugin.configure.
type ConfigureParams struct {
	Config json.RawMessage `json:"config"`
}

// RequestInfo describes one proxy request before it is routed.
//
// Platform and Account reflect changes made by higher-priority plugins.
// Headers are present for HTTP requests (reverse proxy and HTTP forward proxy,
// including CONNECT) and absent for SOCKS5; Proxy-Authorization is never
// included. Header changes only take effect for reverse proxy and plain HTTP
// forward proxy requests.
type RequestInfo struct {
	ProxyType  string              `json:"proxy_type"`
	IsConnect  bool                `json:"is_connect,omitempty"`
	ClientIP   string              `json:"client_ip,omitempty"`
	Platform   string              `json:"platform"`
	Account    string              `json:"account"`
	TargetHost string              `json:"target_host"`
	Method     string              `json:"method,omitempty"`
	URL        string              `json:"url,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
}

// RequestDecision is the result of request.inspect. The zero value means
// "continue without changes".
type RequestDecision struct {
	// Action is "continue" (default) or "reject".
	Action string `json:"action,omitempty"`
	// Status is the HTTP status for a reject (default 403, must be 400-599).
	// SOCKS5 clients receive a "connection not allowed" reply instead.
	Status int `json:"status,omitempty"`
	// Message is the plain-text body returned to the client on reject.
	Message string `json:"message,omitempty"`

	// Platform / Account override the routing identity when non-nil.
	Platform *string `json:"platform,omitempty"`
	Account  *string `json:"account,omitempty"`

	// SetHeaders / RemoveHeaders rewrite upstream request headers. Removals
	// are applied before sets. Ignored for CONNECT and SOCKS5.
	SetHeaders    map[string]string `json:"set_headers,omitempty"`
	RemoveHeaders []string          `json:"remove_headers,omitempty"`
}

// Continue returns a decision that lets the request through unchanged.
func Continue() *RequestDecision { return &RequestDecision{Action: ActionContinue} }

// Reject returns a decision that rejects the request.
func Reject(status int, message string) *RequestDecision {
	return &RequestDecision{Action: ActionReject, Status: status, Message: message}
}

// Event is one asynchronous notification delivered with event.batch.
type Event struct {
	Type string          `json:"type"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// EventBatchParams is sent with event.batch.
type EventBatchParams struct {
	Events []Event `json:"events"`
}

// RequestFinishedData is the payload of request.finished events. Request and
// response bodies/headers are never included.
type RequestFinishedData struct {
	StartedAt      time.Time `json:"started_at"`
	ProxyType      string    `json:"proxy_type"`
	ClientIP       string    `json:"client_ip,omitempty"`
	PlatformID     string    `json:"platform_id,omitempty"`
	PlatformName   string    `json:"platform_name,omitempty"`
	Account        string    `json:"account,omitempty"`
	TargetHost     string    `json:"target_host,omitempty"`
	TargetURL      string    `json:"target_url,omitempty"`
	NodeHash       string    `json:"node_hash,omitempty"`
	NodeTag        string    `json:"node_tag,omitempty"`
	EgressIP       string    `json:"egress_ip,omitempty"`
	DurationNs     int64     `json:"duration_ns"`
	FirstByteNs    int64     `json:"first_byte_ns,omitempty"`
	NetOK          bool      `json:"net_ok"`
	HTTPMethod     string    `json:"http_method,omitempty"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	ResinError     string    `json:"resin_error,omitempty"`
	UpstreamStage  string    `json:"upstream_stage,omitempty"`
	UpstreamErrMsg string    `json:"upstream_err_msg,omitempty"`
	IngressBytes   int64     `json:"ingress_bytes"`
	EgressBytes    int64     `json:"egress_bytes"`
}

// LeaseEventData is the payload of lease.* events.
type LeaseEventData struct {
	PlatformID   string     `json:"platform_id"`
	PlatformName string     `json:"platform_name,omitempty"`
	Account      string     `json:"account"`
	NodeHash     string     `json:"node_hash,omitempty"`
	EgressIP     string     `json:"egress_ip,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
}

// --- JSON-RPC 2.0 wire types ---

// Message is a JSON-RPC 2.0 request, notification or response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Standard JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)
