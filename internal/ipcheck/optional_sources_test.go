package ipcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/node"
)

func TestOptionalSourceConfig(t *testing.T) {
	valid := `{"enabled_sources":["cloudflare-challenge","ai-region"],"cloudflare_challenge_urls":["https://challenge.example.com/"],"ai_region_targets":{"claude":{"url":"https://region.example.com/claude","supported_countries":["US","CA"]},"chatgpt":{"url":"https://region.example.com/chatgpt"}}}`
	cfg, err := ParseConfig(json.RawMessage(valid))
	if err != nil || len(cfg.CloudflareChallengeURLs) != 1 || len(cfg.AIRegionTargets) != 2 {
		t.Fatalf("valid optional configuration: %+v, %v", cfg, err)
	}
	for _, raw := range []string{
		`{"cloudflare_challenge_urls":["http://example.com/"]}`,
		`{"cloudflare_challenge_urls":["https://localhost/"]}`,
		`{"cloudflare_challenge_urls":["https://example.com/a?x=1","https://example.com/a?x=2"]}`,
		`{"ai_region_targets":{"other":{"url":"https://example.com/"}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://127.0.0.1/"}}}`,
		`{"ai_region_targets":{"claude":{}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","unknown":true}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","url":"https://other.example.com/"}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/"},"claude":{"url":"https://other.example.com/"}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","supported_countries":null}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","supported_countries":[]}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","supported_countries":["US","US"]}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","supported_countries":["us"]}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","supported_countries":["USA"]}}}`,
		`{"ai_region_targets":{"claude":{"url":"https://example.com/","supported_countries":["XX"]}}}`,
	} {
		if _, err := ParseConfig(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid optional config %s", raw)
		}
	}
	cfg = DefaultConfig()
	cfg.CloudflareChallengeURLs = make([]string, MaxTargets+1)
	if cfg.Validate() == nil {
		t.Fatal("accepted too many challenge targets")
	}
	if len(DefaultConfig().CloudflareChallengeURLs) != 0 || len(DefaultConfig().AIRegionTargets) != 0 {
		t.Fatal("optional endpoints must have empty defaults")
	}
}

func TestCloudflareOptionalHTTPFixtures(t *testing.T) {
	for _, test := range []struct {
		name, body, mitigation, want string
		status                       int
		ok                           bool
	}{
		{"header", "", "challenge", "challenge", 200, true},
		{"forbidden-challenge", "Just a moment...", "", "challenge", 403, true},
		{"unavailable-challenge", "challenge-platform", "", "challenge", 503, true},
		{"blocked", "Forbidden", "", "blocked", 403, true},
		{"clear", "ordinary page", "", "none", 200, true},
		{"ordinary-challenge-words", "Documentation about challenge-platform and Just a moment", "", "none", 200, true},
		{"unavailable", "Unavailable", "", "unknown", 503, false},
		{"redirect", "", "", "unknown", 302, false},
		{"oversized", strings.Repeat("x", int(MaxResponseBytes)+1), "", "unknown", 200, false},
		{"oversized-header", strings.Repeat("x", int(MaxResponseBytes)+1), "challenge", "challenge", 403, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("unexpected method or credentials")
				}
				w.Header().Set("Cf-Mitigated", test.mitigation)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			got, err := (&cloudflareChallengeSource{targets: []string{server.URL}}).Check(context.Background(), CheckRequest{HTTP: server.Client()})
			if err != nil || got.OK != test.ok || got.Data.CFChallenge != test.want {
				t.Fatalf("got %+v, %v; want %s OK=%t", got, err, test.want, test.ok)
			}
		})
	}
}

type optionalRoundTripFunc func(*http.Request) (*http.Response, error)

func (f optionalRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Deterministic body truncation after recognizable text must never become a
// successful classification. No wall-clock sleep or public network is needed.
type partialTimeoutBody struct{ closed bool }

func (*partialTimeoutBody) Read(p []byte) (int, error) {
	return copy(p, "not available in your region; challenge-platform"), context.DeadlineExceeded
}
func (b *partialTimeoutBody) Close() error { b.closed = true; return nil }

func TestOptionalPartialTimeoutAndFailureIsolation(t *testing.T) {
	for _, first := range []string{"ordinary", "challenge", "blocked"} {
		t.Run(first, func(t *testing.T) {
			body := &partialTimeoutBody{}
			client := &http.Client{Transport: optionalRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/slow" {
					return &http.Response{StatusCode: 503, Header: make(http.Header), Body: body, ContentLength: -1}, nil
				}
				response := echoResponse("ordinary page")
				if first == "challenge" {
					response.Header.Set("Cf-Mitigated", "challenge")
				}
				if first == "blocked" {
					response.StatusCode = 403
				}
				return response, nil
			})}
			result, err := (&cloudflareChallengeSource{targets: []string{"https://one.example.com/", "https://two.example.com/slow?secret=redacted"}}).Check(context.Background(), CheckRequest{HTTP: client})
			want := first
			if first == "ordinary" {
				want = "unknown"
			}
			if err != nil || result.OK || result.Data.CFChallenge != want || !body.closed || result.Data.TargetErrors["https://two.example.com/slow"] != "request timed out" {
				t.Fatalf("partial evidence failed: %+v, %v, body closed=%t", result, err, body.closed)
			}
		})
	}
}

func TestAIRegionOptionalHTTPFixtures(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType, want string
		status                        int
		countries                     []string
		ok                            bool
	}{
		{"supported-trace", "ip=8.8.8.8\nloc=US\n", "text/plain", "unknown", 200, []string{"US", "CA"}, true},
		{"unsupported-trace", "ip=8.8.8.8\nloc=IR\n", "text/plain", "region_blocked", 200, []string{"US", "CA"}, true},
		{"no-country-policy", "loc=IR\n", "text/plain", "unknown", 200, nil, true},
		{"missing-country", "ip=8.8.8.8\n", "text/plain", "unknown", 200, []string{"US"}, true},
		{"unknown-country", "loc=XX\n", "text/plain", "unknown", 200, []string{"US"}, true},
		{"malformed-country", "loc=not-known\n", "text/plain", "unknown", 200, []string{"US"}, true},
		{"duplicate-country", "loc=US\nloc=IR\n", "text/plain", "unknown", 200, []string{"US"}, true},
		{"html-country", "<html>\nloc=IR\n</html>", "text/html", "unknown", 200, []string{"US"}, true},
		{"error-country", "loc=IR\n", "text/plain", "unknown", 503, []string{"US"}, false},
		{"region-code", `{"error":{"code":"unsupported_country_region_territory"}}`, "application/json", "region_blocked", 403, nil, true},
		{"region-message", "not available in your country", "text/html", "region_blocked", 403, nil, true},
		{"generic-success", "Welcome to ChatGPT", "text/html", "unknown", 200, nil, true},
		{"configured-contract", `{"service":"chatgpt","available":true}`, "application/json", "available", 200, nil, true},
		{"json-parameters", `{"service":"chatgpt","available":true}`, "application/json; charset=utf-8", "available", 200, nil, true},
		{"json-prefix", `{"service":"chatgpt","available":true}`, "application/jsonp", "unknown", 200, nil, true},
		{"malformed-media-type", `{"service":"chatgpt","available":true}`, "application/json; charset", "unknown", 200, nil, true},
		{"duplicate-availability", `{"service":"chatgpt","available":false,"available":true}`, "application/json", "unknown", 200, nil, true},
		{"case-aliased-availability", `{"service":"chatgpt","available":false,"AVAILABLE":true}`, "application/json", "unknown", 200, nil, true},
		{"duplicate-service", `{"service":"claude","service":"chatgpt","available":true}`, "application/json", "unknown", 200, nil, true},
		{"unicode-aliased-service", `{"service":"claude","\u017fervice":"chatgpt","available":true}`, "application/json", "unknown", 200, nil, true},
		{"wrong-contract", `{"service":"claude","available":true}`, "application/json", "unknown", 200, nil, true},
		{"rate-limit", "Too many requests", "text/plain", "risky", 429, nil, true},
		{"challenge", "challenge-platform", "text/html", "risky", 503, nil, true},
		{"oversized-block", "not available in your region" + strings.Repeat("x", int(MaxResponseBytes)), "text/html", "unknown", 403, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			source := &aiRegionSource{targets: map[string]AIRegionTargetConfig{"chatgpt": {URL: server.URL, SupportedCountries: test.countries}}}
			got, err := source.Check(context.Background(), CheckRequest{HTTP: server.Client()})
			if err != nil || got.OK != test.ok || got.Data.Services["chatgpt"] != test.want || got.Data.Services["claude"] != "unknown" {
				t.Fatalf("got %+v, %v; want %s OK=%t", got, err, test.want, test.ok)
			}
		})
	}
}

func TestOptionalAdaptersExplicitOptInAndSourceStatus(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("configured=%t/enabled=%t", configured, enabled), func(t *testing.T) {
				h := newFixtureHost()
				cfg := DefaultConfig()
				if configured {
					cfg.CloudflareChallengeURLs = []string{"https://challenge.example.com/"}
					cfg.AIRegionTargets = map[string]AIRegionTargetConfig{"chatgpt": {URL: "https://region.example.com/"}}
				}
				if enabled {
					cfg.EnabledSources = []string{"cloudflare-challenge", "ai-region"}
				}
				e := newTestEngine(t, h, nil, cfg)
				if h.viaCalls.Load() != 0 {
					t.Fatal("constructing engine must not request endpoints")
				}
				for _, status := range e.Sources() {
					if status.Name == "cloudflare-challenge" || status.Name == "ai-region" {
						if status.Available != configured || status.Enabled != (configured && enabled) || (!configured && !strings.Contains(status.Reason, "configure")) {
							t.Fatalf("incorrect optional source status %+v", status)
						}
					}
				}
				result, err := e.CheckNode(context.Background(), testHash(1), []string{"cloudflare-challenge", "ai-region"}, true)
				if err != nil {
					t.Fatal(err)
				}
				wantCalls := int32(0)
				if configured && enabled {
					wantCalls = 2
				} else {
					for _, source := range result.Sources {
						if source.OK || source.Status != "unavailable" {
							t.Fatalf("unconfigured or disabled source ran: %+v", source)
						}
					}
				}
				if h.viaCalls.Load() != wantCalls {
					t.Fatalf("got %d endpoint calls; want %d", h.viaCalls.Load(), wantCalls)
				}
			})
		}
	}
}

func TestAIRegionPartialEvidenceAndConfigIsolation(t *testing.T) {
	h := newFixtureHost()
	body := &partialTimeoutBody{}
	h.via = func(ctx context.Context, hash node.Hash, r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/claude" {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: -1}, nil
		}
		if r.URL.Path != "/chatgpt" || hash != testHash(1) || r.Method != http.MethodGet {
			return nil, errors.New("wrong controlled request")
		}
		response := echoResponse("ip=8.8.8.8\nloc=IR\n")
		response.Header.Set("Content-Type", "text/plain")
		return response, nil
	}
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"ai-region"}
	cfg.AIRegionTargets = map[string]AIRegionTargetConfig{
		"claude":  {URL: "https://region.example.com/claude"},
		"chatgpt": {URL: "https://region.example.com/chatgpt", SupportedCountries: []string{"US"}},
	}
	e := newTestEngine(t, h, nil, cfg)
	target := cfg.AIRegionTargets["chatgpt"]
	target.SupportedCountries[0] = "IR"
	cfg.AIRegionTargets["chatgpt"] = AIRegionTargetConfig{URL: "https://wrong.example.com/"}
	result, err := e.CheckNode(context.Background(), testHash(1), nil, false)
	if err != nil || len(result.Sources) != 1 || result.Sources[0].OK || result.Summary.Services["chatgpt"] != "region_blocked" || result.Summary.Services["claude"] != "unknown" || !body.closed {
		t.Fatalf("lost valid partial evidence or aliased configuration: %+v, %v", result, err)
	}
	if result.Sources[0].Data.TargetErrors["claude"] != "request timed out" {
		t.Fatalf("failed service detail missing: %+v", result.Sources[0])
	}
}

func TestOptionalResponseContentLengthCapClosesBody(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader("not read")}
	client := &http.Client{Transport: optionalRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: MaxResponseBytes + 1}, nil
	})}
	_, err := observeOptionalTarget(context.Background(), client, "https://example.com/")
	if !errors.Is(err, errResponseTooLarge) || !body.closed || body.read {
		t.Fatalf("content length cap did not short-circuit safely: %v closed=%t read=%t", err, body.closed, body.read)
	}
}

type closeTrackingBody struct {
	io.Reader
	closed, read bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) { b.read = true; return b.Reader.Read(p) }
func (b *closeTrackingBody) Close() error               { b.closed = true; return nil }
