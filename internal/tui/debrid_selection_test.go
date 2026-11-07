package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/metacache"
)

func TestMapSelection(t *testing.T) {
	type pf = engine.FileInfo
	type rf = debrid.RemoteFile
	cases := []struct {
		name     string
		preview  []pf
		remote   []rf
		excluded []int
		want     []int
		wantErr  bool
	}{
		{
			name: "root folder, IDs out of order",
			preview: []pf{
				{Index: 0, Path: "Show/S01/e01.mkv", Length: 700},
				{Index: 1, Path: "Show/S01/e02.mkv", Length: 701},
				{Index: 2, Path: "Show/readme.nfo", Length: 1},
			},
			remote:   []rf{{ID: 12, Path: "S01/e02.mkv", Bytes: 701}, {ID: 5, Path: "/S01/e01.mkv", Bytes: 700}, {ID: 9, Path: "readme.nfo", Bytes: 1}},
			excluded: []int{2},
			want:     []int{5, 12},
		},
		{
			name:    "single-file torrent",
			preview: []pf{{Index: 0, Path: "Movie.2024.mkv", Length: 4 << 30}},
			remote:  []rf{{ID: 1, Path: "/Movie.2024.mkv", Bytes: 4 << 30}},
			want:    []int{1},
		},
		{
			name: "duplicate names in different folders",
			preview: []pf{
				{Index: 0, Path: "Pack/A/cover.jpg", Length: 100},
				{Index: 1, Path: "Pack/B/cover.jpg", Length: 100},
				{Index: 2, Path: "Pack/A/film.mkv", Length: 900},
			},
			remote:   []rf{{ID: 3, Path: "B/cover.jpg", Bytes: 100}, {ID: 8, Path: "A/cover.jpg", Bytes: 100}, {ID: 2, Path: "A/film.mkv", Bytes: 900}},
			excluded: []int{0},
			want:     []int{2, 3},
		},
		{
			name: "duplicate sizes match on path",
			preview: []pf{
				{Index: 0, Path: "Album/01.flac", Length: 500},
				{Index: 1, Path: "Album/02.flac", Length: 500},
				{Index: 2, Path: "Album/03.flac", Length: 500},
			},
			remote:   []rf{{ID: 30, Path: "03.flac", Bytes: 500}, {ID: 10, Path: "01.flac", Bytes: 500}, {ID: 20, Path: "02.flac", Bytes: 500}},
			excluded: []int{1},
			want:     []int{10, 30},
		},
		{
			name: "unicode paths",
			preview: []pf{
				{Index: 0, Path: "Amélie (2001)/Amélie.mkv", Length: 2 << 30},
				{Index: 1, Path: "Amélie (2001)/字幕/中文.srt", Length: 60 << 10},
				{Index: 2, Path: "Amélie (2001)/Ελληνικά.srt", Length: 61 << 10},
			},
			remote:   []rf{{ID: 4, Path: "/Ελληνικά.srt", Bytes: 61 << 10}, {ID: 6, Path: "/字幕/中文.srt", Bytes: 60 << 10}, {ID: 7, Path: "/Amélie.mkv", Bytes: 2 << 30}},
			excluded: []int{2},
			want:     []int{6, 7},
		},
		{
			name: "folder named like the torrent",
			preview: []pf{
				{Index: 0, Path: "Show/a.mkv", Length: 10},
				{Index: 1, Path: "Show/Show/a.mkv", Length: 10},
			},
			remote:   []rf{{ID: 1, Path: "Show/a.mkv", Bytes: 10}, {ID: 2, Path: "a.mkv", Bytes: 10}},
			excluded: []int{1},
			want:     []int{2},
		},
		{
			name:    "renamed file with a unique size",
			preview: []pf{{Index: 0, Path: "Doc/odd name.pdf", Length: 4242}, {Index: 1, Path: "Doc/b.txt", Length: 3}},
			remote:  []rf{{ID: 11, Path: "odd_name.pdf", Bytes: 4242}, {ID: 12, Path: "b.txt", Bytes: 3}},
			want:    []int{11, 12},
		},
		{
			name:    "renamed file with a shared size is not guessed",
			preview: []pf{{Index: 0, Path: "Doc/odd name.pdf", Length: 3}, {Index: 1, Path: "Doc/b.txt", Length: 3}},
			remote:  []rf{{ID: 11, Path: "odd_name.pdf", Bytes: 3}, {ID: 12, Path: "b.txt", Bytes: 3}},
			wantErr: true,
		},
		{
			name:    "file missing on Real-Debrid",
			preview: []pf{{Index: 0, Path: "Show/e01.mkv", Length: 7}, {Index: 1, Path: "Show/e02.mkv", Length: 7}},
			remote:  []rf{{ID: 1, Path: "e01.mkv", Bytes: 7}},
			wantErr: true,
		},
		{
			name:    "same path, different size",
			preview: []pf{{Index: 0, Path: "Show/e01.mkv", Length: 7}, {Index: 1, Path: "Show/x", Length: 8}},
			remote:  []rf{{ID: 1, Path: "e01.mkv", Bytes: 9}, {ID: 2, Path: "x", Bytes: 9}},
			wantErr: true,
		},
		{
			name: "padding files are skipped",
			preview: []pf{
				{Index: 0, Path: "Show/e01.mkv", Length: 7},
				{Index: 1, Path: "Show/_____padding_file_0_", Length: 9},
				{Index: 2, Path: "Show/e02.mkv", Length: 8},
			},
			remote:   []rf{{ID: 1, Path: "e01.mkv", Bytes: 7}, {ID: 2, Path: "e02.mkv", Bytes: 8}},
			excluded: []int{2},
			want:     []int{1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			excluded := map[int]bool{}
			for _, i := range tc.excluded {
				excluded[i] = true
			}
			got, err := mapSelection(tc.preview, tc.remote, excluded)
			if tc.wantErr {
				if !errors.Is(err, errUnmatchedSelection) || got != nil {
					t.Fatalf("mapSelection = %v, %v; want errUnmatchedSelection and no IDs", got, err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("mapSelection = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// cachedPreview opens a debrid preview whose list comes from the metadata
// cache, with Real-Debrid's list given by remote.
func cachedPreview(t *testing.T, remote []debrid.RemoteFile, files []metainfo.FileInfo) (*App, *previewRD, debridPreviewOpenMsg) {
	t.Helper()
	fake := &previewRD{convertCalls: 0, name: "Show", files: remote}
	a, _, cfg := debridPreviewApp(t, fake, "linked")
	mi, magnet := testTorrent(t, "Show", files)
	if err := metacache.New(cfg).Store(*mi); err != nil {
		t.Fatal(err)
	}
	open := openDebridPreview(t, a, a.launchCmd(magnet, "Show S01", true))
	if a.preview.list != listFromCache {
		t.Fatalf("list = %v, want the cache", a.preview.list)
	}
	return a, fake, open
}

// tickPreview is the tick's preview work: follow the item and finish a
// parked start.
func tickPreview(a *App) {
	if a.screen == screenPreview {
		a.preview.refresh(a.eng)
		_ = a.resumePendingDebridStart()
	}
}

func toggleFile(t *testing.T, a *App, path string) {
	t.Helper()
	p := &a.preview
	for i, n := range p.rows {
		if n.fileIdx >= 0 && p.filesByIndex[n.fileIdx].Path == path {
			p.win.cursor = i
			a.updatePreview(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			return
		}
	}
	t.Fatalf("no row for %s", path)
}

var showFiles = []metainfo.FileInfo{
	{Path: []string{"e01.mkv"}, Length: 700 << 20},
	{Path: []string{"e02.mkv"}, Length: 701 << 20},
	{Path: []string{"Subs", "en.srt"}, Length: 40 << 10},
}

func TestDebridStartMapsCachedSelectionToIDs(t *testing.T) {
	remote := []debrid.RemoteFile{
		{ID: 14, Path: "/Subs/en.srt", Bytes: 40 << 10},
		{ID: 22, Path: "/e02.mkv", Bytes: 701 << 20},
		{ID: 9, Path: "/e01.mkv", Bytes: 700 << 20},
	}
	a, fake, open := cachedPreview(t, remote, showFiles)
	addDebridPreview(t, a, open)
	toggleFile(t, a, "Show/e02.mkv")

	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
	waitPreview(t, "the parked start", func() bool { tickPreview(a); return a.screen != screenPreview })
	if a.preview.notice != "" {
		t.Fatalf("notice = %q; the list matched", a.preview.notice)
	}
	waitPreview(t, "selectFiles", func() bool { _, b, _ := fake.snapshot(); return len(b) == 1 })
	if _, bodies, _ := fake.snapshot(); bodies[0] != "files=9,14" {
		t.Fatalf("selectFiles body = %q, want files=9,14", bodies[0])
	}
	if item, _ := a.eng.DebridItem(open.hash); !slices.Equal(item.Selected, []int{9, 14}) {
		t.Fatalf("item selection = %v", item.Selected)
	}
}

func TestDebridStartUnmatchedRebuildsFromRealDebrid(t *testing.T) {
	// Real-Debrid renamed one file and two share a size: no safe match.
	remote := []debrid.RemoteFile{
		{ID: 31, Path: "/e01.mkv", Bytes: 700 << 20},
		{ID: 32, Path: "/e02 (renamed).mkv", Bytes: 701 << 20},
		{ID: 33, Path: "/Subs/en.srt", Bytes: 40 << 10},
		{ID: 34, Path: "/extra.mkv", Bytes: 701 << 20},
	}
	a, fake, open := cachedPreview(t, remote, showFiles)
	addDebridPreview(t, a, open)
	toggleFile(t, a, "Show/Subs/en.srt")

	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
	waitPreview(t, "the rebuilt list", func() bool { tickPreview(a); return a.preview.list == listFromRemote })
	p := &a.preview
	if a.screen != screenPreview || p.notice != "file list updated from Real-Debrid; check your selection" {
		t.Fatalf("screen %v notice %q; want the preview kept open with the notice", a.screen, p.notice)
	}
	if _, bodies, _ := fake.snapshot(); len(bodies) != 0 {
		t.Fatalf("selectFiles ran with a guessed ID: %v", bodies)
	}
	if view := ansi.Strip(a.View()); !strings.Contains(view, "check your selection") {
		t.Fatalf("view lacks the notice:\n%s", view)
	}
	// The untick carried over by path; the others stayed chosen.
	for i, f := range p.files {
		wantOut := remote[i].ID == 33
		if p.excluded[f.Index] != wantOut {
			t.Fatalf("%s excluded=%v, want %v", f.Path, p.excluded[f.Index], wantOut)
		}
	}

	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
	if a.screen == screenPreview {
		t.Fatal("second enter did not start")
	}
	waitPreview(t, "selectFiles", func() bool { _, b, _ := fake.snapshot(); return len(b) == 1 })
	if _, bodies, _ := fake.snapshot(); bodies[0] != "files=31,32,34" {
		t.Fatalf("selectFiles body = %q", bodies[0])
	}
}

func TestDebridStartEverythingSendsAll(t *testing.T) {
	remote := []debrid.RemoteFile{
		{ID: 9, Path: "/e01.mkv", Bytes: 700 << 20},
		{ID: 22, Path: "/e02.mkv", Bytes: 701 << 20},
		{ID: 14, Path: "/Subs/en.srt", Bytes: 40 << 10},
	}
	a, fake, open := cachedPreview(t, remote, showFiles)
	// enter before Real-Debrid accepted the torrent: parked, never blocking
	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
	if !a.preview.startPending || a.screen != screenPreview {
		t.Fatalf("pending=%v screen=%v; want a parked start", a.preview.startPending, a.screen)
	}
	addDebridPreview(t, a, open)
	if a.screen != screenResults {
		t.Fatalf("screen = %v; the start should finish once the add returns", a.screen)
	}
	if a.toast.text == "" {
		t.Fatal("no queued toast")
	}
	waitPreview(t, "selectFiles", func() bool { _, b, _ := fake.snapshot(); return len(b) == 1 })
	if _, bodies, _ := fake.snapshot(); bodies[0] != "files=all" {
		t.Fatalf("selectFiles body = %q, want files=all", bodies[0])
	}
	if item, _ := a.eng.DebridItem(open.hash); item.Selected != nil {
		t.Fatalf("item selection = %v, want nil (all)", item.Selected)
	}
}

func TestDebridStartFromRemoteListUsesItsIDs(t *testing.T) {
	fake := &previewRD{convertCalls: 0, name: "Movie", files: []debrid.RemoteFile{
		{ID: 7, Path: "Subs/en.srt", Bytes: 40 << 10},
		{ID: 3, Path: "Movie.mkv", Bytes: 900 << 20},
	}}
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	magnet := "magnet:?xt=urn:btih:8888888888888888888888888888888888888888&dn=Movie"
	open := openDebridPreview(t, a, a.launchCmd(magnet, "Movie", true))
	addDebridPreview(t, a, open)
	waitPreview(t, "Real-Debrid's file list", func() bool { a.preview.refresh(eng); return a.preview.ready })
	toggleFile(t, a, "Movie/Subs/en.srt")
	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
	if a.screen == screenPreview {
		t.Fatal("enter did not start")
	}
	waitPreview(t, "selectFiles", func() bool { _, b, _ := fake.snapshot(); return len(b) == 1 })
	if _, bodies, _ := fake.snapshot(); bodies[0] != "files=3" {
		t.Fatalf("selectFiles body = %q, want files=3", bodies[0])
	}
}
