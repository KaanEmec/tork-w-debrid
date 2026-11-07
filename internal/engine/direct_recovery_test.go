package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// Acceptance tests for roadmap J7.2 (resume key and eligibility) and J7.3
// (per-segment retry and link refresh), on the J7.1 bench server.

// scaleDirectTuning multiplies the retry, throttle and slow-connection
// timings by f until the test ends. Call it before newDirectTestEngine, so
// the timings are restored only after the engine has stopped.
func scaleDirectTuning(tb testing.TB, f float64) {
	tb.Helper()
	saved := directTuning
	tb.Cleanup(func() { directTuning = saved })
	scale := func(d time.Duration) time.Duration {
		return max(time.Duration(float64(d)*f), time.Millisecond)
	}
	directTuning.retryBase = scale(saved.retryBase)
	directTuning.throttleRecover = scale(saved.throttleRecover)
	directTuning.tick = max(scale(saved.tick), 5*time.Millisecond)
	directTuning.healthInterval = scale(saved.healthInterval)
	directTuning.healthWindow = scale(saved.healthWindow)
	directTuning.restartCooldown = scale(saved.restartCooldown)
}

// readTestManifest reads the segment manifest next to a download.
func readTestManifest(t *testing.T, dest string) directManifest {
	t.Helper()
	data, err := os.ReadFile(dest + ".part.meta")
	if err != nil {
		t.Fatal(err)
	}
	var m directManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// checkManifestRanges fails when the manifest's ranges overlap, touch
// without being merged, leave the file, or name bytes the .part file does
// not hold.
func checkManifestRanges(t *testing.T, dest string, m directManifest) {
	t.Helper()
	for i, r := range m.Completed {
		if r.Start < 0 || r.End < r.Start || r.End >= m.Total {
			t.Fatalf("manifest range %d %+v outside 0..%d", i, r, m.Total-1)
		}
		if i > 0 && r.Start <= m.Completed[i-1].End+1 {
			t.Fatalf("manifest ranges %+v and %+v overlap or are not merged", m.Completed[i-1], r)
		}
	}
	f, err := os.Open(dest + ".part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range m.Completed {
		got := make([]byte, rangeLen(r))
		if _, err := f.ReadAt(got, r.Start); err != nil {
			t.Fatalf("read manifest range %+v: %v", r, err)
		}
		want := make([]byte, len(got))
		fillBenchPayload(want, r.Start)
		if !bytes.Equal(got, want) {
			t.Fatalf("manifest range %+v holds wrong bytes", r)
		}
	}
}

func awaitHash(t *testing.T, eng *Engine, h metainfo.Hash, want func(Snapshot) bool, timeout time.Duration) Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, ok := eng.Snapshot(h); ok && want(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, _ := eng.Snapshot(h)
	t.Fatalf("condition not reached: %+v", s)
	return s
}

func isDone(s Snapshot) bool   { return s.State == StateDone }
func isPaused(s Snapshot) bool { return s.State == StatePaused }

// J7.2: a download that reaches 60 %, gets a new URL for the same resume key
// and resumes downloads only the missing 40 %.
func TestKeyedResumeWithNewURLDownloadsOnlyMissingBytes(t *testing.T) {
	const size = 8 << 20
	srv := newBenchServer(t, benchServerConfig{Size: size, ConnRate: 2 << 20, AcceptRanges: true})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 256 << 10
	eng.directMaxConnections = 4
	spec := DirectDownload{
		URL: srv.URL + "/link-a/file.bin", Name: "file.bin",
		ExpectedSize: size, ResumeKey: "infohash/7",
	}
	h, err := eng.AddDirectDownload(spec)
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.BytesCompleted >= size*6/10 }, 20*time.Second)
	if err := eng.Pause(h); err != nil {
		t.Fatal(err)
	}
	// Forget the item, as a restart would: only the .part file and its
	// manifest (keyed by the resume key, not the URL) carry the progress.
	if err := eng.Remove(h, false); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(eng.cfg.DownloadDir, "file.bin")
	m := readTestManifest(t, dest)
	checkManifestRanges(t, dest, m)
	have := rangeBytes(m.Completed)
	if have < size*6/10 || have == size {
		t.Fatalf("manifest holds %d of %d bytes after the pause", have, size)
	}

	srv.reset()
	spec.URL = srv.URL + "/link-b/file.bin"
	h2, err := eng.AddDirectDownload(spec)
	if err != nil {
		t.Fatal(err)
	}
	if h2 != h {
		t.Fatal("the same resume key gave a different download hash")
	}
	awaitHash(t, eng, h, isDone, 20*time.Second)
	if err := verifyBenchFile(dest, size); err != nil {
		t.Fatal(err)
	}
	missing := int64(size) - have
	// Slack: a range split part-way may overshoot by one server write.
	if served := srv.served.Load(); served > missing+128<<10 {
		t.Fatalf("resume served %d bytes, only %d were missing", served, missing)
	}
}

// J7.2: no ETag, no Last-Modified, no checksum, but a known size: still four
// connections.
func TestKnownSizeWithoutValidatorUsesFourConnections(t *testing.T) {
	const size = 4 << 20
	srv := newBenchServer(t, benchServerConfig{Size: size, ConnRate: 4 << 20})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 128 << 10
	eng.directMaxConnections = 4
	h, err := eng.AddDirectDownload(DirectDownload{URL: srv.URL + "/f.bin", Name: "f.bin", ExpectedSize: size})
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 20*time.Second)
	if peak := srv.peak.Load(); peak < 4 {
		t.Fatalf("peak concurrent range requests = %d, want 4", peak)
	}
	if err := verifyBenchFile(filepath.Join(eng.cfg.DownloadDir, "f.bin"), size); err != nil {
		t.Fatal(err)
	}
}

// J7.2: a server whose size disagrees with the expected size fails clearly
// and writes nothing to the final path, on both download paths.
func TestSizeMismatchFailsWithoutWritingFinalFile(t *testing.T) {
	const size = 2 << 20
	for _, chunking := range []bool{true, false} {
		t.Run(fmt.Sprintf("chunking=%v", chunking), func(t *testing.T) {
			srv := newBenchServer(t, benchServerConfig{Size: size, AcceptRanges: true})
			eng := newDirectTestEngine(t)
			eng.cfg.Direct.EnableChunking = chunking
			eng.directMinChunkSize = 128 << 10
			h, err := eng.AddDirectDownload(DirectDownload{
				URL: srv.URL + "/f.bin", Name: "f.bin", ExpectedSize: size + 1, ResumeKey: "infohash/1",
			})
			if err != nil {
				t.Fatal(err)
			}
			snap := awaitHash(t, eng, h, isPaused, 10*time.Second)
			if !strings.Contains(snap.Note, fmt.Sprintf("server reported %d bytes, expected %d", size, size+1)) {
				t.Fatalf("note = %q", snap.Note)
			}
			if _, err := os.Stat(filepath.Join(eng.cfg.DownloadDir, "f.bin")); !os.IsNotExist(err) {
				t.Fatalf("final path exists: %v", err)
			}
		})
	}
}

// J7.2: a version 1 manifest written by v0.1.0 still resumes.
func TestVersion1ManifestStillResumes(t *testing.T) {
	const size = 2 << 20
	// A paced server, so a range that is split part-way does not overshoot
	// by whole socket buffers.
	srv := newBenchServer(t, benchServerConfig{Size: size, ConnRate: 8 << 20, ETag: true, AcceptRanges: true})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 128 << 10
	url := srv.URL + "/image.iso"
	dest := filepath.Join(eng.cfg.DownloadDir, "image.iso")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	part := make([]byte, size) // v0.1.0 preallocated the whole file
	fillBenchPayload(part[:size/2], 0)
	if err := os.WriteFile(dest+".part", part, 0o644); err != nil {
		t.Fatal(err)
	}
	v1 := fmt.Sprintf(`{"version":1,"url":%q,"total":%d,"validator":"\"bench-v1\"","completed":[{"start":0,"end":%d}]}`,
		url, size, size/2-1)
	if err := os.WriteFile(dest+".part.meta", []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := eng.AddDirect(url, "image.iso", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 10*time.Second)
	if err := verifyBenchFile(dest, size); err != nil {
		t.Fatal(err)
	}
	if served := srv.served.Load(); served > size/2+128<<10 {
		t.Fatalf("served %d bytes, only %d were missing", served, size/2)
	}
}

// J7.3: one connection reset half-way through its range is retried without
// user action, and nothing big is downloaded twice.
func TestSegmentResetIsRetriedWithoutUser(t *testing.T) {
	const size = 16 << 20
	scaleDirectTuning(t, 0.01)
	srv := newBenchServer(t, benchServerConfig{
		Size: size, ConnRate: 8 << 20, ETag: true, AcceptRanges: true,
		Fault: func(n int, _ string, start, end int64) benchFault {
			if n == 2 {
				return benchFault{ResetAfter: (end - start + 1) / 2}
			}
			return benchFault{}
		},
	})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 256 << 10
	eng.directMaxConnections = 4
	res := runBenchDownload(t, eng, srv, DirectDownload{URL: srv.URL + "/f.bin", Name: "f.bin"})
	if res.resumes != 0 {
		t.Fatalf("the download stopped %d times and needed p", res.resumes)
	}
	if served := srv.served.Load(); served > size+8<<20 {
		t.Fatalf("served %d bytes for a %d-byte file", served, size)
	}
	if err := verifyBenchFile(filepath.Join(eng.cfg.DownloadDir, "f.bin"), size); err != nil {
		t.Fatal(err)
	}
}

// J7.3: a 410 half-way causes exactly one refresh, even with all four
// workers failing at the same moment.
func TestExpiredLinkIsRefreshedOnceForAllWorkers(t *testing.T) {
	const size = 8 << 20
	scaleDirectTuning(t, 0.01)
	var gone atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := newBenchServer(t, benchServerConfig{
		Size: size, ConnRate: 8 << 20, AcceptRanges: true,
		Fault: func(n int, path string, start, end int64) benchFault {
			if path != "/old" {
				return benchFault{}
			}
			if n <= 4 { // the opening ranges stop half-way
				return benchFault{ResetAfter: (end - start + 1) / 2}
			}
			// The link is dead now: hold the answers until all four workers
			// have asked, then tell them all at once.
			if gone.Add(1) == 4 {
				releaseOnce.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-time.After(3 * time.Second):
			}
			return benchFault{Status: http.StatusGone}
		},
	})
	var refreshes atomic.Int32
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 256 << 10
	eng.directMaxConnections = 4
	h, err := eng.AddDirectDownload(DirectDownload{
		URL: srv.URL + "/old", Name: "f.bin", ExpectedSize: size, ResumeKey: "infohash/3",
		RefreshURL: func(context.Context) (string, error) {
			refreshes.Add(1)
			time.Sleep(50 * time.Millisecond)
			return srv.URL + "/new", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 20*time.Second)
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshURL called %d times, want 1", got)
	}
	if got := gone.Load(); got != 4 {
		t.Fatalf("%d requests saw the 410, want all 4 workers", got)
	}
	if err := verifyBenchFile(filepath.Join(eng.cfg.DownloadDir, "f.bin"), size); err != nil {
		t.Fatal(err)
	}
}

// J7.3: after 429 with Retry-After: 2 the next request waits at least 2 s,
// and the download drops from 4 connections to 3.
func TestBusyServerHonoursRetryAfterAndDropsAConnection(t *testing.T) {
	const size = 16 << 20
	scaleDirectTuning(t, 0.01)
	directTuning.throttleRecover = time.Minute // keep the lowered count for the whole test
	var mu sync.Mutex
	var busyAt time.Time
	var after []time.Time
	srv := newBenchServer(t, benchServerConfig{
		Size: size, ConnRate: 4 << 20, ETag: true, AcceptRanges: true,
		Fault: func(n int, _ string, _, _ int64) benchFault {
			mu.Lock()
			defer mu.Unlock()
			if n == 1 {
				busyAt = time.Now()
				return benchFault{Status: http.StatusTooManyRequests, RetryAfter: "2"}
			}
			if n >= 5 {
				after = append(after, time.Now())
			}
			return benchFault{}
		},
	})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 256 << 10
	eng.directMaxConnections = 4
	h, err := eng.AddDirectDownload(DirectDownload{URL: srv.URL + "/f.bin", Name: "f.bin"})
	if err != nil {
		t.Fatal(err)
	}
	minLimit, peakAfter, sawNote := 99, 0, false
	deadline := time.Now().Add(30 * time.Second)
	for {
		snap, _ := eng.Snapshot(h)
		if snap.State == StateDone {
			break
		}
		if snap.State == StatePaused || time.Now().After(deadline) {
			t.Fatalf("download stopped: %+v", snap)
		}
		if m, ok := eng.Segments(h); ok {
			minLimit = min(minLimit, m.Limit)
			if minLimit == 3 {
				peakAfter = max(peakAfter, m.Connections)
			}
		}
		sawNote = sawNote || snap.Note == "server busy, using 3 connections"
		time.Sleep(5 * time.Millisecond)
	}
	if minLimit != 3 {
		t.Fatalf("lowest connection limit = %d, want 3", minLimit)
	}
	if !sawNote {
		t.Fatal("the row never said the server is busy")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(after) == 0 {
		t.Fatal("no request after the opening four")
	}
	for _, at := range after {
		if wait := at.Sub(busyAt); wait < 2*time.Second {
			t.Fatalf("a request went out %v after the 429, want at least 2s", wait)
		}
	}
	if p := peakAfter; p > 3 {
		t.Fatalf("%d connections after the 429, want at most 3", p)
	}
	if err := verifyBenchFile(filepath.Join(eng.cfg.DownloadDir, "f.bin"), size); err != nil {
		t.Fatal(err)
	}
}

// J7.3: a segment that fails 5 times fails the item with a clear note, the
// manifest keeps every finished byte, and p resumes it.
func TestSegmentFailingFiveTimesFailsAndResumes(t *testing.T) {
	const size = 8 << 20
	const quarter = size / 4
	scaleDirectTuning(t, 0.01)
	var failing atomic.Bool
	failing.Store(true)
	srv := newBenchServer(t, benchServerConfig{
		Size: size, ConnRate: 4 << 20, ETag: true, AcceptRanges: true,
		Fault: func(_ int, _ string, start, _ int64) benchFault {
			if failing.Load() && start >= quarter && start < 2*quarter {
				return benchFault{Status: http.StatusInternalServerError}
			}
			return benchFault{}
		},
	})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 256 << 10
	eng.directMaxConnections = 4
	h, err := eng.AddDirectDownload(DirectDownload{URL: srv.URL + "/f.bin", Name: "f.bin"})
	if err != nil {
		t.Fatal(err)
	}
	snap := awaitHash(t, eng, h, isPaused, 20*time.Second)
	if !strings.Contains(snap.Note, "a segment failed 5 times") || !strings.Contains(snap.Note, "press p") {
		t.Fatalf("note = %q", snap.Note)
	}
	dest := filepath.Join(eng.cfg.DownloadDir, "f.bin")
	m := readTestManifest(t, dest)
	checkManifestRanges(t, dest, m)
	have := rangeBytes(m.Completed)
	if have != snap.BytesCompleted || have == 0 {
		t.Fatalf("manifest holds %d bytes, the row counted %d", have, snap.BytesCompleted)
	}

	failing.Store(false)
	srv.reset()
	if err := eng.Resume(h); err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 20*time.Second)
	if err := verifyBenchFile(dest, size); err != nil {
		t.Fatal(err)
	}
	if served := srv.served.Load(); served > size-have+128<<10 {
		t.Fatalf("resume served %d bytes, only %d were missing", served, size-have)
	}
}

// J7.3 on the single-connection path (what J3.3 needs): a reset is retried,
// then a 410 refreshes the link and the download goes on with a Range
// request from the .part offset.
func TestSingleConnectionRefreshesAndResumesFromPartOffset(t *testing.T) {
	const size = 1 << 20
	scaleDirectTuning(t, 0.01)
	payload := benchPayload(size)
	var oldHits atomic.Int32
	var mu sync.Mutex
	var newRanges []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			if oldHits.Add(1) == 1 {
				w.Header().Set("Content-Length", fmt.Sprint(size))
				w.WriteHeader(http.StatusOK)
				w.Write(payload[:size/2])
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusGone)
		case "/new":
			mu.Lock()
			newRanges = append(newRanges, r.Header.Get("Range"))
			mu.Unlock()
			http.ServeContent(w, r, "f.bin", time.Time{}, bytes.NewReader(payload))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()
	eng := newDirectTestEngine(t)
	eng.cfg.Direct.EnableChunking = false
	var refreshes atomic.Int32
	h, err := eng.AddDirectDownload(DirectDownload{
		URL: ts.URL + "/old", Name: "f.bin", ExpectedSize: size, ResumeKey: "infohash/9",
		Checksum: mustSHA256(t, sumHex(payload)),
		RefreshURL: func(context.Context) (string, error) {
			refreshes.Add(1)
			return ts.URL + "/new", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 10*time.Second)
	if refreshes.Load() != 1 || oldHits.Load() != 2 {
		t.Fatalf("refreshes = %d, requests to the old link = %d; want 1 and 2", refreshes.Load(), oldHits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	var start int
	if len(newRanges) != 1 || !strings.HasPrefix(newRanges[0], "bytes=") {
		t.Fatalf("requests to the new link = %q, want one Range request", newRanges)
	}
	if _, err := fmt.Sscanf(newRanges[0], "bytes=%d-", &start); err != nil || start == 0 {
		t.Fatalf("new link was asked for %q, want a range from the .part offset", newRanges[0])
	}
	got, err := os.ReadFile(filepath.Join(eng.cfg.DownloadDir, "f.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: %v", err)
	}
}

// The single-connection path waits for a busy server and does not retry a
// plain 404 on a link it cannot refresh.
func TestSingleConnectionRetriesBusyServerButNotMissingFile(t *testing.T) {
	const size = 256 << 10
	scaleDirectTuning(t, 0.01)
	payload := benchPayload(size)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			hits.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if hits.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(payload)
	}))
	defer ts.Close()
	eng := newDirectTestEngine(t)
	eng.cfg.Direct.EnableChunking = false
	h, err := eng.AddDirect(ts.URL+"/busy.bin", "busy.bin", sumHex(payload))
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, isDone, 10*time.Second)
	if hits.Load() != 3 {
		t.Fatalf("requests = %d, want 3", hits.Load())
	}

	hits.Store(0)
	h, err = eng.AddDirect(ts.URL+"/missing", "missing.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	snap := awaitHash(t, eng, h, isPaused, 10*time.Second)
	if hits.Load() != 1 || !strings.Contains(snap.Note, "404") {
		t.Fatalf("requests = %d, note = %q; want 1 request and a 404 note", hits.Load(), snap.Note)
	}
}

// countingTransport counts requests whose response body is still open.
type countingTransport struct {
	base http.RoundTripper
	open atomic.Int32
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.open.Add(1)
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		c.open.Add(-1)
		return nil, err
	}
	resp.Body = &countedBody{ReadCloser: resp.Body, open: &c.open}
	return resp, nil
}

type countedBody struct {
	io.ReadCloser
	open *atomic.Int32
	once sync.Once
}

func (b *countedBody) Close() error {
	b.once.Do(func() { b.open.Add(-1) })
	return b.ReadCloser.Close()
}

// countDirectRequests makes eng count its open HTTP requests.
func countDirectRequests(eng *Engine) *countingTransport {
	base := eng.directHTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	ct := &countingTransport{base: base}
	client := *eng.directHTTP
	client.Transport = ct
	eng.directHTTP = &client
	return ct
}

func mustSHA256(t *testing.T, hexSum string) Checksum {
	t.Helper()
	c, err := SHA256Checksum(hexSum)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
