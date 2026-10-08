package ipcheck

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestStrictConfig(t *testing.T) {
	for _, raw := range []string{"", "{}"} {
		cfg, err := ParseConfig(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BackgroundEnabled || len(cfg.EnabledSources) != 2 || cfg.RequestTimeoutMS != 10000 {
			t.Fatalf("unexpected defaults: %+v", cfg)
		}
	}
	cases := []string{
		`{"unknown":1}`, `{"enabled_sources":null}`, `{"request_timeout_ms":100,"request_timeout_ms":200}`, `{"consistency_targets":["https://example.com/ip?site=a","https://example.com/ip?site=b"]}`, `{"background_enabled":true}`, `{"ip_result_ttl":"0s"}`, `{"node_result_ttl":"721h"}`,
		`{"request_timeout_ms":99}`, `{"request_timeout_ms":60001}`, `{"max_concurrent_checks":0}`, `{"max_cache_entries":0}`,
		`{"enabled_sources":["secret-key"]}`, `{"enabled_sources":["db-ip","db-ip"]}`, `{"source_priority":["missing"]}`,
		`{"consistency_targets":[]}`, `{"consistency_targets":["https://a.example.com","https://a.example.com"]}`,
		`{"ip_result_ttl":100}`, `{} {}`, `[]`, `null`, `{"enabled_sources":true}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseConfig(json.RawMessage(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		})
	}
	cfg := DefaultConfig()
	cfg.ConsistencyTargets = make([]string, MaxTargets+1)
	if cfg.Validate() == nil {
		t.Fatal("too many targets accepted")
	}
}

func TestTargetSSRFPolicy(t *testing.T) {
	blocked := []string{
		"http://cloudflare.com/", "https://localhost/", "https://a.localhost/", "https://metadata.google.internal/", "https://router.local/",
		"https://127.0.0.1/", "https://127.1/", "https://2130706433/", "https://0x7f000001/", "https://10.0.0.1/", "https://169.254.169.254/latest/meta-data/",
		"https://100.64.0.1/", "https://192.168.1.1/", "https://[::1]/", "https://[fe80::1]/", "https://[::ffff:127.0.0.1]/",
		"https://[2001:db8::1]/", "https://[64:ff9b::a00:1]/", "https://203.0.113.1/", "https://224.0.0.1/",
		"https://user:password@example.com/", "https://example.com/#fragment", "https://example.com:0/", "https://example.com:65536/", "https://-bad.example/",
		"https://a..example/", "https://exam ple.com/", "https:///example.com", "file:///etc/passwd", "https://example.com/" + strings.Repeat("a", MaxTargetURLLength),
	}
	for _, raw := range blocked {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateTarget(raw); err == nil {
				t.Fatalf("accepted unsafe target %s", raw)
			}
		})
	}
	for _, raw := range []string{"https://cloudflare.com/cdn-cgi/trace", "https://1.1.1.1/", "https://[2606:4700:4700::1111]/", "https://example.com.:443/trace?format=json"} {
		if err := ValidateTarget(raw); err != nil {
			t.Fatalf("rejected %s: %v", raw, err)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for _, raw := range []string{"", "127.0.0.1", "10.1.2.3", "100.64.0.1", "169.254.1.1", "198.18.0.1", "192.0.2.1", "203.0.113.1", "::", "::1", "fc00::1", "fe80::1%eth0", "2001:db8::1", "2002:7f00:1::", "3fff::1"} {
		ip, _ := netip.ParseAddr(raw)
		if IsPublicIP(ip) {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8", "2606:4700:4700::1111"} {
		if !IsPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected %s", raw)
		}
	}
}
