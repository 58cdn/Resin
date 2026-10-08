package ipcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEchoFixtures(t *testing.T) {
	for _, body := range []string{"8.8.8.8\n", "fl=123\nip=8.8.8.8\nts=123\n", `{"ip":"8.8.8.8","other":true}`} {
		ip, err := parseEchoIP([]byte(body))
		if err != nil || ip.String() != "8.8.8.8" {
			t.Fatalf("%q: %v %v", body, ip, err)
		}
	}
	for _, body := range []string{"<html>Just a moment...</html>", `{"address":"8.8.8.8"}`, `{"ip":8}`, `{"ip":"8.8.8.8"} trailing`, "ip=8.8.8.8\nip=1.1.1.1", "127.0.0.1", "garbage", ""} {
		if _, err := parseEchoIP([]byte(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestConsistencyFixtures(t *testing.T) {
	for _, test := range []struct {
		name           string
		second         string
		status         int
		wantOK         bool
		wantConsistent *bool
		wantSites      int
	}{
		{"agree", "8.8.8.8", 200, true, boolptr(true), 2},
		{"disagree", "1.1.1.1", 200, true, boolptr(false), 2},
		{"partial", "error", 503, false, nil, 1},
		{"malformed", "<html>challenge-platform</html>", 200, false, nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/two" {
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.second)
					return
				}
				fmt.Fprint(w, "ip=8.8.8.8\nfl=123\n")
			}))
			defer server.Close()
			source := &consistencySource{targets: []string{server.URL + "/one", server.URL + "/two"}}
			result, err := source.Check(context.Background(), CheckRequest{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if result.OK != test.wantOK || len(result.Data.EgressIPsBySite) != test.wantSites {
				t.Fatalf("unexpected %+v", result)
			}
			if test.wantConsistent == nil {
				if result.Data.EgressConsistent != nil {
					t.Fatal("unknown became known")
				}
			} else if result.Data.EgressConsistent == nil || *result.Data.EgressConsistent != *test.wantConsistent {
				t.Fatal("wrong consistency")
			}
		})
	}
}

func TestConsistencyUnknownTimeoutAndBodyCap(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			<-r.Context().Done()
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", int(MaxResponseBytes)+1))
		default:
			fmt.Fprint(w, "8.8.8.8")
		}
	}))
	defer server.Close()
	for _, path := range []string{"/one", "/slow", "/large"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			result, err := (&consistencySource{targets: []string{server.URL + path}}).Check(ctx, CheckRequest{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if result.Data.EgressConsistent != nil {
				t.Fatal("single response must remain unknown")
			}
			if path != "/one" && (result.OK || result.Error == "") {
				t.Fatal("failed response reported success")
			}
		})
	}
}

func TestChallengeAndRegionFixtures(t *testing.T) {
	tests := []struct {
		status     int
		header     http.Header
		body, want string
	}{
		{200, http.Header{"Cf-Mitigated": []string{"challenge"}}, "", "challenge"},
		{403, nil, "Just a moment...", "challenge"}, {503, nil, "challenge-platform", "challenge"},
		{403, nil, "cf-turnstile", "challenge"}, {403, nil, "Forbidden", "blocked"}, {200, nil, "OK", "none"}, {503, nil, "Unavailable", "unknown"},
	}
	for _, test := range tests {
		if got := ClassifyCloudflareChallenge(test.status, test.header, []byte(test.body)); got != test.want {
			t.Errorf("got %s want %s", got, test.want)
		}
	}
	for _, test := range []struct {
		service       string
		status        int
		header        http.Header
		body, country string
		countries     []string
		want          string
	}{
		{"chatgpt", 403, nil, `{"error":{"code":"unsupported_country_region_territory"}}`, "", nil, "region_blocked"},
		{"claude", 200, nil, "not available in your country", "", nil, "region_blocked"},
		{"claude", 200, nil, "hello", "US", []string{"US", "CA"}, "unknown"},
		{"claude", 200, nil, "hello", "IR", []string{"US", "CA"}, "region_blocked"},
		{"claude", 200, nil, "hello", "XX", []string{"US", "CA"}, "unknown"},
		{"chatgpt", 403, nil, "challenge-platform", "", nil, "risky"},
		{"chatgpt", 429, nil, "rate limit", "", nil, "risky"},
		{"chatgpt", 200, nil, "Welcome to ChatGPT", "", nil, "unknown"},
		{"chatgpt", 200, http.Header{"Content-Type": []string{"application/json"}}, `{"service":"chatgpt","available":true}`, "", nil, "available"},
		{"chatgpt", 200, http.Header{"Content-Type": []string{"application/json"}}, `{"service":"claude","available":true}`, "", nil, "unknown"},
		{"other", 200, nil, "", "", nil, "unknown"},
	} {
		if got := ClassifyAIRegion(test.service, test.status, test.header, []byte(test.body), test.country, test.countries); got != test.want {
			t.Errorf("%+v: got %s want %s", test, got, test.want)
		}
	}
}

func boolptr(value bool) *bool { return &value }
