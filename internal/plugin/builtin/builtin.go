// Package builtin contains plugins compiled into Resin. They use the same
// pluginsdk interfaces as external plugins and are disabled by default.
package builtin

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/proxy"
)

// All returns every builtin plugin.
func All() []plugin.Builtin {
	return []plugin.Builtin{
		accessControlBuiltin(),
		headerRewriteBuiltin(),
		webhookBuiltin(),
	}
}

func raw(v string) json.RawMessage { return json.RawMessage(v) }

// matchSet is the common "who/where" filter used by builtin rules. Empty
// fields match everything; values within a field are ORed and fields are
// ANDed.
type matchSet struct {
	ClientCIDRs []string `json:"client_cidrs,omitempty"`
	Platforms   []string `json:"platforms,omitempty"`
	Accounts    []string `json:"accounts,omitempty"`
	TargetHosts []string `json:"target_hosts,omitempty"`
	ProxyTypes  []string `json:"proxy_types,omitempty"`
}

type compiledMatch struct {
	cidrs      []netip.Prefix
	platforms  []string
	accounts   []string
	hosts      *proxy.TargetBypassMatcher
	anyHosts   bool
	proxyTypes map[string]bool
}

func (m matchSet) compile(where string) (compiledMatch, error) {
	out := compiledMatch{platforms: m.Platforms, accounts: m.Accounts}
	for _, c := range m.ClientCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if p, err := netip.ParsePrefix(c); err == nil {
			out.cidrs = append(out.cidrs, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(c)
		if err != nil {
			return out, fmt.Errorf("%s.client_cidrs: invalid CIDR or IP %q", where, c)
		}
		out.cidrs = append(out.cidrs, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(m.TargetHosts) > 0 {
		out.hosts = proxy.NewTargetBypassMatcher(m.TargetHosts)
		if out.hosts == nil {
			return out, fmt.Errorf("%s.target_hosts: no valid host pattern", where)
		}
	} else {
		out.anyHosts = true
	}
	if len(m.ProxyTypes) > 0 {
		out.proxyTypes = make(map[string]bool, len(m.ProxyTypes))
		for _, t := range m.ProxyTypes {
			t = strings.ToLower(strings.TrimSpace(t))
			switch t {
			case "forward", "reverse", "socks5":
				out.proxyTypes[t] = true
			default:
				return out, fmt.Errorf("%s.proxy_types: unknown proxy type %q (forward, reverse, socks5)", where, t)
			}
		}
	}
	return out, nil
}

func (c compiledMatch) match(proxyType, clientIP, platform, account, host string) bool {
	if c.proxyTypes != nil && !c.proxyTypes[proxyType] {
		return false
	}
	if len(c.cidrs) > 0 {
		addr, err := netip.ParseAddr(strings.Trim(clientIP, "[]"))
		if err != nil {
			return false
		}
		addr = addr.Unmap()
		ok := false
		for _, p := range c.cidrs {
			if p.Contains(addr) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(c.platforms) > 0 && !matchAny(c.platforms, platform, true) {
		return false
	}
	if len(c.accounts) > 0 && !matchAny(c.accounts, account, false) {
		return false
	}
	if !c.anyHosts && !c.hosts.ShouldBypass(host) {
		return false
	}
	return true
}

func matchAny(patterns []string, value string, foldCase bool) bool {
	for _, p := range patterns {
		if wildcardMatch(p, value, foldCase) {
			return true
		}
	}
	return false
}

// wildcardMatch matches value against pattern where '*' matches any run of
// characters (including none).
func wildcardMatch(pattern, value string, foldCase bool) bool {
	if foldCase {
		pattern, value = strings.ToLower(pattern), strings.ToLower(value)
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(value, part)
		if idx < 0 {
			return false
		}
		value = value[idx+len(part):]
	}
	return strings.HasSuffix(value, last)
}

func decodeConfig(config json.RawMessage, v any) error {
	if len(config) == 0 {
		config = raw(`{}`)
	}
	if err := json.Unmarshal(config, v); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return nil
}
