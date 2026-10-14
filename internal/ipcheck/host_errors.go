package ipcheck

// HostErrorKind identifies a host-controlled stage, never a node's IP quality.
// DNS lookup and safety failures are local, not evidence of provider failure.
type HostErrorKind string

const (
	HostInvalidTarget    HostErrorKind = "invalid_target"
	HostNodeUnavailable  HostErrorKind = "node_unavailable"
	HostDNSUnavailable   HostErrorKind = "dns_unavailable"
	HostTransportFailure HostErrorKind = "transport_failure"
)

// HostError deliberately retains no URL, address or underlying private error.
// Caller cancellation is returned separately as context.Canceled/DeadlineExceeded.
type HostError struct {
	Kind HostErrorKind
}

func (e *HostError) Error() string {
	switch e.Kind {
	case HostInvalidTarget:
		return "invalid IP check target"
	case HostNodeUnavailable:
		return "node unavailable"
	case HostDNSUnavailable:
		return "target DNS unavailable"
	case HostTransportFailure:
		return "IP check transport failed"
	default:
		return "IP check host failed"
	}
}
