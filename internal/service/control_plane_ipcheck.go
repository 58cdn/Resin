package service

import (
	"context"
	"errors"
	"net/netip"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
)

// ipQualityEngine resolves the current builtin service on every operation.
// Keeping an engine on ControlPlaneService would bypass plugin enable/disable
// and configuration replacement. Manager errors are deliberately not exposed:
// they may contain runtime details rather than public API information.
func (s *ControlPlaneService) ipQualityEngine() (*ipcheck.Engine, error) {
	if s == nil || s.Plugins == nil {
		return nil, ipQualityUnavailable()
	}
	service, err := s.Plugins.BuiltinService("resin.ip-quality")
	if err != nil {
		return nil, ipQualityUnavailable()
	}
	engine, ok := service.(*ipcheck.Engine)
	if !ok || engine == nil {
		return nil, ipQualityUnavailable()
	}
	return engine, nil
}

func ipQualityUnavailable() *ServiceError {
	return &ServiceError{Code: "SERVICE_UNAVAILABLE", Message: "IP quality plugin is disabled or unavailable"}
}

// CheckNodeIP runs explicitly requested checks without changing node health.
func (s *ControlPlaneService) CheckNodeIP(ctx context.Context, hash string, sources []string, force bool) (*ipcheck.Result, error) {
	h, err := node.ParseHex(hash)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	result, err := engine.CheckNode(ctx, h, sources, force)
	return result, ipQualityServiceError(err)
}

// GetNodeIPCheck reads only cached results. It never starts an external check.
func (s *ControlPlaneService) GetNodeIPCheck(ctx context.Context, hash string) (*ipcheck.Result, error) {
	h, err := node.ParseHex(hash)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	result, err := engine.GetNode(ctx, h)
	return result, ipQualityServiceError(err)
}

func parseIPCheckAddress(value string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(value)
	if err != nil || !ipcheck.IsPublicIP(ip) {
		return netip.Addr{}, invalidArg("ip: must be a valid public IP address")
	}
	return ip.Unmap(), nil
}

// LookupIPCheck checks an arbitrary public IP using by-IP sources only.
// The engine validates the complete source selection before running any source.
func (s *ControlPlaneService) LookupIPCheck(ctx context.Context, value string, sources []string, force bool) (*ipcheck.Result, error) {
	ip, err := parseIPCheckAddress(value)
	if err != nil {
		return nil, err
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	result, err := engine.Lookup(ctx, ip, sources, force)
	return result, ipQualityServiceError(err)
}

// GetIPCheck reads cached by-IP results without triggering network traffic.
func (s *ControlPlaneService) GetIPCheck(ctx context.Context, value string) (*ipcheck.Result, error) {
	ip, err := parseIPCheckAddress(value)
	if err != nil {
		return nil, err
	}
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	result, err := engine.GetIP(ctx, ip)
	return result, ipQualityServiceError(err)
}

// ListIPCheckSources returns the public availability descriptors, never host
// capabilities, plugin config, environment variables, or persistence paths.
func (s *ControlPlaneService) ListIPCheckSources() ([]ipcheck.SourceStatus, error) {
	engine, err := s.ipQualityEngine()
	if err != nil {
		return nil, err
	}
	return engine.Sources(), nil
}

func ipQualityServiceError(err error) error {
	if err == nil {
		return nil
	}
	var validation *ipcheck.ValidationError
	if errors.As(err, &validation) {
		return invalidArg(validation.Error())
	}
	switch {
	case errors.Is(err, ipcheck.ErrNodeNotFound):
		return notFound("node not found")
	case errors.Is(err, ipcheck.ErrNotChecked):
		return notFound("IP quality not checked: no unexpired results")
	case errors.Is(err, ipcheck.ErrNoEgressIP):
		return conflict("node has no public egress IP; probe its egress IP first")
	case errors.Is(err, ipcheck.ErrNodeChanged):
		return conflict("node egress IP changed during check; retry")
	case errors.Is(err, ipcheck.ErrClosed):
		return ipQualityUnavailable()
	case errors.Is(err, ipcheck.ErrBusy):
		return &ServiceError{Code: "SERVICE_UNAVAILABLE", Message: "too many IP quality checks in progress; retry later"}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &ServiceError{Code: "SERVICE_UNAVAILABLE", Message: "IP quality check was canceled or timed out"}
	default:
		return internal("IP quality check failed", err)
	}
}
