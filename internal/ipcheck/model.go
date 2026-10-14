// Package ipcheck implements opt-in, cold-path IP quality checks. It never
// changes node health, latency, circuit breakers, or routing state.
package ipcheck

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

type SourceKind string

const (
	SourceKindViaNode SourceKind = "via_node"
	SourceKindByIP    SourceKind = "by_ip"
	SubjectNode                  = "node_hash"
	SubjectIP                    = "egress_ip"
)

var (
	ErrNodeNotFound = errors.New("node not found")
	ErrNoEgressIP   = errors.New("node has no public egress IP")
	ErrNotChecked   = errors.New("no unexpired IP check results")
	ErrClosed       = errors.New("IP quality plugin is stopped")
	ErrNodeChanged  = errors.New("node egress IP changed during check; retry")
)

// ValidationError is safe to expose through the API. It contains no host errors.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// NodeSnapshot is detached from mutable node state. A host must not return a
// pointer to its pool or outbound transport to the engine.
type NodeSnapshot struct {
	Hash          node.Hash
	EgressIP      netip.Addr
	EgressVersion int64
	Enabled       bool
	Healthy       bool
	Platforms     []string
	Subscriptions []string
}

type GeoIPInfo struct {
	Country string  `json:"country"`
	City    string  `json:"city"`
	ASN     *uint32 `json:"asn"`
	ASOrg   string  `json:"as_org"`
}

// Host is available only to the builtin adapter. DoViaNode must validate DNS
// resolution and every redirect against the same public-HTTPS target policy,
// route through the selected node, and honor req.Context()/ctx cancellation.
// Host implementations must not record these calls as node health probes.
type Host interface {
	NodeSnapshot(context.Context, node.Hash) (NodeSnapshot, bool)
	DoViaNode(context.Context, node.Hash, *http.Request) (*http.Response, error)
	GeoIPLookup(context.Context, netip.Addr) (GeoIPInfo, bool)
}

// Store persists raw source results in the existing cache lifecycle. Load
// returns (nil,nil) on a miss. Store errors never fail an otherwise valid check.
// Implementations must honor context cancellation and support concurrent calls.
type Store interface {
	Load(ctx context.Context, subjectKind, subject, source string) (*SourceResult, error)
	Save(ctx context.Context, subjectKind, subject string, result *SourceResult) error
	DeleteNode(ctx context.Context, hash node.Hash) error
}

type CheckRequest struct {
	NodeHash      node.Hash
	EgressIP      netip.Addr
	EgressVersion int64
	HTTP          *http.Client
}

type Source interface {
	Name() string
	Kind() SourceKind
	RequiresKey() bool
	Check(context.Context, CheckRequest) (*SourceResult, error)
}

// Data is the lossless, typed source payload used to recompute the summary.
// Unknown numeric and boolean values remain nil, not zero/false.
type Data struct {
	Country          string            `json:"country,omitempty"`
	City             string            `json:"city,omitempty"`
	ASN              *uint32           `json:"asn,omitempty"`
	ASOrg            string            `json:"as_org,omitempty"`
	IPType           string            `json:"ip_type,omitempty"`
	IPOrigin         string            `json:"ip_origin,omitempty"`
	FraudScore       *float64          `json:"fraud_score,omitempty"`
	AbuseFlags       []string          `json:"abuse_flags,omitempty"`
	CFChallenge      string            `json:"cf_challenge,omitempty"`
	EgressConsistent *bool             `json:"egress_consistent,omitempty"`
	EgressIPsBySite  map[string]string `json:"egress_ips_by_site,omitempty"`
	Services         map[string]string `json:"services,omitempty"`
	TargetErrors     map[string]string `json:"target_errors,omitempty"`
	Attribution      string            `json:"attribution,omitempty"`
}

type SourceResult struct {
	EgressVersion     int64      `json:"egress_version,omitempty"`
	ConfigFingerprint string     `json:"config_fingerprint,omitempty"`
	Source            string     `json:"source"`
	Kind              SourceKind `json:"kind"`
	OK                bool       `json:"ok"`
	Status            string     `json:"status"` // ok, error, or unavailable
	CheckedAt         time.Time  `json:"checked_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	EgressIP          string     `json:"egress_ip,omitempty"`
	Data              Data       `json:"data"`
	Error             string     `json:"error,omitempty"`
}

type Summary struct {
	Country          string            `json:"country"`
	City             string            `json:"city"`
	ASN              *uint32           `json:"asn"`
	ASOrg            string            `json:"as_org"`
	IPType           string            `json:"ip_type"`
	IPOrigin         string            `json:"ip_origin"`
	FraudScore       *float64          `json:"fraud_score"`
	AbuseFlags       []string          `json:"abuse_flags"`
	CFChallenge      string            `json:"cf_challenge"`
	EgressConsistent *bool             `json:"egress_consistent"`
	EgressIPsBySite  map[string]string `json:"egress_ips_by_site"`
	Services         map[string]string `json:"services"`
	RiskLevel        string            `json:"risk_level"`
}

type Result struct {
	EgressIP          string             `json:"egress_ip"`
	CheckedAt         time.Time          `json:"checked_at,omitzero"`
	Summary           Summary            `json:"summary"`
	Sources           []SourceResult     `json:"sources"`
	SuppressedSources []SuppressedSource `json:"suppressed_sources,omitempty"`
}

type SourceStatus struct {
	Name        string     `json:"name"`
	Kind        SourceKind `json:"kind"`
	Enabled     bool       `json:"enabled"`
	Available   bool       `json:"available"`
	RequiresKey bool       `json:"requires_key"`
	Reason      string     `json:"reason,omitempty"`
	Attribution string     `json:"attribution,omitempty"`
}
