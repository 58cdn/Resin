package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin/builtin"
)

func requireHostKind(t *testing.T, err error, kind ipcheck.HostErrorKind) {
	t.Helper()
	var hostErr *ipcheck.HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != kind {
		t.Fatalf("host failure = %v, want %s", err, kind)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("host error exposed private details: %v", err)
	}
}

func TestIPQualityHostUsesStableProcessController(t *testing.T) {
	controller := requestguard.New(func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) })
	app := &resinApp{ipCheckGuards: controller}
	capabilities := builtinCapabilities{app: app}
	first, ok := capabilities.Capability(builtin.IPQualityHostCapability)
	if !ok {
		t.Fatal("missing builtin host")
	}
	second, _ := capabilities.Capability(builtin.IPQualityHostCapability)
	for _, host := range []any{first, second, ipQualityHost{app}} {
		if host.(ipcheck.RequestGuardHost).RequestGuards() != controller {
			t.Fatal("capability lookup replaced process controller")
		}
	}
	if (ipQualityHost{}).RequestGuards() != nil || (ipQualityHost{&resinApp{}}).RequestGuards() != nil {
		t.Fatal("capability lookup allocated a hidden controller")
	}
}

func TestIPQualityHostLocalStagesHaveFiniteFailures(t *testing.T) {
	host := ipQualityHost{}
	for _, request := range []*http.Request{nil, {Method: http.MethodGet}, {Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: "example.com"}}} {
		_, err := host.DoViaNode(context.Background(), node.Hash{}, request)
		requireHostKind(t, err, ipcheck.HostInvalidTarget)
	}
	unsafe, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1/secret", nil)
	_, err := host.DoViaNode(context.Background(), node.Hash{}, unsafe)
	requireHostKind(t, err, ipcheck.HostInvalidTarget)
	valid, _ := http.NewRequest(http.MethodGet, "https://example.com/secret", nil)
	_, err = host.DoViaNode(context.Background(), node.Hash{}, valid)
	requireHostKind(t, err, ipcheck.HostNodeUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := host.DoViaNode(ctx, node.Hash{}, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("early cancellation lost: %v", err)
	}

	for _, tc := range []struct {
		name string
		ips  []netip.Addr
		err  error
		kind ipcheck.HostErrorKind
	}{
		{"DNS unavailable", nil, errors.New("secret resolver at 127.0.0.1"), ipcheck.HostDNSUnavailable},
		{"empty DNS", nil, nil, ipcheck.HostDNSUnavailable},
		{"DNS timeout", nil, context.DeadlineExceeded, ipcheck.HostDNSUnavailable},
		{"mixed DNS", []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil, ipcheck.HostInvalidTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePublicTarget(context.Background(), "example.com:443", func(context.Context, string, string) ([]netip.Addr, error) { return tc.ips, tc.err })
			requireHostKind(t, err, tc.kind)
			// net/http can wrap the stage error with a private URL; normalize
			// must retain only its finite class, not that enclosing URL.
			wrapped := &url.Error{Op: "Get", URL: "https://example.com/?secret=1", Err: err}
			requireHostKind(t, normalizeIPQualityHostError(context.Background(), wrapped), tc.kind)
		})
	}
}

func TestIPQualityHostCancellationAndTransportClassification(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
		} else {
			cancel()
		}
		defer cancel()
		want := ctx.Err()
		if err := normalizeIPQualityHostError(ctx, errors.New("secret transport")); !errors.Is(err, want) {
			t.Fatalf("caller cancellation/deadline lost: %v", err)
		}
		called := false
		_, err := resolvePublicTarget(ctx, "example.com:443", func(context.Context, string, string) ([]netip.Addr, error) { called = true; return nil, nil })
		if called || !errors.Is(err, want) {
			t.Fatalf("canceled request reached DNS: %v %v", called, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := resolvePublicTarget(ctx, "example.com:443", func(context.Context, string, string) ([]netip.Addr, error) {
		cancel()
		return nil, errors.New("secret DNS failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during lookup lost: %v", err)
	}
	wrapped := fmt.Errorf("secret: %w", context.Canceled)
	if !errors.Is(normalizeIPQualityHostError(context.Background(), wrapped), context.Canceled) {
		t.Fatal("wrapped cancellation counted as transport failure")
	}
	for _, err := range []error{errors.New("secret TLS/HTTP failure"), context.DeadlineExceeded} {
		requireHostKind(t, normalizeIPQualityHostError(context.Background(), err), ipcheck.HostTransportFailure)
	}
}
