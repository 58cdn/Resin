package proxy

import "syscall"

func normalizeSocketErrno(errno syscall.Errno) syscall.Errno {
	// Windows sockets return Winsock codes, not Go's POSIX-style constants.
	switch errno {
	case syscall.WSAECONNRESET:
		return syscall.ECONNRESET
	case syscall.WSAECONNABORTED:
		return syscall.ECONNABORTED
	default:
		return errno
	}
}
