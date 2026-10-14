package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestSummarizeUpstreamError_Canceled(t *testing.T) {
	detail := summarizeUpstreamError(context.Canceled)
	if detail.Kind != "canceled" {
		t.Fatalf("kind: got %q, want %q", detail.Kind, "canceled")
	}
	if detail.Message == "" {
		t.Fatal("expected non-empty message")
	}
}

func TestSummarizeUpstreamError_DNS(t *testing.T) {
	err := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host"}}
	detail := summarizeUpstreamError(err)
	if detail.Kind != "dns_error" {
		t.Fatalf("kind: got %q, want %q", detail.Kind, "dns_error")
	}
}

func TestSummarizeUpstreamError_Errno(t *testing.T) {
	err := &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}
	detail := summarizeUpstreamError(err)
	if detail.Kind != "connection_refused" {
		t.Fatalf("kind: got %q, want %q", detail.Kind, "connection_refused")
	}
	if detail.Errno != "ECONNREFUSED" {
		t.Fatalf("errno: got %q, want %q", detail.Errno, "ECONNREFUSED")
	}
}

func TestSanitizeUpstreamErrMsg_TruncatesAndNormalizes(t *testing.T) {
	raw := strings.Repeat("x", maxUpstreamErrMsgLen+20) + "\n\n"
	got := sanitizeUpstreamErrMsg(raw)
	if len(got) != maxUpstreamErrMsgLen {
		t.Fatalf("len: got %d, want %d", len(got), maxUpstreamErrMsgLen)
	}
	if strings.Contains(got, "\n") {
		t.Fatal("expected normalized single-line message")
	}
}

func TestIsBenignTunnelCopyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: true},
		{name: "eof", err: io.EOF, want: true},
		{name: "net-closed", err: net.ErrClosed, want: true},
		{name: "context-canceled", err: context.Canceled, want: true},
		{name: "closed-network-connection", err: errors.New("write tcp: use of closed network connection"), want: true},
		{name: "generic", err: errors.New("connection reset by peer"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isBenignTunnelCopyError(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsClientReadResetError(t *testing.T) {
	readReset := &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "recv", Err: syscall.ECONNRESET}}
	writeReset := &net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "send", Err: syscall.ECONNRESET}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "client-read", err: readReset, want: true},
		{name: "client-read-aborted", err: &net.OpError{Op: "read", Err: syscall.ECONNABORTED}, want: true},
		{name: "nested-client-read", err: wrapTunnelCopyError(readReset), want: true},
		{name: "upstream-write", err: writeReset},
		{name: "nested-upstream-write", err: wrapTunnelCopyError(writeReset)},
		{name: "write-text-reset", err: &net.OpError{Op: "write", Err: errors.New("connection reset by peer")}},
		{name: "read-text-reset", err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, want: true},
		{name: "read-other-error", err: &net.OpError{Op: "read", Err: syscall.ECONNREFUSED}},
		// Linux splice does not expose whether its source read or destination write
		// failed. Preserve the existing fallback for this opaque copy error.
		{name: "opaque-splice-reset", err: wrapTunnelCopyError(&os.SyscallError{Syscall: "splice", Err: syscall.ECONNRESET}), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientReadResetError(tc.err); got != tc.want {
				t.Fatalf("isClientReadResetError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TCPConn.WriteTo and ReadFrom can wrap the actual socket operation twice.
func wrapTunnelCopyError(err error) error {
	return &net.OpError{Op: "writeto", Err: &net.OpError{Op: "readfrom", Err: err}}
}
