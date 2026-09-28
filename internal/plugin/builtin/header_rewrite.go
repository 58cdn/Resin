package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/internal/proxy"
	"github.com/Resinat/Resin/pkg/pluginsdk"
	"golang.org/x/net/http/httpguts"
)

// HeaderRewriteID is the id of the header-rewrite builtin.
const HeaderRewriteID = "resin.header-rewrite"

func headerRewriteBuiltin() plugin.Builtin {
	return plugin.Builtin{
		Manifest: pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion,
			ID:            HeaderRewriteID,
			Name:          "Header Rewrite",
			Version:       "1.0.0",
			Description:   "Set or remove upstream request headers for reverse proxy and plain HTTP forward proxy requests. Every matching rule is applied in order.",
			Author:        "Resin",
			License:       "MIT",
			Capabilities:  pluginsdk.Capabilities{RequestHook: true},
			ConfigFields: []pluginsdk.ConfigField{
				{Name: "rules", Label: "Rules", Type: pluginsdk.FieldJSON, Default: raw(`[]`),
					Description: `Array of {"platforms":[],"accounts":[],"target_hosts":[],"proxy_types":[],"client_cidrs":[],"remove":["X-Debug"],"set":{"X-Account":"${account}"}}. Values may use ${account}, ${platform}, ${client_ip} and ${target_host}.`},
			},
		},
		New: func() pluginsdk.Plugin { return &headerRewrite{} },
	}
}

type headerRule struct {
	matchSet
	Set    map[string]string `json:"set,omitempty"`
	Remove []string          `json:"remove,omitempty"`
}

type headerRewriteConfig struct {
	Rules []headerRule `json:"rules"`
}

type compiledHeaderValue struct {
	value     string
	templated bool
}

type compiledHeaderRule struct {
	match  compiledMatch
	set    map[string]compiledHeaderValue
	remove []string
}

type headerRewrite struct {
	rules atomic.Pointer[[]compiledHeaderRule]
}

func (h *headerRewrite) Configure(_ context.Context, config json.RawMessage) error {
	var cfg headerRewriteConfig
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	rules := make([]compiledHeaderRule, 0, len(cfg.Rules))
	for i, r := range cfg.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		m, err := r.matchSet.compile(where)
		if err != nil {
			return err
		}
		cr := compiledHeaderRule{match: m, set: make(map[string]compiledHeaderValue, len(r.Set))}
		for name, value := range r.Set {
			key := http.CanonicalHeaderKey(strings.TrimSpace(name))
			if err := checkHeaderName(where+".set", key); err != nil {
				return err
			}
			if !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("%s.set[%s]: invalid header value", where, key)
			}
			cr.set[key] = compiledHeaderValue{value: value, templated: strings.Contains(value, "${")}
		}
		for _, name := range r.Remove {
			key := http.CanonicalHeaderKey(strings.TrimSpace(name))
			if err := checkHeaderName(where+".remove", key); err != nil {
				return err
			}
			cr.remove = append(cr.remove, key)
		}
		if len(cr.set) == 0 && len(cr.remove) == 0 {
			return fmt.Errorf("%s: set or remove is required", where)
		}
		rules = append(rules, cr)
	}
	h.rules.Store(&rules)
	return nil
}

func checkHeaderName(where, key string) error {
	if !httpguts.ValidHeaderFieldName(key) {
		return fmt.Errorf("%s: invalid header name %q", where, key)
	}
	if proxy.IsProtectedHookHeader(key) {
		return fmt.Errorf("%s: header %s cannot be rewritten", where, key)
	}
	return nil
}

func sanitizeHeaderTemplateValue(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, value)
}

func (h *headerRewrite) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	rules := h.rules.Load()
	if rules == nil || req.IsConnect || req.ProxyType == pluginsdk.ProxyTypeSocks5 {
		return nil, nil
	}
	var dec *pluginsdk.RequestDecision
	var replacer *strings.Replacer
	newReplacer := func() *strings.Replacer {
		if replacer == nil {
			replacer = strings.NewReplacer(
				"${account}", sanitizeHeaderTemplateValue(req.Account),
				"${platform}", sanitizeHeaderTemplateValue(req.Platform),
				"${client_ip}", sanitizeHeaderTemplateValue(req.ClientIP),
				"${target_host}", sanitizeHeaderTemplateValue(req.TargetHost),
			)
		}
		return replacer
	}
	for _, r := range *rules {
		if !r.match.match(req.ProxyType, req.ClientIP, req.Platform, req.Account, req.TargetHost) {
			continue
		}
		if dec == nil {
			dec = &pluginsdk.RequestDecision{SetHeaders: map[string]string{}}
		}
		for _, name := range r.remove {
			delete(dec.SetHeaders, name)
			dec.RemoveHeaders = append(dec.RemoveHeaders, name)
		}
		for name, value := range r.set {
			if value.templated {
				dec.SetHeaders[name] = newReplacer().Replace(value.value)
			} else {
				dec.SetHeaders[name] = value.value
			}
		}
	}
	return dec, nil
}
