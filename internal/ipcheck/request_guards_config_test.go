package ipcheck

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
)

const validSourceGuardJSON = `{"source":"egress-consistency","enabled":true,"qps":10,"burst":4,"daily_limit":100,"consecutive_failure_threshold":3,"open_for":"5s"}`

func TestSourceGuardConfigDefaultAndCompletePolicies(t *testing.T) {
	for _, raw := range []string{`{}`, `{"source_guards":[]}`} {
		cfg, err := ParseConfig(json.RawMessage(raw))
		if err != nil || len(cfg.SourceGuards) != 0 {
			t.Fatalf("default guard state: %+v %v", cfg.SourceGuards, err)
		}
	}
	for _, enabled := range []string{"true", "false"} {
		cfg, err := ParseConfig(json.RawMessage(`{"source_guards":[` + strings.Replace(validSourceGuardJSON, `"enabled":true`, `"enabled":`+enabled, 1) + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		policy, err := cfg.SourceGuards[0].Policy()
		if err != nil || policy.Enabled != (enabled == "true") || policy.QPS != 10 || policy.Burst != 4 || policy.DailyLimit != 100 || policy.ConsecutiveFailureThreshold != 3 || policy.OpenFor != 5*time.Second {
			t.Fatalf("converted policy = %+v %v", policy, err)
		}
	}
	// Dormant policies do not require their source to be currently configured
	// or enabled; they also do not make that source available or enable it.
	cfg, err := ParseConfig(json.RawMessage(`{"enabled_sources":["db-ip"],"source_guards":[{"source":"egress-consistency"},{"source":"cloudflare-challenge","enabled":false},{"source":"ai-region"}]}`))
	if err != nil || len(cfg.SourceGuards) != 3 || len(cfg.EnabledSources) != 1 || cfg.EnabledSources[0] != "db-ip" {
		t.Fatalf("dormant policies altered sources: %+v %v", cfg, err)
	}
	for _, configured := range cfg.SourceGuards {
		policy, err := configured.Policy()
		if err != nil || policy != (requestguard.Policy{}) {
			t.Fatalf("explicit zero policy = %+v %v", policy, err)
		}
	}
}

func TestSourceGuardConfigRejectsAmbiguousAndUnsafeInput(t *testing.T) {
	policies := []string{
		`null`, `[]`, `"egress-consistency"`, `{}`,
		`{"source":"db-ip"}`, `{"source":"ippure"}`, `{"source":"EGRESS-CONSISTENCY"}`,
		`{"source":"egress-consistency","source":"ai-region"}`,
		`{"source":"egress-consistency","Source":"ai-region"}`,
		`{"source":"egress-consistency","enabled":null}`,
		`{"source":"egress-consistency","qps":null}`,
		`{"source":"egress-consistency","unknown":1}`,
		`{"source":"egress-consistency","enabled":true}`,
		`{"source":"egress-consistency","enabled":false,"qps":1}`,
		strings.Replace(validSourceGuardJSON, `"qps":10`, `"qps":1.5`, 1),
		strings.Replace(validSourceGuardJSON, `"qps":10`, `"qps":101`, 1),
		strings.Replace(validSourceGuardJSON, `"qps":10`, `"QPS":10`, 1),
		strings.Replace(validSourceGuardJSON, `"burst":4`, `"burst":65`, 1),
		strings.Replace(validSourceGuardJSON, `"daily_limit":100`, `"daily_limit":-1`, 1),
		strings.Replace(validSourceGuardJSON, `"daily_limit":100`, `"daily_limit":18446744073709551616`, 1),
		strings.Replace(validSourceGuardJSON, `"consecutive_failure_threshold":3`, `"consecutive_failure_threshold":0`, 1),
		strings.Replace(validSourceGuardJSON, `"open_for":"5s"`, `"open_for":5000000000`, 1),
		strings.Replace(validSourceGuardJSON, `"open_for":"5s"`, `"open_for":"999ms"`, 1),
		strings.Replace(validSourceGuardJSON, `"open_for":"5s"`, `"open_for":"3601s"`, 1),
		strings.Replace(validSourceGuardJSON, `"open_for":"5s"`, `"open_for":""`, 1),
		strings.Replace(validSourceGuardJSON, `"open_for":"5s"`, `"open_for":"999999999999999999999h"`, 1),
	}
	for _, policy := range policies {
		if _, err := ParseConfig(json.RawMessage(`{"source_guards":[` + policy + `]}`)); err == nil {
			t.Errorf("accepted malformed guard: %s", policy)
		}
	}
	for _, raw := range []string{
		`{"source_guards":null}`,
		`{"source_guards":{}}`,
		`{"source_guards":[],"SOURCE_GUARDS":[]}`,
		`{"source_guards":[` + validSourceGuardJSON + `,` + validSourceGuardJSON + `]}`,
		`{"source_guards":[{"source":"egress-consistency"},{"source":"cloudflare-challenge"},{"source":"ai-region"},{"source":"egress-consistency"}]}`,
	} {
		if _, err := ParseConfig(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted malformed policy collection: %s", raw)
		}
	}
	// Go callers receive the same bounds; parsing is not the sole validation.
	cfg := DefaultConfig()
	cfg.SourceGuards = []SourceGuardPolicy{{Source: "egress-consistency", Enabled: true}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("direct incomplete policy bypassed validation")
	}
}
