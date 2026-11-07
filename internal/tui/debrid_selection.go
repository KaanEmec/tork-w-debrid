package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
)

// errUnmatchedSelection: a chosen file has no sure counterpart in
// Real-Debrid's file list. The caller rebuilds the preview from that list
// rather than guess an ID (roadmap J4.2).
var errUnmatchedSelection = errors.New("a chosen file is not in Real-Debrid's file list")

// mapSelection turns the preview's ticks into Real-Debrid file IDs for
// selectFiles, for a list that came from the metadata cache or a .torrent.
//
// A file matches on its path and size. Real-Debrid's paths leave out the
// torrent's folder, which the preview's paths include, so the path is tried
// without its first segment and then whole. Only when that fails, and the
// size is unique on both sides, does the size alone decide. Each remote file
// is used once, and IDs are never assumed to be index+1. The IDs come back
// sorted, the order Real-Debrid lists the links in.
func mapSelection(previewFiles []engine.FileInfo, remoteFiles []debrid.RemoteFile, excluded map[int]bool) ([]int, error) {
	byPath := make(map[string][]int, len(remoteFiles))
	remoteSizes := make(map[int64]int, len(remoteFiles))
	for j, rf := range remoteFiles {
		key := normalizeRemotePath(rf.Path)
		byPath[key] = append(byPath[key], j)
		remoteSizes[rf.Bytes]++
	}
	previewSizes := make(map[int64]int, len(previewFiles))
	for _, f := range previewFiles {
		previewSizes[f.Length]++
	}

	used := make(map[int]bool, len(remoteFiles))
	match := func(f engine.FileInfo) int {
		full := normalizeRemotePath(f.Path)
		for _, key := range []string{remotePathKey(full), full} {
			for _, j := range byPath[key] {
				if !used[j] && remoteFiles[j].Bytes == f.Length {
					return j
				}
			}
		}
		if remoteSizes[f.Length] == 1 && previewSizes[f.Length] == 1 {
			for j, rf := range remoteFiles {
				if rf.Bytes == f.Length && !used[j] {
					return j
				}
			}
		}
		return -1
	}

	var ids []int
	missing := 0
	for _, f := range previewFiles {
		if excluded[f.Index] {
			continue
		}
		j := match(f)
		if j < 0 {
			if isPaddingPath(f.Path) {
				continue // holds no data; services don't list it
			}
			missing++
			continue
		}
		used[j] = true
		ids = append(ids, remoteFiles[j].ID)
	}
	if missing > 0 {
		return nil, fmt.Errorf("%w (%d of them)", errUnmatchedSelection, missing)
	}
	sort.Ints(ids)
	return ids, nil
}

// normalizeRemotePath writes a path the way Real-Debrid does: forward
// slashes and no leading slash.
func normalizeRemotePath(path string) string {
	return strings.TrimLeft(strings.ReplaceAll(path, "\\", "/"), "/")
}

// isPaddingPath spots padding files that carry no attribute flag (BitComet's
// "_____padding_file_…" and BEP 47's ".pad/" folder).
func isPaddingPath(path string) bool {
	path = normalizeRemotePath(path)
	return strings.Contains(pathBase(path), "_____padding_file_") ||
		strings.HasPrefix(path, ".pad/") || strings.Contains(path, "/.pad/")
}
