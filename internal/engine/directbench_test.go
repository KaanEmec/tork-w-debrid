package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file holds the offline benchmark harness for the HTTPS downloader
// (roadmap J7.1). benchServer behaves like a CDN or debrid host: it caps speed
// per connection, can add latency, can make one connection much slower than
// the others, can leave out validators, and can fail a range part-way through.
// The payload is generated from the byte offset, so even a 4 GB file is never
// stored anywhere except the download under test.

// benchFault tells benchServer how to answer one range request.
type benchFault struct {
	Status     int    // answer with this status instead of 206 (0 = serve)
	RetryAfter string // Retry-After header sent with Status
	ResetAfter int64  // abort the connection after this many body bytes (0 = never)
}

type benchServerConfig struct {
	Size         int64
	ConnRate     int64         // bytes/s per connection; 0 = unlimited
	SlowRate     int64         // bytes/s for the one slow connection; 0 = none
	Latency      time.Duration // delay before every response's headers
	ETag         bool
	LastModified bool
	AcceptRanges bool
	// Fault is called for every range request except the 0-0 probe, with its
	// 1-based index among those requests, the requested path and the range.
	Fault func(n int, path string, start, end int64) benchFault
}

type benchServer struct {
	*httptest.Server
	cfg benchServerConfig

	served   atomic.Int64 // body bytes written, probes included
	requests atomic.Int32 // range requests, probe excluded
	probes   atomic.Int32
	active   atomic.Int32 // range streams in flight, probe excluded
	peak     atomic.Int32

	mu        sync.Mutex
	slowConn  int64 // id of the connection that gets SlowRate (0 = not chosen yet)
	nextConn  atomic.Int64
	tailStart time.Time // when active last dropped from >=2 to 1
	lastEnd   time.Time
	ranges    []string // Range headers seen, probe excluded
}

type benchConnKey struct{}

func newBenchServer(tb testing.TB, cfg benchServerConfig) *benchServer {
	tb.Helper()
	s := &benchServer{cfg: cfg}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	ts.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, benchConnKey{}, s.nextConn.Add(1))
	}
	ts.Start()
	s.Server = ts
	tb.Cleanup(ts.Close)
	return s
}

// reset clears the per-download counters between benchmark iterations.
func (s *benchServer) reset() {
	s.served.Store(0)
	s.requests.Store(0)
	s.probes.Store(0)
	s.peak.Store(0)
	s.mu.Lock()
	s.slowConn = 0
	s.tailStart, s.lastEnd = time.Time{}, time.Time{}
	s.ranges = nil
	s.mu.Unlock()
}

// tail is how long the download ran on a single stream at the end, after
// running on two or more. It is 0 when the download never ran in parallel.
func (s *benchServer) tail() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tailStart.IsZero() || s.lastEnd.Before(s.tailStart) {
		return 0
	}
	return s.lastEnd.Sub(s.tailStart)
}

func (s *benchServer) seenRanges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *benchServer) serve(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Latency > 0 {
		time.Sleep(s.cfg.Latency)
	}
	if s.cfg.ETag {
		w.Header().Set("ETag", `"bench-v1"`)
	}
	if s.cfg.LastModified {
		w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 10:00:00 GMT")
	}
	if s.cfg.AcceptRanges {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	size := s.cfg.Size
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return
	}
	start, end, ranged := parseBenchRange(r.Header.Get("Range"), size)
	if !ranged {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		s.stream(w, r, 0, size-1, s.cfg.ConnRate, 0)
		return
	}
	if start == 0 && end == 0 {
		s.probes.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		s.stream(w, r, 0, 0, 0, 0)
		return
	}
	n := int(s.requests.Add(1))
	s.mu.Lock()
	s.ranges = append(s.ranges, r.Header.Get("Range"))
	s.mu.Unlock()
	var fault benchFault
	if s.cfg.Fault != nil {
		fault = s.cfg.Fault(n, r.URL.Path, start, end)
	}
	if fault.Status != 0 {
		if fault.RetryAfter != "" {
			w.Header().Set("Retry-After", fault.RetryAfter)
		}
		w.WriteHeader(fault.Status)
		return
	}
	if v := r.Header.Get("If-Range"); v != "" && s.cfg.ETag && v != `"bench-v1"` {
		start, end = 0, size-1
		w.WriteHeader(http.StatusOK)
	} else {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	}
	rate := s.cfg.ConnRate
	if s.cfg.SlowRate > 0 {
		id, _ := r.Context().Value(benchConnKey{}).(int64)
		s.mu.Lock()
		if s.slowConn == 0 {
			s.slowConn = id
		}
		if s.slowConn == id {
			rate = s.cfg.SlowRate
		}
		s.mu.Unlock()
	}
	s.beginStream()
	defer s.endStream()
	s.stream(w, r, start, end, rate, fault.ResetAfter)
}

func (s *benchServer) beginStream() {
	now := s.active.Add(1)
	for {
		old := s.peak.Load()
		if now <= old || s.peak.CompareAndSwap(old, now) {
			break
		}
	}
	if now >= 2 {
		s.mu.Lock()
		s.tailStart = time.Time{}
		s.mu.Unlock()
	}
}

func (s *benchServer) endStream() {
	left := s.active.Add(-1)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.lastEnd = now
	if left == 1 {
		s.tailStart = now
	}
}

// stream writes payload bytes [start, end], pacing them to rate bytes/s, and
// aborts the connection after resetAfter bytes when that is positive.
func (s *benchServer) stream(w http.ResponseWriter, r *http.Request, start, end, rate, resetAfter int64) {
	buf := make([]byte, 32<<10)
	began := time.Now()
	var sent int64
	flusher, _ := w.(http.Flusher)
	for off := start; off <= end; {
		n := min(int64(len(buf)), end-off+1)
		if resetAfter > 0 && sent+n > resetAfter {
			n = resetAfter - sent
		}
		if n > 0 {
			fillBenchPayload(buf[:n], off)
			wrote, err := w.Write(buf[:n])
			s.served.Add(int64(wrote))
			sent += int64(wrote)
			off += int64(wrote)
			if err != nil {
				return
			}
		}
		if resetAfter > 0 && sent >= resetAfter {
			if flusher != nil {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		if rate > 0 {
			if flusher != nil {
				flusher.Flush()
			}
			due := began.Add(time.Duration(float64(sent) / float64(rate) * float64(time.Second)))
			if wait := time.Until(due); wait > 0 {
				select {
				case <-time.After(wait):
				case <-r.Context().Done():
					return
				}
			}
		}
	}
}

func parseBenchRange(raw string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(raw, "bytes=")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, size-1)
	}
	return start, end, true
}

// fillBenchPayload writes the deterministic payload bytes that start at off.
func fillBenchPayload(buf []byte, off int64) {
	for len(buf) > 0 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], splitmix64(uint64(off/8)))
		n := copy(buf, word[off%8:])
		buf = buf[n:]
		off += int64(n)
	}
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// benchPayload returns the first size payload bytes (for small test files).
func benchPayload(size int64) []byte {
	buf := make([]byte, size)
	fillBenchPayload(buf, 0)
	return buf
}

// verifyBenchFile compares a downloaded file with the generated payload.
func verifyBenchFile(path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	got := make([]byte, 1<<20)
	want := make([]byte, 1<<20)
	var off int64
	for {
		n, err := io.ReadFull(f, got)
		if n > 0 {
			fillBenchPayload(want[:n], off)
			if !bytes.Equal(got[:n], want[:n]) {
				return fmt.Errorf("content differs in the MiB at offset %d", off)
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if off != size {
		return fmt.Errorf("file is %d bytes, want %d", off, size)
	}
	return nil
}

// benchScale describes the file size and per-connection cap of a run. The
// quick scale keeps `go test -bench Direct` under two minutes; it is the
// 1 GB / 5 MB/s case shrunk 16x in size and sped up ~3x, with min_chunk_size
// shrunk by the same factor, so every ratio (speed-up, tail share) is kept.
type benchScale struct {
	Name     string
	Size     int64
	ConnRate int64
	MinChunk int64
}

func benchScales() []benchScale {
	const mb = 1 << 20
	if os.Getenv("TORKWD_BENCH_FULL") == "" {
		return []benchScale{{Name: "64MB", Size: 64 * mb, ConnRate: 16 * mb, MinChunk: 640 << 10}}
	}
	rate := int64(5 * mb)
	if v, err := strconv.ParseFloat(os.Getenv("TORKWD_BENCH_CAP_MBPS"), 64); err == nil && v > 0 {
		rate = int64(v * mb)
	}
	return []benchScale{
		{Name: "100MB", Size: 100 * mb, ConnRate: rate, MinChunk: 10 * mb},
		{Name: "1GB", Size: 1 << 30, ConnRate: rate, MinChunk: 10 * mb},
		{Name: "4GB", Size: 4 << 30, ConnRate: rate, MinChunk: 10 * mb},
	}
}

type benchResult struct {
	elapsed time.Duration
	resumes int
}

// runBenchDownload downloads one file from srv and waits for it to finish,
// pressing "p" (Resume) whenever the item stops with an error, up to 10 times.
func runBenchDownload(tb testing.TB, eng *Engine, srv *benchServer, spec DirectDownload) benchResult {
	tb.Helper()
	began := time.Now()
	h, err := eng.AddDirectDownload(spec)
	if err != nil {
		tb.Fatal(err)
	}
	var res benchResult
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		snap, ok := eng.Snapshot(h)
		if !ok {
			tb.Fatal("download disappeared")
		}
		switch snap.State {
		case StateDone:
			res.elapsed = time.Since(began)
			return res
		case StatePaused:
			if res.resumes >= 10 {
				tb.Fatalf("gave up after %d resumes: %s", res.resumes, snap.Note)
			}
			res.resumes++
			if err := eng.Resume(h); err != nil {
				tb.Fatal(err)
			}
		}
	}
	tb.Fatal("download did not finish")
	return res
}

func removeBenchDownload(tb testing.TB, eng *Engine, spec DirectDownload) string {
	tb.Helper()
	h := directHash(spec.URL)
	if err := eng.Remove(h, true); err != nil {
		tb.Fatal(err)
	}
	return filepath.Join(eng.cfg.DownloadDir, spec.Name)
}

type benchCase struct {
	name      string
	conns     int
	server    benchServerConfig
	spec      func(url string, size int64) DirectDownload
	reportCap int64 // per-connection cap for the x-ideal metric (0 = skip)
}

func runBenchCase(b *testing.B, scale benchScale, bc benchCase) {
	cfg := bc.server
	cfg.Size = scale.Size
	srv := newBenchServer(b, cfg)
	eng := newDirectTestEngine(b)
	eng.directMinChunkSize = scale.MinChunk
	eng.directMaxConnections = bc.conns
	url := srv.URL + "/bench.bin"
	var elapsed, tail time.Duration
	var served int64
	var resumes, peak, reqs int
	b.SetBytes(scale.Size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		srv.reset()
		spec := DirectDownload{URL: url, Name: "bench.bin"}
		if bc.spec != nil {
			spec = bc.spec(url, scale.Size)
		}
		b.StartTimer()
		res := runBenchDownload(b, eng, srv, spec)
		b.StopTimer()
		elapsed += res.elapsed
		tail += srv.tail()
		served += srv.served.Load()
		resumes += res.resumes
		peak = max(peak, int(srv.peak.Load()))
		reqs += int(srv.requests.Load())
		path := filepath.Join(eng.cfg.DownloadDir, spec.Name)
		if err := verifyBenchFile(path, scale.Size); err != nil {
			b.Fatal(err)
		}
		removeBenchDownload(b, eng, spec)
		b.StartTimer()
	}
	b.StopTimer()
	n := float64(b.N)
	mbps := float64(scale.Size) * n / elapsed.Seconds() / (1 << 20)
	b.ReportMetric(mbps, "MB/s")
	b.ReportMetric(tail.Seconds()/n, "tail-s")
	b.ReportMetric(float64(served)/(float64(scale.Size)*n), "served/size")
	b.ReportMetric(float64(peak), "peak-conns")
	b.ReportMetric(float64(reqs)/n, "range-reqs")
	if resumes > 0 {
		b.ReportMetric(float64(resumes)/n, "resumes")
	}
	if bc.reportCap > 0 {
		ideal := float64(bc.reportCap*int64(bc.conns)) / (1 << 20)
		b.ReportMetric(mbps/ideal, "x-ideal")
	}
}

// BenchmarkDirectConnections measures throughput against a server that caps
// every connection, for 1 to 16 connections. A perfect scheduler reaches
// x-ideal = 1; without work stealing the first worker's half-file claim leaves
// a long tail on one connection.
func BenchmarkDirectConnections(b *testing.B) {
	for _, scale := range benchScales() {
		for _, conns := range []int{1, 2, 4, 8, 16} {
			b.Run(fmt.Sprintf("size=%s/conns=%d", scale.Name, conns), func(b *testing.B) {
				runBenchCase(b, scale, benchCase{
					conns:     conns,
					server:    benchServerConfig{ConnRate: scale.ConnRate, ETag: true, AcceptRanges: true},
					reportCap: scale.ConnRate,
				})
			})
		}
	}
}

// BenchmarkDirectSlowConnection gives one connection a tenth of the others'
// speed (0.5 MB/s against 5 MB/s at full scale).
func BenchmarkDirectSlowConnection(b *testing.B) {
	for _, scale := range benchScales() {
		b.Run(fmt.Sprintf("size=%s/conns=4", scale.Name), func(b *testing.B) {
			runBenchCase(b, scale, benchCase{
				conns: 4,
				server: benchServerConfig{
					ConnRate: scale.ConnRate, SlowRate: scale.ConnRate / 10,
					ETag: true, AcceptRanges: true,
				},
				reportCap: scale.ConnRate,
			})
		})
	}
}

// BenchmarkDirectNoValidator is a debrid-style host: no ETag, no
// Last-Modified and no checksum, but the size is known from the API.
func BenchmarkDirectNoValidator(b *testing.B) {
	for _, scale := range benchScales() {
		b.Run(fmt.Sprintf("size=%s/conns=4", scale.Name), func(b *testing.B) {
			runBenchCase(b, scale, benchCase{
				conns:  4,
				server: benchServerConfig{ConnRate: scale.ConnRate, AcceptRanges: true},
				spec: func(url string, size int64) DirectDownload {
					return DirectDownload{URL: url, Name: "bench.bin", ExpectedSize: size}
				},
				reportCap: scale.ConnRate,
			})
		})
	}
}

// BenchmarkDirectMidRangeFailure resets the second range connection half-way
// through its range. served/size above 1 is data downloaded twice; resumes
// counts how often the item stopped and needed "p".
func BenchmarkDirectMidRangeFailure(b *testing.B) {
	for _, scale := range benchScales() {
		b.Run(fmt.Sprintf("size=%s/conns=4", scale.Name), func(b *testing.B) {
			runBenchCase(b, scale, benchCase{
				conns: 4,
				server: benchServerConfig{
					ConnRate: scale.ConnRate, ETag: true, AcceptRanges: true,
					Fault: func(n int, _ string, start, end int64) benchFault {
						if n == 2 {
							return benchFault{ResetAfter: (end - start + 1) / 2}
						}
						return benchFault{}
					},
				},
				reportCap: scale.ConnRate,
			})
		})
	}
}

// BenchmarkDirectLatency adds 30 ms before every response, which is what a
// new range request costs on a distant host. range-reqs shows how many
// requests (and so reconnects) a download needed.
func BenchmarkDirectLatency(b *testing.B) {
	for _, scale := range benchScales() {
		b.Run(fmt.Sprintf("size=%s/conns=4", scale.Name), func(b *testing.B) {
			runBenchCase(b, scale, benchCase{
				conns: 4,
				server: benchServerConfig{
					ConnRate: scale.ConnRate, Latency: 30 * time.Millisecond,
					ETag: true, AcceptRanges: true,
				},
				reportCap: scale.ConnRate,
			})
		})
	}
}

// TestBenchServerServesRangesAndFaults keeps the harness itself honest.
func TestBenchServerServesRangesAndFaults(t *testing.T) {
	const size = 1 << 20
	srv := newBenchServer(t, benchServerConfig{
		Size: size, ETag: true,
		Fault: func(n int, _ string, _, _ int64) benchFault {
			switch n {
			case 2:
				return benchFault{Status: http.StatusServiceUnavailable, RetryAfter: "2"}
			case 3:
				return benchFault{ResetAfter: 1000}
			}
			return benchFault{}
		},
	})
	get := func(rng string) (*http.Response, []byte, error) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/f", nil)
		req.Header.Set("Range", rng)
		resp, err := srv.Client().Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp, body, err
	}
	resp, body, err := get("bytes=100-199")
	if err != nil || resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, benchPayload(size)[100:200]) {
		t.Fatalf("range = %v, %v", resp, err)
	}
	if resp, _, _ = get("bytes=0-99"); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("fault = %v", resp)
	}
	if _, body, err = get("bytes=0-"); err == nil && len(body) != 1000 {
		t.Fatalf("reset served %d bytes without an error", len(body))
	}
	if got := srv.requests.Load(); got != 3 {
		t.Fatalf("range requests = %d, want 3", got)
	}
}
