package engine

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// Acceptance tests for roadmap J4.3, the ready check. Every duration is the
// real one divided by scale, so the tests keep the real ratios (a 5 s
// window, 1 s polls) without waiting 5 s.

// timedRD is fakeDebrid with a clock: the torrent is ready readyAfter after
// the selection, and every Info call's time is kept.
type timedRD struct {
	*fakeDebrid
	readyAfter time.Duration

	mu       sync.Mutex
	selectAt time.Time
	infoAt   []time.Time
}

func newTimedRD(files []rdFile, readyAfter time.Duration) *timedRD {
	r := &timedRD{fakeDebrid: &fakeDebrid{}, readyAfter: readyAfter}
	ready := readyTorrent("Show", files, rdLinks(len(files)))
	r.fakeDebrid.info = func(n int, selected bool) (debrid.RemoteInfo, error) {
		r.mu.Lock()
		at := r.selectAt
		r.mu.Unlock()
		info, err := ready(n, selected)
		if selected && time.Since(at) < r.readyAfter {
			info.Status, info.Progress, info.Links = debrid.Downloading, 40, nil
			info.SpeedBps, info.Seeders = 12<<20, 8
		}
		return info, err
	}
	return r
}

func (r *timedRD) SelectFiles(ctx context.Context, id string, ids []int) error {
	r.mu.Lock()
	r.selectAt = time.Now()
	r.mu.Unlock()
	return r.fakeDebrid.SelectFiles(ctx, id, ids)
}

func (r *timedRD) Info(ctx context.Context, id string) (debrid.RemoteInfo, error) {
	r.mu.Lock()
	r.infoAt = append(r.infoAt, time.Now())
	r.mu.Unlock()
	return r.fakeDebrid.Info(ctx, id)
}

// infoCallsWithin counts the Info calls made in [selection, selection+d).
func (r *timedRD) infoCallsWithin(d time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, at := range r.infoAt {
		if !at.Before(r.selectAt) && at.Sub(r.selectAt) < d {
			n++
		}
	}
	return n
}

// readyCheckTimings scales the real window (5 s) and fast poll (1 s).
func readyCheckTimings(t *testing.T, scale time.Duration) time.Duration {
	t.Helper()
	fastDebrid(t, 5*time.Second/scale)
	debridTuning.fastPoll = time.Second / scale
	debridTuning.downloadPoll = 5 * time.Second / scale
	debridTuning.queuedPoll = 5 * time.Second / scale
	scaleDirectTuning(t, 0.01)
	return 5 * time.Second / scale
}

// Cached path: ready 800 ms after the selection (scaled). The row never
// shows the cloud, local bytes arrive, no "not cached" notice is due, and
// no more than 6 Info calls fall inside the window.
func TestReadyCheckCachedNeverShowsCloud(t *testing.T) {
	const scale = 10
	window := readyCheckTimings(t, scale)
	files := []rdFile{{1, "S01/e01.mkv", testPayload(512<<10, 1)}, {2, "S01/e02.mkv", testPayload(256<<10, 2)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 2 << 20
	rd := newTimedRD(files, 800*time.Millisecond/scale)
	rd.unlock = unlockTo(host, files)
	eng := newDebridTestEngine(t, rd)
	eng.directMinChunkSize = 32 << 10
	h := startLocal(t, eng, t.TempDir())

	var states []TorrentState
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no local bytes; states %v", states)
		}
		s, ok := eng.Snapshot(h)
		if !ok {
			t.Fatal("item disappeared")
		}
		if len(states) == 0 || states[len(states)-1] != s.State {
			states = append(states, s.State)
		}
		if s.State == StateRemote {
			t.Fatalf("a cached torrent showed the cloud: states %v", states)
		}
		if s.State == StateFetchingMeta && !s.DebridStarting() {
			t.Fatalf("waiting snapshot does not read starting: %+v", s)
		}
		if got := eng.TakeDebridNotice(h); got != DebridNoticeNone {
			t.Fatalf("notice %v for a cached torrent", got)
		}
		if (s.State == StateDownloading && s.BytesCompleted > 0) || s.State == StateDone {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if states[0] != StateFetchingMeta || slices.Contains(states, StateRemote) {
		t.Fatalf("states %v, want starting then downloading", states)
	}
	if n := rd.infoCallsWithin(window); n > 6 {
		t.Fatalf("%d Info calls inside the window, want at most 6", n)
	}
}

// Uncached path: ready long after the window. The row switches to the cloud
// after the window with one "not cached" notice, then starts the local
// phase by itself with one "ready" notice.
func TestReadyCheckUncachedShowsCloudThenStartsLocal(t *testing.T) {
	const scale = 50 // window 100 ms, ready after 600 ms ("30 s" in spirit)
	window := readyCheckTimings(t, scale)
	files := []rdFile{{1, "movie.mkv", testPayload(512<<10, 3)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 2 << 20
	rd := newTimedRD(files, 6*window)
	rd.unlock = unlockTo(host, files)
	eng := newDebridTestEngine(t, rd)
	eng.directMinChunkSize = 32 << 10
	h := startLocal(t, eng, t.TempDir())

	var states []TorrentState
	var notices []DebridNotice
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("not done; states %v notices %v", states, notices)
		}
		n := eng.TakeDebridNotice(h) // first, so the snapshot is not older
		s, _ := eng.Snapshot(h)
		if len(states) == 0 || states[len(states)-1] != s.State {
			states = append(states, s.State)
		}
		if n != DebridNoticeNone {
			notices = append(notices, n)
			switch {
			case n == DebridNoticeNotCached && s.State != StateRemote:
				t.Fatalf("not-cached notice while the row is %v", s.State)
			case n == DebridNoticeReady && s.State == StateRemote:
				t.Fatal("ready notice while the row still shows the cloud")
			}
		}
		if s.State == StateRemote && (s.Remote == nil || s.Remote.Seeders != 8) {
			t.Fatalf("cloud snapshot = %+v", s)
		}
		if s.State == StateDone {
			break
		}
		time.Sleep(time.Millisecond)
	}
	i := slices.Index(states, StateRemote)
	if i < 1 || states[0] != StateFetchingMeta || !slices.Contains(states[i:], StateDone) {
		t.Fatalf("states %v, want starting, cloud, then the local phase", states)
	}
	for range 5 { // never again
		if n := eng.TakeDebridNotice(h); n != DebridNoticeNone {
			notices = append(notices, n)
		}
	}
	if !slices.Equal(notices, []DebridNotice{DebridNoticeNotCached, DebridNoticeReady}) {
		t.Fatalf("notices %v, want one not-cached then one ready", notices)
	}
	if n := rd.infoCallsWithin(window); n > 6 {
		t.Fatalf("%d Info calls inside the window, want at most 6", n)
	}
}

// A UI that attaches after the item left the cloud still gets the ready
// notice once, and not the stale "not cached" one.
func TestReadyCheckNoticeWaitsForLateUI(t *testing.T) {
	const scale = 50
	window := readyCheckTimings(t, scale)
	files := []rdFile{{1, "movie.mkv", testPayload(64<<10, 4)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 64 << 10 // one second of local download
	rd := newTimedRD(files, 3*window)
	rd.unlock = unlockTo(host, files)
	eng := newDebridTestEngine(t, rd)
	h := startLocal(t, eng, t.TempDir())
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.State == StateDownloading }, 10*time.Second)
	if n := eng.TakeDebridNotice(h); n != DebridNoticeReady {
		t.Fatalf("late notice = %v, want ready", n)
	}
	if n := eng.TakeDebridNotice(h); n != DebridNoticeNone {
		t.Fatalf("second notice = %v", n)
	}
}

// The window is measured from the selection, not from the add: a slow
// conversion before it must not eat into it.
func TestReadyCheckWindowStartsAtSelection(t *testing.T) {
	const scale = 50
	window := readyCheckTimings(t, scale)
	files := []rdFile{{1, "movie.mkv", testPayload(64<<10, 5)}}
	rd := newTimedRD(files, time.Hour)
	inner := rd.fakeDebrid.info
	rd.fakeDebrid.info = func(n int, selected bool) (debrid.RemoteInfo, error) {
		if n < 8 { // converting for well over a window
			return debrid.RemoteInfo{Name: "Show", Status: debrid.Converting}, nil
		}
		return inner(n, selected)
	}
	eng := newDebridTestEngine(t, rd)
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.StartDebrid(h, nil, DebridOptions{DownloadDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "the selection", 10*time.Second, func() bool { return len(rd.selectCalls()) == 1 })
	if s, _ := eng.Snapshot(h); s.State != StateFetchingMeta {
		t.Fatalf("state right after a late selection = %v, want starting", s.State)
	}
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.State == StateRemote }, 10*time.Second)
	rd.mu.Lock()
	waited := time.Since(rd.selectAt)
	rd.mu.Unlock()
	if waited < window {
		t.Fatalf("cloud shown %v after the selection, want at least %v", waited, window)
	}
}
