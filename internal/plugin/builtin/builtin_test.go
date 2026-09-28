package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// tpFindBuiltin returns the builtin with the given id from All().
func tpFindBuiltin(t *testing.T, id string) plugin.Builtin {
	t.Helper()
	for _, b := range All() {
		if b.Manifest.ID == id {
			return b
		}
	}
	t.Fatalf("builtin %q not found in All()", id)
	return plugin.Builtin{}
}

// tpConfigure creates a fresh instance of the builtin and configures it.
func tpConfigure(t *testing.T, id, config string) pluginsdk.Plugin {
	t.Helper()
	p := tpFindBuiltin(t, id).New()
	if err := p.Configure(context.Background(), json.RawMessage(config)); err != nil {
		t.Fatalf("Configure(%s, %s): %v", id, config, err)
	}
	return p
}

// tpConfigureErr configures a fresh instance and returns the error.
func tpConfigureErr(t *testing.T, id, config string) error {
	t.Helper()
	p := tpFindBuiltin(t, id).New()
	return p.Configure(context.Background(), json.RawMessage(config))
}

// tpInspect runs InspectRequest on p, failing the test on error.
func tpInspect(t *testing.T, p pluginsdk.Plugin, req pluginsdk.RequestInfo) *pluginsdk.RequestDecision {
	t.Helper()
	ins, ok := p.(pluginsdk.RequestInspector)
	if !ok {
		t.Fatalf("%T does not implement RequestInspector", p)
	}
	dec, err := ins.InspectRequest(context.Background(), &req)
	if err != nil {
		t.Fatalf("InspectRequest: %v", err)
	}
	return dec
}

func tpWithURL(t *testing.T, cfg json.RawMessage, url string) json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &obj); err != nil {
		t.Fatalf("unmarshal default config: %v", err)
	}
	u, _ := json.Marshal(url)
	obj["url"] = u
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return out
}

func TestBuiltin_AllManifestsValid(t *testing.T) {
	all := All()
	wantIDs := map[string]bool{AccessControlID: false, HeaderRewriteID: false, WebhookID: false}
	if len(all) != len(wantIDs) {
		t.Fatalf("All() returned %d builtins, want %d", len(all), len(wantIDs))
	}
	for _, b := range all {
		b := b
		t.Run(b.Manifest.ID, func(t *testing.T) {
			seen, known := wantIDs[b.Manifest.ID]
			if !known {
				t.Fatalf("unexpected builtin id %q", b.Manifest.ID)
			}
			if seen {
				t.Fatalf("duplicate builtin id %q", b.Manifest.ID)
			}
			wantIDs[b.Manifest.ID] = true

			if err := b.Manifest.Validate(); err != nil {
				t.Fatalf("Manifest.Validate: %v", err)
			}
			if b.New == nil {
				t.Fatal("New is nil")
			}

			cfg := b.Manifest.DefaultConfig()
			if b.Manifest.ID == WebhookID {
				// url is required and has no default.
				if err := b.Manifest.ValidateConfig(cfg); err == nil {
					t.Fatal("webhook default config without url should fail ValidateConfig")
				}
				cfg = tpWithURL(t, cfg, "https://hooks.example.com/resin")
			}
			if err := b.Manifest.ValidateConfig(cfg); err != nil {
				t.Fatalf("ValidateConfig(default %s): %v", cfg, err)
			}
			p := b.New()
			if err := p.Configure(context.Background(), cfg); err != nil {
				t.Fatalf("Configure(default %s): %v", cfg, err)
			}

			_, isInspector := p.(pluginsdk.RequestInspector)
			_, isHandler := p.(pluginsdk.EventHandler)
			if b.Manifest.Capabilities.RequestHook != isInspector {
				t.Fatalf("RequestHook capability=%v but implements RequestInspector=%v",
					b.Manifest.Capabilities.RequestHook, isInspector)
			}
			if (len(b.Manifest.Capabilities.Events) > 0) != isHandler {
				t.Fatalf("Events capability=%v but implements EventHandler=%v",
					b.Manifest.Capabilities.Events, isHandler)
			}
		})
	}
}

func TestBuiltin_NewReturnsIndependentInstances(t *testing.T) {
	b := tpFindBuiltin(t, AccessControlID)
	denyAll := b.New()
	if err := denyAll.Configure(context.Background(), json.RawMessage(`{"default_action":"deny"}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	other := b.New()
	if dec := tpInspect(t, other, pluginsdk.RequestInfo{ProxyType: "forward"}); dec != nil {
		t.Fatalf("unconfigured second instance should not reject, got %+v", dec)
	}
	if dec := tpInspect(t, denyAll, pluginsdk.RequestInfo{ProxyType: "forward"}); dec == nil || dec.Action != pluginsdk.ActionReject {
		t.Fatalf("configured instance should reject, got %+v", dec)
	}
}

func TestBuiltin_RejectsUnknownConfigFields(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		config string
	}{
		{"access control client cidr typo", AccessControlID, `{"client_cidr":["10.0.0.0/8"]}`},
		{"access control rule typo", AccessControlID, `{"rule":[]}`},
		{"header rewrite target host typo", HeaderRewriteID, `{"rules":[{"set":{"X-A":"v"},"target_host":["example.com"]}]}`},
		{"webhook header typo", WebhookID, `{"url":"https://hooks.example.com","headerz":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tpConfigureErr(t, tc.id, tc.config); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Configure(%s) error = %v, want unknown field", tc.id, err)
			}
		})
	}
}

func TestAccessControl_ClientCIDRMatchesIPv6Zone(t *testing.T) {
	p := tpConfigure(t, AccessControlID, `{"default_action":"deny","rules":[{"action":"allow","client_cidrs":["fe80::/64"]}]}`)
	if dec := tpInspect(t, p, pluginsdk.RequestInfo{ProxyType: pluginsdk.ProxyTypeForward, ClientIP: "fe80::1%eth0"}); dec != nil {
		t.Fatalf("zone-qualified IPv6 address was not allowed: %+v", dec)
	}
}

func TestBuiltin_WildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, value string
		fold           bool
		want           bool
	}{
		{"abc", "abc", false, true},
		{"abc", "ABC", false, false},
		{"abc", "ABC", true, true},
		{"*", "", false, true},
		{"*", "anything", false, true},
		{"user-*", "user-1", false, true},
		{"user-*", "user-", false, true},
		{"user-*", "admin-1", false, false},
		{"*-admin", "ops-admin", false, true},
		{"*-admin", "ops-admins", false, false},
		{"a*b*c", "abc", false, true},
		{"a*b*c", "a-x-b-y-c", false, true},
		{"a*b*c", "acb", false, false},
		{"*ab*ab", "ab", false, false},
		{"*ab*ab", "abab", false, true},
		{"a*a", "a", false, false},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.value, tc.fold); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q, fold=%v) = %v, want %v", tc.pattern, tc.value, tc.fold, got, tc.want)
		}
	}
}

func tpAssertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}
