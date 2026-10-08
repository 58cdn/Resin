package ipcheck

import (
	"bytes"
	"encoding/json"
	"io"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	MaxTargets               = 8
	MaxTargetURLLength       = 2048
	MaxResponseBytes   int64 = 256 << 10
)

type Config struct {
	EnabledSources          []string                        `json:"enabled_sources"`
	IPResultTTL             string                          `json:"ip_result_ttl"`
	NodeResultTTL           string                          `json:"node_result_ttl"`
	RequestTimeoutMS        int                             `json:"request_timeout_ms"`
	ConsistencyTargets      []string                        `json:"consistency_targets"`
	CloudflareChallengeURLs []string                        `json:"cloudflare_challenge_urls"`
	AIRegionTargets         map[string]AIRegionTargetConfig `json:"ai_region_targets"`
	BackgroundEnabled       bool                            `json:"background_enabled"`
	MaxConcurrentChecks     int                             `json:"max_concurrent_checks"`
	MaxCacheEntries         int                             `json:"max_cache_entries"`
	SourcePriority          []string                        `json:"source_priority"`
}

func DefaultConfig() Config {
	return Config{
		EnabledSources: []string{"egress-consistency", "db-ip"},
		IPResultTTL:    "24h", NodeResultTTL: "6h", RequestTimeoutMS: 10000,
		ConsistencyTargets:      []string{"https://cloudflare.com/cdn-cgi/trace"},
		CloudflareChallengeURLs: []string{}, AIRegionTargets: map[string]AIRegionTargetConfig{},
		MaxConcurrentChecks: 8, MaxCacheEntries: 4096,
		SourcePriority: []string{"db-ip", "egress-consistency"},
	}
}

func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return Config{}, invalid("config must be a JSON object")
	}
	if err := validateConfigObject(raw); err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, invalid("invalid IP quality config: " + err.Error())
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, invalid("config must contain one JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) Validate() error {
	for name, value := range map[string]string{"ip_result_ttl": cfg.IPResultTTL, "node_result_ttl": cfg.NodeResultTTL} {
		d, err := time.ParseDuration(value)
		if err != nil || d < time.Second || d > 30*24*time.Hour {
			return invalid(name + " must be between 1s and 720h")
		}
	}
	if cfg.RequestTimeoutMS < 100 || cfg.RequestTimeoutMS > 60000 {
		return invalid("request_timeout_ms must be between 100 and 60000")
	}
	if cfg.BackgroundEnabled {
		return invalid("background_enabled is not supported in P1")
	}
	if cfg.MaxConcurrentChecks < 1 || cfg.MaxConcurrentChecks > 64 {
		return invalid("max_concurrent_checks must be between 1 and 64")
	}
	if cfg.MaxCacheEntries < 1 || cfg.MaxCacheEntries > 100000 {
		return invalid("max_cache_entries must be between 1 and 100000")
	}
	if len(cfg.ConsistencyTargets) == 0 || len(cfg.ConsistencyTargets) > MaxTargets {
		return invalid("consistency_targets must contain between 1 and 8 URLs")
	}
	targets := map[string]bool{}
	for _, target := range cfg.ConsistencyTargets {
		if err := ValidateTarget(target); err != nil {
			return err
		}
		u, _ := url.Parse(target)
		u.RawQuery = ""
		u.ForceQuery = false
		label := u.String()
		if targets[label] {
			return invalid("duplicate consistency target")
		}
		targets[label] = true
	}
	for _, group := range [][]string{cfg.EnabledSources, cfg.SourcePriority} {
		if len(group) > len(sourceCatalog) {
			return invalid("too many sources")
		}
		seen := map[string]bool{}
		for _, name := range group {
			if _, ok := sourceCatalog[name]; !ok {
				return invalid("unknown source: " + name)
			}
			if seen[name] {
				return invalid("duplicate source: " + name)
			}
			seen[name] = true
		}
	}
	if err := validateOptionalTargets(cfg.CloudflareChallengeURLs, cfg.AIRegionTargets); err != nil {
		return err
	}
	return nil
}

func invalid(message string) error { return &ValidationError{Message: message} }

// ValidateTarget performs static SSRF validation. Hosts must additionally pin
// public DNS answers at connection time, reject mixed/private DNS answers, and
// validate every redirect. Configuration validation never accesses the network.
func ValidateTarget(raw string) error {
	if len(raw) == 0 || len(raw) > MaxTargetURLLength {
		return invalid("target URL length must be between 1 and 2048")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return invalid("targets must be absolute HTTPS URLs without credentials or fragments")
	}
	if strings.Contains(raw, "#") {
		return invalid("target URL fragments are not permitted")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return invalid("invalid target port")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		if !IsPublicIP(ip) {
			return invalid("target IP must be public")
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home") || !strings.Contains(host, ".") || len(host) > 253 {
		return invalid("target hostname must be a public DNS name")
	}
	allNumeric := true
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid("invalid target hostname")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return invalid("invalid target hostname")
			}
			if r < '0' || r > '9' {
				allNumeric = false
			}
		}
	}
	if allNumeric {
		return invalid("ambiguous numeric target hostname")
	}
	return nil
}

var nonPublicPrefixes = func() []netip.Prefix {
	values := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"}
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}()

// IsPublicIP rejects private, link-local, reserved/documentation and transition
// addresses. IPv4-mapped IPv6 addresses are normalized before validation.
func IsPublicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func validateIP(ip netip.Addr) error {
	if !IsPublicIP(ip) {
		return invalid("IP must be a valid public address")
	}
	return nil
}

// Reject ambiguous duplicates and null values instead of silently preserving a
// default or accepting the last copy of a security-relevant configuration key.
func validateConfigObject(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return invalid("invalid JSON configuration")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return invalid("invalid JSON configuration")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return invalid("duplicate configuration field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return invalid("invalid JSON configuration")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalid("configuration fields cannot be null")
		}
		// Optional endpoint maps contain security-relevant nested fields too.
		// Reject ambiguous duplicate keys and null policies at every object level.
		if trimmed := bytes.TrimSpace(value); len(trimmed) > 0 && trimmed[0] == '{' {
			if err := validateConfigObject(trimmed); err != nil {
				return err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return invalid("invalid JSON configuration")
	}
	return nil
}
