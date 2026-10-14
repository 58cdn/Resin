package proxy

import (
	"net"
	"os"
	"syscall"
	"testing"
)

func TestWindowsSocketResetClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
		code  string
		kind  string
	}{
		{"reset", syscall.WSAECONNRESET, "ECONNRESET", "connection_reset"},
		{"aborted", syscall.WSAECONNABORTED, "ECONNABORTED", "connection_aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, opaque := range []error{tc.errno, wrapTunnelCopyError(tc.errno)} {
				if isClientReadResetError(opaque) {
					t.Errorf("Winsock reset without an explicit read must not be exempted: %v", opaque)
				}
			}
			for _, op := range []string{"read", "write"} {
				syscallName := "wsarecv"
				if op == "write" {
					syscallName = "wsasend"
				}
				err := wrapTunnelCopyError(&net.OpError{Op: op, Err: &os.SyscallError{Syscall: syscallName, Err: tc.errno}})
				for _, input := range []error{tc.errno, err} {
					detail := summarizeUpstreamError(input)
					if detail.Errno != tc.code || detail.Kind != tc.kind {
						t.Errorf("%s: detail = %+v, want errno %s and kind %s", op, detail, tc.code, tc.kind)
					}
					if isBenignTunnelCopyError(input) {
						t.Errorf("%s: reset must not be globally benign", op)
					}
				}
				if got, want := isClientReadResetError(err), op == "read"; got != want {
					t.Errorf("%s: client read reset = %v, want %v", op, got, want)
				}
			}
		})
	}
}
