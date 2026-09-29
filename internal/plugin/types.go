// Package plugin implements the Resin plugin host: discovery of builtin and
// package plugins, lifecycle management, the request hook chain used by the
// proxy data plane, asynchronous event delivery, and the plugin marketplace.
//
// The protocol shared with plugins lives in pkg/pluginsdk. Builtin plugins
// implement the same pluginsdk interfaces in-process; package plugins run as
// child processes speaking JSON-RPC over stdio (see process.go).
package plugin

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// Source identifies where a plugin comes from.
const (
	SourceBuiltin = "builtin"
	SourcePackage = "package"
)

// Status values reported for a plugin.
const (
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusError    = "error"
)

// Limits and defaults for per-plugin settings.
const (
	DefaultPriority       = 0
	MinPriority           = -10000
	MaxPriority           = 10000
	DefaultTimeoutMs      = 1000
	MinTimeoutMs          = 10
	MaxTimeoutMs          = 60000
	defaultEventQueueSize = 4096
	defaultEventBatchSize = 256
	defaultEventFlush     = time.Second
	eventDeliveryTimeout  = 10 * time.Second
)

// Errors returned by Manager operations. Callers map them with errors.Is.
var (
	ErrNotFound               = errors.New("plugin not found")
	ErrInvalidArgument        = errors.New("invalid argument")
	ErrConflict               = errors.New("conflict")
	ErrExternalDisabled       = errors.New("external plugins are disabled (set RESIN_EXTERNAL_PLUGINS_ENABLED=true to enable)")
	ErrStartFailed            = errors.New("plugin failed to start")
	ErrMarketplaceUnavailable = errors.New("marketplace unavailable")
	ErrMarketplaceDownload    = errors.New("marketplace download failed")
)

// Builtin describes a plugin compiled into Resin.
type Builtin struct {
	Manifest pluginsdk.Manifest
	// New returns a fresh, unconfigured instance. The instance may implement
	// pluginsdk.RequestInspector, pluginsdk.EventHandler and io.Closer.
	New func() pluginsdk.Plugin
}

// SettingsStore persists per-plugin settings.
type SettingsStore interface {
	ListPluginSettings() ([]model.PluginSettings, error)
	UpsertPluginSettings(settings model.PluginSettings) error
	DeletePluginSettings(id string) error
}

// Update is a partial settings change. Nil fields are left unchanged.
type Update struct {
	Enabled    *bool
	Priority   *int
	TimeoutMs  *int
	FailClosed *bool
	// Config replaces the whole config object when non-nil.
	Config json.RawMessage
}

// Stats are cumulative counters since the plugin entry was loaded.
type Stats struct {
	Requests        int64 `json:"requests"`
	Rejects         int64 `json:"rejects"`
	Errors          int64 `json:"errors"`
	Timeouts        int64 `json:"timeouts"`
	AvgLatencyUs    int64 `json:"avg_latency_us"`
	EventsDelivered int64 `json:"events_delivered"`
	EventsDropped   int64 `json:"events_dropped"`
	EventErrors     int64 `json:"event_errors"`
	Restarts        int64 `json:"restarts"`
}

// Info is the externally visible state of one plugin.
type Info struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Version      string                  `json:"version"`
	Description  string                  `json:"description"`
	Author       string                  `json:"author"`
	Homepage     string                  `json:"homepage"`
	License      string                  `json:"license"`
	Source       string                  `json:"source"`
	Capabilities pluginsdk.Capabilities  `json:"capabilities"`
	ConfigFields []pluginsdk.ConfigField `json:"config_fields"`
	Enabled      bool                    `json:"enabled"`
	Priority     int                     `json:"priority"`
	TimeoutMs    int                     `json:"timeout_ms"`
	FailClosed   bool                    `json:"fail_closed"`
	Config       json.RawMessage         `json:"config"`
	Status       string                  `json:"status"`
	LastError    string                  `json:"last_error"`
	Stats        Stats                   `json:"stats"`
	CreatedAt    string                  `json:"created_at,omitempty"`
	UpdatedAt    string                  `json:"updated_at,omitempty"`
}

// MarketplaceEntry is one plugin listed by a marketplace index, annotated
// with local install state.
type MarketplaceEntry struct {
	IndexEntry
	Marketplace      string `json:"marketplace"`
	InstalledVersion string `json:"installed_version"`
	UpdateAvailable  bool   `json:"update_available"`
	Installable      bool   `json:"installable"`
	Reason           string `json:"reason,omitempty"`

	sourceIndex  int
	rawArtifacts []IndexArtifact
}
