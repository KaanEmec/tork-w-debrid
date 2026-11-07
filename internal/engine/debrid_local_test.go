package engine

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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
)

// Acceptance tests for roadmap J3.3 (local phase) and the engine side of
// J7.5 (connection settings), with a fake provider and a local file host.

// rdHost serves files by the last element of the URL path, with Range
// support and no validators, like Real-Debrid's download servers.
type rdHost struct {
	*httptest.Server
	rate  int64 // bytes/s per response; 0 = unlimited
	fault func(urlPath, rng string) (status int, cutAfter int64)

	mu       sync.Mutex
	files    map[string][]byte
	ranges   map[string][]string // Range headers by URL path, HEAD excluded
	active   map[string]int      // GETs in flight per file name
	peakFile int                 // most files in flight at once
	inflight atomic.Int32        // requests in flight (HEAD included)
	peak     atomic.Int32
	served   atomic.Int64
}

func newRDHost(t *testing.T, files map[string][]byte) *rdHost {
	t.Helper()
	h := &rdHost{files: files, ranges: map[string][]string{}, active: map[string]int{}}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *rdHost) url(link, name string) string { return h.URL + "/" + link + "/" + name }

func (h *rdHost) seen(urlPath string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ranges[urlPath])
}

func (h *rdHost) serve(w http.ResponseWriter, r *http.Request) {
	now := h.inflight.Add(1)
	defer h.inflight.Add(-1)
	for {
		old := h.peak.Load()
		if now <= old || h.peak.CompareAndSwap(old, now) {
			break
		}
	}
	name := path.Base(r.URL.Path)
	rng := r.Header.Get("Range")
	h.mu.Lock()
	data, ok := h.files[name]
	if r.Method == http.MethodGet {
		h.ranges[r.URL.Path] = append(h.ranges[r.URL.Path], rng)
		h.active[name]++
		busy := 0
		for _, n := range h.active {
			if n > 0 {
				busy++
			}
		}
		h.peakFile = max(h.peakFile, busy)
		defer func() {
			h.mu.Lock()
			h.active[name]--
			h.mu.Unlock()
		}()
	}
	h.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	var cut int64
	if h.fault != nil {
		status, after := h.fault(r.URL.Path, rng)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		cut = after
	}
	w.Header().Set("Accept-Ranges", "bytes")
	size := int64(len(data))
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return
	}
	start, end, ranged := parseBenchRange(rng, size)
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		start, end = 0, size-1
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}
	flusher, _ := w.(http.Flusher)
	began := time.Now()
	var sent int64
	for off := start; off <= end; {
		n := min(int64(16<<10), end-off+1)
		if cut > 0 && sent+n > cut {
			n = cut - sent
		}
		wrote, err := w.Write(data[off : off+n])
		h.served.Add(int64(wrote))
		sent += int64(wrote)
		off += int64(wrote)
		if err != nil {
			return
		}
		if cut > 0 && sent >= cut {
			flusher.Flush()
			panic(http.ErrAbortHandler)
		}
		if h.rate > 0 {
			flusher.Flush()
			due := began.Add(time.Duration(float64(sent) / float64(h.rate) * float64(time.Second)))
			select {
			case <-time.After(time.Until(due)):
			case <-r.Context().Done():
				return
			}
		}
	}
}

// rdFile is one file of a fake remote torrent.
type rdFile struct {
	id   int
	path string // path inside the torrent
	data []byte
}

func testPayload(size int, seed byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

// readyTorrent answers Info with the file list until the selection, then as
// ready with the given links.
func readyTorrent(name string, files []rdFile, links []string) func(int, bool) (debrid.RemoteInfo, error) {
	return func(_ int, selected bool) (debrid.RemoteInfo, error) {
		info := debrid.RemoteInfo{Name: name, Status: debrid.WaitingSelection}
		for _, f := range files {
			info.Files = append(info.Files, debrid.RemoteFile{ID: f.id, Path: f.path, Bytes: int64(len(f.data)), Selected: selected})
		}
		if selected {
			info.Status, info.Progress, info.Links = debrid.Ready, 100, links
			for _, f := range files {
				info.Bytes += int64(len(f.data))
			}
		}
		return info, nil
	}
}

// unlockTo maps link "rd/<n>" to the host URL of files[n].
func unlockTo(host *rdHost, files []rdFile) func(int, string) (debrid.Unlocked, error) {
	return func(_ int, link string) (debrid.Unlocked, error) {
		i, err := strconv.Atoi(strings.TrimPrefix(link, "rd/"))
		if err != nil || i >= len(files) {
			return debrid.Unlocked{}, &debrid.Error{Provider: "Real-Debrid", Kind: debrid.ErrNotFound}
		}
		f := files[i]
		return debrid.Unlocked{URL: host.url("l"+strconv.Itoa(i), path.Base(f.path)), Filename: path.Base(f.path), Bytes: int64(len(f.data)), Link: link}, nil
	}
}

func rdLinks(n int) []string {
	links := make([]string, n)
	for i := range links {
		links[i] = "rd/" + strconv.Itoa(i)
	}
	return links
}

func hostFiles(files []rdFile) map[string][]byte {
	m := map[string][]byte{}
	for _, f := range files {
		m[path.Base(f.path)] = f.data
	}
	return m
}

// newLocalTestEngine is a debrid engine with fast retries and small segments.
func newLocalTestEngine(t *testing.T, fake *fakeDebrid) *Engine {
	t.Helper()
	fastDebrid(t, 20*time.Millisecond)
	scaleDirectTuning(t, 0.01)
	eng := newDebridTestEngine(t, fake)
	eng.directMinChunkSize = 32 << 10
	return eng
}

// startLocal adds the test magnet, selects every file and starts it into dir.
func startLocal(t *testing.T, eng *Engine, dir string) metainfo.Hash {
	t.Helper()
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "the file list", 5*time.Second, func() bool { _, ok := eng.DebridFiles(h); return ok })
	if err := eng.StartDebrid(h, nil, DebridOptions{DownloadDir: dir}); err != nil {
		t.Fatal(err)
	}
	return h
}

func checkFile(t *testing.T, p string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes, want %d (or content differs)", p, len(got), len(want))
	}
}

func TestDebridLocalDownloadsFilesToTorrentFolder(t *testing.T) {
	files := []rdFile{
		{1, "S01/e01.mkv", testPayload(600<<10, 1)},
		{2, "S01/e02.mkv", testPayload(400<<10, 2)},
		{3, "readme.nfo", testPayload(3000, 3)},
	}
	host := newRDHost(t, hostFiles(files))
	host.rate = 4 << 20
	fake := &fakeDebrid{info: readyTorrent("Show", files, rdLinks(3)), unlock: unlockTo(host, files)}
	eng := newLocalTestEngine(t, fake)
	dir := t.TempDir()
	h := startLocal(t, eng, dir)

	var last int64 = -1
	var states []TorrentState
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("not done; states %v", states)
		}
		snaps := eng.Snapshots()
		if len(snaps) != 1 {
			t.Fatalf("Snapshots() has %d rows, want only the torrent's", len(snaps))
		}
		s := snaps[0]
		if len(states) == 0 || states[len(states)-1] != s.State {
			states = append(states, s.State)
		}
		if s.State == StateDownloading || s.State == StateDone {
			if s.BytesCompleted < last {
				t.Fatalf("combined bytes went back from %d to %d", last, s.BytesCompleted)
			}
			last = s.BytesCompleted
		}
		if s.State == StatePaused {
			t.Fatalf("item stopped: %q", s.Note)
		}
		if s.State == StateDone {
			if want := int64(600<<10 + 400<<10 + 3000); s.BytesCompleted != want || s.Length != want {
				t.Fatalf("done snapshot %d of %d", s.BytesCompleted, s.Length)
			}
			if s.Seed || s.Via != "realdebrid" || s.DataPath != filepath.Join(dir, "Show") {
				t.Fatalf("done snapshot = %+v", s)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, f := range files {
		checkFile(t, filepath.Join(dir, "Show", filepath.FromSlash(f.path)), f.data)
	}
	if host.peakFile > debridLocalFiles {
		t.Fatalf("%d files downloaded at once, want at most %d", host.peakFile, debridLocalFiles)
	}
	if info, _ := eng.DebridItem(h); info.Phase != DebridDone {
		t.Fatalf("phase = %v", info.Phase)
	}
	if len(fake.deleted()) != 0 {
		t.Fatal("the cloud copy was deleted although delete_after_download is off")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "Show", "*", "*.part*"))
	if len(leftovers) != 0 {
		t.Fatalf("leftover partial files: %v", leftovers)
	}
}

func TestDebridLocalSingleFileGoesInDownloadDirAndDeletesAfter(t *testing.T) {
	files := []rdFile{{1, "movie.mkv", testPayload(300<<10, 9)}}
	host := newRDHost(t, hostFiles(files))
	fake := &fakeDebrid{info: readyTorrent("movie.mkv", files, rdLinks(1)), unlock: unlockTo(host, files)}
	eng := newLocalTestEngine(t, fake)
	eng.SetDebridSettings(config.DebridConfig{Connections: 4, MaxTotalConnections: 8, DeleteAfterDownload: true})
	dir := t.TempDir()
	h := startLocal(t, eng, dir)
	s := awaitHash(t, eng, h, isDone, 20*time.Second)
	if s.DataPath != filepath.Join(dir, "movie.mkv") {
		t.Fatalf("DataPath = %q", s.DataPath)
	}
	checkFile(t, filepath.Join(dir, "movie.mkv"), files[0].data)
	awaitCond(t, "the remote Delete", 5*time.Second, func() bool { return len(fake.deleted()) == 1 })
	if got := fake.deleted(); got[0] != "RID1" {
		t.Fatalf("deleted %v", got)
	}
}

// A link that starts answering 410 mid-transfer is unlocked again, and the
// fresh link resumes from the bytes already in the .part file.
func TestDebridLocalRefreshesGoneLinkAndResumesWithRange(t *testing.T) {
	const size = 1 << 20
	files := []rdFile{{1, "big.bin", testPayload(size, 5)}}
	host := newRDHost(t, hostFiles(files))
	var cutDone atomic.Bool
	host.fault = func(urlPath, rng string) (int, int64) {
		if !strings.HasPrefix(urlPath, "/old/") || rng == "bytes=0-0" || rng == "" {
			return 0, 0
		}
		if cutDone.CompareAndSwap(false, true) {
			return 0, 300 << 10 // the transfer breaks part-way
		}
		return http.StatusGone, 0 // and the link is dead from now on
	}
	fake := &fakeDebrid{info: readyTorrent("big.bin", files, rdLinks(1))}
	fake.unlock = func(n int, link string) (debrid.Unlocked, error) {
		dirName := "old"
		if n > 0 {
			dirName = "new"
		}
		return debrid.Unlocked{URL: host.url(dirName, "big.bin"), Filename: "big.bin", Bytes: size, Link: link}, nil
	}
	eng := newLocalTestEngine(t, fake)
	eng.SetDebridSettings(config.DebridConfig{Connections: 1, MaxTotalConnections: 8})
	dir := t.TempDir()
	h := startLocal(t, eng, dir)
	awaitHash(t, eng, h, isDone, 20*time.Second)
	checkFile(t, filepath.Join(dir, "big.bin"), files[0].data)
	if got := fake.unlockCalls.Load(); got != 2 {
		t.Fatalf("Unlock called %d times, want 2 (start and refresh)", got)
	}
	fresh := slices.DeleteFunc(host.seen("/new/big.bin"), func(r string) bool { return r == "bytes=0-0" })
	if len(fresh) == 0 {
		t.Fatal("the fresh link was never used")
	}
	start, _, ok := parseBenchRange(fresh[0], size)
	if !ok || start == 0 || start > 300<<10 {
		t.Fatalf("first request on the fresh link: Range %q, want one from the .part offset", fresh[0])
	}
}

func TestDebridLocalOneArchiveForManyFiles(t *testing.T) {
	rar := testPayload(200<<10, 7)
	host := newRDHost(t, map[string][]byte{"Show.rar": rar})
	files := []rdFile{{1, "a.mkv", testPayload(100, 1)}, {2, "b.mkv", testPayload(100, 2)}, {3, "c.nfo", testPayload(10, 3)}}
	fake := &fakeDebrid{info: readyTorrent("Show", files, rdLinks(1))}
	fake.unlock = func(_ int, link string) (debrid.Unlocked, error) {
		return debrid.Unlocked{URL: host.url("x", "Show.rar"), Filename: "Show.rar", Bytes: int64(len(rar)), Link: link}, nil
	}
	eng := newLocalTestEngine(t, fake)
	dir := t.TempDir()
	h := startLocal(t, eng, dir)
	s := awaitHash(t, eng, h, isDone, 20*time.Second)
	if s.Note != "Real-Debrid sent one archive" {
		t.Fatalf("note = %q", s.Note)
	}
	checkFile(t, filepath.Join(dir, "Show", "Show.rar"), rar)
	entries, _ := os.ReadDir(filepath.Join(dir, "Show"))
	if len(entries) != 1 {
		t.Fatalf("torrent folder holds %d entries, want only the archive", len(entries))
	}
}

func TestDebridLocalRefusesUnsafePathsAndMismatchedLinks(t *testing.T) {
	for _, c := range []struct {
		name  string
		files []rdFile
		links int
		note  string
	}{
		{"dotdot", []rdFile{{1, "../evil.bin", testPayload(10, 1)}, {2, "ok.bin", testPayload(10, 2)}}, 2, "unsafe path"},
		{"absolute", []rdFile{{1, "ok.bin", testPayload(10, 2)}, {2, "a/../../evil.bin", testPayload(10, 1)}}, 2, "unsafe path"},
		{"more links", []rdFile{{1, "a.bin", testPayload(10, 1)}, {2, "b.bin", testPayload(10, 2)}}, 3, "not guessing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			host := newRDHost(t, hostFiles(c.files))
			fake := &fakeDebrid{info: readyTorrent("Show", c.files, rdLinks(c.links)), unlock: unlockTo(host, c.files)}
			eng := newLocalTestEngine(t, fake)
			root := t.TempDir()
			dir := filepath.Join(root, "dl")
			h := startLocal(t, eng, dir)
			s := awaitHash(t, eng, h, isPaused, 10*time.Second)
			if !strings.Contains(s.Note, c.note) {
				t.Fatalf("note = %q, want it to mention %q", s.Note, c.note)
			}
			if fake.unlockCalls.Load() != 0 {
				t.Fatal("links were unlocked for a refused torrent")
			}
			var written []string
			_ = filepath.Walk(root, func(p string, fi os.FileInfo, _ error) error {
				if fi != nil && !fi.IsDir() {
					written = append(written, p)
				}
				return nil
			})
			if len(written) != 0 {
				t.Fatalf("files written: %v", written)
			}
		})
	}
}

func TestDebridLocalRefusesToOverwriteDifferentFile(t *testing.T) {
	files := []rdFile{{1, "movie.mkv", testPayload(100<<10, 9)}}
	host := newRDHost(t, hostFiles(files))
	fake := &fakeDebrid{info: readyTorrent("movie.mkv", files, rdLinks(1)), unlock: unlockTo(host, files)}
	eng := newLocalTestEngine(t, fake)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := startLocal(t, eng, dir)
	s := awaitHash(t, eng, h, isPaused, 10*time.Second)
	if !strings.Contains(s.Note, "refusing to overwrite") {
		t.Fatalf("note = %q", s.Note)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "movie.mkv")); string(got) != "mine" {
		t.Fatal("the existing file was overwritten")
	}
}

// J7.5: with 8 connections per file and two files running, the host never
// sees more than debrid.max_total_connections at once; a changed setting
// reaches the running transfer.
func TestDebridConnectionLimits(t *testing.T) {
	files := []rdFile{
		{1, "a.bin", testPayload(3<<20, 1)},
		{2, "b.bin", testPayload(3<<20, 2)},
	}
	host := newRDHost(t, hostFiles(files))
	host.rate = 1 << 20
	fake := &fakeDebrid{info: readyTorrent("Pack", files, rdLinks(2)), unlock: unlockTo(host, files)}
	eng := newLocalTestEngine(t, fake)
	eng.SetDebridSettings(config.DebridConfig{Connections: 8, MaxTotalConnections: 4})
	h := startLocal(t, eng, t.TempDir())
	awaitHash(t, eng, h, isDone, 30*time.Second)
	if peak := host.peak.Load(); peak > 4 || peak < 2 {
		t.Fatalf("peak connections = %d, want 2..4 (max_total_connections 4)", peak)
	}
}

func TestDebridConnectionSettingReachesRunningTransfer(t *testing.T) {
	files := []rdFile{{1, "a.bin", testPayload(4<<20, 1)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 1 << 20
	fake := &fakeDebrid{info: readyTorrent("a.bin", files, rdLinks(1)), unlock: unlockTo(host, files)}
	eng := newLocalTestEngine(t, fake)
	eng.SetDebridSettings(config.DebridConfig{Connections: 1, MaxTotalConnections: 8})
	h := startLocal(t, eng, t.TempDir())
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.State == StateDownloading && s.BytesCompleted > 256<<10 }, 10*time.Second)
	if peak := host.peak.Load(); peak != 1 {
		t.Fatalf("peak = %d with one connection", peak)
	}
	eng.SetDebridSettings(config.DebridConfig{Connections: 4, MaxTotalConnections: 8})
	awaitCond(t, "more connections", 10*time.Second, func() bool { return host.inflight.Load() >= 3 })
	awaitHash(t, eng, h, isDone, 30*time.Second)
}
