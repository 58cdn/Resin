package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const (
	indexFetchTimeout = 15 * time.Second
	maxIndexBytes     = 5 << 20
	// MaxPackageBytes bounds a downloaded or uploaded plugin archive.
	MaxPackageBytes = 100 << 20
)

// Index is the document served by a plugin marketplace URL:
//
//	{
//	  "schema_version": 1,
//	  "name": "My plugins",
//	  "plugins": [
//	    {
//	      "id": "acme.rate-limit", "name": "Rate limit", "version": "1.2.0",
//	      "artifacts": [
//	        {"os": "linux", "arch": "amd64", "url": "rate-limit-linux-amd64.tar.gz", "sha256": "..."},
//	        {"os": "any", "arch": "any", "url": "https://example.com/rate-limit.zip", "sha256": "..."}
//	      ]
//	    }
//	  ]
//	}
//
// Artifact URLs may be relative to the index URL.
type Index struct {
	SchemaVersion int          `json:"schema_version"`
	Name          string       `json:"name,omitempty"`
	Plugins       []IndexEntry `json:"plugins"`
}

// IndexEntry is one plugin in a marketplace index.
type IndexEntry struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Version         string          `json:"version"`
	Description     string          `json:"description,omitempty"`
	Author          string          `json:"author,omitempty"`
	Homepage        string          `json:"homepage,omitempty"`
	License         string          `json:"license,omitempty"`
	Tags            []string        `json:"tags,omitempty"`
	MinResinVersion string          `json:"min_resin_version,omitempty"`
	Artifacts       []IndexArtifact `json:"artifacts"`
}

// IndexArtifact is one downloadable package of a plugin version.
type IndexArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

// MarketplaceError reports a marketplace URL that could not be read.
type MarketplaceError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// MarketplaceListing is the merged view of every configured marketplace.
type MarketplaceListing struct {
	Sources []string           `json:"sources"`
	Plugins []MarketplaceEntry `json:"plugins"`
	Errors  []MarketplaceError `json:"errors"`
}

// Marketplace fetches every configured index and annotates entries with
// local install state. When two indexes list the same id the first wins.
func (m *Manager) Marketplace(ctx context.Context) (MarketplaceListing, error) {
	if !m.cfg.ExternalEnabled {
		return MarketplaceListing{}, ErrExternalDisabled
	}
	listing := MarketplaceListing{
		Sources: []string{},
		Plugins: []MarketplaceEntry{},
		Errors:  []MarketplaceError{},
	}
	seen := make(map[string]bool)
	for _, src := range m.cfg.MarketplaceURLs {
		shown := RedactURL(src)
		listing.Sources = append(listing.Sources, shown)
		idx, err := m.fetchIndex(ctx, src)
		if err != nil {
			listing.Errors = append(listing.Errors, MarketplaceError{URL: shown, Error: err.Error()})
			continue
		}
		for _, ie := range idx.Plugins {
			if seen[ie.ID] {
				continue
			}
			seen[ie.ID] = true
			listing.Plugins = append(listing.Plugins, MarketplaceEntry{IndexEntry: ie, Marketplace: shown})
		}
	}

	m.mu.Lock()
	for i := range listing.Plugins {
		me := &listing.Plugins[i]
		if e, ok := m.entries[me.ID]; ok {
			if e.source == SourceBuiltin {
				me.Reason = "id is reserved by a builtin plugin"
				continue
			}
			me.InstalledVersion = e.manifest.Version
			me.UpdateAvailable = isNewerVersion(me.Version, e.manifest.Version)
		}
		if err := checkMinResinVersion(me.MinResinVersion, m.cfg.ResinVersion); err != nil {
			me.Reason = err.Error()
			continue
		}
		if _, ok := pickArtifact(me.Artifacts, runtime.GOOS, runtime.GOARCH); !ok {
			me.Reason = fmt.Sprintf("no package for %s-%s", runtime.GOOS, runtime.GOARCH)
			continue
		}
		me.Installable = true
	}
	m.mu.Unlock()
	sort.SliceStable(listing.Plugins, func(i, j int) bool { return listing.Plugins[i].ID < listing.Plugins[j].ID })
	return listing, nil
}

func (m *Manager) fetchIndex(ctx context.Context, src string) (*Index, error) {
	u, err := url.Parse(src)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("marketplace URL must be http(s)")
	}
	ctx, cancel := context.WithTimeout(ctx, indexFetchTimeout)
	defer cancel()
	data, err := m.httpGet(ctx, src, maxIndexBytes)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("invalid index: %w", err)
	}
	if idx.SchemaVersion != 0 && idx.SchemaVersion != pluginsdk.SchemaVersion {
		return nil, fmt.Errorf("unsupported index schema_version %d", idx.SchemaVersion)
	}
	valid := idx.Plugins[:0]
	for _, ie := range idx.Plugins {
		if !pluginsdk.ValidPluginID(ie.ID) || strings.TrimSpace(ie.Version) == "" {
			continue
		}
		if ie.Name == "" {
			ie.Name = ie.ID
		}
		arts := ie.Artifacts[:0]
		for _, a := range ie.Artifacts {
			raw := strings.TrimSpace(a.URL)
			if raw == "" {
				continue
			}
			ref, err := url.Parse(raw)
			if err != nil {
				continue
			}
			abs := u.ResolveReference(ref)
			if abs.Scheme != "http" && abs.Scheme != "https" {
				continue
			}
			a.URL = abs.String()
			a.OS = strings.ToLower(strings.TrimSpace(a.OS))
			a.Arch = strings.ToLower(strings.TrimSpace(a.Arch))
			a.SHA256 = strings.ToLower(strings.TrimSpace(a.SHA256))
			arts = append(arts, a)
		}
		ie.Artifacts = arts
		valid = append(valid, ie)
	}
	idx.Plugins = valid
	return &idx, nil
}

func (m *Manager) httpGet(ctx context.Context, src string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: invalid request", RedactURL(src))
	}
	req.Header.Set("User-Agent", "Resin/"+m.cfg.ResinVersion)
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		// *url.Error embeds the full URL, which may carry an access token.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("GET %s: %w", RedactURL(src), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: HTTP %d", RedactURL(src), resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", RedactURL(src), limit)
	}
	return data, nil
}

// RedactURL strips credentials and query parameters from a marketplace URL so
// it can be shown in logs and API responses.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}

// pickArtifact returns the best artifact for goos/goarch: an exact match,
// then an os match with arch "any", then "any"/"any".
func pickArtifact(arts []IndexArtifact, goos, goarch string) (IndexArtifact, bool) {
	best, bestScore := IndexArtifact{}, -1
	for _, a := range arts {
		osOK := a.OS == goos
		archOK := a.Arch == goarch
		score := -1
		switch {
		case osOK && archOK:
			score = 3
		case osOK && (a.Arch == "any" || a.Arch == ""):
			score = 2
		case (a.OS == "any" || a.OS == "") && (a.Arch == "any" || a.Arch == ""):
			score = 1
		}
		if score > bestScore {
			best, bestScore = a, score
		}
	}
	return best, bestScore > 0
}

// Install downloads a plugin from the marketplace and installs (or upgrades)
// it. An upgraded plugin keeps its settings and is restarted if enabled.
func (m *Manager) Install(ctx context.Context, id string) (Info, error) {
	if !m.cfg.ExternalEnabled {
		return Info{}, ErrExternalDisabled
	}
	listing, err := m.Marketplace(ctx)
	if err != nil {
		return Info{}, err
	}
	var target *MarketplaceEntry
	for i := range listing.Plugins {
		if listing.Plugins[i].ID == id {
			target = &listing.Plugins[i]
			break
		}
	}
	if target == nil {
		return Info{}, fmt.Errorf("%w: %s is not listed by any marketplace", ErrNotFound, id)
	}
	if !target.Installable {
		return Info{}, fmt.Errorf("%w: %s", ErrInvalidArgument, target.Reason)
	}
	art, _ := pickArtifact(target.Artifacts, runtime.GOOS, runtime.GOARCH)
	if len(art.SHA256) != sha256.Size*2 {
		return Info{}, fmt.Errorf("%w: marketplace artifact for %s has no valid sha256", ErrInvalidArgument, id)
	}
	if art.Size > MaxPackageBytes {
		return Info{}, fmt.Errorf("%w: package is larger than %d bytes", ErrInvalidArgument, MaxPackageBytes)
	}
	data, err := m.httpGet(ctx, art.URL, MaxPackageBytes)
	if err != nil {
		return Info{}, fmt.Errorf("download: %w", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != art.SHA256 {
		return Info{}, fmt.Errorf("%w: sha256 mismatch for %s (got %s)", ErrInvalidArgument, id, got)
	}
	return m.installArchive(ctx, data, id)
}

// InstallArchive installs an uploaded .zip or .tar.gz plugin package.
func (m *Manager) InstallArchive(ctx context.Context, data []byte) (Info, error) {
	if !m.cfg.ExternalEnabled {
		return Info{}, ErrExternalDisabled
	}
	return m.installArchive(ctx, data, "")
}
