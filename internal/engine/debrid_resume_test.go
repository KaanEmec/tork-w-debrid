package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

// Acceptance tests for roadmap J3.4: debrid downloads survive a restart in
// either phase.

// killableEngine is a debrid test engine whose Close a test may call early
// (a "kill"); the cleanup closes it only once.
func killableEngine(t *testing.T, fake debrid.Provider) (*Engine, func()) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	cfg, err := config.LoadFrom(filepath.Join(t.TempDir(), ".tork"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.directMinChunkSize = 32 << 10
	if fake != nil {
		eng.SetDebridProvider(fake)
	}
	var once sync.Once
	kill := func() { once.Do(eng.Close) }
	t.Cleanup(kill)
	return eng, kill
}

// saveEntry writes the item's state.json entry, as the TUI does, and reads
// the file back as a new run would.
func saveEntry(t *testing.T, eng *Engine, h metainfo.Hash, statePath string) state.Entry {
	t.Helper()
	entry, ok := eng.DebridEntry(h)
	if !ok {
		t.Fatal("no state entry for a started debrid item")
	}
	if s, ok := eng.Snapshot(h); ok {
		ApplyDebridState(&entry, s)
		entry.BytesCompleted, entry.Length = s.BytesCompleted, s.Length
	}
	st := &state.State{}
	st.Upsert(entry)
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.Load(statePath)
	if err != nil || len(loaded.Entries) != 1 {
		t.Fatalf("state reload: %v, %d entries", err, len(loaded.Entries))
	}
	return loaded.Entries[0]
}

func checkStateFileClean(t *testing.T, statePath string, secrets ...string) {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(secrets, "token", "https://", "http://") {
		if strings.Contains(string(data), s) {
			t.Fatalf("state.json contains %q:\n%s", s, data)
		}
	}
}

func TestDebridResumesFromCloudPhaseAfterRestart(t *testing.T) {
	fastDebrid(t, 10*time.Millisecond)
	scaleDirectTuning(t, 0.01)
	files := []rdFile{{1, "e01.mkv", testPayload(200<<10, 1)}, {2, "e02.mkv", testPayload(100<<10, 2)}}
	host := newRDHost(t, hostFiles(files))
	var ready atomic.Bool
	fake := &fakeDebrid{unlock: unlockTo(host, files)}
	readyInfo := readyTorrent("Show", files, rdLinks(2))
	fake.info = func(n int, selected bool) (debrid.RemoteInfo, error) {
		info, err := readyInfo(n, selected)
		if selected && !ready.Load() {
			info.Status, info.Progress, info.Links = debrid.Downloading, 40, nil
		}
		return info, err
	}
	dir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")

	eng1, kill := killableEngine(t, fake)
	h := startLocal(t, eng1, dir)
	awaitHash(t, eng1, h, func(s Snapshot) bool { return s.State == StateRemote }, 10*time.Second)
	entry := saveEntry(t, eng1, h, statePath)
	if entry.Via != "realdebrid" || entry.RemoteID != "RID1" || entry.Phase != DebridSavedCloud || entry.Magnet != testMagnet {
		t.Fatalf("saved entry = %+v", entry)
	}
	kill()

	ready.Store(true)
	eng2, _ := killableEngine(t, fake)
	if _, err := eng2.ResumeDebrid(entry); err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng2, h, isDone, 20*time.Second)
	for _, f := range files {
		checkFile(t, filepath.Join(dir, "Show", f.path), f.data)
	}
	if n := fake.addCount(); n != 1 {
		t.Fatalf("the torrent was added %d times, want once", n)
	}
	entry = saveEntry(t, eng2, h, statePath)
	if entry.Phase != DebridSavedLocal {
		t.Fatalf("phase after the download = %q", entry.Phase)
	}
	checkStateFileClean(t, statePath, host.URL)
}

func TestDebridResumesFromLocalPhaseWithRange(t *testing.T) {
	fastDebrid(t, 10*time.Millisecond)
	scaleDirectTuning(t, 0.01)
	const size = 2 << 20
	files := []rdFile{{1, "big.bin", testPayload(size, 4)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 1 << 20
	fake := &fakeDebrid{info: readyTorrent("big.bin", files, rdLinks(1))}
	fake.unlock = func(n int, link string) (debrid.Unlocked, error) {
		// every unlock gives a new URL, as Real-Debrid does
		return debrid.Unlocked{URL: host.url("u"+strconv.Itoa(n), "big.bin"), Filename: "big.bin", Bytes: size, Link: link}, nil
	}
	dir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")

	eng1, kill := killableEngine(t, fake)
	eng1.SetDebridSettings(config.DebridConfig{Connections: 2, MaxTotalConnections: 8})
	h := startLocal(t, eng1, dir)
	awaitHash(t, eng1, h, func(s Snapshot) bool { return s.BytesCompleted >= size/4 }, 20*time.Second)
	entry := saveEntry(t, eng1, h, statePath)
	if entry.Phase != DebridSavedLocal {
		t.Fatalf("saved phase = %q", entry.Phase)
	}
	kill()
	unlocks := fake.unlockCalls.Load()
	host.served.Store(0)

	eng2, _ := killableEngine(t, fake)
	eng2.SetDebridSettings(config.DebridConfig{Connections: 2, MaxTotalConnections: 8})
	if _, err := eng2.ResumeDebrid(entry); err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng2, h, isDone, 30*time.Second)
	checkFile(t, filepath.Join(dir, "big.bin"), files[0].data)
	if got := fake.unlockCalls.Load(); got != unlocks+1 {
		t.Fatalf("the second run unlocked %d times, want once", got-unlocks)
	}
	// the second run only uses its own fresh link, and never asks for byte 0
	fresh := host.seen("/u" + strconv.Itoa(int(unlocks)) + "/big.bin")
	var ranged int
	for _, r := range fresh {
		if r == "bytes=0-0" {
			continue
		}
		start, _, ok := parseBenchRange(r, size)
		if !ok || start == 0 {
			t.Fatalf("second run asked for %q, want a Range past the saved bytes", r)
		}
		ranged++
	}
	if ranged == 0 {
		t.Fatal("the second run made no ranged request on the fresh link")
	}
	if served := host.served.Load(); served >= size {
		t.Fatalf("the second run downloaded %d bytes of %d", served, size)
	}
	checkStateFileClean(t, statePath, host.URL)
}

func TestDebridResumeReAddsTorrentDeletedOnService(t *testing.T) {
	fastDebrid(t, 10*time.Millisecond)
	scaleDirectTuning(t, 0.01)
	files := []rdFile{{1, "a.bin", testPayload(100, 1)}, {2, "b.bin", testPayload(50<<10, 2)}, {3, "c.bin", testPayload(100, 3)}}
	host := newRDHost(t, hostFiles(files))
	fake := &fakeDebrid{gone: map[string]bool{"RIDold": true}}
	fake.unlock = func(_ int, link string) (debrid.Unlocked, error) {
		return debrid.Unlocked{URL: host.url("x", "b.bin"), Filename: "b.bin", Bytes: 50 << 10, Link: link}, nil
	}
	fake.info = func(_ int, selected bool) (debrid.RemoteInfo, error) {
		info := debrid.RemoteInfo{Name: "Pack", Status: debrid.WaitingSelection}
		for _, f := range files {
			info.Files = append(info.Files, debrid.RemoteFile{ID: f.id, Path: f.path, Bytes: int64(len(f.data)), Selected: selected && f.id == 2})
		}
		if selected {
			info.Status, info.Bytes, info.Links = debrid.Ready, 50<<10, []string{"rd/1"}
		}
		return info, nil
	}
	dir := t.TempDir()
	eng, _ := killableEngine(t, fake)
	entry := state.Entry{
		Magnet: testMagnet, Name: "Pack", DownloadDir: dir,
		Via: "realdebrid", RemoteID: "RIDold", RemoteFiles: []int{2}, Phase: DebridSavedCloud,
	}
	h, err := eng.ResumeDebrid(entry)
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 20*time.Second)
	if got := fake.selectCalls(); len(got) != 1 || !slices.Equal(got[0], []int{2}) {
		t.Fatalf("SelectFiles calls = %v, want the saved selection [[2]]", got)
	}
	if fake.addCount() != 1 || fake.adds[0] != testMagnet {
		t.Fatalf("adds = %v, want the saved magnet once", fake.adds)
	}
	checkFile(t, filepath.Join(dir, "Pack", "b.bin"), files[1].data)
	if _, err := os.Stat(filepath.Join(dir, "Pack", "a.bin")); !os.IsNotExist(err) {
		t.Fatal("a file that was not chosen was downloaded")
	}
	if info, _ := eng.DebridItem(h); info.RemoteID != "RID1" {
		t.Fatalf("remote ID after the re-add = %q", info.RemoteID)
	}
}

func TestDebridNeedsLoginHoldsUntilLinked(t *testing.T) {
	fastDebrid(t, 10*time.Millisecond)
	scaleDirectTuning(t, 0.01)
	files := []rdFile{{1, "m.mkv", testPayload(20<<10, 6)}}
	host := newRDHost(t, hostFiles(files))
	entry := state.Entry{
		Magnet: testMagnet, Name: "m.mkv", DownloadDir: t.TempDir(),
		Via: "realdebrid", RemoteID: "RID9", Phase: DebridSavedLocal,
	}

	// no provider at all: held at once, without a call
	eng, _ := killableEngine(t, nil)
	h, err := eng.ResumeDebrid(entry)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.Snapshot(h)
	if s.State != StatePaused || s.Note != "☁ Real-Debrid account needs login" {
		t.Fatalf("snapshot without a provider = state %v note %q", s.State, s.Note)
	}

	// a provider whose login was revoked: held after the first call
	revoked := &fakeDebrid{infoErr: &debrid.Error{Provider: "Real-Debrid", Kind: debrid.ErrNotLinked}}
	eng.SetDebridProvider(revoked)
	awaitCond(t, "the first Info", 5*time.Second, func() bool { return revoked.infoCalls.Load() > 0 })
	s = awaitHash(t, eng, h, isPaused, 5*time.Second)
	if s.Note != "☁ Real-Debrid account needs login" {
		t.Fatalf("note = %q", s.Note)
	}
	calls := revoked.infoCalls.Load()
	time.Sleep(40 * time.Millisecond)
	if got := revoked.infoCalls.Load(); got != calls {
		t.Fatalf("Info polled %d more times while the login is missing", got-calls)
	}
	if info, _ := eng.DebridItem(h); info.Phase != DebridLocal {
		t.Fatalf("phase while held = %v", info.Phase)
	}

	// linking again continues the download
	linked := &fakeDebrid{info: readyTorrent("m.mkv", files, rdLinks(1)), unlock: unlockTo(host, files), selected: true}
	eng.SetDebridProvider(linked)
	awaitHash(t, eng, h, isDone, 20*time.Second)
	checkFile(t, filepath.Join(entry.DownloadDir, "m.mkv"), files[0].data)
}

func TestDebridResumeOfFinishedEntryMakesNoCall(t *testing.T) {
	fake := &fakeDebrid{}
	eng, _ := killableEngine(t, fake)
	dir := t.TempDir()
	entry := state.Entry{
		Magnet: testMagnet, Name: "Show", DownloadDir: dir, DataPath: filepath.Join(dir, "Show"),
		Via: "realdebrid", RemoteID: "RID1", Phase: DebridSavedLocal, Done: true, Length: 1234, BytesCompleted: 1234,
	}
	h, err := eng.ResumeDebrid(entry)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng.Snapshot(h)
	if s.State != StateDone || s.Length != 1234 || s.BytesCompleted != 1234 || s.DataPath != entry.DataPath {
		t.Fatalf("snapshot = %+v", s)
	}
	time.Sleep(20 * time.Millisecond)
	if fake.infoCalls.Load() != 0 || fake.unlockCalls.Load() != 0 {
		t.Fatal("a finished entry called the service")
	}
}

func TestDebridSelectionMapsByPathAndSize(t *testing.T) {
	it := &debridItem{
		selected: []int{2, 3},
		prevFiles: []debrid.RemoteFile{
			{ID: 1, Path: "a", Bytes: 1}, {ID: 2, Path: "b", Bytes: 2}, {ID: 3, Path: "c", Bytes: 3},
		},
	}
	now := []debrid.RemoteFile{{ID: 10, Path: "c", Bytes: 3}, {ID: 11, Path: "b", Bytes: 2}, {ID: 12, Path: "a", Bytes: 1}}
	if got := it.selectionFor(now); !slices.Equal(got, []int{11, 10}) {
		t.Fatalf("selection = %v, want [11 10]", got)
	}
	it.prevFiles = nil
	if got := it.selectionFor(now); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("selection without an old list = %v", got)
	}
}

// "Real-Debrid sent one archive" is saved with the entry and shown again
// after a restart.
func TestDebridArchiveNoteSurvivesRestart(t *testing.T) {
	entry := state.Entry{Magnet: testMagnet}
	if !ApplyDebridState(&entry, Snapshot{Debrid: &DebridState{Via: "realdebrid", RemoteID: "R", Note: "Real-Debrid sent one archive"}}) ||
		entry.RemoteNote != "Real-Debrid sent one archive" {
		t.Fatalf("entry note = %q", entry.RemoteNote)
	}
	// a fresh run before its files are planned has no note: keep the saved one
	ApplyDebridState(&entry, Snapshot{Debrid: &DebridState{Via: "realdebrid", RemoteID: "R"}})
	if entry.RemoteNote == "" {
		t.Fatal("the saved note was cleared")
	}
	eng := newDebridTestEngine(t, &fakeDebrid{})
	entry.Name, entry.Done, entry.Length, entry.DownloadDir = "Show", true, 10, t.TempDir()
	h, err := eng.ResumeDebrid(entry)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := eng.Snapshot(h); s.State != StateDone || s.Note != "Real-Debrid sent one archive" {
		t.Fatalf("resumed: state %v, note %q", s.State, s.Note)
	}
}
