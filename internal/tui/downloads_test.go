package tui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kaanemec/tork-w-debrid/internal/clipboard"
	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/state"
	"github.com/muesli/termenv"
)

func TestDownloadItemsShowsMissingCompletedData(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir(), SeedAfterComplete: true}
	st := &state.State{}
	st.Upsert(state.Entry{
		Magnet:      "magnet:?xt=urn:btih:abc",
		Name:        "gone",
		AddedAt:     time.Now().UTC(),
		Done:        true,
		DownloadDir: cfg.DownloadDir,
		DataPath:    filepath.Join(cfg.DownloadDir, "gone"),
	})
	app := &App{cfg: cfg, st: st, downloads: newDownloadsModel()}
	cmd := app.startDownloadPathCheck(true)
	app.onDownloadPaths(cmd().(downloadPathsMsg))

	items := app.downloadItems()
	if len(items) != 1 {
		t.Fatalf("downloadItems returned %d items, want 1", len(items))
	}
	if items[0].State != engine.StateMissing {
		t.Fatalf("state = %s, want missing data", items[0].State)
	}
}

func TestUncheckedCompletedPathDoesNotTouchFilesystemFromViewModel(t *testing.T) {
	st := &state.State{}
	st.Upsert(state.Entry{
		Magnet:   "magnet:?xt=urn:btih:unchecked",
		Name:     "remote payload",
		Done:     true,
		DataPath: "/a/path/not-yet-checked-in-the-background",
	})
	app := &App{st: st, downloads: newDownloadsModel()}

	item := app.downloadItems()[0]
	if item.State != engine.StateDone || !strings.Contains(item.Note, "checking files") {
		t.Fatalf("unchecked item = state %s note %q", item.State, item.Note)
	}
}

func TestDownloadItemsShowsRelinkNeededData(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir(), SeedAfterComplete: true}
	st := &state.State{}
	st.Upsert(state.Entry{
		Magnet:      "magnet:?xt=urn:btih:def",
		Name:        "legacy",
		AddedAt:     time.Now().UTC(),
		NeedsRelink: true,
	})
	app := &App{cfg: cfg, st: st}

	items := app.downloadItems()
	if len(items) != 1 {
		t.Fatalf("downloadItems returned %d items, want 1", len(items))
	}
	if items[0].State != engine.StateMissing {
		t.Fatalf("state = %s, want missing data", items[0].State)
	}
}

func TestDownloadProjectionIsCachedAndActiveItemsLead(t *testing.T) {
	st := &state.State{Entries: []state.Entry{
		{Magnet: "done", Name: "old", Done: true, Length: 100, BytesCompleted: 100, DataPath: "/done"},
		{Magnet: "active", Name: "new"},
	}}
	a := &App{width: 100, height: 30, st: st, downloads: newDownloadsModel()}
	a.downloads.pathExists["/done"] = true
	a.downloads.snaps = []engine.Snapshot{{
		Magnet: "active", Name: "new", State: engine.StateDownloading,
		Length: 200, BytesCompleted: 50, SpeedBps: 25,
	}}
	a.refreshDownloadItems()

	first := a.downloadItems()
	second := a.downloadItems()
	if len(first) != 2 || &first[0] != &second[0] {
		t.Fatal("download projection was rebuilt instead of reused")
	}
	if first[0].Magnet != "active" {
		t.Fatalf("queue begins with %q, want active transfer", first[0].Magnet)
	}

	a.downloads.win.cursor = 0
	a.downloads.syncSelection(first)
	a.downloads.selectionPinned = true
	a.downloads.snaps[0].State = engine.StateDone
	a.downloads.snaps[0].BytesCompleted = 200
	a.refreshDownloadItems()
	if got := a.downloadItems()[a.downloads.win.cursor].Magnet; got != "active" {
		t.Fatalf("selection moved to %q when active item joined history", got)
	}
}

func TestDownloadMetricsAreWeightedAcrossQueue(t *testing.T) {
	m := summarizeDownloads([]downloadItem{
		{State: engine.StateDownloading, Length: 1000, BytesCompleted: 250, SpeedBps: 100, PeersActive: 2, PeersTotal: 5, Seeders: 1},
		{State: engine.StateDone, Length: 500, BytesCompleted: 500},
		{State: engine.StatePaused, Length: 500, BytesCompleted: 250},
	})
	if m.Total != 3 || m.Active != 1 || m.Done != 1 || m.Paused != 1 {
		t.Fatalf("state metrics = %+v", m)
	}
	if m.Length != 2000 || m.BytesCompleted != 1000 || m.Progress() != 0.5 {
		t.Fatalf("byte metrics = completed %d length %d progress %.2f", m.BytesCompleted, m.Length, m.Progress())
	}
	if m.ETA() != 7500*time.Millisecond {
		t.Fatalf("queue ETA = %s, want 7.5s", m.ETA())
	}
}

func TestDownloadsDashboardShowsTransferFactsAndFits(t *testing.T) {
	a := &App{width: 100, height: 32, screen: screenDownloads, downloads: newDownloadsModel()}
	a.downloads.snaps = []engine.Snapshot{{
		Magnet: "magnet:?xt=urn:btih:active", Name: "A very useful Linux image",
		State: engine.StateDownloading, Length: 4 << 30, BytesCompleted: 1 << 30,
		SpeedBps: 12 << 20, ETA: 4 * time.Minute, PeersActive: 8, PeersTotal: 21, Seeders: 5,
		DownloadDir: "/downloads", DataPath: "/downloads/linux.iso",
		Metadata: engine.MetadataStatus{Trackers: 7, Source: engine.MetadataPeers, DHTEnabled: true},
	}}
	a.refreshDownloadItems()
	view := a.viewDownloads()
	for _, want := range []string{
		"downloads", "↓ 12.0 MiB/s", "A very useful Linux image", "25%", "12.0 MiB/s · 4m00s",
		"/downloads/linux.iso", "peers 8/21", "5 seeders",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard omits %q", want)
		}
	}
	if strings.Contains(view, "╭") || strings.Contains(view, "╰") {
		t.Error("the detail card's border is gone in Nightglass")
	}
	assertRenderFits(t, view, a.width)

	a.width, a.height = 32, 14
	assertRenderFits(t, a.viewDownloads(), a.width)
}

func TestDeleteDownloadDataRefusesUnsafePath(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := deleteDownloadData(downloadItem{DownloadDir: dir, DataPath: outside})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("deleteDownloadData err = %v, want unsafe refusal", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unsafe path was removed: %v", err)
	}
}

func TestMovePayloadMovesPartFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "image.iso")
	newPath := filepath.Join(dir, "moved", "image.iso")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath+".part", []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath+".part.meta", []byte("manifest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := movePayload(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath + ".part"); !os.IsNotExist(err) {
		t.Fatalf("old part still exists: %v", err)
	}
	got, err := os.ReadFile(newPath + ".part")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "partial" {
		t.Fatalf("moved part = %q", got)
	}
	got, err = os.ReadFile(newPath + ".part.meta")
	if err != nil || string(got) != "manifest" {
		t.Fatalf("moved manifest = %q, %v", got, err)
	}
}

func TestDeleteDownloadDataRemovesDirectSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image.iso")
	for _, suffix := range []string{"", ".part", ".part.meta"} {
		if err := os.WriteFile(path+suffix, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := deleteDownloadData(downloadItem{DownloadDir: dir, DataPath: path}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".part", ".part.meta"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("%s remained: %v", path+suffix, err)
		}
	}
}

func TestClipboardSequenceCopiesFullText(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")

	got := clipboard.Sequence("/home/cat/Downloads/image.iso").String()
	want := "\x1b]52;c;L2hvbWUvY2F0L0Rvd25sb2Fkcy9pbWFnZS5pc28=\x07"
	if got != want {
		t.Fatalf("clipboard sequence = %q, want %q", got, want)
	}
}

func TestVerificationGuardsDoNotPersistRefusedPauseOrSeed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	cfg, err := config.LoadFrom(filepath.Join(t.TempDir(), ".tork"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("c", 40)
	h, err := eng.Add(magnet, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	verifyErr := make(chan error, 1)
	go func() {
		_, err := eng.Verify(ctx, h)
		verifyErr <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if snap, ok := eng.Snapshot(h); ok && snap.State == engine.StateVerifying {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("verification did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	st := &state.State{}
	st.Upsert(state.Entry{Magnet: magnet, Seed: state.Bool(false)})
	app := &App{cfg: cfg, eng: eng, st: st}
	stale := downloadItem{Hash: h, Magnet: magnet, State: engine.StateDone, Live: true}
	app.togglePause(stale)
	if st.Entries[0].Paused || !strings.Contains(app.errText, "verification") {
		t.Fatalf("pause guard persisted=%v err=%q", st.Entries[0].Paused, app.errText)
	}
	app.toggleSeed(stale)
	if *st.Entries[0].Seed || !strings.Contains(app.errText, "verification") {
		t.Fatalf("seed guard persisted=%v err=%q", *st.Entries[0].Seed, app.errText)
	}
	cancel()
	<-verifyErr
}

func TestVerifyPersistedDirectUsesActivatedHashAndShowsResult(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	cfg, err := config.LoadFrom(filepath.Join(t.TempDir(), ".tork"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	payload := []byte("persisted ISO")
	if err := os.MkdirAll(cfg.DownloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DownloadDir, "image.iso")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	url := "https://example.invalid/image.iso"
	st := &state.State{}
	st.Upsert(state.Entry{Magnet: url, Name: "image.iso", SHA256: fmt.Sprintf("%x", sha256.Sum256(payload)), Done: true, DownloadDir: cfg.DownloadDir, DataPath: path})
	app := New(cfg, eng, nil, st, nil)
	it := app.itemFromEntry(&st.Entries[0], 0)
	cmd := app.verifyDownload(it)
	if cmd == nil {
		t.Fatal("verify command is nil")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("verify command message = %T, want tea.BatchMsg", msg)
	}
	var done verifyDoneMsg
	found := false
	for _, child := range batch {
		if msg := child(); msg != nil {
			if verifyMsg, ok := msg.(verifyDoneMsg); ok {
				done, found = verifyMsg, true
			}
		}
	}
	if !found || done.err != nil || done.hash == (metainfo.Hash{}) {
		t.Fatalf("verify result found=%v msg=%+v", found, done)
	}
	app.onVerifyDone(done)
	if app.toast.text != "verified - all data is valid" || app.toast.tone != toastOK {
		t.Fatalf("verify toast=%q tone=%v", app.toast.text, app.toast.tone)
	}
}

func TestApplyVerifyResultClearsCompletionState(t *testing.T) {
	now := time.Now()
	entry := state.Entry{Done: true, Paused: true, BytesCompleted: 42, CompletedAt: &now}
	if !applyVerifyResultToEntry(&entry, engine.VerifyResult{NeedsRepair: true, BadPieces: 1}) {
		t.Fatal("torrent repair did not report a state change")
	}
	if entry.Done || entry.CompletedAt != nil || entry.Paused || entry.BytesCompleted != 42 {
		t.Fatalf("torrent repair entry = %+v", entry)
	}
	entry.Done, entry.BytesCompleted, entry.CompletedAt = true, 42, &now
	if !applyVerifyResultToEntry(&entry, engine.VerifyResult{NeedsRepair: true, ChecksumMismatch: true}) {
		t.Fatal("direct mismatch did not report a state change")
	}
	if entry.Done || entry.CompletedAt != nil || !entry.Paused || entry.BytesCompleted != 0 {
		t.Fatalf("direct mismatch entry = %+v", entry)
	}
}

func cloudSnap(name string, state engine.TorrentState, status debrid.RemoteStatus, progress float64) engine.Snapshot {
	s := engine.Snapshot{
		Magnet: "magnet:?xt=urn:btih:" + name, Name: name, State: state, Via: "realdebrid",
		Length: 1000, BytesCompleted: int64(progress * 10), SpeedBps: 12 << 20, Seeders: 8,
		Remote: &engine.RemoteProgress{Status: status, Progress: progress},
	}
	return s
}

func TestCloudRowsShowOnlyForTheCloudPhase(t *testing.T) {
	a := &App{width: 100, height: 30, screen: screenDownloads, downloads: newDownloadsModel()}
	local := cloudSnap("local", engine.StateDownloading, debrid.Downloading, 100)
	local.SpeedBps, local.BytesCompleted = 5<<20, 400
	a.downloads.snaps = []engine.Snapshot{
		cloudSnap("busy", engine.StateRemote, debrid.Downloading, 43),
		cloudSnap("waiting", engine.StateRemote, debrid.Queued, 0),
		local,
	}
	a.refreshDownloadItems()
	items := a.downloadItems()
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}
	byName := map[string]downloadItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if it := byName["busy"]; it.Route != routeCloud || it.CloudNote != "Real-Debrid 43 % · 12.0 MiB/s · 8 seeders" || it.CloudProgress != 0.43 {
		t.Errorf("busy = %+v", it)
	}
	if it := byName["waiting"]; it.Route != routeCloud || it.CloudNote != "queued on Real-Debrid" {
		t.Errorf("waiting = %+v", it)
	}
	if it := byName["local"]; it.Route != routeLocal || !it.ViaDebrid {
		t.Errorf("local = %+v", it)
	}
	if m := a.downloads.metrics; m.Cloud != 2 || m.SpeedBps != 5<<20 {
		t.Errorf("metrics = %+v, want 2 cloud items and only the local speed", m)
	}
	for _, it := range items { // the cloud glyph and text belong to the first two only
		plain := ansi.Strip(strings.Join(a.renderDownloadItem(it, false, 100), "\n"))
		wantCloud := it.Name != "local"
		if got := strings.Contains(plain, glyphs.cloud); got != wantCloud {
			t.Errorf("%s: cloud glyph shown = %v\n%s", it.Name, got, plain)
		}
		if strings.Contains(plain, "Real-Debrid") != wantCloud {
			t.Errorf("%s: Real-Debrid text mismatch\n%s", it.Name, plain)
		}
	}
	view := ansi.Strip(a.viewDownloads())
	if !strings.Contains(view, "↓ 5.0 MiB/s · ☁\uFE0E 2") {
		t.Errorf("header chip missing:\n%s", view)
	}
	assertRenderFits(t, a.viewDownloads(), 100)
	a.downloads.snaps = a.downloads.snaps[2:]
	a.refreshDownloadItems()
	if strings.Contains(ansi.Strip(a.viewDownloads()), "☁") {
		t.Error("the cloud chip shows with no cloud item")
	}
}

func TestCloudDetailLineExplainsTheHandOff(t *testing.T) {
	it := downloadItem{Route: routeCloud, ViaDebrid: true, RemoteFiles: 3}
	got := ansi.Strip(debridDetail(it, 200))
	for _, want := range []string{"Route: Real-Debrid", "3 files selected", "keep torkwd running"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail %q misses %q", got, want)
		}
	}
	if debridDetail(downloadItem{State: engine.StateDownloading, ViaDebrid: true}, 100) != "" {
		t.Error("a local debrid row must look like any other download")
	}
}

func TestFailedDebridRowShowsReasonAndWay(t *testing.T) {
	a := &App{width: 100, height: 30, screen: screenDownloads, downloads: newDownloadsModel()}
	a.downloads.items = []downloadItem{{Name: "Dead torrent", ViaDebrid: true, Failed: true, Live: true,
		State: engine.StatePaused, Note: "☁ RD dead: no seeders"}}
	a.downloads.itemsReady = true
	view := ansi.Strip(a.viewDownloads())
	for _, want := range []string{"dead: no seeders", "Route: Real-Debrid", "t download it as a normal torrent"} {
		if !strings.Contains(view, want) {
			t.Errorf("failed row misses %q:\n%s", want, view)
		}
	}
	assertRenderFits(t, a.viewDownloads(), 100)
}

// A Real-Debrid row in trouble shows the problem and `t` on the row, in the
// footer and once in a toast; `t` asks first unless Real-Debrid refused it.
func TestDebridTroubleOffersTheTorrent(t *testing.T) {
	a := &App{width: 110, height: 30, screen: screenDownloads, downloads: newDownloadsModel()}
	stuck := downloadItem{Name: "Stuck show", Magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		ViaDebrid: true, Live: true, Route: routeCloud, State: engine.StateRemote, Trouble: "RD stuck · 0 seeders"}
	a.downloads.items = []downloadItem{stuck}
	a.downloads.itemsReady = true
	view := ansi.Strip(a.viewDownloads())
	for _, want := range []string{"!  Stuck show", "RD stuck · 0 seeders  t torrent", "t download it as a normal torrent · or keep waiting", "enter open   t torrent"} {
		if !strings.Contains(view, want) {
			t.Errorf("trouble row misses %q:\n%s", want, view)
		}
	}
	assertRenderFits(t, a.viewDownloads(), 110)

	if cmd := a.downloadAsTorrent(stuck); cmd != nil {
		t.Fatal("a row Real-Debrid has not refused must ask before switching")
	}
	conf := a.downloads.confirmRemove
	if conf == nil || !conf.toTorrent || !conf.deleteRemote || conf.deleteData {
		t.Fatalf("dialog = %+v, want the torrent dialog with the cloud copy deleted", conf)
	}
	if view := ansi.Strip(a.viewDownloads()); !strings.Contains(view, "download as a normal torrent (peers see your IP)?") || !strings.Contains(view, "c cloud copy: delete") {
		t.Errorf("dialog text:\n%s", view)
	}
	a.downloads.confirmRemove = nil

	local := stuck
	local.DebridLocal, local.Route, local.State = true, routeLocal, engine.StatePaused
	a.downloadAsTorrent(local)
	if c := a.downloads.confirmRemove; c == nil || !c.deleteData {
		t.Fatalf("local phase dialog = %+v, want Real-Debrid's partial files deleted", c)
	}
	a.downloads.confirmRemove = nil

	refused := stuck
	refused.Failed = true
	if cmd := a.downloadAsTorrent(refused); cmd == nil || a.downloads.confirmRemove != nil {
		t.Fatal("a refused row switches at once")
	}

	snap := engine.Snapshot{Name: "Stuck show", Via: "realdebrid", Trouble: "can't reach RD"}
	if a.troubleToast(snap) == nil {
		t.Fatal("no toast for new trouble")
	}
	if a.troubleToast(snap) != nil {
		t.Fatal("the toast came twice")
	}
	snap.Trouble = ""
	a.troubleToast(snap)
	snap.Trouble = "can't reach RD"
	if a.troubleToast(snap) == nil {
		t.Fatal("trouble that came back is not told again")
	}
}

func TestRemoveDialogOffersTheCloudToggle(t *testing.T) {
	a := &App{width: 100, height: 30, screen: screenDownloads, downloads: newDownloadsModel()}
	cloud := downloadItem{Name: "c", Live: true, ViaDebrid: true, Route: routeCloud}
	local := downloadItem{Name: "l", Live: true, ViaDebrid: true, State: engine.StateDownloading}
	torrent := downloadItem{Name: "t", Live: true, State: engine.StateDownloading}

	if c := a.newRemoveConfirm(cloud, false); !c.debrid || !c.deleteRemote {
		t.Errorf("cloud phase: %+v, want the toggle on", c)
	}
	if c := a.newRemoveConfirm(local, false); !c.debrid || c.deleteRemote {
		t.Errorf("local phase: %+v, want the toggle off", c)
	}
	if c := a.newRemoveConfirm(torrent, true); c.debrid || c.options().DeleteRemote || !c.options().DeleteData {
		t.Errorf("torrent: %+v", c)
	}

	a.downloads.items, a.downloads.itemsReady = []downloadItem{cloud}, true
	a.downloads.confirmRemove = a.newRemoveConfirm(cloud, false)
	if view := ansi.Strip(a.helpLineForTest()); !strings.Contains(view, "cloud copy: delete") {
		t.Errorf("dialog footer = %q", view)
	}
	a.updateRemoveConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if a.downloads.confirmRemove.options().DeleteRemote {
		t.Error("c did not turn the cloud delete off")
	}
	if view := ansi.Strip(a.helpLineForTest()); !strings.Contains(view, "cloud copy: keep") {
		t.Errorf("dialog footer = %q", view)
	}
}

func (a *App) helpLineForTest() string { return a.viewDownloads() }

// fourSegments is a 100 MB file on four connections: the first two segments
// are mostly done, the third has just started, the fourth is retrying.
func fourSegments() engine.SegmentMap {
	const mb = 1 << 20
	return engine.SegmentMap{
		Total: 100 * mb, Limit: 4, Connections: 3,
		Completed: []engine.ByteSpan{{Start: 0, End: 12*mb - 1}},
		Segments: []engine.SegmentInfo{
			{Start: 12 * mb, End: 37*mb - 1, Done: 10 * mb, SpeedBps: 6.1e6},
			{Start: 37 * mb, End: 62*mb - 1, Done: 5 * mb, SpeedBps: 5.9e6},
			{Start: 62 * mb, End: 87*mb - 1, Done: 1 * mb, SpeedBps: 6.0e6},
			{Start: 87 * mb, End: 100*mb - 1, SpeedBps: 0.4e6, Retrying: true, Tries: 1},
		},
	}
}

func TestSegmentCellsFromAFixedMap(t *testing.T) {
	cells, speeds, warn := segmentCells(fourSegments(), 16)
	if len(cells) != 16 || len(speeds) != 4 || len(warn) != 4 {
		t.Fatalf("%d cells, %d speeds, %d marks", len(cells), len(speeds), len(warn))
	}
	if cells[0] != segmentDone || cells[1] != segmentDone {
		t.Errorf("the first 12 MB must be done: %v", cells)
	}
	if cells[15] != segmentPart || cells[14] != segmentPart {
		t.Errorf("the last cells have a connection in them: %v", cells)
	}
	if !warn[3] || warn[0] {
		t.Errorf("only the retrying connection is marked: %v", warn)
	}
	if c, _, _ := segmentCells(engine.SegmentMap{Total: 100, Limit: 1, Segments: []engine.SegmentInfo{{End: 99}}}, 16); c != nil {
		t.Error("a single-connection download gets no map")
	}
}

func TestSegmentLineShowsMapSpeedsAndAmberRetry(t *testing.T) {
	withProfile(t, termenv.TrueColor, true)
	cells, speeds, warn := segmentCells(fourSegments(), 16)
	for _, w := range []int{80, 60, 30} {
		line := segmentLine(cells, speeds, warn, w-4)
		if lipgloss.Width(line) > w-4 {
			t.Errorf("segment line is %d cells at width %d", lipgloss.Width(line), w)
		}
	}
	line := segmentLine(cells, speeds, warn, 76)
	plain := ansi.Strip(line)
	for _, want := range []string{"▕", "▏", "4 connections · 6.1 · 5.9 · 6.0 · 0.4 MB/s"} {
		if !strings.Contains(plain, want) {
			t.Errorf("line %q misses %q", plain, want)
		}
	}
	amber := ""
	for _, r := range sgrRuns(line) {
		if r.text == "0.4" {
			amber = r.fg
		}
		if r.text == "6.1" && r.fg == amber && amber != "" {
			t.Error("a healthy connection is drawn in the retry colour")
		}
	}
	if amber == "" {
		t.Error("the retried connection has no colour")
	}
	healthy := ""
	for _, r := range sgrRuns(line) {
		if r.text == "6.1" {
			healthy = r.fg
		}
	}
	if healthy == amber {
		t.Errorf("retried and healthy speeds share colour %q", amber)
	}
}

func TestSelectedRowDrawsSegmentLineAt80Columns(t *testing.T) {
	a := &App{width: 80, height: 24, screen: screenDownloads, downloads: newDownloadsModel()}
	cells, speeds, warn := segmentCells(fourSegments(), 16)
	a.downloads.items = []downloadItem{
		{Name: "a.iso", State: engine.StateDownloading, Length: 100, BytesCompleted: 40, SpeedBps: 18e6, Segments: cells, ConnSpeeds: speeds, ConnWarn: warn},
		{Name: "b.iso", State: engine.StateDownloading, Length: 100, BytesCompleted: 40, SpeedBps: 18e6, Segments: cells, ConnSpeeds: speeds, ConnWarn: warn},
	}
	a.downloads.itemsReady = true
	view := a.viewDownloads()
	assertRenderFits(t, view, 80)
	if n := strings.Count(ansi.Strip(view), "4 connections"); n != 1 {
		t.Errorf("the segment line shows %d times, want once (selected row only)", n)
	}
}

// A debrid row held by a network outage restarts by itself in the engine;
// the tick must keep running so the row (and state.json) follow it.
func TestTickContinuesWhileDebridRowRetries(t *testing.T) {
	a := &App{screen: screenDownloads}
	a.downloads.snaps = []engine.Snapshot{{State: engine.StatePaused}}
	if a.tickShouldContinue() {
		t.Fatal("a plainly paused row keeps the tick running")
	}
	a.downloads.snaps[0].Retrying = true
	if !a.tickShouldContinue() {
		t.Fatal("the tick stops while a held row waits to restart by itself")
	}
}

// A debrid download finished in an earlier run is only in state.json, not in
// the engine. Removing it must still offer the cloud copy and delete it by
// the saved remote ID.
func TestRemoveSavedDebridRowDeletesCloudCopy(t *testing.T) {
	fake := &previewRD{}
	a, _, _ := debridPreviewApp(t, fake, "linked")
	a.screen = screenDownloads
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Old"
	a.st.Upsert(state.Entry{Magnet: magnet, Name: "Old", Done: true, Via: "realdebrid", RemoteID: "RID9", Phase: "local"})
	row := downloadItem{Name: "Old", Magnet: magnet, ViaDebrid: true}

	c := a.newRemoveConfirm(row, false)
	if !c.debrid || c.deleteRemote || c.remoteID != "RID9" {
		t.Fatalf("saved debrid row: %+v, want the toggle, off, with RID9", c)
	}
	a.downloads.confirmRemove = c
	a.updateRemoveConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	_, cmd := a.updateRemoveConfirm(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("no cloud delete was started")
	}
	msg, ok := cmd().(removeDebridDoneMsg)
	if !ok || !msg.res.RemoteDeleted {
		t.Fatalf("result %#v", msg)
	}
	if a.st.Find(magnet) != nil {
		t.Error("the row is still in state.json")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deletes) != 1 || fake.deletes[0] != "RID9" {
		t.Errorf("deletes = %v, want [RID9]", fake.deletes)
	}

	// a plain torrent row from state.json gets no toggle
	a.st.Upsert(state.Entry{Magnet: "magnet:?xt=urn:btih:89abcdef0123456789abcdef0123456789abcdef", Name: "T"})
	if c := a.newRemoveConfirm(downloadItem{Magnet: "magnet:?xt=urn:btih:89abcdef0123456789abcdef0123456789abcdef"}, false); c.debrid {
		t.Error("a torrent row offers a cloud copy")
	}
}

func TestSavedDebridRowKeepsArchiveNote(t *testing.T) {
	a := &App{width: 100, height: 30, screen: screenDownloads, downloads: newDownloadsModel()}
	a.downloads.pathExists = map[string]bool{"/d/Sintel": true}
	it := a.itemFromEntry(&state.Entry{Magnet: "m", Name: "Sintel", Done: true, DataPath: "/d/Sintel", Via: "realdebrid", RemoteNote: "Real-Debrid sent one archive"}, 0)
	if it.Note != "Real-Debrid sent one archive" || !showDownloadNote(it.Note) {
		t.Fatalf("note = %q", it.Note)
	}
}
