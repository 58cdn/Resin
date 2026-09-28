package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// --- helpers (ti prefix) ---

type tiRoute struct {
	status int
	body   []byte
}

// tiMarket is an httptest server serving canned responses by URL path.
type tiMarket struct {
	srv *httptest.Server

	mu     sync.Mutex
	routes map[string]tiRoute
	hits   map[string]int
	agents map[string]bool
}

func tiNewMarket(t *testing.T) *tiMarket {
	t.Helper()
	mk := &tiMarket{routes: map[string]tiRoute{}, hits: map[string]int{}, agents: map[string]bool{}}
	mk.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mk.mu.Lock()
		rt, ok := mk.routes[r.URL.Path]
		mk.hits[r.URL.Path]++
		mk.agents[r.UserAgent()] = true
		mk.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rt.status != 0 {
			w.WriteHeader(rt.status)
		}
		_, _ = w.Write(rt.body)
	}))
	t.Cleanup(mk.srv.Close)
	return mk
}

func (mk *tiMarket) set(path string, status int, body []byte) {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	mk.routes[path] = tiRoute{status: status, body: body}
}

func (mk *tiMarket) setIndex(t *testing.T, path string, idx Index) {
	t.Helper()
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	mk.set(path, 0, data)
}

func (mk *tiMarket) url(path string) string { return mk.srv.URL + path }

func (mk *tiMarket) hitCount(path string) int {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	return mk.hits[path]
}

func (mk *tiMarket) sawAgent(ua string) bool {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	return mk.agents[ua]
}

func tiSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func tiBuiltin(id, version string) Builtin {
	return Builtin{
		Manifest: pluginsdk.Manifest{SchemaVersion: pluginsdk.SchemaVersion, ID: id, Name: "Builtin " + id, Version: version},
		New:      func() pluginsdk.Plugin { return tiNopPlugin{} },
	}
}

// tiAnyArtifact is an artifact usable on every platform.
func tiAnyArtifact(url, sha string) IndexArtifact {
	return IndexArtifact{OS: "any", Arch: "any", URL: url, SHA256: sha}
}

func tiMarketEntry(t *testing.T, listing MarketplaceListing, id string) MarketplaceEntry {
	t.Helper()
	for _, p := range listing.Plugins {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("%s not in marketplace listing", id)
	return MarketplaceEntry{}
}

// tiRoundTripper lets a test fail HTTP requests without touching the network.
type tiRoundTripper func(*http.Request) (*http.Response, error)

func (f tiRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// --- RedactURL / pickArtifact ---

func TestMarketplaceRedactURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://user:pass@example.com/index.json?token=abc", "https://example.com/index.json"},
		{"https://user@example.com/a/b.json", "https://example.com/a/b.json"},
		{"https://example.com/index.json?sig=1&x=2", "https://example.com/index.json"},
		{"https://example.com:8443/plugins/index.json", "https://example.com:8443/plugins/index.json"},
		{"http://127.0.0.1/i.json", "http://127.0.0.1/i.json"},
		{"://missing-scheme", "<invalid url>"},
		{"http://[::1", "<invalid url>"},
		{"http://example.com/%zz", "<invalid url>"},
	}
	for _, tc := range tests {
		got := RedactURL(tc.in)
		if got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "pass") || strings.Contains(got, "token") || strings.Contains(got, "sig=") {
			t.Errorf("RedactURL(%q) = %q leaks credentials", tc.in, got)
		}
	}
}

func TestMarketplacePickArtifact(t *testing.T) {
	exact := IndexArtifact{OS: "linux", Arch: "amd64", URL: "exact"}
	exact2 := IndexArtifact{OS: "linux", Arch: "amd64", URL: "exact2"}
	osAny := IndexArtifact{OS: "linux", Arch: "any", URL: "os-any"}
	osEmpty := IndexArtifact{OS: "linux", Arch: "", URL: "os-empty"}
	anyAny := IndexArtifact{OS: "any", Arch: "any", URL: "any-any"}
	anyAny2 := IndexArtifact{OS: "any", Arch: "any", URL: "any-any2"}
	emptyEmpty := IndexArtifact{URL: "empty-empty"}
	otherOS := IndexArtifact{OS: "darwin", Arch: "amd64", URL: "darwin"}
	otherOSAny := IndexArtifact{OS: "windows", Arch: "any", URL: "windows-any"}
	otherArch := IndexArtifact{OS: "linux", Arch: "arm64", URL: "arm64"}

	tests := []struct {
		name string
		arts []IndexArtifact
		want string // "" => no match
	}{
		{"exact first", []IndexArtifact{exact, osAny, anyAny}, "exact"},
		{"exact last", []IndexArtifact{anyAny, osAny, exact}, "exact"},
		{"os+any over any/any", []IndexArtifact{anyAny, osAny}, "os-any"},
		{"empty arch counts as any", []IndexArtifact{anyAny, osEmpty}, "os-empty"},
		{"empty os/arch counts as any/any", []IndexArtifact{otherOS, emptyEmpty}, "empty-empty"},
		{"any/any fallback", []IndexArtifact{otherOS, otherArch, otherOSAny, anyAny}, "any-any"},
		{"first exact wins ties", []IndexArtifact{exact, exact2}, "exact"},
		{"first any/any wins ties", []IndexArtifact{anyAny2, anyAny}, "any-any2"},
		{"no compatible artifact", []IndexArtifact{otherOS, otherArch, otherOSAny}, ""},
		{"empty list", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickArtifact(tc.arts, "linux", "amd64")
			if tc.want == "" {
				if ok {
					t.Fatalf("pickArtifact = %+v, want no match", got)
				}
				return
			}
			if !ok || got.URL != tc.want {
				t.Fatalf("pickArtifact = %+v, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

// --- Marketplace listing ---

func TestMarketplaceListing(t *testing.T) {
	ctx := context.Background()
	mk := tiNewMarket(t)
	goosArch := runtime.GOOS + "-" + runtime.GOARCH
	sha := strings.Repeat("ab", 32)

	mk.setIndex(t, "/a/index.json", Index{SchemaVersion: 1, Name: "A", Plugins: []IndexEntry{
		{ID: "acme.zeta", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg/zeta.zip", sha)}},
		{ID: "acme.dup", Name: "From A", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("dup.zip", sha)}},
		{ID: "acme.dup", Name: "From A again", Version: "2.0.0"},
		{ID: "acme.builtin", Name: "Shadow", Version: "9.9.9", Artifacts: []IndexArtifact{tiAnyArtifact("b.zip", sha)}},
		{ID: "acme.future", Version: "1.0.0", MinResinVersion: "9.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("f.zip", sha)}},
		{ID: "acme.noart", Version: "1.0.0", Artifacts: []IndexArtifact{{OS: "plan9", Arch: "mips", URL: "p.zip", SHA256: sha}}},
		{ID: "acme.installed", Version: "1.1.0", Artifacts: []IndexArtifact{tiAnyArtifact("i.zip", sha)}},
		{ID: "acme.big", Version: "1.9.0", Artifacts: []IndexArtifact{tiAnyArtifact("big.zip", sha)}},
		{ID: "Bad ID", Version: "1.0.0"},
		{ID: "../escape", Version: "1.0.0"},
		{ID: "acme.nover", Version: "  "},
		{ID: "acme.arts", Version: "1.0.0", Artifacts: []IndexArtifact{
			{OS: "any", Arch: "any", URL: "file:///etc/passwd", SHA256: sha},
			{OS: "any", Arch: "any", URL: "", SHA256: sha},
			{OS: "any", Arch: "any", URL: "ftp://example.com/x.zip", SHA256: sha},
			{OS: "any", Arch: "any", URL: "javascript:alert(1)", SHA256: sha},
			{OS: " LINUX ", Arch: "AMD64", URL: "https://cdn.example.com/abs.zip", SHA256: " " + strings.ToUpper(sha) + " "},
			{OS: "any", Arch: "any", URL: "//cdn.example.com/proto-relative.zip", SHA256: sha},
			{OS: "any", Arch: "any", URL: "/root.zip", SHA256: sha},
		}},
	}})
	mk.set("/broken/index.json", http.StatusInternalServerError, []byte("boom"))
	mk.set("/junk/index.json", 0, []byte("<html>not json</html>"))
	mk.setIndex(t, "/v2/index.json", Index{SchemaVersion: 2, Plugins: []IndexEntry{{ID: "acme.v2", Version: "1.0.0"}}})
	mk.setIndex(t, "/b/index.json", Index{Plugins: []IndexEntry{ // schema_version 0 is accepted
		{ID: "acme.dup", Name: "From B", Version: "3.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("dup.zip", sha)}},
		{ID: "acme.alpha", Version: "2.0.0", Artifacts: []IndexArtifact{tiAnyArtifact(mk.url("/b/alpha.tar.gz"), sha)}},
	}})

	urls := []string{
		mk.url("/a/index.json?token=secret-a"),
		mk.url("/broken/index.json?token=secret-broken"),
		"ftp://user:secret-ftp@example.com/index.json",
		mk.url("/junk/index.json"),
		mk.url("/v2/index.json"),
		mk.url("/b/index.json"),
	}
	m := tiNewManager(t, ManagerConfig{
		ExternalEnabled: true,
		MarketplaceURLs: urls,
		HTTPClient:      mk.srv.Client(),
		Builtins:        []Builtin{tiBuiltin("acme.builtin", "1.0.0")},
	})
	for id, version := range map[string]string{"acme.installed": "1.0.0", "acme.big": "1.10.0"} {
		if _, err := m.InstallArchive(ctx, tiZip(t, tiPackage(tiManifest(t, id, version, nil), "")...)); err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
	}

	listing, err := m.Marketplace(ctx)
	if err != nil {
		t.Fatalf("Marketplace: %v", err)
	}
	blob, _ := json.Marshal(listing)
	if strings.Contains(string(blob), "secret") {
		t.Fatalf("listing leaks a marketplace secret: %s", blob)
	}
	if !mk.sawAgent("Resin/1.0.0") {
		t.Fatalf("index requests did not send User-Agent Resin/1.0.0")
	}

	wantSources := []string{
		mk.url("/a/index.json"),
		mk.url("/broken/index.json"),
		"ftp://example.com/index.json",
		mk.url("/junk/index.json"),
		mk.url("/v2/index.json"),
		mk.url("/b/index.json"),
	}
	if fmt.Sprint(listing.Sources) != fmt.Sprint(wantSources) {
		t.Fatalf("Sources = %v, want %v", listing.Sources, wantSources)
	}

	wantErrs := map[string]string{
		mk.url("/broken/index.json"):   "HTTP 500",
		"ftp://example.com/index.json": "must be http(s)",
		mk.url("/junk/index.json"):     "invalid index",
		mk.url("/v2/index.json"):       "schema_version 2",
	}
	if len(listing.Errors) != len(wantErrs) {
		t.Fatalf("Errors = %+v, want %d", listing.Errors, len(wantErrs))
	}
	for _, e := range listing.Errors {
		want, ok := wantErrs[e.URL]
		if !ok || !strings.Contains(e.Error, want) {
			t.Errorf("error %+v, want URL in %v with message containing %q", e, wantErrs, want)
		}
	}

	var ids []string
	for _, p := range listing.Plugins {
		ids = append(ids, p.ID)
	}
	wantIDs := []string{"acme.alpha", "acme.arts", "acme.big", "acme.builtin", "acme.dup", "acme.future", "acme.installed", "acme.noart", "acme.zeta"}
	if fmt.Sprint(ids) != fmt.Sprint(wantIDs) {
		t.Fatalf("plugin ids = %v, want %v (sorted, invalid entries dropped, duplicates merged)", ids, wantIDs)
	}

	indexA, indexB := mk.url("/a/index.json"), mk.url("/b/index.json")

	zeta := tiMarketEntry(t, listing, "acme.zeta")
	if !zeta.Installable || zeta.Reason != "" || zeta.Marketplace != indexA || zeta.Name != "acme.zeta" ||
		zeta.InstalledVersion != "" || zeta.UpdateAvailable {
		t.Errorf("acme.zeta = %+v", zeta)
	}
	if len(zeta.Artifacts) != 1 || zeta.Artifacts[0].URL != mk.url("/a/pkg/zeta.zip") {
		t.Errorf("relative artifact not resolved against the index URL: %+v", zeta.Artifacts)
	}

	dup := tiMarketEntry(t, listing, "acme.dup")
	if dup.Name != "From A" || dup.Version != "1.0.0" || dup.Marketplace != indexA {
		t.Errorf("duplicate id: got %+v, want the first entry of the first index", dup)
	}

	alpha := tiMarketEntry(t, listing, "acme.alpha")
	if !alpha.Installable || alpha.Marketplace != indexB || alpha.Artifacts[0].URL != mk.url("/b/alpha.tar.gz") {
		t.Errorf("acme.alpha = %+v", alpha)
	}

	bi := tiMarketEntry(t, listing, "acme.builtin")
	if bi.Installable || bi.Reason != "id is reserved by a builtin plugin" || bi.InstalledVersion != "" || bi.UpdateAvailable {
		t.Errorf("acme.builtin = %+v", bi)
	}

	future := tiMarketEntry(t, listing, "acme.future")
	if future.Installable || !strings.Contains(future.Reason, "9.0.0") {
		t.Errorf("acme.future = %+v, want a min_resin_version reason", future)
	}

	noart := tiMarketEntry(t, listing, "acme.noart")
	if noart.Installable || noart.Reason != "no package for "+goosArch {
		t.Errorf("acme.noart = %+v", noart)
	}

	inst := tiMarketEntry(t, listing, "acme.installed")
	if inst.InstalledVersion != "1.0.0" || !inst.UpdateAvailable || !inst.Installable {
		t.Errorf("acme.installed = %+v, want an available update", inst)
	}

	big := tiMarketEntry(t, listing, "acme.big")
	if big.InstalledVersion != "1.10.0" || big.UpdateAvailable || !big.Installable {
		t.Errorf("acme.big = %+v, want no update (1.9.0 is older than 1.10.0)", big)
	}

	arts := tiMarketEntry(t, listing, "acme.arts").Artifacts
	wantArts := []IndexArtifact{
		{OS: "linux", Arch: "amd64", URL: "https://cdn.example.com/abs.zip", SHA256: sha},
		{OS: "any", Arch: "any", URL: "http://cdn.example.com/proto-relative.zip", SHA256: sha},
		{OS: "any", Arch: "any", URL: mk.url("/root.zip"), SHA256: sha},
	}
	if fmt.Sprint(arts) != fmt.Sprint(wantArts) {
		t.Errorf("acme.arts artifacts =\n%+v\nwant\n%+v", arts, wantArts)
	}
}

func TestMarketplaceEmptyAndDisabled(t *testing.T) {
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true})
	listing, err := m.Marketplace(context.Background())
	if err != nil {
		t.Fatalf("Marketplace without URLs: %v", err)
	}
	if listing.Sources == nil || listing.Plugins == nil || listing.Errors == nil {
		t.Fatalf("listing has nil slices (would serialize as null): %+v", listing)
	}
	if len(listing.Sources)+len(listing.Plugins)+len(listing.Errors) != 0 {
		t.Fatalf("listing = %+v, want empty", listing)
	}

	off := tiNewManager(t, ManagerConfig{ExternalEnabled: false, MarketplaceURLs: []string{"https://example.com/index.json"}})
	if _, err := off.Marketplace(context.Background()); !errors.Is(err, ErrExternalDisabled) {
		t.Fatalf("Marketplace with external plugins disabled: %v", err)
	}
	if _, err := off.Install(context.Background(), "acme.demo"); !errors.Is(err, ErrExternalDisabled) {
		t.Fatalf("Install with external plugins disabled: %v", err)
	}
}

func TestMarketplaceIndexTooLarge(t *testing.T) {
	mk := tiNewMarket(t)
	mk.set("/index.json", 0, make([]byte, maxIndexBytes+1))
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, MarketplaceURLs: []string{mk.url("/index.json")}, HTTPClient: mk.srv.Client()})
	listing, err := m.Marketplace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Errors) != 1 || !strings.Contains(listing.Errors[0].Error, "exceeds") {
		t.Fatalf("Errors = %+v, want a size error", listing.Errors)
	}
}

func TestMarketplaceTransportErrorIsRedacted(t *testing.T) {
	client := &http.Client{Transport: tiRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial refused")
	})}
	src := "https://user:secret-pw@marketplace.invalid/index.json?token=secret-token"
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, MarketplaceURLs: []string{src}, HTTPClient: client})
	listing, err := m.Marketplace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Errors) != 1 {
		t.Fatalf("Errors = %+v", listing.Errors)
	}
	e := listing.Errors[0]
	if e.URL != "https://marketplace.invalid/index.json" || !strings.Contains(e.Error, "dial refused") {
		t.Fatalf("error = %+v", e)
	}
	if strings.Contains(e.Error, "secret") || strings.Contains(e.URL, "secret") {
		t.Fatalf("transport error leaks the URL secret: %+v", e)
	}
}

func TestMarketplaceDropsBlankArtifactURL(t *testing.T) {
	mk := tiNewMarket(t)
	mk.setIndex(t, "/index.json", Index{SchemaVersion: 1, Plugins: []IndexEntry{
		{ID: "acme.blank", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("   ", strings.Repeat("0", 64))}},
	}})
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, MarketplaceURLs: []string{mk.url("/index.json")}, HTTPClient: mk.srv.Client()})
	listing, err := m.Marketplace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blank := tiMarketEntry(t, listing, "acme.blank")
	if len(blank.Artifacts) != 0 {
		t.Fatalf("blank artifact URL kept: %+v", blank.Artifacts)
	}
	if blank.Installable {
		t.Fatalf("entry with only a blank artifact URL is installable: %+v", blank)
	}
}

// --- Install ---

func TestMarketplaceInstallAndUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mk := tiNewMarket(t)
	v1 := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", nil), "acme.demo/", tiEntry{Name: "acme.demo/v1.txt", Body: "1"})...)
	v2 := tiTarGz(t, tiPackage(tiManifest(t, "acme.demo", "1.1.0", nil), "", tiEntry{Name: "v2.txt", Body: "2"})...)
	mk.set("/pkg/demo-1.0.0.zip", 0, v1)
	mk.set("/pkg/demo-1.1.0.tar.gz", 0, v2)
	// Upper-case digests are normalized.
	mk.setIndex(t, "/index.json", Index{SchemaVersion: 1, Plugins: []IndexEntry{
		{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg/demo-1.0.0.zip", strings.ToUpper(tiSHA(v1)))}},
	}})
	m := tiNewManager(t, ManagerConfig{
		ExternalEnabled: true,
		PluginDir:       dir,
		MarketplaceURLs: []string{mk.url("/index.json?token=t")},
		HTTPClient:      mk.srv.Client(),
	})

	info, err := m.Install(ctx, "acme.demo")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if info.ID != "acme.demo" || info.Version != "1.0.0" || info.Source != SourcePackage || info.Enabled || info.Status != StatusStopped {
		t.Fatalf("Install info = %+v", info)
	}
	if !tiExists(filepath.Join(dir, "acme.demo", "v1.txt")) {
		t.Fatal("package files missing after install")
	}
	if list := m.List(); len(list) != 1 || list[0].ID != "acme.demo" {
		t.Fatalf("List = %+v", list)
	}
	if _, err := m.Update(ctx, "acme.demo", Update{Priority: tiIntPtr(3)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	mk.setIndex(t, "/index.json", Index{SchemaVersion: 1, Plugins: []IndexEntry{
		{ID: "acme.demo", Version: "1.1.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg/demo-1.1.0.tar.gz", tiSHA(v2))}},
	}})
	listing, err := m.Marketplace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e := tiMarketEntry(t, listing, "acme.demo"); e.InstalledVersion != "1.0.0" || !e.UpdateAvailable {
		t.Fatalf("before upgrade: %+v", e)
	}
	info, err = m.Install(ctx, "acme.demo")
	if err != nil {
		t.Fatalf("upgrade Install: %v", err)
	}
	if info.Version != "1.1.0" || info.Priority != 3 {
		t.Fatalf("upgraded info = %+v", info)
	}
	if tiExists(filepath.Join(dir, "acme.demo", "v1.txt")) || !tiExists(filepath.Join(dir, "acme.demo", "v2.txt")) {
		t.Fatal("upgrade did not replace the package files")
	}
	listing, err = m.Marketplace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e := tiMarketEntry(t, listing, "acme.demo"); e.InstalledVersion != "1.1.0" || e.UpdateAvailable {
		t.Fatalf("after upgrade: %+v", e)
	}
	if got := tiDirNames(t, dir); len(got) != 1 || got[0] != "acme.demo" {
		t.Fatalf("plugin dir contents = %v", got)
	}
}

func TestMarketplaceInstallHonorsSourcePrecedence(t *testing.T) {
	pkg := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", nil), "")...)
	mk := tiNewMarket(t)
	mk.set("/first/index.json", http.StatusBadGateway, []byte("first source unavailable"))
	mk.set("/second/pkg.zip", 0, pkg)
	mk.setIndex(t, "/second/index.json", Index{SchemaVersion: 1, Plugins: []IndexEntry{{
		ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg))},
	}}})
	m := tiNewManager(t, ManagerConfig{
		ExternalEnabled: true,
		PluginDir:       t.TempDir(),
		MarketplaceURLs: []string{mk.url("/first/index.json"), mk.url("/second/index.json")},
		HTTPClient:      mk.srv.Client(),
	})
	_, err := m.Install(context.Background(), "acme.demo")
	if !errors.Is(err, ErrMarketplaceUnavailable) {
		t.Fatalf("Install error = %v, want ErrMarketplaceUnavailable", err)
	}
	if got := mk.hitCount("/second/pkg.zip"); got != 0 {
		t.Fatalf("lower-priority artifact downloaded %d times", got)
	}
}

func TestMarketplaceInstallRejects(t *testing.T) {
	good := func(t *testing.T) []byte {
		return tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", nil), "")...)
	}
	tests := []struct {
		name     string
		id       string
		pkg      func(t *testing.T) []byte // served at /pkg.zip
		entry    func(pkg []byte) IndexEntry
		wantErr  error // nil => only check the message
		wantMsg  string
		wantHits int // expected downloads of /pkg.zip
	}{
		{
			name: "sha256 mismatch", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA([]byte("other")))}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "sha256 mismatch", wantHits: 1,
		},
		{
			name: "missing sha256", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", "")}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "no package for",
		},
		{
			name: "short sha256", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg)[:63])}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "no package for",
		},
		{
			name: "not listed", id: "acme.missing", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg))}}
			},
			wantErr: ErrNotFound, wantMsg: "not listed",
		},
		{
			name: "requires newer resin", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", MinResinVersion: "9.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg))}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "9.0.0",
		},
		{
			name: "no artifact for platform", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{{OS: "plan9", Arch: "mips", URL: "pkg.zip", SHA256: tiSHA(pkg)}}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "no package for",
		},
		{
			name: "builtin id", id: "acme.builtin", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.builtin", Version: "2.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg))}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "reserved by a builtin",
		},
		{
			name: "package id differs from index id", id: "acme.demo",
			pkg: func(t *testing.T) []byte {
				return tiZip(t, tiPackage(tiManifest(t, "acme.other", "1.0.0", nil), "")...)
			},
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg))}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "does not match", wantHits: 1,
		},
		{
			name: "declared size too large", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				a := tiAnyArtifact("pkg.zip", tiSHA(pkg))
				a.Size = MaxPackageBytes + 1
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{a}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "no package for",
		},
		{
			name: "download 404", id: "acme.demo", pkg: good,
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("missing.zip?sig=secret", tiSHA(pkg))}}
			},
			wantErr: ErrMarketplaceDownload, wantMsg: "HTTP 404",
		},
		{
			name: "valid digest but not an archive", id: "acme.demo",
			pkg: func(t *testing.T) []byte { return []byte("just some text") },
			entry: func(pkg []byte) IndexEntry {
				return IndexEntry{ID: "acme.demo", Version: "1.0.0", Artifacts: []IndexArtifact{tiAnyArtifact("pkg.zip", tiSHA(pkg))}}
			},
			wantErr: ErrInvalidArgument, wantMsg: "unsupported package format", wantHits: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mk := tiNewMarket(t)
			pkg := tc.pkg(t)
			mk.set("/pkg.zip", 0, pkg)
			mk.setIndex(t, "/index.json", Index{SchemaVersion: 1, Plugins: []IndexEntry{tc.entry(pkg)}})
			m := tiNewManager(t, ManagerConfig{
				ExternalEnabled: true,
				PluginDir:       dir,
				MarketplaceURLs: []string{mk.url("/index.json")},
				HTTPClient:      mk.srv.Client(),
				Builtins:        []Builtin{tiBuiltin("acme.builtin", "1.0.0")},
			})
			_, err := m.Install(context.Background(), tc.id)
			if err == nil {
				t.Fatal("Install succeeded")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v is not %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not contain %q", err, tc.wantMsg)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks an artifact URL secret: %v", err)
			}
			if got := mk.hitCount("/pkg.zip"); got != tc.wantHits {
				t.Fatalf("package downloaded %d times, want %d", got, tc.wantHits)
			}
			for _, in := range m.List() {
				if in.Source == SourcePackage {
					t.Fatalf("package installed despite the error: %+v", in)
				}
			}
			if got := tiDirNames(t, dir); len(got) != 0 {
				t.Fatalf("plugin dir not clean after failed install: %v", got)
			}
		})
	}
}
