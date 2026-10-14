package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/plugin/builtin"
	"github.com/Resinat/Resin/internal/state"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// builtinCapabilities is never exposed to external plugin processes.
type builtinCapabilities struct {
	app    *resinApp
	engine *state.StateEngine
}

func (h builtinCapabilities) Capability(name string) (any, bool) {
	switch name {
	case builtin.IPQualityHostCapability:
		return ipQualityHost{h.app}, true
	case builtin.IPQualityStoreCapability:
		return h.engine.IPCheckStore(), true
	default:
		return nil, false
	}
}

type ipQualityHost struct{ app *resinApp }

func (h ipQualityHost) RequestGuards() *requestguard.Controller {
	if h.app == nil {
		return nil
	}
	return h.app.ipCheckGuards
}

func (h ipQualityHost) NodeSnapshot(_ context.Context, hash node.Hash) (ipcheck.NodeSnapshot, bool) {
	if h.app.topoRuntime == nil || h.app.topoRuntime.pool == nil {
		return ipcheck.NodeSnapshot{}, false
	}
	pool := h.app.topoRuntime.pool
	entry, ok := pool.GetEntry(hash)
	if !ok {
		return ipcheck.NodeSnapshot{}, false
	}
	return ipcheck.NodeSnapshot{Hash: hash, EgressIP: entry.GetEgressIP(), EgressVersion: entry.LastEgressUpdate.Load(), Enabled: !pool.IsNodeDisabled(hash), Healthy: entry.IsHealthy(), Subscriptions: entry.SubscriptionIDs()}, true
}
func (h ipQualityHost) GeoIPLookup(_ context.Context, ip netip.Addr) (ipcheck.GeoIPInfo, bool) {
	if h.app.geoSvc == nil {
		return ipcheck.GeoIPInfo{}, false
	}
	info, ok := h.app.geoSvc.LookupDetails(ip)
	return ipcheck.GeoIPInfo{Country: info.Country, City: info.City, ASN: info.ASN, ASOrg: info.ASOrg}, ok
}

// DoViaNode accepts only GET public HTTPS, disables redirects, and pins DNS to
// a validated public address before handing it to the outbound. TLS still uses
// the original hostname. Remote DNS cannot rebind a validated hostname to LAN.
// Only dedicated IP-check dial counters are touched; health, latency, routing,
// traffic and the existing egress/latency probe metrics are unchanged.
func (h ipQualityHost) DoViaNode(ctx context.Context, hash node.Hash, req *http.Request) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || req.URL == nil || req.Method != http.MethodGet {
		return nil, &ipcheck.HostError{Kind: ipcheck.HostInvalidTarget}
	}
	if err := ipcheck.ValidateTarget(req.URL.String()); err != nil {
		return nil, &ipcheck.HostError{Kind: ipcheck.HostInvalidTarget}
	}
	if h.app == nil || h.app.topoRuntime == nil || h.app.topoRuntime.pool == nil {
		return nil, &ipcheck.HostError{Kind: ipcheck.HostNodeUnavailable}
	}
	entry, ok := h.app.topoRuntime.pool.GetEntry(hash)
	if !ok {
		return nil, &ipcheck.HostError{Kind: ipcheck.HostNodeUnavailable}
	}
	ob := entry.Outbound.Load()
	if ob == nil {
		return nil, &ipcheck.HostError{Kind: ipcheck.HostNodeUnavailable}
	}
	var beginDial func() func(error)
	if h.app.metricsManager != nil {
		beginDial = h.app.metricsManager.BeginIPCheckDial
	}
	transport := newIPQualityTransport(*ob, net.DefaultResolver.LookupNetIP, beginDial)
	client := newIPQualityClient(transport)
	clean, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL.String(), nil)
	if err != nil {
		return nil, &ipcheck.HostError{Kind: ipcheck.HostInvalidTarget}
	}
	clean.Header.Set("User-Agent", "Resin-IPQuality/1.0")
	clean.Header.Set("Accept", "text/plain, application/json, text/html")
	response, err := client.Do(clean)
	if err != nil {
		return response, normalizeIPQualityHostError(ctx, err)
	}
	return response, nil
}

// This boundary is after validated request construction. Remaining untyped
// client errors are HTTP/TLS/outbound transport failures; local DNS/safety
// errors preserve their finite kind through net/http's URL wrapping.
func normalizeIPQualityHostError(ctx context.Context, err error) error {
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var local *ipcheck.HostError
	if errors.As(err, &local) {
		return &ipcheck.HostError{Kind: local.Kind}
	}
	return &ipcheck.HostError{Kind: ipcheck.HostTransportFailure}
}

// resolvePublicTarget is separated for deterministic DNS-rebinding tests.
func resolvePublicTarget(ctx context.Context, addr string, lookup func(context.Context, string, string) ([]netip.Addr, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", &ipcheck.HostError{Kind: ipcheck.HostInvalidTarget}
	}
	ips, err := lookup(ctx, "ip", host)
	if canceled := ctx.Err(); canceled != nil {
		return "", canceled
	}
	if err != nil || len(ips) == 0 {
		if errors.Is(err, context.Canceled) {
			return "", context.Canceled
		}
		return "", &ipcheck.HostError{Kind: ipcheck.HostDNSUnavailable}
	}
	for _, ip := range ips {
		if !ipcheck.IsPublicIP(ip) {
			return "", &ipcheck.HostError{Kind: ipcheck.HostInvalidTarget}
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

func newIPQualityTransport(ob adapter.Outbound, lookup func(context.Context, string, string) ([]netip.Addr, error), beginDial func() func(error)) *http.Transport {
	return &http.Transport{
		Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			pinned, err := resolvePublicTarget(ctx, addr, lookup)
			if err != nil {
				return nil, err
			}
			// Count actual outbound calls, after DNS safety checks. A successful
			// dial says nothing about TLS, HTTP status, or observation quality.
			var finish func(error)
			if beginDial != nil {
				finish = beginDial()
			}
			conn, err := ob.DialContext(ctx, network, M.ParseSocksaddr(pinned))
			if finish != nil {
				finish(err)
			}
			return conn, err
		},
	}
}

func newIPQualityClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
