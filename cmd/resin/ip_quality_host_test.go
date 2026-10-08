package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestResolvePublicTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		ips  []string
		want string
		fail bool
	}{
		{"public", []string{"8.8.8.8"}, "8.8.8.8:443", false},
		{"public v6", []string{"2606:4700:4700::1111"}, "[2606:4700:4700::1111]:443", false},
		{"mixed DNS", []string{"8.8.8.8", "127.0.0.1"}, "", true},
		{"private", []string{"192.168.1.1"}, "", true},
		{"mapped loopback", []string{"::ffff:127.0.0.1"}, "", true},
		{"link local", []string{"169.254.169.254"}, "", true},
		{"empty", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(_ context.Context, network, host string) ([]netip.Addr, error) {
				if network != "ip" || host != "example.com" {
					t.Fatalf("unexpected lookup %s %s", network, host)
				}
				var ips []netip.Addr
				for _, s := range tc.ips {
					ips = append(ips, netip.MustParseAddr(s))
				}
				return ips, nil
			}
			got, err := resolvePublicTarget(context.Background(), "example.com:443", lookup)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got %s %v", got, err)
			}
		})
	}
	_, err := resolvePublicTarget(context.Background(), "example.com:443", func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("secret /internal/path")
	})
	if err == nil || err.Error() != "target DNS unavailable" {
		t.Fatalf("error leaked: %v", err)
	}
}

type ipQualityTestOutbound struct {
	adapter.Outbound
	target string
	got    []string
}

func (o *ipQualityTestOutbound) DialContext(ctx context.Context, network string, target M.Socksaddr) (net.Conn, error) {
	o.got = append(o.got, target.String())
	return (&net.Dialer{}).DialContext(ctx, network, o.target)
}
func TestIPQualityTransportUsesPinnedNodeOutbound(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" {
			t.Errorf("lost original host: %s", r.Host)
		}
		w.Write([]byte("ip=8.8.8.8\n"))
	}))
	defer server.Close()
	ob := &ipQualityTestOutbound{target: server.Listener.Addr().String()}
	transport := newIPQualityTransport(ob, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com/cdn-cgi/trace", nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if len(ob.got) != 1 || ob.got[0] != "8.8.8.8:443" {
		t.Fatalf("outbound dial=%v", ob.got)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
}

type ipQualityRoundTrip func(*http.Request) (*http.Response, error)

func (f ipQualityRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func TestIPQualityClientRejectsRedirects(t *testing.T) {
	calls := 0
	client := newIPQualityClient(ipQualityRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://169.254.169.254/metadata"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}))
	response, err := client.Get("https://example.com/trace")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != 1 || response.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed: %d %d", calls, response.StatusCode)
	}
}
