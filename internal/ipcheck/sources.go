package ipcheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
)

const geoIPAttribution = "Offline GeoIP provider; DB-IP data, when configured, is licensed under CC BY 4.0 (https://db-ip.com/db/download/ip-to-country-lite)"

var sourceCatalog = map[string]SourceStatus{
	"egress-consistency":   {Name: "egress-consistency", Kind: SourceKindViaNode, Available: true},
	"db-ip":                {Name: "db-ip", Kind: SourceKindByIP, Available: true, Attribution: geoIPAttribution},
	"ippure":               {Name: "ippure", Kind: SourceKindViaNode, Reason: "unavailable: stable API and usage terms are not verified"},
	"cloudflare-challenge": {Name: "cloudflare-challenge", Kind: SourceKindViaNode, Reason: cloudflareConfigurationReason},
	"ai-region":            {Name: "ai-region", Kind: SourceKindViaNode, Reason: aiRegionConfigurationReason},
	"scamalytics":          {Name: "scamalytics", Kind: SourceKindByIP, RequiresKey: true, Reason: "unavailable: keyed provider is not implemented in P1"},
	"ipdata":               {Name: "ipdata", Kind: SourceKindByIP, RequiresKey: true, Reason: "unavailable: keyed provider is not implemented in P1"},
	"cloudflare-radar":     {Name: "cloudflare-radar", Kind: SourceKindByIP, RequiresKey: true, Reason: "unavailable: keyed provider is not implemented in P1"},
}

type geoIPSource struct{ host Host }

func (*geoIPSource) Name() string      { return "db-ip" }
func (*geoIPSource) Kind() SourceKind  { return SourceKindByIP }
func (*geoIPSource) RequiresKey() bool { return false }
func (s *geoIPSource) Check(ctx context.Context, req CheckRequest) (*SourceResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, ok := s.host.GeoIPLookup(ctx, req.EgressIP)
	if !ok {
		return &SourceResult{Status: "unavailable", Error: "offline GeoIP database is unavailable", Data: Data{Attribution: geoIPAttribution}}, nil
	}
	return &SourceResult{OK: true, Status: "ok", Data: Data{Country: info.Country, City: info.City, ASN: info.ASN, ASOrg: info.ASOrg, Attribution: geoIPAttribution}}, nil
}

type consistencySource struct{ targets []string }

func (*consistencySource) Name() string      { return "egress-consistency" }
func (*consistencySource) Kind() SourceKind  { return SourceKindViaNode }
func (*consistencySource) RequiresKey() bool { return false }
func (s *consistencySource) Check(ctx context.Context, req CheckRequest) (*SourceResult, error) {
	result := &SourceResult{OK: true, Status: "ok", Data: Data{EgressIPsBySite: map[string]string{}, TargetErrors: map[string]string{}}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, target := range s.targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			observed, err := echoIP(ctx, req.HTTP, target)
			u, _ := url.Parse(target)
			u.RawQuery = ""
			u.ForceQuery = false
			label := u.String()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Data.TargetErrors[label] = publicSourceError(err)
				return
			}
			result.Data.EgressIPsBySite[label] = observed.String()
		}(target)
	}
	wg.Wait()
	if len(result.Data.TargetErrors) > 0 {
		result.OK = false
		result.Status = "error"
		result.Error = "one or more egress targets failed"
	}
	// One successful observation cannot prove cross-site consistency. Partial
	// success can disprove consistency, but cannot prove it for missing targets.
	var first string
	var differs bool
	for _, ip := range result.Data.EgressIPsBySite {
		if first == "" {
			first = ip
		} else if first != ip {
			differs = true
		}
	}
	if differs {
		value := false
		result.Data.EgressConsistent = &value
	} else if len(result.Data.TargetErrors) == 0 && len(result.Data.EgressIPsBySite) >= 2 {
		value := true
		result.Data.EgressConsistent = &value
	}
	return result, nil
}

var (
	errBadEcho          = errors.New("invalid IP echo response")
	errBadStatus        = errors.New("unexpected HTTP status")
	errResponseTooLarge = errors.New("response body exceeds size limit")
)

func echoIP(ctx context.Context, client *http.Client, target string) (netip.Addr, error) {
	if client == nil {
		return netip.Addr{}, errors.New("HTTP unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("Accept", "text/plain, application/json")
	response, err := client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	if response.Body == nil {
		return netip.Addr{}, errBadEcho
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return netip.Addr{}, errBadStatus
	}
	if response.ContentLength > MaxResponseBytes {
		return netip.Addr{}, errResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return netip.Addr{}, err
	}
	if int64(len(body)) > MaxResponseBytes {
		return netip.Addr{}, errResponseTooLarge
	}
	return parseEchoIP(body)
}

func parseEchoIP(body []byte) (netip.Addr, error) {
	text := strings.TrimSpace(string(body))
	candidate := text
	if strings.HasPrefix(text, "{") {
		var value struct {
			IP string `json:"ip"`
		}
		if err := decodeObservationObject(body, &value); err != nil {
			return netip.Addr{}, errBadEcho
		}
		candidate = value.IP
	} else if strings.Contains(text, "\n") || strings.HasPrefix(text, "ip=") {
		candidate = traceValue(body, "ip")
	}
	ip, err := netip.ParseAddr(candidate)
	if err != nil || !IsPublicIP(ip) {
		return netip.Addr{}, errBadEcho
	}
	return ip.Unmap(), nil
}

func publicSourceError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, errBadEcho):
		return errBadEcho.Error()
	case errors.Is(err, errBadStatus):
		return errBadStatus.Error()
	case errors.Is(err, errResponseTooLarge):
		return errResponseTooLarge.Error()
	default:
		return "source request failed"
	}
}

// viaNodeTransport contains no independent direct-network fallback. The host
// owns outbound acquisition, DNS validation, and dialing through this node.
type viaNodeTransport struct {
	host   Host
	hash   CheckRequest
	guards *requestguard.Controller
	source requestguard.Source
	report *attemptReport
}

func (t viaNodeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := ValidateTarget(req.URL.String()); err != nil {
		return nil, err
	}
	if t.guards == nil {
		return t.host.DoViaNode(req.Context(), t.hash.NodeHash, req)
	}
	permit, err := t.guards.Admit(req.Context(), t.source)
	if t.report != nil {
		t.report.record(err)
	}
	if err != nil {
		return nil, err
	}
	defer permit.Finish(requestguard.Neutral)
	response, err := t.host.DoViaNode(req.Context(), t.hash.NodeHash, req)
	permit.Finish(guardOutcome(req.Context(), response, err))
	return response, err
}
