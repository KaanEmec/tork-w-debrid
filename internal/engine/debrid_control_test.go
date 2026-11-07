package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

// Acceptance tests for roadmap J3.5: pause, resume, remove, move and verify
// on debrid items.

// freshUnlocks answers every Unlock with a new URL for files[i] ("rd/<i>").
func freshUnlocks(host *rdHost, files []rdFile) func(int, string) (debrid.Unlocked, error) {
	return func(n int, link string) (debrid.Unlocked, error) {
		i, _ := strconv.Atoi(strings.TrimPrefix(link, "rd/"))
		f := files[i]
		name := filepath.Base(f.path)
		return debrid.Unlocked{URL: host.url("u"+strconv.Itoa(n), name), Filename: name, Bytes: int64(len(f.data)), Link: link}, nil
	}
}

func nonProbeRanges(host *rdHost, urlPath string) []string {
	var out []string
	for _, r := range host.seen(urlPath) {
		if r != "bytes=0-0" {
			out = append(out, r)
		}
	}
	return out
}

func TestDebridPauseResumeLocalUnlocksAgainAndUsesRange(t *testing.T) {
	const size = 2 << 20
	files := []rdFile{{1, "big.bin", testPayload(size, 8)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 1 << 20
	fake := &fakeDebrid{info: readyTorrent("big.bin", files, rdLinks(1)), unlock: freshUnlocks(host, files)}
	eng := newLocalTestEngine(t, fake)
	eng.SetDebridSettings(config.DebridConfig{Connections: 2, MaxTotalConnections: 8})
	dir := t.TempDir()
	h := startLocal(t, eng, dir)
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.BytesCompleted >= size/4 }, 20*time.Second)
	if err := eng.Pause(h); err != nil {
		t.Fatal(err)
	}
	s, _ := eng.Snapshot(h)
	if s.State != StatePaused || s.Note != "paused" {
		t.Fatalf("paused snapshot = state %v note %q", s.State, s.Note)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.bin.part")); err != nil {
		t.Fatalf("the .part file is gone after pause: %v", err)
	}
	unlocks := fake.unlockCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if fake.unlockCalls.Load() != unlocks || host.inflight.Load() != 0 {
		t.Fatal("the paused item kept working")
	}

	if err := eng.Resume(h); err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 30*time.Second)
	checkFile(t, filepath.Join(dir, "big.bin"), files[0].data)
	if got := fake.unlockCalls.Load(); got != unlocks+1 {
		t.Fatalf("resume unlocked %d times, want once", got-unlocks)
	}
	second := nonProbeRanges(host, "/u"+strconv.Itoa(int(unlocks))+"/big.bin")
	if len(second) == 0 {
		t.Fatal("no request on the link unlocked for the resume")
	}
	for _, r := range second {
		if start, _, ok := parseBenchRange(r, size); !ok || start == 0 {
			t.Fatalf("resumed transfer asked for %q, want a Range past the saved bytes", r)
		}
	}
}

func TestDebridPauseInCloudStopsPollingOnly(t *testing.T) {
	fastDebrid(t, 5*time.Millisecond)
	fake := &fakeDebrid{info: func(_ int, selected bool) (debrid.RemoteInfo, error) {
		if !selected {
			return remoteAt(debrid.WaitingSelection, 0, false), nil
		}
		return remoteAt(debrid.Downloading, 30, true), nil
	}}
	eng := newDebridTestEngine(t, fake)
	h := startLocal(t, eng, t.TempDir())
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.State == StateRemote }, 5*time.Second)
	if err := eng.Pause(h); err != nil {
		t.Fatal(err)
	}
	calls := fake.infoCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if fake.infoCalls.Load() != calls {
		t.Fatal("a paused cloud item kept polling")
	}
	if s, _ := eng.Snapshot(h); s.State != StatePaused || s.Remote == nil || s.BytesCompleted == 0 {
		t.Fatalf("paused cloud snapshot = %+v", s)
	}
	if len(fake.deleted()) != 0 {
		t.Fatal("pause touched the cloud copy")
	}
	if err := eng.Resume(h); err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "polling again", 5*time.Second, func() bool { return fake.infoCalls.Load() > calls })
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.State == StateRemote }, 5*time.Second)
}

func TestDebridRemoveDeletesRemoteOnlyWhenAsked(t *testing.T) {
	for _, c := range []struct {
		name      string
		opts      RemoveOptions
		deleteErr error
		deletes   int
		note      string
	}{
		{"keep cloud copy", RemoveOptions{DeleteData: true}, nil, 0, ""},
		{"delete cloud copy", RemoveOptions{DeleteData: true, DeleteRemote: true}, nil, 1, ""},
		{"delete fails", RemoveOptions{DeleteData: true, DeleteRemote: true},
			&debrid.Error{Provider: "Real-Debrid", HTTPStatus: 503, Kind: debrid.ErrUnavailable}, 1, "cloud copy not deleted: Real-Debrid: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := []rdFile{{1, "a/x.bin", testPayload(1<<20, 1)}, {2, "y.bin", testPayload(1<<20, 2)}}
			host := newRDHost(t, hostFiles(files))
			host.rate = 1 << 20
			fake := &fakeDebrid{info: readyTorrent("Show", files, rdLinks(2)), unlock: freshUnlocks(host, files), deleteErr: c.deleteErr}
			eng := newLocalTestEngine(t, fake)
			dir := t.TempDir()
			h := startLocal(t, eng, dir)
			awaitHash(t, eng, h, func(s Snapshot) bool { return s.BytesCompleted > 64<<10 }, 20*time.Second)
			res, err := eng.RemoveWithOptions(h, c.opts)
			if err != nil {
				t.Fatalf("local remove failed: %v", err)
			}
			if got := len(fake.deleted()); got != c.deletes {
				t.Fatalf("Delete called %d times, want %d", got, c.deletes)
			}
			if !strings.HasPrefix(res.Note, c.note) || (c.note == "") != (res.Note == "") {
				t.Fatalf("note = %q, want prefix %q", res.Note, c.note)
			}
			if c.deleteErr != nil && (res.RemoteDeleted || !errors.Is(res.RemoteErr, debrid.ErrUnavailable)) {
				t.Fatalf("result = %+v", res)
			}
			if _, ok := eng.Snapshot(h); ok {
				t.Fatal("the row is still there")
			}
			if _, err := os.Stat(filepath.Join(dir, "Show")); !os.IsNotExist(err) {
				t.Fatalf("the torrent folder is still there: %v", err)
			}
			n := host.served.Load()
			time.Sleep(30 * time.Millisecond)
			if host.served.Load() != n || host.inflight.Load() != 0 {
				t.Fatal("a removed item kept downloading")
			}
		})
	}
}

// Move as the TUI does it: remove the item, move the torrent folder, resume
// the entry from the new folder. The resume writes there, with Range.
func TestDebridMoveFolderThenResumeWritesThere(t *testing.T) {
	const size = 2 << 20
	files := []rdFile{{1, "S01/e01.mkv", testPayload(size, 3)}, {2, "e02.mkv", testPayload(4000, 4)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 1 << 20
	fake := &fakeDebrid{info: readyTorrent("Show", files, rdLinks(2)), unlock: freshUnlocks(host, files)}
	eng := newLocalTestEngine(t, fake)
	oldDir, newDir := t.TempDir(), t.TempDir()
	h := startLocal(t, eng, oldDir)
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.BytesCompleted >= size/4 }, 20*time.Second)
	entry, _ := eng.DebridEntry(h)
	if s, ok := eng.Snapshot(h); ok {
		ApplyDebridState(&entry, s)
	}
	if entry.DataPath != filepath.Join(oldDir, "Show") {
		t.Fatalf("DataPath = %q, want the torrent folder", entry.DataPath)
	}
	if err := eng.Remove(h, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(entry.DataPath, filepath.Join(newDir, "Show")); err != nil {
		t.Fatal(err)
	}
	entry.DownloadDir, entry.DataPath = newDir, filepath.Join(newDir, "Show")
	unlocks := fake.unlockCalls.Load()
	if _, err := eng.ResumeDebrid(entry); err != nil {
		t.Fatal(err)
	}
	s := awaitHash(t, eng, h, isDone, 30*time.Second)
	if s.DataPath != filepath.Join(newDir, "Show") {
		t.Fatalf("DataPath after the move = %q", s.DataPath)
	}
	for _, f := range files {
		checkFile(t, filepath.Join(newDir, "Show", f.path), f.data)
	}
	if entries, _ := os.ReadDir(oldDir); len(entries) != 0 {
		t.Fatalf("the old folder got files again: %v", entries)
	}
	for _, r := range nonProbeRanges(host, "/u"+strconv.Itoa(int(unlocks))+"/e01.mkv") {
		if start, _, _ := parseBenchRange(r, size); start == 0 {
			t.Fatalf("resumed e01.mkv from byte 0 (%q)", r)
		}
	}
}

func TestDebridVerifyComparesSizesAndRepairs(t *testing.T) {
	files := []rdFile{{1, "a.bin", testPayload(100<<10, 1)}, {2, "b.bin", testPayload(50<<10, 2)}}
	host := newRDHost(t, hostFiles(files))
	fake := &fakeDebrid{info: readyTorrent("Pack", files, rdLinks(2)), unlock: freshUnlocks(host, files)}
	eng := newLocalTestEngine(t, fake)
	dir := t.TempDir()
	h := startLocal(t, eng, dir)
	awaitHash(t, eng, h, isDone, 20*time.Second)

	res, err := eng.Verify(t.Context(), h)
	if err != nil || res.NeedsRepair || res.CheckedFiles != 2 {
		t.Fatalf("verify of good files = %+v, %v", res, err)
	}
	bad := filepath.Join(dir, "Pack", "b.bin")
	if err := os.Truncate(bad, 10); err != nil {
		t.Fatal(err)
	}
	res, err = eng.Verify(t.Context(), h)
	if err != nil || !res.NeedsRepair || res.WrongSize != 1 || res.QuarantinePath != bad+".corrupt" {
		t.Fatalf("verify of a short file = %+v, %v", res, err)
	}
	awaitHash(t, eng, h, isDone, 20*time.Second)
	checkFile(t, bad, files[1].data)
	checkFile(t, filepath.Join(dir, "Pack", "a.bin"), files[0].data)

	// a download finished in an earlier run asks the service for the sizes
	entry, _ := eng.DebridEntry(h)
	if s, ok := eng.Snapshot(h); ok {
		ApplyDebridState(&entry, s)
	}
	entry.Done = true
	if err := eng.Remove(h, false); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResumeDebrid(entry); err != nil {
		t.Fatal(err)
	}
	res, err = eng.Verify(t.Context(), h)
	if err != nil || res.NeedsRepair || res.CheckedFiles != 2 {
		t.Fatalf("verify after a restart = %+v, %v", res, err)
	}
}

func TestDebridSeedIsAlwaysOff(t *testing.T) {
	e := state.Entry{Seed: state.Bool(true)}
	if !ApplyDebridState(&e, Snapshot{Debrid: &DebridState{Via: "realdebrid"}}) || e.Seed == nil || *e.Seed {
		t.Fatalf("seed after ApplyDebridState = %v", e.Seed)
	}
}
