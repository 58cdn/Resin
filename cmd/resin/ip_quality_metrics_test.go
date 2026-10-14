package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/Resinat/Resin/internal/metrics"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

type ipQualityMetricsOutbound struct {
	adapter.Outbound
	dial func(context.Context, string, M.Socksaddr) (net.Conn, error)
}

func (o *ipQualityMetricsOutbound) DialContext(ctx context.Context, network string, addr M.Socksaddr) (net.Conn, error) {
	return o.dial(ctx, network, addr)
}

func ipQualityPublicLookup(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

func TestIPQualityDialMetricsCountOnlyActualOutboundCalls(t *testing.T) {
	mgr := metrics.NewManager(metrics.ManagerConfig{})
	called := false
	ob := &ipQualityMetricsOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		called = true
		return nil, errors.New("unexpected outbound call")
	}}
	for _, lookup := range []func(context.Context, string, string) ([]netip.Addr, error){
		func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("DNS failed") },
		func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
		},
	} {
		transport := newIPQualityTransport(ob, lookup, mgr.BeginIPCheckDial)
		if _, err := transport.DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
			t.Fatal("unsafe/unavailable DNS accepted")
		}
	}
	if called || mgr.SnapshotIPCheckDials().DialsStarted != 0 {
		t.Fatal("pre-dial DNS rejection counted as an outbound dial")
	}
}

func TestIPQualityDialMetricsObservePendingAndReturnedError(t *testing.T) {
	for _, tc := range []struct {
		name             string
		err              error
		canceled, failed uint64
	}{
		{name: "failure", err: errors.New("synthetic failure"), failed: 1},
		{name: "wrapped cancellation", err: fmt.Errorf("dial: %w", context.Canceled), canceled: 1},
		{name: "deadline", err: context.DeadlineExceeded, failed: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := metrics.NewManager(metrics.ManagerConfig{})
			ob := &ipQualityMetricsOutbound{dial: func(_ context.Context, network string, addr M.Socksaddr) (net.Conn, error) {
				if network != "tcp" || addr.String() != "8.8.8.8:443" {
					t.Fatalf("unpinned call: %s %v", network, addr)
				}
				s := mgr.SnapshotIPCheckDials()
				if s.DialsStarted != 1 || s.DialsInFlight != 1 || s.DialsCompleted != 0 {
					t.Fatalf("pending dial not observable: %+v", s)
				}
				return nil, tc.err
			}}
			transport := newIPQualityTransport(ob, ipQualityPublicLookup, mgr.BeginIPCheckDial)
			_, err := transport.DialContext(context.Background(), "tcp", "example.com:443")
			if err != tc.err {
				t.Fatalf("dial error changed: %v", err)
			}
			s := mgr.SnapshotIPCheckDials()
			if s.DialsStarted != 1 || s.DialsCompleted != 1 || s.DialsInFlight != 0 || s.DialsSucceeded != 0 || s.DialsCanceled != tc.canceled || s.DialsFailed != tc.failed {
				t.Fatalf("wrong returned-error accounting: %+v", s)
			}
		})
	}
}

func TestIPQualityDialSuccessDoesNotMeanHTTPSuccess(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	mgr := metrics.NewManager(metrics.ManagerConfig{})
	ob := &ipQualityTestOutbound{target: server.Listener.Addr().String()}
	transport := newIPQualityTransport(ob, ipQualityPublicLookup, mgr.BeginIPCheckDial)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	defer transport.CloseIdleConnections()
	response, err := newIPQualityClient(transport).Get("https://example.com/observation")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("HTTP status changed: %d", response.StatusCode)
	}
	s := mgr.SnapshotIPCheckDials()
	if s.DialsStarted != 1 || s.DialsSucceeded != 1 || s.DialsFailed != 0 || s.DialsInFlight != 0 {
		t.Fatalf("HTTP status affected dial result: %+v", s)
	}
}
