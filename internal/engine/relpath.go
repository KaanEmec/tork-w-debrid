package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Multi-file downloads (a debrid torrent's files) write into subfolders of the
// download dir, such as "Show.S01/Season 1/ep01.mkv". The relative path comes
// from untrusted torrent metadata, so it is checked part by part and every
// folder on the way is refused when it is a symlink (or a Windows junction)
// that could lead outside the download dir.

const (
	// maxRelPathParts bounds a relative path's depth.
	maxRelPathParts = 16
	// maxRelPathBytes bounds a relative path's length, so Windows MAX_PATH
	// problems show up as a clear refusal rather than an odd I/O error.
	maxRelPathBytes = 1024
)

// safeRelPath reports whether rel is a safe relative path that uses "/" as
// its separator. Every part must be a safe single file name: no empty parts,
// ".", "..", drive letters, backslashes, reserved Windows names, control or
// bidi characters, or trailing dots and spaces.
func safeRelPath(rel string) bool {
	if rel == "" || len(rel) > maxRelPathBytes {
		return false
	}
	parts := strings.Split(rel, "/")
	if len(parts) > maxRelPathParts {
		return false
	}
	for _, part := range parts {
		// safeDirectFilename refuses "", ".", "..", "\" and ":" (so also
		// drive letters and a leading "/" that leaves an empty first part).
		if !safeDirectFilename(part) {
			return false
		}
	}
	return true
}

// safeDirectName checks a direct download's Name: a single file name as the
// ISO shelf uses, or a "/"-separated relative path for multi-file downloads.
func safeDirectName(name string) bool {
	if strings.Contains(name, "/") {
		return safeRelPath(name)
	}
	return safeDirectFilename(name)
}

// safeRelDataPath joins a checked relative path under dir. The result is
// always inside dir, and no folder that already exists between dir and the
// file is a symlink or other link.
func safeRelDataPath(dir, rel string) (string, bool) {
	if !safeRelPath(rel) {
		return "", false
	}
	dest, ok := safeDataPath(dir, filepath.FromSlash(rel))
	if !ok {
		return "", false
	}
	if err := checkRelParents(dir, rel, false); err != nil {
		return "", false
	}
	return dest, true
}

// ensureRelParents creates the folders of rel under dir (0o755), refusing any
// existing folder on the way that is a link or not a folder. A single name
// needs no folders and returns nil at once.
func ensureRelParents(dir, rel string) error {
	if !strings.Contains(rel, "/") {
		return nil
	}
	if !safeRelPath(rel) {
		return fmt.Errorf("unsafe file path %q", rel)
	}
	return checkRelParents(dir, rel, true)
}

var errUnsafeParent = errors.New("a folder on the download path is a link or not a folder; refusing to write through it")

// checkRelParents walks the folders of rel below dir. With create, missing
// folders are made; without it, the walk stops at the first missing one.
func checkRelParents(dir, rel string, create bool) error {
	base, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	parts := strings.Split(rel, "/")
	cur := base
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				return nil
			}
			if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if fi, err = os.Lstat(cur); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		// Type() is exactly ModeDir only for a plain folder: symlinks carry
		// ModeSymlink and Windows junctions ModeIrregular.
		if fi.Mode().Type() != fs.ModeDir {
			return errUnsafeParent
		}
	}
	return nil
}
