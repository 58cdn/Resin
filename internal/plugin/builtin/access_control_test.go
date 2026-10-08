package builtin

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

func tpAccessDenied(t *testing.T, config string, req pluginsdk.RequestInfo) bool {
	t.Helper()
	p := tpConfigure(t, AccessControlID, config)
	dec := tpInspect(t, p, req)
	if dec == nil {
		return false
	}
	if dec.Action != pluginsdk.ActionReject {
		t.Fatalf("unexpected non-reject decision: %+v", dec)
	}
	return true
}

func TestAccessControl_ConfigureErrors(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"not json", `not json`, "invalid config"},
		{"rules wrong type", `{"rules":"nope"}`, "invalid config"},
		{"reject_status wrong type", `{"reject_status":"403"}`, "invalid config"},
		{"bad default action", `{"default_action":"maybe"}`, "default_action must be allow or deny"},
		{"status too low", `{"reject_status":200}`, "reject_status must be between 400 and 599"},
		{"status too high", `{"reject_status":600}`, "reject_status must be between 400 and 599"},
		{"negative status", `{"reject_status":-1}`, "reject_status must be between 400 and 599"},
		{"bad action", `{"rules":[{"action":"block"}]}`, "rules[0].action must be allow or deny"},
		{"missing action", `{"rules":[{"platforms":["a"]}]}`, "rules[0].action must be allow or deny"},
		{"bad action second rule", `{"rules":[{"action":"allow"},{"action":"x"}]}`, "rules[1].action must be allow or deny"},
		{"bad cidr", `{"rules":[{"action":"deny","client_cidrs":["not-an-ip"]}]}`, `rules[0].client_cidrs: invalid CIDR or IP "not-an-ip"`},
		{"bad cidr mask", `{"rules":[{"action":"deny","client_cidrs":["10.0.0.0/99"]}]}`, "rules[0].client_cidrs: invalid CIDR or IP"},
		{"empty target hosts", `{"rules":[{"action":"deny","target_hosts":["  ",""]}]}`, "rules[0].target_hosts: no valid host pattern"},
		{"unknown proxy type", `{"rules":[{"action":"deny","proxy_types":["http"]}]}`, `rules[0].proxy_types: unknown proxy type "http"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpAssertErrContains(t, tpConfigureErr(t, AccessControlID, tc.config), tc.want)
		})
	}
}

func TestAccessControl_ConfigureAcceptsEmptyAndNormalizes(t *testing.T) {
	for _, cfg := range []string{``, `{}`, `{"default_action":""}`, `{"default_action":" ALLOW "}`, `{"reject_status":0}`, `{"rules":[]}`, `{"rules":null}`} {
		if err := tpConfigureErr(t, AccessControlID, cfg); err != nil {
			t.Fatalf("Configure(%q): unexpected error %v", cfg, err)
		}
		if tpAccessDenied(t, cfg, pluginsdk.RequestInfo{ProxyType: "forward", Platform: "p"}) {
			t.Fatalf("Configure(%q) should allow by default", cfg)
		}
	}
	// Empty strings in client_cidrs are skipped.
	if err := tpConfigureErr(t, AccessControlID, `{"rules":[{"action":"deny","client_cidrs":["", "  "]}]}`); err != nil {
		t.Fatalf("blank client_cidrs should be ignored: %v", err)
	}
}

func TestAccessControl_UnconfiguredAllows(t *testing.T) {
	p := tpFindBuiltin(t, AccessControlID).New()
	if dec := tpInspect(t, p, pluginsdk.RequestInfo{ProxyType: "forward"}); dec != nil {
		t.Fatalf("unconfigured plugin should return nil decision, got %+v", dec)
	}
}

func TestAccessControl_DefaultAction(t *testing.T) {
	req := pluginsdk.RequestInfo{ProxyType: "forward", Platform: "p", Account: "a", TargetHost: "example.com:443", ClientIP: "10.0.0.1"}

	if tpAccessDenied(t, `{"default_action":"allow"}`, req) {
		t.Fatal("default_action allow should allow")
	}

	p := tpConfigure(t, AccessControlID, `{"default_action":"deny"}`)
	dec := tpInspect(t, p, req)
	if dec == nil || dec.Action != pluginsdk.ActionReject {
		t.Fatalf("default_action deny should reject, got %+v", dec)
	}
	if dec.Status != http.StatusForbidden {
		t.Fatalf("default reject status = %d, want 403", dec.Status)
	}
	if dec.Message != "Request denied by access control" {
		t.Fatalf("default reject message = %q", dec.Message)
	}

	// Case-insensitive action names.
	if !tpAccessDenied(t, `{"default_action":" Deny "}`, req) {
		t.Fatal("default_action \" Deny \" should reject")
	}
}

func TestAccessControl_CustomRejectStatusAndMessage(t *testing.T) {
	p := tpConfigure(t, AccessControlID, `{"default_action":"deny","reject_status":451,"reject_message":"  go away  "}`)
	dec := tpInspect(t, p, pluginsdk.RequestInfo{ProxyType: "reverse"})
	if dec == nil {
		t.Fatal("expected reject")
	}
	if dec.Action != pluginsdk.ActionReject || dec.Status != 451 || dec.Message != "go away" {
		t.Fatalf("decision = %+v, want reject 451 \"go away\"", dec)
	}

	// Blank message falls back to the default; status 0 falls back to 403.
	p = tpConfigure(t, AccessControlID, `{"default_action":"deny","reject_status":0,"reject_message":"   "}`)
	dec = tpInspect(t, p, pluginsdk.RequestInfo{ProxyType: "reverse"})
	if dec == nil || dec.Status != http.StatusForbidden || dec.Message != "Request denied by access control" {
		t.Fatalf("decision = %+v, want default 403 message", dec)
	}

	// Rule denial uses the same status/message.
	p = tpConfigure(t, AccessControlID, `{"rules":[{"action":"deny","accounts":["bad"]}],"reject_status":429,"reject_message":"slow down"}`)
	dec = tpInspect(t, p, pluginsdk.RequestInfo{ProxyType: "forward", Account: "bad"})
	if dec == nil || dec.Status != 429 || dec.Message != "slow down" {
		t.Fatalf("rule decision = %+v, want 429 \"slow down\"", dec)
	}
}

func TestAccessControl_FirstMatchWins(t *testing.T) {
	// allow for platform "trusted" precedes a catch-all deny.
	cfg := `{"default_action":"allow","rules":[
		{"action":"allow","platforms":["trusted"]},
		{"action":"deny"}
	]}`
	if tpAccessDenied(t, cfg, pluginsdk.RequestInfo{ProxyType: "forward", Platform: "trusted"}) {
		t.Fatal("first matching allow rule should win")
	}
	if !tpAccessDenied(t, cfg, pluginsdk.RequestInfo{ProxyType: "forward", Platform: "other"}) {
		t.Fatal("catch-all deny should apply to non-matching platform")
	}

	// deny for one account precedes a catch-all allow, with default deny.
	cfg = `{"default_action":"deny","rules":[
		{"action":"deny","accounts":["mallory"]},
		{"action":"allow","platforms":["*"]}
	]}`
	if !tpAccessDenied(t, cfg, pluginsdk.RequestInfo{ProxyType: "forward", Platform: "p", Account: "mallory"}) {
		t.Fatal("first matching deny rule should win")
	}
	if tpAccessDenied(t, cfg, pluginsdk.RequestInfo{ProxyType: "forward", Platform: "p", Account: "alice"}) {
		t.Fatal("second allow rule should win for alice")
	}

	// No rule matches -> default action.
	cfg = `{"default_action":"deny","rules":[{"action":"allow","platforms":["only"]}]}`
	if !tpAccessDenied(t, cfg, pluginsdk.RequestInfo{ProxyType: "forward", Platform: "else"}) {
		t.Fatal("no match should fall back to default deny")
	}
}

func TestAccessControl_Matching(t *testing.T) {
	type check struct {
		req  pluginsdk.RequestInfo
		deny bool
	}
	fwd := func(mut func(*pluginsdk.RequestInfo)) pluginsdk.RequestInfo {
		r := pluginsdk.RequestInfo{
			ProxyType:  "forward",
			ClientIP:   "203.0.113.9",
			Platform:   "plat",
			Account:    "acct",
			TargetHost: "example.org:443",
		}
		if mut != nil {
			mut(&r)
		}
		return r
	}
	cases := []struct {
		name   string
		rule   string
		checks []check
	}{
		{
			name: "empty match set matches everything",
			rule: `{"action":"deny"}`,
			checks: []check{
				{fwd(nil), true},
				{pluginsdk.RequestInfo{}, true},
			},
		},
		{
			name: "client cidr and single ip",
			rule: `{"action":"deny","client_cidrs":["10.0.0.0/8"," 192.168.1.5 ","2001:db8::/32"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "10.1.2.3" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "192.168.1.5" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "192.168.1.6" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "::ffff:10.0.0.1" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "2001:db8::1" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "[2001:db8::1]" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "2001:db9::1" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "garbage" }), false},
			},
		},
		{
			name: "non-canonical cidr is masked",
			rule: `{"action":"deny","client_cidrs":["10.1.2.3/16"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "10.1.200.1" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "10.2.0.1" }), false},
			},
		},
		{
			name: "platform wildcard is case-insensitive",
			rule: `{"action":"deny","platforms":["Team-*","Prod"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.Platform = "team-a" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Platform = "TEAM-B" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Platform = "prod" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Platform = "production" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Platform = "other" }), false},
			},
		},
		{
			name: "account wildcard is case-sensitive",
			rule: `{"action":"deny","accounts":["user-*","*-admin"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "user-1" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "USER-1" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "ops-admin" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "ops-Admin" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "" }), false},
			},
		},
		{
			name: "account star matches empty account",
			rule: `{"action":"deny","accounts":["*"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "" }), true},
			},
		},
		{
			name: "target host exact",
			rule: `{"action":"deny","target_hosts":["api.example.com"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "api.example.com" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "API.Example.COM" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "api.example.com:443" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "x.api.example.com" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "example.com" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "" }), false},
			},
		},
		{
			name: "target host glob",
			rule: `{"action":"deny","target_hosts":["*.example.com"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "a.example.com" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "a.b.example.com:8443" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "example.com" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "example.com.evil.net" }), false},
			},
		},
		{
			name: "target host cidr",
			rule: `{"action":"deny","target_hosts":["10.0.0.0/8","2001:db8::/32"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "10.2.3.4" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "10.2.3.4:443" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "[2001:db8::5]:443" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "11.0.0.1" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "ten.example.com" }), false},
			},
		},
		{
			name: "target host local",
			rule: `{"action":"deny","target_hosts":["<local>"]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "localhost" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "localhost:8080" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "intranet" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "example.com" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "10.0.0.1" }), false},
			},
		},
		{
			name: "proxy types",
			rule: `{"action":"deny","proxy_types":["socks5"," Reverse "]}`,
			checks: []check{
				{fwd(func(r *pluginsdk.RequestInfo) { r.ProxyType = "socks5" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ProxyType = "reverse" }), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ProxyType = "forward" }), false},
			},
		},
		{
			name: "fields are ANDed",
			rule: `{"action":"deny","platforms":["plat"],"accounts":["acct"],"proxy_types":["forward"],"client_cidrs":["203.0.113.0/24"],"target_hosts":["example.org"]}`,
			checks: []check{
				{fwd(nil), true},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Platform = "x" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.Account = "x" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ProxyType = "reverse" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.ClientIP = "198.51.100.1" }), false},
				{fwd(func(r *pluginsdk.RequestInfo) { r.TargetHost = "example.net" }), false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fmt.Sprintf(`{"default_action":"allow","rules":[%s]}`, tc.rule)
			p := tpConfigure(t, AccessControlID, cfg)
			for i, c := range tc.checks {
				dec := tpInspect(t, p, c.req)
				got := dec != nil && dec.Action == pluginsdk.ActionReject
				if got != c.deny {
					t.Errorf("check %d (%+v): denied=%v, want %v", i, c.req, got, c.deny)
				}
			}
		})
	}
}

func TestAccessControl_ReconfigureReplacesRules(t *testing.T) {
	p := tpConfigure(t, AccessControlID, `{"default_action":"deny"}`)
	req := pluginsdk.RequestInfo{ProxyType: "forward"}
	if dec := tpInspect(t, p, req); dec == nil {
		t.Fatal("expected deny before reconfigure")
	}
	if err := p.Configure(t.Context(), []byte(`{"default_action":"allow"}`)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if dec := tpInspect(t, p, req); dec != nil {
		t.Fatalf("expected allow after reconfigure, got %+v", dec)
	}
	// A failed reconfigure keeps the previous state.
	if err := p.Configure(t.Context(), []byte(`{"default_action":"nope"}`)); err == nil {
		t.Fatal("expected reconfigure error")
	}
	if dec := tpInspect(t, p, req); dec != nil {
		t.Fatalf("failed reconfigure should keep previous allow state, got %+v", dec)
	}
}
