package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// AccessControlID is the id of the access-control builtin.
const AccessControlID = "resin.access-control"

func accessControlBuiltin() plugin.Builtin {
	return plugin.Builtin{
		Manifest: pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion,
			ID:            AccessControlID,
			Name:          "Access Control",
			Version:       "1.0.0",
			Description:   "Allow or deny proxy requests by client IP, platform, account, target host and proxy type. Rules are evaluated in order; the first matching rule wins.",
			Author:        "Resin",
			License:       "MIT",
			Capabilities:  pluginsdk.Capabilities{RequestHook: true},
			ConfigFields: []pluginsdk.ConfigField{
				{Name: "default_action", Label: "Default action", Type: pluginsdk.FieldEnum, EnumValues: []string{"allow", "deny"}, Default: raw(`"allow"`),
					Description: "Action when no rule matches."},
				{Name: "rules", Label: "Rules", Type: pluginsdk.FieldJSON, Default: raw(`[]`),
					Description: `Array of {"action":"allow|deny","client_cidrs":[],"platforms":[],"accounts":[],"target_hosts":[],"proxy_types":[]}. Empty fields match everything; "*" wildcards are supported for platforms and accounts, and target_hosts accepts exact hosts, globs like *.example.com, CIDRs and <local>.`},
				{Name: "reject_status", Label: "Reject status", Type: pluginsdk.FieldInteger, Default: raw(`403`),
					Description: "HTTP status returned for denied requests (400-599)."},
				{Name: "reject_message", Label: "Reject message", Type: pluginsdk.FieldString, Default: raw(`"Request denied by access control"`)},
			},
		},
		New: func() pluginsdk.Plugin { return &accessControl{} },
	}
}

type accessRule struct {
	Action string `json:"action"`
	matchSet
}

type accessControlConfig struct {
	DefaultAction string       `json:"default_action"`
	Rules         []accessRule `json:"rules"`
	RejectStatus  int          `json:"reject_status"`
	RejectMessage string       `json:"reject_message"`
}

type compiledAccessRule struct {
	allow bool
	match compiledMatch
}

type accessControlState struct {
	defaultAllow bool
	rules        []compiledAccessRule
	status       int
	message      string
}

type accessControl struct {
	state atomic.Pointer[accessControlState]
}

func (a *accessControl) Configure(_ context.Context, config json.RawMessage) error {
	cfg := accessControlConfig{DefaultAction: "allow", RejectStatus: http.StatusForbidden}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	st := &accessControlState{status: cfg.RejectStatus, message: strings.TrimSpace(cfg.RejectMessage)}
	switch strings.ToLower(strings.TrimSpace(cfg.DefaultAction)) {
	case "", "allow":
		st.defaultAllow = true
	case "deny":
	default:
		return fmt.Errorf("default_action must be allow or deny")
	}
	if st.status == 0 {
		st.status = http.StatusForbidden
	}
	if st.status < 400 || st.status > 599 {
		return fmt.Errorf("reject_status must be between 400 and 599")
	}
	if st.message == "" {
		st.message = "Request denied by access control"
	}
	for i, r := range cfg.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		var allow bool
		switch strings.ToLower(strings.TrimSpace(r.Action)) {
		case "allow":
			allow = true
		case "deny":
		default:
			return fmt.Errorf("%s.action must be allow or deny", where)
		}
		m, err := r.matchSet.compile(where)
		if err != nil {
			return err
		}
		st.rules = append(st.rules, compiledAccessRule{allow: allow, match: m})
	}
	a.state.Store(st)
	return nil
}

func (a *accessControl) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	st := a.state.Load()
	if st == nil {
		return nil, nil
	}
	allow := st.defaultAllow
	for _, r := range st.rules {
		if r.match.match(req.ProxyType, req.ClientIP, req.Platform, req.Account, req.TargetHost) {
			allow = r.allow
			break
		}
	}
	if allow {
		return nil, nil
	}
	return pluginsdk.Reject(st.status, st.message), nil
}
