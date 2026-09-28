package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const (
	maxArchiveFiles     = 4096
	maxExtractedBytes   = 512 << 20
	maxExtractedFileLen = 256 << 20
)

// installArchive extracts a package archive into PluginDir and activates it.
// expectedID, when non-empty, must match the manifest id.
func (m *Manager) installArchive(ctx context.Context, data []byte, expectedID string) (Info, error) {
	if m.cfg.PluginDir == "" {
		return Info{}, fmt.Errorf("%w: plugin directory is not configured", ErrConflict)
	}
	if err := os.MkdirAll(m.cfg.PluginDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("create plugin directory: %w", err)
	}
	tmp, err := os.MkdirTemp(m.cfg.PluginDir, ".tmp-install-")
	if err != nil {
		return Info{}, fmt.Errorf("create temp directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := extractArchive(data, tmp); err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	root, err := packageRoot(tmp)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, pluginsdk.ManifestFileName))
	if err != nil {
		return Info{}, fmt.Errorf("%w: read manifest: %v", ErrInvalidArgument, err)
	}
	mf, err := pluginsdk.ParseManifest(raw)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if expectedID != "" && mf.ID != expectedID {
		return Info{}, fmt.Errorf("%w: package id %q does not match %q", ErrInvalidArgument, mf.ID, expectedID)
	}
	if _, ok := m.builtins[mf.ID]; ok {
		return Info{}, fmt.Errorf("%w: id %q is reserved by a builtin plugin", ErrConflict, mf.ID)
	}
	if err := checkMinResinVersion(mf.MinResinVersion, m.cfg.ResinVersion); err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	rt, ok := mf.RuntimeFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return Info{}, fmt.Errorf("%w: package has no runtime for %s-%s", ErrInvalidArgument, runtime.GOOS, runtime.GOARCH)
	}
	if cmd := rt.Command[0]; !filepath.IsAbs(cmd) && (strings.ContainsAny(cmd, `/\`) || strings.HasPrefix(cmd, ".")) {
		full, err := resolveCommand(root, cmd)
		if err != nil {
			return Info{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
		_ = os.Chmod(full, 0o755)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return Info{}, fmt.Errorf("%w: plugin manager is stopped", ErrConflict)
	}
	id := mf.ID
	dest := filepath.Join(m.cfg.PluginDir, id)
	old := m.entries[id]
	if old != nil {
		m.stopEntry(ctx, old)
	}

	backup := ""
	if _, err := os.Stat(dest); err == nil {
		backup = filepath.Join(m.cfg.PluginDir, fmt.Sprintf(".old-%s-%d", id, time.Now().UnixNano()))
		if err := os.Rename(dest, backup); err != nil {
			m.restoreOld(ctx, old)
			return Info{}, fmt.Errorf("replace existing package: %w", err)
		}
	}
	if err := os.Rename(root, dest); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dest)
		}
		m.restoreOld(ctx, old)
		return Info{}, fmt.Errorf("activate package: %w", err)
	}

	e := m.loadPackage(id)
	if e.settings.Enabled {
		if err := m.startEntry(ctx, e, e.config); err != nil {
			// Roll back to the previous version.
			_ = os.RemoveAll(dest)
			if backup != "" {
				_ = os.Rename(backup, dest)
			}
			m.restoreOld(ctx, old)
			return Info{}, fmt.Errorf("%w: %v", ErrStartFailed, err)
		}
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	m.entries[id] = e
	m.rebuildChain()
	m.cfg.Logf("[plugin] installed %s %s", id, e.manifest.Version)
	return m.info(e), nil
}

// restoreOld restarts a previously running entry after a failed upgrade.
func (m *Manager) restoreOld(ctx context.Context, old *entry) {
	if old == nil {
		return
	}
	if old.settings.Enabled && old.inst == nil {
		_ = m.startEntry(ctx, old, old.config)
	}
	m.rebuildChain()
}

// packageRoot finds plugin.json at the archive root or inside a single
// top-level directory.
func packageRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, pluginsdk.ManifestFileName)); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		sub := filepath.Join(dir, entries[0].Name())
		if _, err := os.Stat(filepath.Join(sub, pluginsdk.ManifestFileName)); err == nil {
			return sub, nil
		}
	}
	return "", fmt.Errorf("%s not found at the package root", pluginsdk.ManifestFileName)
}

func extractArchive(data []byte, dest string) error {
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return extractZip(data, dest)
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return extractTarGz(data, dest)
	default:
		return errors.New("unsupported package format (expected .zip or .tar.gz)")
	}
}

// safeJoin validates an archive member name and returns its destination.
func safeJoin(dest, name string) (string, error) {
	clean := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(clean, "/") || (len(clean) > 1 && clean[1] == ':') {
		return "", fmt.Errorf("absolute path %q in archive", name)
	}
	clean = path.Clean(clean)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the package", name)
	}
	full := filepath.Join(dest, filepath.FromSlash(clean))
	rel, err := filepath.Rel(dest, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the package", name)
	}
	return full, nil
}

type extractBudget struct {
	files int
	bytes int64
}

func (b *extractBudget) addFile() error {
	b.files++
	if b.files > maxArchiveFiles {
		return fmt.Errorf("archive has more than %d entries", maxArchiveFiles)
	}
	return nil
}

func writeExtracted(full string, r io.Reader, executable bool, budget *extractBudget) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxExtractedFileLen+1))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxExtractedFileLen {
		return fmt.Errorf("file %s is larger than %d bytes", filepath.Base(full), maxExtractedFileLen)
	}
	budget.bytes += n
	if budget.bytes > maxExtractedBytes {
		return fmt.Errorf("package expands to more than %d bytes", maxExtractedBytes)
	}
	return nil
}

func extractZip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("invalid zip: %w", err)
	}
	budget := &extractBudget{}
	for _, f := range zr.File {
		if err := budget.addFile(); err != nil {
			return err
		}
		full, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if full == "" {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeExtracted(full, rc, mode&0o111 != 0, budget)
			rc.Close()
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q (links and special files are not allowed)", f.Name)
		}
	}
	return nil
}

func extractTarGz(data []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	budget := &extractBudget{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := budget.addFile(); err != nil {
			return err
		}
		full, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		if full == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeExtracted(full, tr, hdr.Mode&0o111 != 0, budget); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q (links and special files are not allowed)", hdr.Name)
		}
	}
}
