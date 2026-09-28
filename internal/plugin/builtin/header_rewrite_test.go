package builtin

import (
	"reflect"
	"testing"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

func tpHeaderReq() pluginsdk.RequestInfo {
	return pluginsdk.RequestInfo{
		ProxyType:  pluginsdk.ProxyTypeReverse,
		ClientIP:   "198.51.100.7",
		Platform:   "plat-a",
		Account:    "alice",
		TargetHost: "api.example.com",
		Method:     "GET",
		URL:        "https://api.example.com/v1",
		Headers:    map[string][]string{"X-Debug": {"1"}},
	}
}

func TestHeaderRewrite_Substitution(t *testing.T) {
	p := tpConfigure(t, HeaderRewriteID, `{"rules":[{"set":{
		"x-account":"${account}",
		"X-Platform":"${platform}",
		" x-client-ip ":"${client_ip}",
		"X-Target":"${target_host}",
		"X-Combo":"${account}@${platform} via ${target_host} from ${client_ip}",
		"X-Literal":"static ${unknown}"
	}}]}`)
	dec := tpInspect(t, p, tpHeaderReq())
	if dec == nil {
		t.Fatal("expected a decision")
	}
	if dec.Action != "" && dec.Action != pluginsdk.ActionContinue {
		t.Fatalf("action = %q, want continue", dec.Action)
	}
	want := map[string]string{
		"X-Account":   "alice",
		"X-Platform":  "plat-a",
		"X-Client-Ip": "198.51.100.7",
		"X-Target":    "api.example.com",
		"X-Combo":     "alice@plat-a via api.example.com from 198.51.100.7",
		"X-Literal":   "static ${unknown}",
	}
	if !reflect.DeepEqual(dec.SetHeaders, want) {
		t.Fatalf("SetHeaders = %#v, want %#v", dec.SetHeaders, want)
	}
	if len(dec.RemoveHeaders) != 0 {
		t.Fatalf("RemoveHeaders = %v, want none", dec.RemoveHeaders)
	}
	if dec.Platform != nil || dec.Account != nil {
		t.Fatalf("header rewrite must not override routing identity: %+v", dec)
	}
}

func TestHeaderRewrite_SubstitutionStripsCRLF(t *testing.T) {
	p := tpConfigure(t, HeaderRewriteID, `{"rules":[{"set":{"X-Account":"${account}"}}]}`)
	req := tpHeaderReq()
	req.Account = "evil\r\nX-Injected: 1"
	dec := tpInspect(t, p, req)
	if dec == nil {
		t.Fatal("expected a decision")
	}
	if got := dec.SetHeaders["X-Account"]; got != "evilX-Injected: 1" {
		t.Fatalf("X-Account = %q, want CR/LF stripped", got)
	}
}

func TestHeaderRewrite_SetAndRemove(t *testing.T) {
	p := tpConfigure(t, HeaderRewriteID, `{"rules":[{"remove":["x-debug"," X-Forwarded-For "],"set":{"X-Tag":"v"}}]}`)
	dec := tpInspect(t, p, tpHeaderReq())
	if dec == nil {
		t.Fatal("expected a decision")
	}
	if !reflect.DeepEqual(dec.RemoveHeaders, []string{"X-Debug", "X-Forwarded-For"}) {
		t.Fatalf("RemoveHeaders = %v", dec.RemoveHeaders)
	}
	if !reflect.DeepEqual(dec.SetHeaders, map[string]string{"X-Tag": "v"}) {
		t.Fatalf("SetHeaders = %v", dec.SetHeaders)
	}

	// Remove-only rule is valid.
	p = tpConfigure(t, HeaderRewriteID, `{"rules":[{"remove":["X-Debug"]}]}`)
	dec = tpInspect(t, p, tpHeaderReq())
	if dec == nil || !reflect.DeepEqual(dec.RemoveHeaders, []string{"X-Debug"}) || len(dec.SetHeaders) != 0 {
		t.Fatalf("remove-only decision = %+v", dec)
	}
}

func TestHeaderRewrite_RulesAppliedInOrder(t *testing.T) {
	// A later remove cancels an earlier set; a later set wins over an
	// earlier remove (removals are applied before sets by the host).
	p := tpConfigure(t, HeaderRewriteID, `{"rules":[
		{"set":{"X-A":"first","X-Keep":"1"},"remove":["X-B"]},
		{"remove":["x-a"],"set":{"X-B":"second"}},
		{"platforms":["nomatch"],"set":{"X-Keep":"overwritten"}},
		{"set":{"X-Keep":"2"}}
	]}`)
	dec := tpInspect(t, p, tpHeaderReq())
	if dec == nil {
		t.Fatal("expected a decision")
	}
	wantSet := map[string]string{"X-B": "second", "X-Keep": "2"}
	if !reflect.DeepEqual(dec.SetHeaders, wantSet) {
		t.Fatalf("SetHeaders = %v, want %v", dec.SetHeaders, wantSet)
	}
	wantRemove := []string{"X-B", "X-A"}
	if !reflect.DeepEqual(dec.RemoveHeaders, wantRemove) {
		t.Fatalf("RemoveHeaders = %v, want %v", dec.RemoveHeaders, wantRemove)
	}
}

func TestHeaderRewrite_MatchFiltersAndSkips(t *testing.T) {
	p := tpConfigure(t, HeaderRewriteID, `{"rules":[{"platforms":["plat-*"],"accounts":["alice"],"target_hosts":["*.example.com"],"set":{"X-Hit":"1"}}]}`)

	if dec := tpInspect(t, p, tpHeaderReq()); dec == nil || dec.SetHeaders["X-Hit"] != "1" {
		t.Fatalf("matching request should get header, got %+v", dec)
	}

	miss := tpHeaderReq()
	miss.Account = "bob"
	if dec := tpInspect(t, p, miss); dec != nil {
		t.Fatalf("non-matching request should get nil decision, got %+v", dec)
	}

	connect := tpHeaderReq()
	connect.ProxyType = pluginsdk.ProxyTypeForward
	connect.IsConnect = true
	if dec := tpInspect(t, p, connect); dec != nil {
		t.Fatalf("CONNECT request should be skipped, got %+v", dec)
	}

	socks := tpHeaderReq()
	socks.ProxyType = pluginsdk.ProxyTypeSocks5
	if dec := tpInspect(t, p, socks); dec != nil {
		t.Fatalf("SOCKS5 request should be skipped, got %+v", dec)
	}

	plainForward := tpHeaderReq()
	plainForward.ProxyType = pluginsdk.ProxyTypeForward
	if dec := tpInspect(t, p, plainForward); dec == nil || dec.SetHeaders["X-Hit"] != "1" {
		t.Fatalf("plain forward request should get header, got %+v", dec)
	}

	unconfigured := tpFindBuiltin(t, HeaderRewriteID).New()
	if dec := tpInspect(t, unconfigured, tpHeaderReq()); dec != nil {
		t.Fatalf("unconfigured plugin should return nil, got %+v", dec)
	}

	empty := tpConfigure(t, HeaderRewriteID, `{"rules":[]}`)
	if dec := tpInspect(t, empty, tpHeaderReq()); dec != nil {
		t.Fatalf("no rules should return nil, got %+v", dec)
	}
}

func TestHeaderRewrite_InvalidConfig(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"not json", `{`, "invalid config"},
		{"rules wrong type", `{"rules":5}`, "invalid config"},
		{"set wrong type", `{"rules":[{"set":{"X-A":1}}]}`, "invalid config"},
		{"empty rule", `{"rules":[{}]}`, "rules[0]: set or remove is required"},
		{"empty set and remove", `{"rules":[{"set":{},"remove":[]}]}`, "rules[0]: set or remove is required"},
		{"second rule empty", `{"rules":[{"set":{"X-A":"1"}},{"platforms":["p"]}]}`, "rules[1]: set or remove is required"},
		{"header with space", `{"rules":[{"set":{"Bad Header":"v"}}]}`, "rules[0].set: invalid header name"},
		{"header with colon", `{"rules":[{"set":{"X-A:b":"v"}}]}`, "rules[0].set: invalid header name"},
		{"header with parentheses", `{"rules":[{"set":{"X(Paren)":"v"}}]}`, "rules[0].set: invalid header name"},
		{"header with unicode", `{"rules":[{"set":{"X-Ümlaut":"v"}}]}`, "rules[0].set: invalid header name"},
		{"blank header name", `{"rules":[{"set":{"  ":"v"}}]}`, "rules[0].set: invalid header name"},
		{"blank remove name", `{"rules":[{"remove":[""]}]}`, "rules[0].remove: invalid header name"},
		{"protected host", `{"rules":[{"set":{"host":"evil"}}]}`, "header Host cannot be rewritten"},
		{"protected content-length", `{"rules":[{"remove":["content-length"]}]}`, "header Content-Length cannot be rewritten"},
		{"protected transfer-encoding", `{"rules":[{"set":{"Transfer-Encoding":"chunked"}}]}`, "header Transfer-Encoding cannot be rewritten"},
		{"protected connection", `{"rules":[{"remove":["Connection"]}]}`, "header Connection cannot be rewritten"},
		{"protected upgrade", `{"rules":[{"set":{"upgrade":"websocket"}}]}`, "header Upgrade cannot be rewritten"},
		{"protected te", `{"rules":[{"remove":["TE"]}]}`, "header Te cannot be rewritten"},
		{"protected trailer", `{"rules":[{"set":{"Trailer":"x"}}]}`, "header Trailer cannot be rewritten"},
		{"value with CRLF", `{"rules":[{"set":{"X-A":"a\r\nb"}}]}`, "rules[0].set[X-A]: invalid header value"},
		{"value with NUL", `{"rules":[{"set":{"X-A":"a\u0000b"}}]}`, "invalid header value"},
		{"value with control", `{"rules":[{"set":{"X-A":"a\u0001b"}}]}`, "invalid header value"},
		{"value with DEL", `{"rules":[{"set":{"X-A":"a\u007fb"}}]}`, "invalid header value"},
		{"bad proxy type", `{"rules":[{"set":{"X-A":"v"},"proxy_types":["ftp"]}]}`, "rules[0].proxy_types: unknown proxy type"},
		{"bad client cidr", `{"rules":[{"set":{"X-A":"v"},"client_cidrs":["x"]}]}`, "rules[0].client_cidrs: invalid CIDR or IP"},
		{"no valid target host", `{"rules":[{"set":{"X-A":"v"},"target_hosts":[" "]}]}`, "rules[0].target_hosts: no valid host pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tpAssertErrContains(t, tpConfigureErr(t, HeaderRewriteID, tc.config), tc.want)
		})
	}
}

func TestHeaderRewrite_FailedReconfigureKeepsRules(t *testing.T) {
	p := tpConfigure(t, HeaderRewriteID, `{"rules":[{"set":{"X-A":"1"}}]}`)
	if err := p.Configure(t.Context(), []byte(`{"rules":[{"set":{"Host":"x"}}]}`)); err == nil {
		t.Fatal("expected reconfigure error")
	}
	if dec := tpInspect(t, p, tpHeaderReq()); dec == nil || dec.SetHeaders["X-A"] != "1" {
		t.Fatalf("previous rules should remain active, got %+v", dec)
	}
}
