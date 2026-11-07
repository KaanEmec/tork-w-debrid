package engine

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// Tests for roadmap J7.4: the opening split, work stealing, the
// slow-connection check, and a property test that splits, fails and resumes
// ranges at random.

// writeSeg streams up to n bytes into w the way fetchSegment does and
// reports whether w reached its end.
func writeSeg(run *segmentRun, w *segWorker, n int64) bool {
	for n > 0 {
		_, k, restart := run.reserve(w, min(n, 1<<20))
		if restart || k == 0 {
			return false
		}
		done, _, _, _ := run.commit(w, k)
		n -= k
		if done {
			return true
		}
	}
	return false
}

func claimSeg(t *testing.T, run *segmentRun, w *segWorker, now time.Time) byteRange {
	t.Helper()
	r, wait, ok := run.claim(w, now)
	if !ok || wait != 0 {
		t.Fatalf("claim = %+v, wait %v, ok %v", r, wait, ok)
	}
	run.attempt(w)
	return r
}

func TestFirstClaimsSplitEvenlyAndStealHalves(t *testing.T) {
	const mb = 1 << 20
	now := time.Now()
	run := newSegmentRun(100*mb, mb, nil, 4)
	var ws []*segWorker
	for i := range 4 {
		w := run.addWorker()
		ws = append(ws, w)
		r := claimSeg(t, run, w, now)
		if want := (byteRange{int64(i) * 25 * mb, int64(i+1)*25*mb - 1}); r != want {
			t.Fatalf("opening claim %d = %+v, want %+v", i, r, want)
		}
	}
	if !writeSeg(run, ws[0], 25*mb) {
		t.Fatal("worker 0 did not reach its end")
	}
	run.release(ws[0], false, 0, now)
	for _, w := range ws[1:] {
		writeSeg(run, w, mb)
	}
	// The free worker takes the upper half of what the first running range
	// with the most left still has to go.
	r := claimSeg(t, run, ws[0], now)
	if want := (byteRange{38 * mb, 50*mb - 1}); r != want || ws[1].end != 38*mb-1 {
		t.Fatalf("stolen = %+v (victim end %d), want %+v (victim end %d)", r, ws[1].end, want, 38*mb-1)
	}

	// Never split below min_chunk_size, and never split a range whose
	// request has not delivered data yet.
	run = newSegmentRun(4*mb, mb, nil, 4)
	ws = ws[:0]
	for range 4 {
		w := run.addWorker()
		ws = append(ws, w)
		claimSeg(t, run, w, now)
	}
	writeSeg(run, ws[0], mb)
	run.release(ws[0], false, 0, now)
	if r, _, ok := run.claim(ws[0], now); ok {
		t.Fatalf("split a range that has not delivered data: %+v", r)
	}
	writeSeg(run, ws[1], 1)
	w := run.addWorker()
	if r, _, ok := run.claim(w, now); ok {
		t.Fatalf("split below min_chunk_size: %+v", r)
	}
}

func TestHealthCheckRestartsSlowConnectionOncePerCooldown(t *testing.T) {
	const mb = 1 << 20
	t0 := time.Now()
	run := newSegmentRun(1<<30, mb, nil, 4)
	var ws []*segWorker
	for range 4 {
		w := run.addWorker()
		ws = append(ws, w)
		claimSeg(t, run, w, t0)
	}
	slow := ws[2]
	step := directTuning.healthInterval
	var restarts []time.Duration
	for at := step; at <= 85*time.Second; at += step {
		now := t0.Add(at)
		for _, w := range ws {
			rate := int64(2 * mb) // bytes per second
			if w == slow {
				rate = mb / 5
			}
			if !writeSeg(run, w, rate*int64(step/time.Second)) && w.restart {
				// the restart reached the worker: it hands its range back
				// and reconnects
				run.release(w, false, 0, now)
				claimSeg(t, run, w, now)
				writeSeg(run, w, rate*int64(step/time.Second))
			}
		}
		run.healthCheck(now)
		for _, w := range ws {
			if w.restart {
				if w != slow {
					t.Fatalf("a fast connection was restarted at %v", at)
				}
				restarts = append(restarts, at)
			}
		}
	}
	// The check needs a full 10 s window (from 5 s), then two slow checks
	// in a row: the first restart is at 20 s, the next a minute later.
	want := []time.Duration{20 * time.Second, 80 * time.Second}
	if !slices.Equal(restarts, want) {
		t.Fatalf("restarts at %v, want %v", restarts, want)
	}
	if m := run.snapshot(t0.Add(85 * time.Second)); m.Restarts != 2 {
		t.Fatalf("snapshot counts %d restarts, want 2", m.Restarts)
	}
}

// schedModel drives a segmentRun the way the workers and the supervisor do,
// writing into an in-memory file, and checks after every step that the
// finished, waiting and running ranges tile the file exactly, that every
// byte the run reports as written holds the right data, and that no byte is
// counted twice.
type schedModel struct {
	t         testing.TB
	data      []byte
	pos       int
	total     int64
	minChunk  int64
	src, file []byte
	run       *segmentRun
	ws        []*segWorker
	now       time.Time
	counted   int64 // bytes the run should report as written
	saved     []byteRange
}

func (m *schedModel) next() int {
	if m.pos >= len(m.data) {
		return 0
	}
	m.pos++
	return int(m.data[m.pos-1])
}

// resume starts a new run from the saved manifest. Bytes it does not list
// are overwritten with junk, as a crash may leave them.
func (m *schedModel) resume(completed []byteRange, conns int) {
	keep := make([]bool, m.total)
	for _, r := range completed {
		for i := r.Start; i <= r.End; i++ {
			keep[i] = true
		}
	}
	for i := range m.file {
		if !keep[i] {
			m.file[i] = 0xAA
		}
	}
	m.run = newSegmentRun(m.total, m.minChunk, completed, conns)
	m.ws = nil
	m.counted = rangeBytes(completed)
}

// spawn adds workers like the supervisor does.
func (m *schedModel) spawn() {
	for m.run.spawnable() {
		m.run.addWorker()
	}
	m.ws = m.ws[:0]
	for _, w := range m.run.workers {
		if !w.exited {
			m.ws = append(m.ws, w)
		}
	}
}

func (m *schedModel) claim(w *segWorker) {
	if w.active || w.exited {
		return
	}
	_, wait, ok := m.run.claim(w, m.now)
	switch {
	case !ok:
	case wait > 0:
		m.now = m.now.Add(wait)
	default:
		m.run.attempt(w)
	}
}

func (m *schedModel) write(w *segWorker, n int64) {
	if !w.active {
		return
	}
	off, k, restart := m.run.reserve(w, n)
	if restart || k == 0 {
		m.run.release(w, false, 0, m.now)
		return
	}
	copy(m.file[off:off+k], m.src[off:off+k])
	done, _, _, _ := m.run.commit(w, k)
	m.counted += k
	if done {
		m.run.release(w, false, 0, m.now)
	}
}

func (m *schedModel) check() {
	m.t.Helper()
	s := m.run
	s.mu.Lock()
	var tiles []byteRange
	tiles = append(tiles, s.completed...)
	for _, p := range s.pending {
		tiles = append(tiles, p.byteRange)
	}
	for _, w := range s.workers {
		if !w.exited && w.active {
			if w.pos < w.start || w.pos > w.end+1 || w.reserved != 0 {
				s.mu.Unlock()
				m.t.Fatalf("worker state start %d pos %d end %d reserved %d", w.start, w.pos, w.end, w.reserved)
			}
			tiles = append(tiles, byteRange{w.start, w.end})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(tiles, func(a, b byteRange) int { return cmp.Compare(a.Start, b.Start) })
	next := int64(0)
	for _, r := range tiles {
		if r.Start != next || r.End < r.Start {
			m.t.Fatalf("ranges do not tile the file at %d: %+v", next, tiles)
		}
		next = r.End + 1
	}
	if next != m.total {
		m.t.Fatalf("ranges end at %d, file is %d bytes: %+v", next, m.total, tiles)
	}
	done := s.doneRanges()
	for i, r := range done {
		if i > 0 && r.Start <= done[i-1].End+1 {
			m.t.Fatalf("written ranges overlap or are not merged: %+v", done)
		}
		if !bytes.Equal(m.file[r.Start:r.End+1], m.src[r.Start:r.End+1]) {
			m.t.Fatalf("written range %+v holds wrong bytes", r)
		}
	}
	if got := rangeBytes(done); got != m.counted {
		m.t.Fatalf("run reports %d bytes written, workers wrote %d", got, m.counted)
	}
}

func runSchedModel(t testing.TB, data []byte) {
	m := &schedModel{t: t, data: data, now: time.Unix(1_700_000_000, 0)}
	m.total = int64(1 + m.next()<<8 + m.next())
	m.minChunk = int64(1 + (m.next()%8)*512)
	m.src = benchPayload(m.total)
	m.file = make([]byte, m.total)
	m.resume(nil, 1+m.next()%6)
	for m.pos < len(m.data) && !m.run.finished() {
		m.spawn()
		if len(m.ws) == 0 {
			m.now = m.now.Add(time.Second)
			m.next()
			continue
		}
		op, w := m.next()%10, m.ws[m.next()%len(m.ws)]
		switch op {
		case 0, 1:
			m.claim(w)
		case 2, 3, 4:
			m.write(w, int64(1+m.next()*m.next()%4096))
		case 5: // the connection failed
			if w.active {
				m.run.release(w, true, time.Duration(m.next())*time.Millisecond, m.now)
			}
		case 6: // the slow-connection check asks it to reconnect
			m.run.mu.Lock()
			w.restart = w.active
			m.run.mu.Unlock()
		case 7:
			m.now = m.now.Add(time.Duration(m.next()) * 10 * time.Millisecond)
		case 8:
			if m.next()%2 == 0 {
				m.run.busy(time.Duration(m.next())*10*time.Millisecond, m.now)
			} else {
				m.run.setWant(1+m.next()%6, m.now)
			}
		case 9:
			if m.next()%2 == 0 {
				m.saved = m.run.doneRanges() // checkpoint
			} else {
				m.resume(m.saved, 1+m.next()%6) // crash, then resume from the checkpoint
			}
		}
		m.check()
	}
	// Run to the end with every connection streaming.
	for i := 0; ; i++ {
		if i > 100_000 {
			t.Fatalf("the run did not finish: %d of %d bytes", rangeBytes(m.run.doneRanges()), m.total)
		}
		m.spawn()
		if m.run.finished() {
			break
		}
		if len(m.ws) == 0 {
			m.now = m.now.Add(time.Second)
			continue
		}
		for _, w := range m.ws {
			m.claim(w)
			m.write(w, 1<<20)
		}
		m.check()
	}
	if rangeBytes(m.run.doneRanges()) != m.total || !bytes.Equal(m.file, m.src) {
		t.Fatal("finished file differs from the source")
	}
}

func FuzzSegmentScheduler(f *testing.F) {
	f.Add([]byte{0x20, 0x00, 3, 3, 0, 0, 2, 0, 50, 50, 5, 1, 0, 0, 2, 1, 9, 9, 0, 9, 1, 3})
	f.Add([]byte{0xff, 0xff, 0, 5, 0, 0, 0, 1, 0, 2, 2, 0, 200, 200, 6, 1, 2, 1, 9, 9, 8, 0, 2, 9, 1, 2, 0, 0, 0})
	f.Add([]byte{0x01, 0x10, 7, 1, 8, 0, 1, 50, 2, 0, 255, 255, 5, 0, 7, 0, 100, 0, 1, 2, 3, 9, 0, 0, 9, 1, 1, 4})
	f.Fuzz(func(t *testing.T, data []byte) {
		runSchedModel(t, data)
	})
}

// TestSegmentSchedulerRandomOps runs the fuzz model on many random op
// streams in a normal test run.
func TestSegmentSchedulerRandomOps(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for i := range 300 {
		data := make([]byte, 64+rng.IntN(2000))
		for j := range data {
			data[j] = byte(rng.Uint32())
		}
		t.Run("", func(t *testing.T) {
			defer func() {
				if t.Failed() {
					t.Logf("op stream %d: %x", i, data)
				}
			}()
			runSchedModel(t, data)
		})
	}
}

// TestRandomFaultsAndPausesKeepFileIntact downloads through random resets,
// server errors, pauses and connection changes; the file must still match
// its checksum and every saved manifest must be exact.
func TestRandomFaultsAndPausesKeepFileIntact(t *testing.T) {
	const size = 4 << 20
	scaleDirectTuning(t, 0.01)
	sum := sha256.Sum256(benchPayload(size))
	for seed := uint64(1); seed <= 3; seed++ {
		var mu sync.Mutex
		rng := rand.New(rand.NewPCG(seed, 99))
		srv := newBenchServer(t, benchServerConfig{
			Size: size, ConnRate: 4 << 20, ETag: true, AcceptRanges: true,
			Fault: func(_ int, _ string, start, end int64) benchFault {
				mu.Lock()
				defer mu.Unlock()
				switch p := rng.Float64(); {
				case p < 0.25:
					return benchFault{ResetAfter: 1 + rng.Int64N(end-start+1)}
				case p < 0.30:
					return benchFault{Status: http.StatusServiceUnavailable, RetryAfter: "0"}
				case p < 0.35:
					return benchFault{Status: http.StatusInternalServerError}
				}
				return benchFault{}
			},
		})
		eng := newDirectTestEngine(t)
		eng.directMinChunkSize = 64 << 10
		eng.directMaxConnections = 4
		dest := filepath.Join(eng.cfg.DownloadDir, "f.bin")
		h, err := eng.AddDirectDownload(DirectDownload{
			URL: srv.URL + "/f.bin", Name: "f.bin", Checksum: mustSHA256(t, hex.EncodeToString(sum[:])),
		})
		if err != nil {
			t.Fatal(err)
		}
		pauses, stops := 0, 0
		deadline := time.Now().Add(60 * time.Second)
		for {
			if time.Now().After(deadline) {
				t.Fatalf("seed %d: download did not finish", seed)
			}
			time.Sleep(10 * time.Millisecond)
			snap, _ := eng.Snapshot(h)
			if snap.State == StateDone {
				break
			}
			mu.Lock()
			roll, conns := rng.Float64(), rng.IntN(7)
			mu.Unlock()
			switch {
			case snap.State == StatePaused:
				// stopped after too many failures: press p
				if stops++; stops > 20 {
					t.Fatalf("seed %d: stopped %d times: %s", seed, stops, snap.Note)
				}
				checkSavedManifest(t, dest)
				if err := eng.Resume(h); err != nil {
					t.Fatal(err)
				}
			case roll < 0.1 && pauses < 4:
				pauses++
				if err := eng.Pause(h); err != nil {
					t.Fatal(err)
				}
				checkSavedManifest(t, dest)
				if err := eng.Resume(h); err != nil {
					t.Fatal(err)
				}
			case roll < 0.2:
				if err := eng.SetDirectConnections(h, conns); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := verifyBenchFile(dest, size); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
}

// checkSavedManifest checks the manifest next to dest, if the run got far
// enough to write one.
func checkSavedManifest(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(dest + ".part.meta"); os.IsNotExist(err) {
		return
	}
	checkManifestRanges(t, dest, readTestManifest(t, dest))
}
