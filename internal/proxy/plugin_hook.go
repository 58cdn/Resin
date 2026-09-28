package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

var (
	// ErrPluginRejected is the default rejection returned when a plugin
	// rejects a request without choosing its own status.
	ErrPluginRejected = &ProxyError{
		HTTPCode:   http.StatusForbidden,
		ResinError: "PLUGIN_REJECTED",
		Message:    "Request rejected by plugin",
	}
	// ErrPluginUnavailable is returned when a fail-closed plugin errors or
	// times out.
	ErrPluginUnavailable = &ProxyError{
		HTTPCode:   http.StatusServiceUnavailable,
		ResinError: "PLUGIN_ERROR",
		Message:    "Request plugin unavailable",
	}
)

const socks5ReplyNotAllowed = 0x02

// RequestHook lets plugins inspect a proxy request after authentication and
// before routing. Implementations must be safe for concurrent use.
type RequestHook interface {
	// Active reports whether any request plugin is enabled. It is called on
	// every request and must be cheap.
	Active() bool
	// InspectRequest runs the plugin chain. req may be mutated.
	InspectRequest(ctx context.Context, req *pluginsdk.RequestInfo) RequestHookResult
}

// HeaderOp is one upstream header change requested by a plugin.
type HeaderOp struct {
	Name   string
	Value  string
	Remove bool
}

// RequestHookResult is the combined outcome of the request plugin chain.
type RequestHookResult struct {
	// Reject, when non-nil, aborts the request with this error.
	Reject *ProxyError
	// PluginID is the plugin that rejected the request (if any).
	PluginID string
	// Platform and Account are the (possibly rewritten) routing identity.
	Platform string
	Account  string
	// HeaderOps are applied in order to the upstream request headers.
	HeaderOps []HeaderOp
}

func hookActive(h RequestHook) bool { return h != nil && h.Active() }

// applyHookReject records a plugin rejection on the request lifecycle.
func (l *requestLifecycle) applyHookReject(pe *ProxyError) {
	l.setProxyError(pe)
	l.setHTTPStatus(pe.HTTPCode)
}

// hookHeaders snapshots request headers for plugins. Credentials used to talk
// to Resin itself are never exposed.
func hookHeaders(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "Proxy-Authorization") {
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	return out
}

// protectedHookHeaders cannot be changed by plugins because they are managed
// by the HTTP transport or would break message framing.
var protectedHookHeaders = map[string]bool{
	"Host":              true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Upgrade":           true,
	"Te":                true,
	"Trailer":           true,
}

// IsProtectedHookHeader reports whether plugins may not modify the header.
func IsProtectedHookHeader(name string) bool {
	return protectedHookHeaders[http.CanonicalHeaderKey(name)]
}

func applyHeaderOps(h http.Header, ops []HeaderOp) {
	for _, op := range ops {
		if op.Name == "" || IsProtectedHookHeader(op.Name) {
			continue
		}
		if op.Remove {
			h.Del(op.Name)
			continue
		}
		h.Set(op.Name, op.Value)
	}
}

func writePluginReject(w http.ResponseWriter, res RequestHookResult) {
	if res.PluginID != "" {
		w.Header().Set("X-Resin-Plugin", res.PluginID)
	}
	writeProxyError(w, res.Reject)
}

// ProxyTypeName returns the plugin protocol name of a proxy type.
func ProxyTypeName(t ProxyType) string {
	switch t {
	case ProxyTypeReverse:
		return pluginsdk.ProxyTypeReverse
	case ProxyTypeSocks5Forward:
		return pluginsdk.ProxyTypeSocks5
	default:
		return pluginsdk.ProxyTypeForward
	}
}
