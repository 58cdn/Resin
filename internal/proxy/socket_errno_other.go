//go:build !windows

package proxy

import "syscall"

func normalizeSocketErrno(errno syscall.Errno) syscall.Errno {
	return errno
}
