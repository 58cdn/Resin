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

	changeCtx := context.WithoutCancel(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return Info{}, fmt.Errorf("%w: plugin manager is stopped", ErrConflict)
	}
	id := mf.ID
	dest := filepath.Join(m.cfg.PluginDir, id)
	old := m.entries[id]
	if old != nil {
		m.stopEntry(changeCtx, old)
	}

	backup := ""
	if _, statErr := os.Stat(dest); statErr == nil {
		backup = filepath.Join(m.cfg.PluginDir, fmt.Sprintf(".old-%s-%d", id, time.Now().UnixNano()))
		if err := os.Rename(dest, backup); err != nil {
			if restoreErr := m.restoreOld(changeCtx, old); restoreErr != nil {
				return Info{}, fmt.Errorf("replace existing package: %w (restore old plugin: %v)", err, restoreErr)
			}
			return Info{}, fmt.Errorf("replace existing package: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Info{}, fmt.Errorf("inspect existing package: %w", statErr)
	}
	if err := os.Rename(root, dest); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, dest); restoreErr != nil {
				return Info{}, fmt.Errorf("activate package: %w (restore backup %q: %v)", err, backup, restoreErr)
			}
		}
		if restoreErr := m.restoreOld(changeCtx, old); restoreErr != nil {
			return Info{}, fmt.Errorf("activate package: %w (restore old plugin: %v)", err, restoreErr)
		}
		return Info{}, fmt.Errorf("activate package: %w", err)
	}

	e := m.loadPackage(id)
	if e.settings.Enabled {
		if err := m.startEntry(changeCtx, e, e.config); err != nil {
			// Roll back to the previous version. Never restart the old entry
			// until the new directory has been removed and the backup restored.
			if removeErr := os.RemoveAll(dest); removeErr != nil {
				return Info{}, fmt.Errorf("%w: %v (rollback remove %q: %v; backup retained)", ErrStartFailed, err, dest, removeErr)
			}
			if backup != "" {
				if restoreErr := os.Rename(backup, dest); restoreErr != nil {
					return Info{}, fmt.Errorf("%w: %v (rollback restore %q: %v; backup retained)", ErrStartFailed, err, backup, restoreErr)
				}
			}
			if restoreErr := m.restoreOld(changeCtx, old); restoreErr != nil {
				return Info{}, fmt.Errorf("%w: %v (restore old plugin: %v)", ErrStartFailed, err, restoreErr)
			}
			return Info{}, fmt.Errorf("%w: %v", ErrStartFailed, err)
		}
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			m.cfg.Logf("[plugin] remove upgrade backup %s: %v", backup, err)
		}
	}
	m.entries[id] = e
	m.rebuildChain()
	m.cfg.Logf("[plugin] installed %s %s", id, e.manifest.Version)
	return m.info(e), nil
}

// restoreOld restarts a previously running entry after a failed upgrade.
func (m *Manager) restoreOld(ctx context.Context, old *entry) error {
	ctx = context.WithoutCancel(ctx)
	if old == nil {
		return nil
	}
	if old.settings.Enabled && old.inst == nil {
		if err := m.startEntry(ctx, old, old.config); err != nil {
			m.rebuildChain()
			return err
		}
	}
	m.rebuildChain()
	return nil
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

func looksExecutableHeader(header []byte) bool {
	return bytes.HasPrefix(header, []byte("#!")) ||
		bytes.HasPrefix(header, []byte{0x7f, 'E', 'L', 'F'}) ||
		bytes.HasPrefix(header, []byte("MZ")) ||
		bytes.HasPrefix(header, []byte{0xfe, 0xed, 0xfa, 0xce}) ||
		bytes.HasPrefix(header, []byte{0xce, 0xfa, 0xed, 0xfe}) ||
		bytes.HasPrefix(header, []byte{0xfe, 0xed, 0xfa, 0xcf}) ||
		bytes.HasPrefix(header, []byte{0xcf, 0xfa, 0xed, 0xfe})
}

func detectExecutable(r io.Reader, executable, detectHeader bool) (io.Reader, bool, error) {
	if executable || !detectHeader {
		return r, executable, nil
	}
	header := make([]byte, 4)
	n, err := io.ReadFull(r, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false, err
	}
	return io.MultiReader(bytes.NewReader(header[:n]), r), looksExecutableHeader(header[:n]), nil
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
			reader, executable, detectErr := detectExecutable(rc, mode&0o111 != 0, mode.Perm() == 0)
			if detectErr == nil {
				detectErr = writeExtracted(full, reader, executable, budget)
			}
			closeErr := rc.Close()
			if detectErr != nil {
				return detectErr
			}
			if closeErr != nil {
				return closeErr
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
			reader, executable, detectErr := detectExecutable(tr, hdr.Mode&0o111 != 0, hdr.Mode&0o777 == 0)
			if detectErr != nil {
				return detectErr
			}
			if err := writeExtracted(full, reader, executable, budget); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q (links and special files are not allowed)", hdr.Name)
		}
	}
}
