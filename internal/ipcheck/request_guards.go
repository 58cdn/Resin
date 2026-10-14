package ipcheck

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
)

// RequestGuardHost is an optional internal capability. Production hosts return
// the same controller across engine, plugin and configuration generations.
// Resolving this capability must not allocate or apply a policy.
type RequestGuardHost interface {
	RequestGuards() *requestguard.Controller
}

// SourceGuardPolicy is configuration, not an observation or provider billing
// promise. Omitted sources have the zero (disabled) policy. Even a disabled
// nonzero policy must specify a complete valid set of limits.
type SourceGuardPolicy struct {
	Source                      string `json:"source"`
	Enabled                     bool   `json:"enabled"`
	QPS                         int    `json:"qps"`
	Burst                       int    `json:"burst"`
	DailyLimit                  uint64 `json:"daily_limit"`
	ConsecutiveFailureThreshold int    `json:"consecutive_failure_threshold"`
	OpenFor                     string `json:"open_for"`
}

// UnmarshalJSON keeps the new security-relevant policy fields unambiguous,
// including objects nested inside the source_guards array.
func (p *SourceGuardPolicy) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return invalid("source guard must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return invalid("invalid source guard")
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return invalid("duplicate source guard field")
		}
		switch name {
		case "source", "enabled", "qps", "burst", "daily_limit", "consecutive_failure_threshold", "open_for":
		default:
			return invalid("unknown source guard field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid("source guard fields cannot be null or invalid")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return invalid("invalid source guard")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("source guard must contain one object")
	}
	type plainPolicy SourceGuardPolicy
	var decoded plainPolicy
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return invalid("invalid source guard field type")
	}
	*p = SourceGuardPolicy(decoded)
	return nil
}

func (p SourceGuardPolicy) Policy() (requestguard.Policy, error) {
	switch requestguard.Source(p.Source) {
	case requestguard.EgressConsistency, requestguard.CloudflareChallenge, requestguard.AIRegion:
	default:
		return requestguard.Policy{}, invalid("source guard requires a supported network source")
	}
	policy := requestguard.Policy{
		Enabled: p.Enabled, QPS: p.QPS, Burst: p.Burst, DailyLimit: p.DailyLimit,
		ConsecutiveFailureThreshold: p.ConsecutiveFailureThreshold,
	}
	if p.OpenFor != "" {
		duration, err := time.ParseDuration(p.OpenFor)
		if err != nil || duration < time.Second || duration > time.Hour {
			return requestguard.Policy{}, invalid("source guard open_for must be between 1s and 1h")
		}
		policy.OpenFor = duration
	}
	if err := policy.Validate(); err != nil {
		return requestguard.Policy{}, invalid("source guard requires complete bounded limits")
	}
	return policy, nil
}

func (cfg Config) validateSourceGuards() error {
	if len(cfg.SourceGuards) > 3 {
		return invalid("source_guards must contain at most 3 policies")
	}
	seen := map[string]bool{}
	for _, p := range cfg.SourceGuards {
		if seen[p.Source] {
			return invalid("duplicate source guard")
		}
		seen[p.Source] = true
		if _, err := p.Policy(); err != nil {
			return err
		}
	}
	return nil
}
