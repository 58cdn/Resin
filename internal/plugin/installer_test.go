package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// --- shared helpers (ti prefix) ---

// tiStore is an in-memory SettingsStore.
type tiStore struct {
	mu   sync.Mutex
	rows map[string]model.PluginSettings
}

func tiNewStore() *tiStore { return &tiStore{rows: make(map[string]model.PluginSettings)} }

func (s *tiStore) ListPluginSettings() ([]model.PluginSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.PluginSettings, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func (s *tiStore) UpsertPluginSettings(row model.PluginSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.ID] = row
	return nil
}

func (s *tiStore) DeletePluginSettings(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

func (s *tiStore) get(id string) (model.PluginSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return r, ok
}

// tiNopPlugin is a do-nothing builtin plugin.
type tiNopPlugin struct{}

func (tiNopPlugin) Configure(context.Context, json.RawMessage) error { return nil }

// tiNewManager fills defaults (store, plugin dir, no-op logger, version
// 1.0.0), starts the manager and stops it at test cleanup.
func tiNewManager(t *testing.T, cfg ManagerConfig) *Manager {
	t.Helper()
	if cfg.Store == nil {
		cfg.Store = tiNewStore()
	}
	if cfg.PluginDir == "" {
		cfg.PluginDir = t.TempDir()
	}
	if cfg.ResinVersion == "" {
		cfg.ResinVersion = "1.0.0"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	m := NewManager(cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m
}

const tiCommand = "bin/does-not-matter"

// tiManifest renders a package manifest whose only runtime is "any" with
// tiCommand. extra keys override or extend the defaults.
func tiManifest(t *testing.T, id, version string, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"schema_version": pluginsdk.SchemaVersion,
		"id":             id,
		"name":           "Test " + id,
		"version":        version,
		"capabilities":   map[string]any{"request_hook": true},
		"runtimes": map[string]any{
			pluginsdk.RuntimeAny: map[string]any{"command": []string{tiCommand}},
		},
	}
	for k, v := range extra {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return data
}

// tiEntry is one archive member.
type tiEntry struct {
	Name string
	Body string
	// Mode is used for zip entries (SetMode) and the tar permission bits.
	Mode os.FileMode
	// Type is the tar type flag (default TypeReg, or TypeDir for names
	// ending in "/").
	Type     byte
	Linkname string
	PAX      map[string]string
}

// tiPackage returns the members of a minimal package under prefix ("" for the
// archive root or "dir/").
func tiPackage(manifest []byte, prefix string, extra ...tiEntry) []tiEntry {
	entries := []tiEntry{
		{Name: prefix + pluginsdk.ManifestFileName, Body: string(manifest)},
		{Name: prefix + tiCommand, Body: "#!/bin/sh\nexit 0\n", Mode: 0o755},
	}
	return append(entries, extra...)
}

func tiZip(t *testing.T, entries ...tiEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.Name, Method: zip.Deflate}
		if e.Mode != 0 {
			hdr.SetMode(e.Mode)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip header %q: %v", e.Name, err)
		}
		if e.Body != "" {
			if _, err := w.Write([]byte(e.Body)); err != nil {
				t.Fatalf("zip write %q: %v", e.Name, err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func tiTarGz(t *testing.T, entries ...tiEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
			if strings.HasSuffix(e.Name, "/") {
				typ = tar.TypeDir
			}
		}
		mode := int64(e.Mode.Perm())
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.Name, Typeflag: typ, Mode: mode, Linkname: e.Linkname, PAXRecords: e.PAX}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.Body))
		}
		if typ == tar.TypeXGlobalHeader {
			hdr.Mode = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.Name, err)
		}
		if typ == tar.TypeReg && e.Body != "" {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatalf("tar write %q: %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// tiDirNames lists the names in dir (sorted).
func tiDirNames(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return out
}

func tiReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func tiExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func tiFind(infos []Info, id string) (Info, bool) {
	for _, in := range infos {
		if in.ID == id {
			return in, true
		}
	}
	return Info{}, false
}

func tiIntPtr(v int) *int    { return &v }
func tiBoolPtr(v bool) *bool { return &v }

// --- safeJoin ---

func TestInstallerSafeJoinRejects(t *testing.T) {
	dest := t.TempDir()
	for _, name := range []string{
		"..",
		"../x",
		"../../x",
		"a/../../x",
		"a/b/../../../x",
		`..\x`,
		`a\..\..\x`,
		"/etc/x",
		"C:/x",
		"/",
		`\etc\x`,
		`C:\Windows\x`,
		"C:/x",
		"c:x",
		"C:",
		`\\server\share\x`,
		"//server/share/x",
	} {
		if full, err := safeJoin(dest, name); err == nil {
			t.Errorf("safeJoin(%q) = %q, want error", name, full)
		}
	}
}

func TestInstallerSafeJoinAccepts(t *testing.T) {
	dest := t.TempDir()
	tests := []struct {
		name string
		want string // relative to dest; "" means skipped entry
	}{
		{"a", "a"},
		{"a/b/c.txt", filepath.Join("a", "b", "c.txt")},
		{`a\b\c.txt`, filepath.Join("a", "b", "c.txt")},
		{"./a/./b", filepath.Join("a", "b")},
		{"a/../b", "b"},
		{"a/b/", filepath.Join("a", "b")},
		{"a..b/c", filepath.Join("a..b", "c")},
		{"..a/b", filepath.Join("..a", "b")},
		{".", ""},
		{"./", ""},
		{"a/..", ""},
		{"", ""},
	}
	for _, tc := range tests {
		full, err := safeJoin(dest, tc.name)
		if err != nil {
			t.Errorf("safeJoin(%q): unexpected error %v", tc.name, err)
			continue
		}
		if tc.want == "" {
			if full != "" {
				t.Errorf("safeJoin(%q) = %q, want skipped entry", tc.name, full)
			}
			continue
		}
		if want := filepath.Join(dest, tc.want); full != want {
			t.Errorf("safeJoin(%q) = %q, want %q", tc.name, full, want)
		}
		if !strings.HasPrefix(full, dest+string(filepath.Separator)) {
			t.Errorf("safeJoin(%q) = %q escapes %q", tc.name, full, dest)
		}
	}
}

// --- extraction ---

func TestInstallerExtractZip(t *testing.T) {
	dest := t.TempDir()
	data := tiZip(t,
		tiEntry{Name: "pkg/"},
		tiEntry{Name: "pkg/plugin.json", Body: `{"x":1}`},
		tiEntry{Name: "pkg/bin/run", Body: "exe", Mode: 0o755},
		tiEntry{Name: "pkg/deep/nested/file.txt", Body: "nested"}, // parent dirs created implicitly
		tiEntry{Name: "./"},
	)
	if err := extractZip(data, dest); err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if got := tiReadFile(t, filepath.Join(dest, "pkg", "plugin.json")); got != `{"x":1}` {
		t.Fatalf("plugin.json = %q", got)
	}
	if got := tiReadFile(t, filepath.Join(dest, "pkg", "deep", "nested", "file.txt")); got != "nested" {
		t.Fatalf("nested file = %q", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dest, "pkg", "bin", "run"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Fatalf("executable bit lost: %v", st.Mode())
		}
		st, _ = os.Stat(filepath.Join(dest, "pkg", "plugin.json"))
		if st.Mode().Perm()&0o111 != 0 {
			t.Fatalf("regular file became executable: %v", st.Mode())
		}
	}
}

func TestInstallerExtractZipRejects(t *testing.T) {
	tests := []struct {
		name    string
		entries []tiEntry
		wantErr string
	}{
		{"zip slip", []tiEntry{{Name: "ok.txt", Body: "ok"}, {Name: "../evil.txt", Body: "evil"}}, "escapes"},
		{"nested zip slip", []tiEntry{{Name: "a/../../evil.txt", Body: "evil"}}, "escapes"},
		{"backslash zip slip", []tiEntry{{Name: `..\evil.txt`, Body: "evil"}}, "escapes"},
		{"absolute path", []tiEntry{{Name: "/evil.txt", Body: "evil"}}, "absolute"},
		{"drive path", []tiEntry{{Name: `C:\evil.txt`, Body: "evil"}}, "absolute"},
		{"symlink", []tiEntry{{Name: "link", Body: "../../etc/passwd", Mode: os.ModeSymlink | 0o777}}, "links"},
		{"named pipe", []tiEntry{{Name: "fifo", Mode: os.ModeNamedPipe | 0o644}}, "not allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractZip(tiZip(t, tc.entries...), dest)
			if err == nil {
				t.Fatal("extractZip succeeded")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
			if tiExists(filepath.Join(parent, "evil.txt")) {
				t.Fatal("archive wrote outside the destination")
			}
			if tiExists(filepath.Join(dest, "link")) {
				t.Fatal("symlink entry was created")
			}
		})
	}

	if err := extractZip([]byte("PK\x03\x04 definitely not a zip"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "invalid zip") {
		t.Fatalf("corrupt zip error = %v", err)
	}
}

func TestInstallerExtractZipEntryLimit(t *testing.T) {
	// "./" entries count against the budget but touch no files, so this
	// stays cheap.
	many := func(n int) []tiEntry {
		out := make([]tiEntry, n)
		for i := range out {
			out[i] = tiEntry{Name: "./"}
		}
		return out
	}
	if err := extractZip(tiZip(t, many(maxArchiveFiles)...), t.TempDir()); err != nil {
		t.Fatalf("archive with exactly %d entries rejected: %v", maxArchiveFiles, err)
	}
	err := extractZip(tiZip(t, many(maxArchiveFiles+1)...), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("archive with %d entries: err = %v", maxArchiveFiles+1, err)
	}
}

func TestInstallerExtractTarGz(t *testing.T) {
	dest := t.TempDir()
	data := tiTarGz(t,
		tiEntry{Name: "pax_global_header", Type: tar.TypeXGlobalHeader, PAX: map[string]string{"comment": "git archive"}},
		tiEntry{Name: "pkg/"},
		tiEntry{Name: "pkg/plugin.json", Body: `{"x":2}`},
		tiEntry{Name: "pkg/bin/run", Body: "exe", Mode: 0o755},
		tiEntry{Name: "pkg/a/b/c.txt", Body: "deep"},
		tiEntry{Name: "pkg/empty.txt"},
	)
	if err := extractTarGz(data, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if got := tiReadFile(t, filepath.Join(dest, "pkg", "plugin.json")); got != `{"x":2}` {
		t.Fatalf("plugin.json = %q", got)
	}
	if got := tiReadFile(t, filepath.Join(dest, "pkg", "a", "b", "c.txt")); got != "deep" {
		t.Fatalf("deep file = %q", got)
	}
	if got := tiReadFile(t, filepath.Join(dest, "pkg", "empty.txt")); got != "" {
		t.Fatalf("empty file = %q", got)
	}
	if tiExists(filepath.Join(dest, "pax_global_header")) {
		t.Fatal("global PAX header was extracted as a file")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dest, "pkg", "bin", "run"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Fatalf("executable bit lost: %v", st.Mode())
		}
	}
}

func TestInstallerExtractTarGzRejects(t *testing.T) {
	tests := []struct {
		name    string
		entries []tiEntry
		wantErr string
	}{
		{"tar slip", []tiEntry{{Name: "ok.txt", Body: "ok"}, {Name: "../evil.txt", Body: "evil"}}, "escapes"},
		{"nested tar slip", []tiEntry{{Name: "a/b/../../../evil.txt", Body: "evil"}}, "escapes"},
		{"absolute path", []tiEntry{{Name: "/evil.txt", Body: "evil"}}, "absolute"},
		{"symlink", []tiEntry{{Name: "link", Type: tar.TypeSymlink, Linkname: "../../etc/passwd"}}, "links"},
		{"hard link", []tiEntry{{Name: "ok.txt", Body: "ok"}, {Name: "link", Type: tar.TypeLink, Linkname: "ok.txt"}}, "links"},
		{"char device", []tiEntry{{Name: "dev", Type: tar.TypeChar}}, "not allowed"},
		{"fifo", []tiEntry{{Name: "fifo", Type: tar.TypeFifo}}, "not allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			err := extractTarGz(tiTarGz(t, tc.entries...), dest)
			if err == nil {
				t.Fatal("extractTarGz succeeded")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
			if tiExists(filepath.Join(parent, "evil.txt")) {
				t.Fatal("archive wrote outside the destination")
			}
			if tiExists(filepath.Join(dest, "link")) {
				t.Fatal("link entry was created")
			}
		})
	}

	if err := extractTarGz([]byte{0x1f, 0x8b, 0x00}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "invalid gzip") {
		t.Fatalf("corrupt gzip error = %v", err)
	}
	var garbage bytes.Buffer
	gz := gzip.NewWriter(&garbage)
	_, _ = gz.Write(bytes.Repeat([]byte("x"), 512))
	_ = gz.Close()
	if err := extractTarGz(garbage.Bytes(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "invalid tar") {
		t.Fatalf("corrupt tar error = %v", err)
	}
}

func TestInstallerExtractTarGzEntryLimit(t *testing.T) {
	many := make([]tiEntry, maxArchiveFiles+1)
	for i := range many {
		many[i] = tiEntry{Name: "./", Type: tar.TypeDir}
	}
	err := extractTarGz(tiTarGz(t, many...), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("tar with %d entries: err = %v", len(many), err)
	}
	// Global PAX headers do not count as entries.
	ok := []tiEntry{{Name: "pax_global_header", Type: tar.TypeXGlobalHeader, PAX: map[string]string{"comment": "x"}}}
	ok = append(ok, many[:maxArchiveFiles]...)
	if err := extractTarGz(tiTarGz(t, ok...), t.TempDir()); err != nil {
		t.Fatalf("tar with %d entries + global header: %v", maxArchiveFiles, err)
	}
}

func TestInstallerExtractArchiveDetectsFormat(t *testing.T) {
	pkg := tiPackage([]byte(`{}`), "")
	for name, data := range map[string][]byte{
		"zip":    tiZip(t, pkg...),
		"tar.gz": tiTarGz(t, pkg...),
	} {
		dest := t.TempDir()
		if err := extractArchive(data, dest); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !tiExists(filepath.Join(dest, pluginsdk.ManifestFileName)) {
			t.Fatalf("%s: manifest not extracted", name)
		}
	}
	for _, data := range [][]byte{nil, []byte("hello"), []byte("PK"), {0x1f}} {
		err := extractArchive(data, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "unsupported package format") {
			t.Fatalf("extractArchive(%q) = %v", data, err)
		}
	}
}

func TestInstallerExtractBudget(t *testing.T) {
	b := &extractBudget{}
	for i := 0; i < maxArchiveFiles; i++ {
		if err := b.addFile(); err != nil {
			t.Fatalf("addFile #%d: %v", i+1, err)
		}
	}
	if err := b.addFile(); err == nil {
		t.Fatalf("addFile #%d succeeded", maxArchiveFiles+1)
	}

	dir := t.TempDir()
	// Exactly at the total limit is fine.
	b = &extractBudget{bytes: maxExtractedBytes - 10}
	if err := writeExtracted(filepath.Join(dir, "a", "ok.txt"), strings.NewReader("0123456789"), false, b); err != nil {
		t.Fatalf("writeExtracted at the limit: %v", err)
	}
	if b.bytes != maxExtractedBytes {
		t.Fatalf("budget bytes = %d, want %d", b.bytes, int64(maxExtractedBytes))
	}
	// One byte over the total limit is rejected.
	b = &extractBudget{bytes: maxExtractedBytes - 9}
	err := writeExtracted(filepath.Join(dir, "over.txt"), strings.NewReader("0123456789"), false, b)
	if err == nil || !strings.Contains(err.Error(), "expands to more than") {
		t.Fatalf("writeExtracted over the limit: %v", err)
	}
	// The per-file limit (maxExtractedFileLen, 256 MiB) is not exercised
	// here: it would need writing a quarter gigabyte to disk.
}

// --- packageRoot ---

func TestInstallerPackageRoot(t *testing.T) {
	write := func(t *testing.T, root string, files ...string) {
		t.Helper()
		for _, f := range files {
			p := filepath.Join(root, filepath.FromSlash(f))
			if strings.HasSuffix(f, "/") {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name  string
		files []string
		want  string // relative to root; "-" means error
	}{
		{"manifest at root", []string{"plugin.json", "bin/x"}, "."},
		{"manifest at root wins over subdir", []string{"plugin.json", "sub/plugin.json"}, "."},
		{"single top-level dir", []string{"acme-1.0/plugin.json", "acme-1.0/bin/x"}, "acme-1.0"},
		{"single dir without manifest", []string{"acme/README.md"}, "-"},
		{"two top-level dirs", []string{"a/plugin.json", "b/plugin.json"}, "-"},
		{"dir plus root file", []string{"a/plugin.json", "README.md"}, "-"},
		{"nested two levels", []string{"a/b/plugin.json"}, "-"},
		{"single file", []string{"other.json"}, "-"},
		{"empty", nil, "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.files...)
			got, err := packageRoot(root)
			if tc.want == "-" {
				if err == nil {
					t.Fatalf("packageRoot = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("packageRoot: %v", err)
			}
			if want := filepath.Join(root, tc.want); filepath.Clean(got) != want {
				t.Fatalf("packageRoot = %q, want %q", got, want)
			}
		})
	}
	if _, err := packageRoot(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("packageRoot of a missing dir succeeded")
	}
}

// --- InstallArchive end-to-end ---

func TestInstallerInstallArchiveZip(t *testing.T) {
	dir := t.TempDir()
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir})
	data := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", map[string]any{"description": "demo"}), "acme.demo-1.0.0/",
		tiEntry{Name: "acme.demo-1.0.0/README.md", Body: "readme"})...)

	info, err := m.InstallArchive(context.Background(), data)
	if err != nil {
		t.Fatalf("InstallArchive: %v", err)
	}
	if info.ID != "acme.demo" || info.Version != "1.0.0" || info.Source != SourcePackage || info.Enabled ||
		info.Status != StatusStopped || info.LastError != "" || info.Description != "demo" {
		t.Fatalf("install info = %+v", info)
	}
	list := m.List()
	if len(list) != 1 {
		t.Fatalf("List = %+v, want 1 entry", list)
	}
	if got := list[0]; got.ID != "acme.demo" || got.Source != SourcePackage || got.Enabled || got.Status != StatusStopped {
		t.Fatalf("List entry = %+v", got)
	}
	if _, err := m.Get("acme.demo"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	pkgDir := filepath.Join(dir, "acme.demo")
	for _, f := range []string{pluginsdk.ManifestFileName, filepath.FromSlash(tiCommand), "README.md"} {
		if !tiExists(filepath.Join(pkgDir, f)) {
			t.Fatalf("%s missing from installed package", f)
		}
	}
	if got := tiDirNames(t, dir); len(got) != 1 || got[0] != "acme.demo" {
		t.Fatalf("plugin dir contents = %v, want only acme.demo (no temp leftovers)", got)
	}
}

func TestInstallerInstallArchiveTarGzAtRoot(t *testing.T) {
	dir := t.TempDir()
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir})
	data := tiTarGz(t, tiPackage(tiManifest(t, "acme.tar", "2.0.0", nil), "")...)
	info, err := m.InstallArchive(context.Background(), data)
	if err != nil {
		t.Fatalf("InstallArchive: %v", err)
	}
	if info.ID != "acme.tar" || info.Source != SourcePackage || info.Enabled {
		t.Fatalf("install info = %+v", info)
	}
	if !tiExists(filepath.Join(dir, "acme.tar", filepath.FromSlash(tiCommand))) {
		t.Fatal("command missing from installed package")
	}
	if got := tiDirNames(t, dir); len(got) != 1 || got[0] != "acme.tar" {
		t.Fatalf("plugin dir contents = %v", got)
	}
}

func TestInstallerInstallArchiveRejects(t *testing.T) {
	good := func(t *testing.T) []byte { return tiManifest(t, "acme.demo", "1.0.0", nil) }
	tests := []struct {
		name    string
		archive func(t *testing.T) []byte
		wantErr error
		wantMsg string
	}{
		{"not an archive", func(t *testing.T) []byte { return []byte("hello world") }, ErrInvalidArgument, "unsupported package format"},
		{"no manifest", func(t *testing.T) []byte {
			return tiZip(t, tiEntry{Name: "README.md", Body: "x"}, tiEntry{Name: "bin/x", Body: "x"})
		}, ErrInvalidArgument, "plugin.json not found"},
		{"invalid manifest json", func(t *testing.T) []byte {
			return tiZip(t, tiPackage([]byte(`{"schema_version":1,`), "")...)
		}, ErrInvalidArgument, "invalid manifest"},
		{"manifest unknown field", func(t *testing.T) []byte {
			return tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", map[string]any{"bogus": 1}), "")...)
		}, ErrInvalidArgument, "unknown field"},
		{"min_resin_version too new", func(t *testing.T) []byte {
			return tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", map[string]any{"min_resin_version": "9.0.0"}), "")...)
		}, ErrInvalidArgument, "9.0.0"},
		{"invalid min_resin_version", func(t *testing.T) []byte {
			return tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", map[string]any{"min_resin_version": "latest"}), "")...)
		}, ErrInvalidArgument, "invalid min_resin_version"},
		{"no runtime for platform", func(t *testing.T) []byte {
			mf := tiManifest(t, "acme.demo", "1.0.0", map[string]any{
				"runtimes": map[string]any{"plan9-mips": map[string]any{"command": []string{tiCommand}}},
			})
			return tiZip(t, tiPackage(mf, "")...)
		}, ErrInvalidArgument, "no runtime"},
		{"command missing from package", func(t *testing.T) []byte {
			return tiZip(t, tiEntry{Name: pluginsdk.ManifestFileName, Body: string(good(t))})
		}, ErrInvalidArgument, "not found in plugin package"},
		{"command escapes package", func(t *testing.T) []byte {
			mf := tiManifest(t, "acme.demo", "1.0.0", map[string]any{
				"runtimes": map[string]any{"any": map[string]any{"command": []string{"../outside"}}},
			})
			return tiZip(t, tiPackage(mf, "")...)
		}, ErrInvalidArgument, "escapes"},
		{"zip slip", func(t *testing.T) []byte {
			return tiZip(t, tiPackage(good(t), "", tiEntry{Name: "../evil.txt", Body: "x"})...)
		}, ErrInvalidArgument, "escapes"},
		{"symlink", func(t *testing.T) []byte {
			return tiTarGz(t, tiPackage(good(t), "", tiEntry{Name: "link", Type: tar.TypeSymlink, Linkname: "/etc/passwd"})...)
		}, ErrInvalidArgument, "links"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "plugins")
			m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir})
			_, err := m.InstallArchive(context.Background(), tc.archive(t))
			if err == nil {
				t.Fatal("InstallArchive succeeded")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v is not %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not contain %q", err, tc.wantMsg)
			}
			if list := m.List(); len(list) != 0 {
				t.Fatalf("List after failed install = %+v", list)
			}
			if got := tiDirNames(t, dir); len(got) != 0 {
				t.Fatalf("plugin dir not clean after failed install: %v", got)
			}
			if tiExists(filepath.Join(parent, "evil.txt")) {
				t.Fatal("archive wrote outside the plugin directory")
			}
		})
	}
}

func TestInstallerInstallArchiveExpectedIDMismatch(t *testing.T) {
	dir := t.TempDir()
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir})
	data := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", nil), "")...)
	_, err := m.installArchive(context.Background(), data, "acme.other")
	if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("installArchive with wrong expected id: %v", err)
	}
	if len(m.List()) != 0 || len(tiDirNames(t, dir)) != 0 {
		t.Fatalf("mismatched package was installed: %v", tiDirNames(t, dir))
	}
	if _, err := m.installArchive(context.Background(), data, "acme.demo"); err != nil {
		t.Fatalf("installArchive with matching expected id: %v", err)
	}
}

func TestInstallerInstallArchiveBuiltinConflict(t *testing.T) {
	dir := t.TempDir()
	m := tiNewManager(t, ManagerConfig{
		ExternalEnabled: true,
		PluginDir:       dir,
		Builtins: []Builtin{{
			Manifest: pluginsdk.Manifest{SchemaVersion: pluginsdk.SchemaVersion, ID: "acme.demo", Name: "Builtin demo", Version: "1.0.0"},
			New:      func() pluginsdk.Plugin { return tiNopPlugin{} },
		}},
	})
	data := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "5.0.0", nil), "")...)
	_, err := m.InstallArchive(context.Background(), data)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "builtin") {
		t.Fatalf("InstallArchive over builtin id: %v", err)
	}
	info, err := m.Get("acme.demo")
	if err != nil || info.Source != SourceBuiltin || info.Version != "1.0.0" {
		t.Fatalf("builtin entry after conflict = %+v, %v", info, err)
	}
	if got := tiDirNames(t, dir); len(got) != 0 {
		t.Fatalf("plugin dir contents = %v", got)
	}
}

func TestInstallerInstallArchiveExternalDisabled(t *testing.T) {
	dir := t.TempDir()
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: false, PluginDir: dir})
	data := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", nil), "")...)
	if _, err := m.InstallArchive(context.Background(), data); !errors.Is(err, ErrExternalDisabled) {
		t.Fatalf("InstallArchive with external plugins disabled: %v", err)
	}
	if got := tiDirNames(t, dir); len(got) != 0 {
		t.Fatalf("plugin dir contents = %v", got)
	}
}

func TestInstallerInstallArchiveNeedsPluginDirAndRunningManager(t *testing.T) {
	data := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", nil), "")...)

	noDir := NewManager(ManagerConfig{ExternalEnabled: true, ResinVersion: "1.0.0", Logf: func(string, ...any) {}})
	if _, err := noDir.InstallArchive(context.Background(), data); !errors.Is(err, ErrConflict) {
		t.Fatalf("InstallArchive without PluginDir: %v", err)
	}

	dir := t.TempDir()
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir})
	m.Stop(context.Background())
	if _, err := m.InstallArchive(context.Background(), data); !errors.Is(err, ErrConflict) {
		t.Fatalf("InstallArchive after Stop: %v", err)
	}
	if got := tiDirNames(t, dir); len(got) != 0 {
		t.Fatalf("plugin dir contents after install on stopped manager = %v", got)
	}
}

func TestInstallerInstallArchiveDevVersionSkipsMinCheck(t *testing.T) {
	m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, ResinVersion: "dev"})
	data := tiZip(t, tiPackage(tiManifest(t, "acme.future", "1.0.0", map[string]any{"min_resin_version": "9.0.0"}), "")...)
	info, err := m.InstallArchive(context.Background(), data)
	if err != nil {
		t.Fatalf("InstallArchive on dev build: %v", err)
	}
	if info.Status != StatusStopped || info.LastError != "" {
		t.Fatalf("install info = %+v", info)
	}
}

func TestInstallerUpgradeReplacesFilesAndKeepsSettings(t *testing.T) {
	dir := t.TempDir()
	store := tiNewStore()
	cfg := ManagerConfig{ExternalEnabled: true, PluginDir: dir, Store: store}
	m := tiNewManager(t, cfg)
	ctx := context.Background()
	fields := map[string]any{"config_fields": []any{
		map[string]any{"name": "greeting", "type": "string", "default": "hello"},
	}}

	v1 := tiZip(t, tiPackage(tiManifest(t, "acme.demo", "1.0.0", fields), "",
		tiEntry{Name: "old.txt", Body: "old"})...)
	info, err := m.InstallArchive(ctx, v1)
	if err != nil {
		t.Fatalf("install v1: %v", err)
	}
	if string(info.Config) != `{"greeting":"hello"}` {
		t.Fatalf("default config = %s", info.Config)
	}
	if _, err := m.Update(ctx, "acme.demo", Update{
		Priority:   tiIntPtr(7),
		TimeoutMs:  tiIntPtr(2500),
		FailClosed: tiBoolPtr(true),
		Config:     json.RawMessage(`{"greeting": "hi", "extra": 1}`),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	v2 := tiTarGz(t, tiPackage(tiManifest(t, "acme.demo", "1.1.0", fields), "pkg/",
		tiEntry{Name: "pkg/new.txt", Body: "new"})...)
	info, err = m.InstallArchive(ctx, v2)
	if err != nil {
		t.Fatalf("install v2: %v", err)
	}
	want := func(t *testing.T, info Info) {
		t.Helper()
		if info.Version != "1.1.0" || info.Enabled || info.Priority != 7 || info.TimeoutMs != 2500 || !info.FailClosed {
			t.Fatalf("upgraded info = %+v", info)
		}
		if string(info.Config) != `{"greeting":"hi","extra":1}` {
			t.Fatalf("upgraded config = %s", info.Config)
		}
	}
	want(t, info)
	if list := m.List(); len(list) != 1 {
		t.Fatalf("List after upgrade = %+v", list)
	}
	pkgDir := filepath.Join(dir, "acme.demo")
	if tiExists(filepath.Join(pkgDir, "old.txt")) {
		t.Fatal("file from the previous version survived the upgrade")
	}
	if got := tiReadFile(t, filepath.Join(pkgDir, "new.txt")); got != "new" {
		t.Fatalf("new.txt = %q", got)
	}
	if got := tiDirNames(t, dir); len(got) != 1 || got[0] != "acme.demo" {
		t.Fatalf("plugin dir contents after upgrade = %v (backup or temp leftovers?)", got)
	}
	row, ok := store.get("acme.demo")
	if !ok || row.Priority != 7 || row.TimeoutMs != 2500 {
		t.Fatalf("stored settings = %+v, %v", row, ok)
	}

	// A fresh manager over the same directory and store sees the upgrade and
	// the persisted settings.
	m2 := tiNewManager(t, cfg)
	info, err = m2.Get("acme.demo")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	want(t, info)
}

func TestInstallerRescanReportsMinResinVersionError(t *testing.T) {
	writePkg := func(t *testing.T, dir, id string, extra map[string]any) {
		t.Helper()
		pkg := filepath.Join(dir, id)
		if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, pluginsdk.ManifestFileName), tiManifest(t, id, "1.0.0", extra), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, filepath.FromSlash(tiCommand)), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	t.Run("older resin", func(t *testing.T) {
		dir := t.TempDir()
		m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir, ResinVersion: "1.0.0"})
		if len(m.List()) != 0 {
			t.Fatalf("unexpected plugins: %+v", m.List())
		}
		writePkg(t, dir, "acme.future", map[string]any{"min_resin_version": "9.0.0"})
		infos, err := m.Rescan(ctx)
		if err != nil {
			t.Fatalf("Rescan: %v", err)
		}
		info, ok := tiFind(infos, "acme.future")
		if !ok {
			t.Fatalf("Rescan did not find the package: %+v", infos)
		}
		if info.Status != StatusError || !strings.Contains(info.LastError, "9.0.0") || info.Enabled {
			t.Fatalf("info = %+v, want status error mentioning 9.0.0", info)
		}
		if got, _ := m.Get("acme.future"); got.Status != StatusError {
			t.Fatalf("Get status = %q", got.Status)
		}
		_, err = m.Update(ctx, "acme.future", Update{Enabled: tiBoolPtr(true)})
		if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "9.0.0") {
			t.Fatalf("enabling an incompatible plugin: %v", err)
		}
	})

	t.Run("dev build", func(t *testing.T) {
		dir := t.TempDir()
		m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir, ResinVersion: "dev"})
		writePkg(t, dir, "acme.future", map[string]any{"min_resin_version": "9.0.0"})
		infos, err := m.Rescan(ctx)
		if err != nil {
			t.Fatalf("Rescan: %v", err)
		}
		info, ok := tiFind(infos, "acme.future")
		if !ok || info.Status != StatusStopped || info.LastError != "" {
			t.Fatalf("info = %+v (found %v), want stopped without error", info, ok)
		}
	})

	t.Run("present at start", func(t *testing.T) {
		dir := t.TempDir()
		writePkg(t, dir, "acme.future", map[string]any{"min_resin_version": "9.0.0"})
		m := tiNewManager(t, ManagerConfig{ExternalEnabled: true, PluginDir: dir, ResinVersion: "1.0.0"})
		info, err := m.Get("acme.future")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !strings.Contains(info.LastError, "9.0.0") {
			t.Fatalf("last_error = %q, want it to mention 9.0.0", info.LastError)
		}
		if info.Status != StatusError {
			t.Fatalf("status = %q, want %q", info.Status, StatusError)
		}
	})
}
