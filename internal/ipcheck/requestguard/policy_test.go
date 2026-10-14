package requestguard

import (
	"errors"
	"testing"
	"time"
)

func validPolicy() Policy {
	return Policy{Enabled: true, QPS: 10, Burst: 4, DailyLimit: 100,
		ConsecutiveFailureThreshold: 3, OpenFor: 5 * time.Second}
}

func TestPolicyBoundsAndDisabledValidation(t *testing.T) {
	for _, policy := range []Policy{
		{},
		{Enabled: true, QPS: 1, Burst: 1, DailyLimit: 1, ConsecutiveFailureThreshold: 1, OpenFor: time.Second},
		{Enabled: true, QPS: 100, Burst: 64, DailyLimit: 1_000_000, ConsecutiveFailureThreshold: 100, OpenFor: time.Hour},
	} {
		if err := policy.Validate(); err != nil {
			t.Fatalf("valid policy rejected: %+v: %v", policy, err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*Policy)
	}{
		{"qps_zero", func(p *Policy) { p.QPS = 0 }},
		{"qps_negative", func(p *Policy) { p.QPS = -1 }},
		{"qps_large", func(p *Policy) { p.QPS = 101 }},
		{"burst_zero", func(p *Policy) { p.Burst = 0 }},
		{"burst_negative", func(p *Policy) { p.Burst = -1 }},
		{"burst_large", func(p *Policy) { p.Burst = 65 }},
		{"daily_zero", func(p *Policy) { p.DailyLimit = 0 }},
		{"daily_large", func(p *Policy) { p.DailyLimit = 1_000_001 }},
		{"threshold_zero", func(p *Policy) { p.ConsecutiveFailureThreshold = 0 }},
		{"threshold_negative", func(p *Policy) { p.ConsecutiveFailureThreshold = -1 }},
		{"threshold_large", func(p *Policy) { p.ConsecutiveFailureThreshold = 101 }},
		{"cooldown_zero", func(p *Policy) { p.OpenFor = 0 }},
		{"cooldown_negative", func(p *Policy) { p.OpenFor = -time.Second }},
		{"cooldown_short", func(p *Policy) { p.OpenFor = time.Second - 1 }},
		{"cooldown_long", func(p *Policy) { p.OpenFor = time.Hour + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				p := validPolicy()
				p.Enabled = enabled
				tc.edit(&p)
				if err := p.Validate(); !errors.Is(err, ErrInvalidPolicy) {
					t.Fatalf("policy %+v error = %v", p, err)
				}
			}
		})
	}
	if err := (Policy{Enabled: true}).Validate(); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("incomplete enabled policy: %v", err)
	}
}
