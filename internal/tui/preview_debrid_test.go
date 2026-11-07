package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/metacache"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

// previewRD is a scripted debrid.Provider for the preview flow. Info answers
// "converting" until convertCalls calls have happened, then lists files.
type previewRD struct {
	mu           sync.Mutex
	convertCalls int
	name         string
	files        []debrid.RemoteFile
	addErr       error
	infoCalls    int
	adds         []string // magnets, or "torrent"
	bodies       []string // selectFiles bodies as Real-Debrid receives them
	deletes      []string
}

func (f *previewRD) Name() string { return "Real-Debrid" }

func (f *previewRD) AddMagnet(_ context.Context, magnet string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.adds = append(f.adds, magnet)
	return fmt.Sprintf("RID%d", len(f.adds)), nil
}

func (f *previewRD) AddTorrentFile(_ context.Context, _ []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.adds = append(f.adds, "torrent")
	return fmt.Sprintf("RID%d", len(f.adds)), nil
}

func (f *previewRD) Info(_ context.Context, id string) (debrid.RemoteInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	info := debrid.RemoteInfo{ID: id, Name: f.name, Status: debrid.Converting}
	if f.infoCalls > f.convertCalls {
		info.Status = debrid.WaitingSelection
		if len(f.bodies) > 0 {
			info.Status = debrid.Queued
		}
		info.Files = slices.Clone(f.files)
	}
	return info, nil
}

func (f *previewRD) SelectFiles(_ context.Context, _ string, ids []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := "files=all"
	if len(ids) > 0 {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprint(id)
		}
		body = "files=" + strings.Join(parts, ",")
	}
	f.bodies = append(f.bodies, body)
	return nil
}

func (f *previewRD) Unlock(context.Context, string) (debrid.Unlocked, error) {
	return debrid.Unlocked{}, errors.New("not used")
}

func (f *previewRD) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return nil
}

func (f *previewRD) Account(context.Context) (debrid.Account, error) {
	return debrid.Account{Username: "test", Premium: true}, nil
}

func (f *previewRD) snapshot() (adds, bodies, deletes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.adds), slices.Clone(f.bodies), slices.Clone(f.deletes)
}

// debridPreviewApp is an App with a real engine whose debrid provider is fake
// and a linked account on disk. linkState is "linked", "needs-login" or "".
func debridPreviewApp(t *testing.T, fake debrid.Provider, linkState string) (*App, *engine.Engine, *config.Config) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	cfg, err := config.LoadFrom(filepath.Join(t.TempDir(), ".tork"))
	if err != nil {
		t.Fatal(err)
	}
	if linkState != "" {
		link := debrid.NewLink(config.DebridProviderRealDebrid, debrid.Credentials{
			AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour),
		}, time.Now())
		link.NeedsLogin = linkState == "needs-login"
		if err := debrid.NewStore(cfg).Save(link); err != nil {
			t.Fatal(err)
		}
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	if fake != nil {
		eng.SetDebridProvider(fake)
	}
	a := &App{cfg: cfg, eng: eng, st: &state.State{}, screen: screenResults, width: 120, height: 36}
	return a, eng, cfg
}

// testTorrent builds a multi-file .torrent and returns it with its magnet.
func testTorrent(t *testing.T, name string, files []metainfo.FileInfo) (*metainfo.MetaInfo, string) {
	t.Helper()
	var total int64
	for _, f := range files {
		total += f.Length
	}
	const pieceLen = 1 << 18
	info := metainfo.Info{Name: name, PieceLength: pieceLen, Files: files,
		Pieces: make([]byte, 20*((total+pieceLen-1)/pieceLen))}
	data, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: data}
	h := mi.HashInfoBytes()
	return mi, "magnet:?xt=urn:btih:" + h.HexString() + "&dn=" + name
}

// openDebridPreview runs a launcher's command and opens what it returns,
// then sends the add the way Bubble Tea would.
func openDebridPreview(t *testing.T, a *App, cmd tea.Cmd) debridPreviewOpenMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("launcher returned no command")
	}
	open, ok := cmd().(debridPreviewOpenMsg)
	if !ok {
		t.Fatal("launcher did not take the debrid route")
	}
	a.onDebridPreviewOpen(open)
	return open
}

func addDebridPreview(t *testing.T, a *App, open debridPreviewOpenMsg) {
	t.Helper()
	added, ok := a.debridAddCmd(open.gen, open.src, open.data)().(debridPreviewAddedMsg)
	if !ok {
		t.Fatal("add returned the wrong message")
	}
	a.onDebridPreviewAdded(added)
}

func waitPreview(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNoSwarm checks the torrent client never saw h: no torrent was added
// (so no metadata lookup and no peer connection), and every row is debrid.
func assertNoSwarm(t *testing.T, eng *engine.Engine, h metainfo.Hash) {
	t.Helper()
	if _, ok := eng.MetadataDiscovery(h); ok {
		t.Fatal("the torrent client has the torrent: the swarm was contacted")
	}
	if _, ok := eng.Files(h); ok {
		t.Fatal("the torrent client has the torrent's files")
	}
	for _, s := range eng.Snapshots() {
		if s.Via != "realdebrid" || s.PeersTotal != 0 {
			t.Fatalf("snapshot %+v is not a debrid item", s)
		}
	}
}

func TestDebridPreviewCacheHitShowsFilesBeforeRealDebrid(t *testing.T) {
	fake := &previewRD{convertCalls: 1 << 30} // Real-Debrid never finishes reading
	a, eng, cfg := debridPreviewApp(t, fake, "linked")
	mi, magnet := testTorrent(t, "Sintel", []metainfo.FileInfo{
		{Path: []string{"Sintel.mkv"}, Length: 900 << 20},
		{Path: []string{"Subs", "en.srt"}, Length: 40 << 10},
	})
	if err := metacache.New(cfg).Store(*mi); err != nil {
		t.Fatal(err)
	}

	open := openDebridPreview(t, a, a.launchCmd(magnet, "Sintel 2010", true))
	p := &a.preview
	if !p.ready || p.list != listFromCache || len(p.files) != 2 {
		t.Fatalf("preview ready=%v list=%v files=%v; want the cached list at once", p.ready, p.list, p.files)
	}
	if p.files[0].Path != "Sintel/Sintel.mkv" {
		t.Fatalf("path = %q, want the torrent client's naming", p.files[0].Path)
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"via Real-Debrid", "file list from the metadata cache · no swarm contacted", "Sintel.mkv"} {
		if !strings.Contains(view, want) {
			t.Errorf("preview missing %q:\n%s", want, view)
		}
	}

	addDebridPreview(t, a, open)
	if adds, _, _ := fake.snapshot(); !slices.Equal(adds, []string{"torrent"}) {
		t.Fatalf("adds = %v; a cache hit sends the .torrent", adds)
	}
	assertNoSwarm(t, eng, mi.HashInfoBytes())

	a.updatePreview(tea.KeyMsg{Type: tea.KeyEsc})
	if a.screen != screenResults {
		t.Fatalf("esc went to %v", a.screen)
	}
	waitPreview(t, "the remote delete", func() bool { _, _, del := fake.snapshot(); return len(del) == 1 })
	if _, ok := eng.DebridItem(mi.HashInfoBytes()); ok {
		t.Fatal("the debrid item survived esc")
	}
}

func TestDebridPreviewCacheMissWaitsForRealDebridList(t *testing.T) {
	fake := &previewRD{convertCalls: 1, name: "Movie", files: []debrid.RemoteFile{
		{ID: 7, Path: "Subs/en.srt", Bytes: 40 << 10},
		{ID: 3, Path: "Movie.mkv", Bytes: 900 << 20},
	}}
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	magnet := "magnet:?xt=urn:btih:3333333333333333333333333333333333333333&dn=Movie"

	open := openDebridPreview(t, a, a.launchCmd(magnet, "Movie 2024", true))
	if a.preview.ready {
		t.Fatal("preview has files before Real-Debrid read the torrent")
	}
	if view := ansi.Strip(a.View()); !strings.Contains(view, "Real-Debrid is reading the torrent…") || strings.Contains(view, "metadata peers") {
		t.Fatalf("waiting screen:\n%s", view)
	}
	addDebridPreview(t, a, open)
	waitPreview(t, "Real-Debrid's file list", func() bool { a.preview.refresh(eng); return a.preview.ready })

	p := &a.preview
	if p.list != listFromRemote || p.files[0].Path != "Movie/Subs/en.srt" || p.remoteIDs[0] != 7 || p.remoteIDs[1] != 3 {
		t.Fatalf("files = %+v ids = %v", p.files, p.remoteIDs)
	}
	if view := ansi.Strip(a.View()); !strings.Contains(view, "file list from Real-Debrid · no swarm contacted") {
		t.Fatalf("provenance line missing:\n%s", view)
	}
	assertNoSwarm(t, eng, open.hash)

	a.updatePreview(tea.KeyMsg{Type: tea.KeyEsc})
	waitPreview(t, "the remote delete", func() bool { _, _, del := fake.snapshot(); return len(del) == 1 })
}

func TestDebridPreviewTorrentFileUsesItsList(t *testing.T) {
	fake := &previewRD{convertCalls: 1 << 30}
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	mi, _ := testTorrent(t, "Album", []metainfo.FileInfo{
		{Path: []string{"01.flac"}, Length: 30 << 20},
		{Path: []string{"02.flac"}, Length: 31 << 20},
	})
	path := filepath.Join(t.TempDir(), "album.torrent")
	writeTorrent(t, path, mi)

	open := openDebridPreview(t, a, a.launchTorrentFilePreviewCmd(path, ""))
	if !a.preview.ready || a.preview.list != listFromTorrent || a.preview.name != "Album" {
		t.Fatalf("preview = ready %v list %v name %q", a.preview.ready, a.preview.list, a.preview.name)
	}
	addDebridPreview(t, a, open)
	assertNoSwarm(t, eng, mi.HashInfoBytes())
}

func TestPreviewWithoutDebridIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		link    string
		enabled bool
	}{{"not linked", "", true}, {"switched off", "linked", false}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, cfg := debridPreviewApp(t, &previewRD{}, tc.link)
			cfg.Debrid.Enabled = tc.enabled
			magnet := "magnet:?xt=urn:btih:4444444444444444444444444444444444444444"
			msg := a.launchCmd(magnet, "", true)()
			ready, ok := msg.(previewReadyMsg)
			if !ok || ready.err != nil {
				t.Fatalf("launch = %#v; want today's torrent preview", msg)
			}
			a.onPreviewReady(ready)
			if a.preview.debrid {
				t.Fatal("torrent preview is in debrid mode")
			}
			if strings.Contains(ansi.Strip(a.View()), "via Real-Debrid") {
				t.Fatal("torrent preview says via Real-Debrid")
			}
		})
	}
}

func TestDebridPreviewNeedsLoginOffersTorrent(t *testing.T) {
	fake := &previewRD{}
	a, _, _ := debridPreviewApp(t, fake, "needs-login")
	magnet := "magnet:?xt=urn:btih:5555555555555555555555555555555555555555&dn=Show"
	openDebridPreview(t, a, a.launchCmd(magnet, "Show", true))
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "needs a new login") || !strings.Contains(view, "t normal torrent") {
		t.Fatalf("needs-login preview:\n%s", view)
	}
	if adds, _, _ := fake.snapshot(); len(adds) != 0 {
		t.Fatalf("adds = %v; nothing may be sent without a login", adds)
	}
	_, cmd := a.updatePreview(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if a.screen != screenResults || cmd == nil {
		t.Fatalf("t: screen %v cmd %v", a.screen, cmd)
	}
	if _, ok := cmd().(previewReadyMsg); !ok {
		t.Fatal("t did not open the torrent preview")
	}
}

func TestDebridPreviewRefusedAddOffersTorrent(t *testing.T) {
	fake := &previewRD{addErr: fmt.Errorf("add: %w", debrid.ErrTooManyActive)}
	a, _, _ := debridPreviewApp(t, fake, "linked")
	magnet := "magnet:?xt=urn:btih:6666666666666666666666666666666666666666&dn=Show"
	open := openDebridPreview(t, a, a.launchCmd(magnet, "Show", true))
	addDebridPreview(t, a, open)
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "too many active downloads") || !strings.Contains(view, "t normal torrent") {
		t.Fatalf("refused preview:\n%s", view)
	}
	if _, cmd := a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || a.screen != screenPreview {
		t.Fatal("enter on a refused preview did something")
	}
	_, cmd := a.updatePreview(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if _, ok := cmd().(previewReadyMsg); !ok {
		t.Fatal("t did not open the torrent preview")
	}
}

// A preview the user left while the add was in flight must not leave the
// torrent on Real-Debrid.
func TestDebridPreviewLateAddIsCancelled(t *testing.T) {
	fake := &previewRD{convertCalls: 1 << 30}
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	magnet := "magnet:?xt=urn:btih:7777777777777777777777777777777777777777&dn=Late"
	open := openDebridPreview(t, a, a.launchCmd(magnet, "Late", true))
	a.updatePreview(tea.KeyMsg{Type: tea.KeyEsc})
	addDebridPreview(t, a, open)
	waitPreview(t, "the remote delete", func() bool { _, _, del := fake.snapshot(); return len(del) == 1 })
	if _, ok := eng.DebridItem(open.hash); ok {
		t.Fatal("late add left a debrid item")
	}
}

func writeTorrent(t *testing.T, path string, mi *metainfo.MetaInfo) {
	t.Helper()
	var b strings.Builder
	if err := mi.Write(&b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
