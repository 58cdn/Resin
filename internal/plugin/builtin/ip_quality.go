package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const IPQualityID = "resin.ip-quality"
const IPQualityHostCapability = "resin.ip-quality.host"
const IPQualityStoreCapability = "resin.ip-quality.store"

func ipQualityBuiltin() plugin.Builtin {
	return plugin.Builtin{
		Manifest: pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion, ID: IPQualityID, Name: "IP Quality", Version: "1.0.0",
			Description: "Manual, cold-path IP quality checks. Offline GeoIP and opt-in node egress consistency; no background checks.", Author: "Resin", License: "MIT",
			ConfigFields: []pluginsdk.ConfigField{
				{Name: "cloudflare_challenge_urls", Label: "Optional Cloudflare challenge URLs", Type: pluginsdk.FieldJSON, Default: raw(`[]`), Description: "Explicit public HTTPS targets; no defaults, only used when cloudflare-challenge is enabled and manually requested."},
				{Name: "ai_region_targets", Label: "Optional AI region observation targets", Type: pluginsdk.FieldJSON, Default: raw(`{}`), Description: "Map of claude/chatgpt to {url,supported_countries?}; no default endpoints or country lists. Supported region alone does not prove availability."},
				{Name: "max_concurrent_checks", Label: "Concurrent checks", Type: pluginsdk.FieldInteger, Default: raw(`8`)},
				{Name: "max_cache_entries", Label: "Memory cache entries", Type: pluginsdk.FieldInteger, Default: raw(`4096`)},
				{Name: "source_priority", Label: "Source priority", Type: pluginsdk.FieldJSON, Default: raw(`["db-ip","egress-consistency"]`)},
				{Name: "enabled_sources", Label: "Enabled sources", Type: pluginsdk.FieldJSON, Default: raw(`["egress-consistency","db-ip"]`)},
				{Name: "ip_result_ttl", Label: "IP result TTL", Type: pluginsdk.FieldString, Default: raw(`"24h"`)},
				{Name: "node_result_ttl", Label: "Node result TTL", Type: pluginsdk.FieldString, Default: raw(`"6h"`)},
				{Name: "request_timeout_ms", Label: "Request timeout (ms)", Type: pluginsdk.FieldInteger, Default: raw(`10000`)},
				{Name: "consistency_targets", Label: "HTTPS consistency targets", Type: pluginsdk.FieldJSON, Default: raw(`["https://cloudflare.com/cdn-cgi/trace"]`)},
				{Name: "background_enabled", Label: "Background checks (unsupported in P1)", Type: pluginsdk.FieldBoolean, Default: raw(`false`)},
			},
		},
		NewWithHost: func(host plugin.BuiltinHost) pluginsdk.Plugin { return &ipQuality{host: host} },
	}
}

type ipQuality struct {
	host   plugin.BuiltinHost
	engine atomic.Pointer[ipcheck.Engine]
}

func (p *ipQuality) Configure(_ context.Context, raw json.RawMessage) error {
	cfg, err := ipcheck.ParseConfig(raw)
	if err != nil {
		return err
	}
	if p.host == nil {
		return fmt.Errorf("IP quality host unavailable")
	}
	h, ok := p.host.Capability(IPQualityHostCapability)
	if !ok {
		return fmt.Errorf("IP quality host unavailable")
	}
	host, ok := h.(ipcheck.Host)
	if !ok {
		return fmt.Errorf("IP quality host unavailable")
	}
	var store ipcheck.Store
	if s, exists := p.host.Capability(IPQualityStoreCapability); exists {
		store, _ = s.(ipcheck.Store)
	}
	engine, err := ipcheck.NewEngine(host, store, cfg)
	if err != nil {
		return err
	}
	if old := p.engine.Swap(engine); old != nil {
		_ = old.Close()
	}
	return nil
}
func (p *ipQuality) BuiltinService() any {
	if e := p.engine.Load(); e != nil {
		return e
	}
	return nil
}
func (p *ipQuality) Close() error {
	if old := p.engine.Swap(nil); old != nil {
		return old.Close()
	}
	return nil
}
