package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Typed reasons a download folder cannot be saved. The UI can match them with
// errors.Is; the wrapped error text is already short enough to show as is.
var (
	ErrDownloadDirEmpty       = errors.New("choose a folder")
	ErrDownloadDirRelative    = errors.New("folder must be an absolute path")
	ErrDownloadDirInsideTork  = errors.New("folder must not be inside tork's own config folder")
	ErrDownloadDirNotDir      = errors.New("not a folder")
	ErrDownloadDirUncreatable = errors.New("folder cannot be created")
	ErrDownloadDirNotWritable = errors.New("folder is not writable")
)

// ValidateDownloadDir checks a folder the user wants as the default download
// folder and returns its cleaned absolute form. A missing folder is created
// (0o755), and writability is proven with a probe file that is removed again.
// Only a leading ~ is expanded; nothing else is interpreted.
func (c *Config) ValidateDownloadDir(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrDownloadDirEmpty
	}
	dir := expandHome(raw)
	if !filepath.IsAbs(dir) {
		return "", ErrDownloadDirRelative
	}
	dir = filepath.Clean(dir)
	if c.dir != "" && pathWithin(dir, c.dir) {
		return "", ErrDownloadDirInsideTork
	}
	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("%w: %s", ErrDownloadDirNotDir, dir)
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("%w: %v", ErrDownloadDirUncreatable, err)
		}
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrDownloadDirUncreatable, err)
	}
	probe, err := os.CreateTemp(dir, ".tork-write-probe-*")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrDownloadDirNotWritable, dir)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrDownloadDirNotWritable, err)
	}
	return dir, nil
}

// WriteDownloadDir validates dir and saves it as download_dir in config.yaml,
// leaving every other byte of the file's structure, its comments and its mode
// alone. A folder under the home folder is stored as ~/…, so the file stays
// readable; loading expands it again. It returns the absolute folder.
//
// It does not touch the running engine or saved downloads. A caller applying
// the change at runtime should, in this order: pin the old default on saved
// entries without a folder (state.State.PinDownloadDir), call this, then
// engine.Engine.SetDefaultDownloadDir and finally update c.DownloadDir, so
// nothing that already exists appears to move.
func (c *Config) WriteDownloadDir(raw string) (string, error) {
	dir, err := c.ValidateDownloadDir(raw)
	if err != nil {
		return "", err
	}
	if err := c.writeScalar([]string{"download_dir"}, "!!str", homeRelative(dir)); err != nil {
		return "", err
	}
	return dir, nil
}

// homeRelative renders a path under the home folder as ~/…, using forward
// slashes so expandHome reads it back on every platform.
func homeRelative(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	rel, err := filepath.Rel(filepath.Clean(home), dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return dir
	}
	return "~/" + filepath.ToSlash(rel)
}

// pathWithin reports whether path is base or inside it.
func pathWithin(path, base string) bool {
	base = filepath.Clean(base)
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
