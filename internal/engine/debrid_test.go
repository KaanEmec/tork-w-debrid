package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// fakeDebrid is a scripted debrid.Provider. info decides each Info answer
// from the number of Info calls so far and whether SelectFiles ran.
type fakeDebrid struct {
	mu        sync.Mutex
	info      func(n int, selected bool) (debrid.RemoteInfo, error)
	selectErr error
	addErr    error

	// unlock answers Unlock (n counts the calls from 0); nil waits until the
	// call is cancelled, so the local phase stays "starting".
	unlock      func(n int, link string) (debrid.Unlocked, error)
	unlockCalls atomic.Int32
	deleteErr   error
	gone        map[string]bool // remote IDs Info answers ErrNotFound for
	infoErr     error           // when set, every Info call fails with it

	infoCalls atomic.Int32
	adds      []string // magnets, or "torrent" for AddTorrentFile
	addedData [][]byte
	selects   [][]int
	deletes   []string
	selected  bool
}

func (f *fakeDebrid) Name() string { return "Real-Debrid" }

func (f *fakeDebrid) AddMagnet(_ context.Context, magnet string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.adds = append(f.adds, magnet)
	return fmt.Sprintf("RID%d", len(f.adds)), nil
}

func (f *fakeDebrid) AddTorrentFile(_ context.Context, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.adds = append(f.adds, "torrent")
	f.addedData = append(f.addedData, data)
	return fmt.Sprintf("RID%d", len(f.adds)), nil
}

func (f *fakeDebrid) Info(_ context.Context, id string) (debrid.RemoteInfo, error) {
	n := int(f.infoCalls.Add(1)) - 1
	f.mu.Lock()
	selected := f.selected
	fn := f.info
	gone, infoErr := f.gone[id], f.infoErr
	f.mu.Unlock()
	if infoErr != nil {
		return debrid.RemoteInfo{}, infoErr
	}
	if gone {
		return debrid.RemoteInfo{}, &debrid.Error{Provider: "Real-Debrid", Op: "torrents/info", HTTPStatus: 404, Kind: debrid.ErrNotFound}
	}
	info, err := fn(n, selected)
	info.ID = id
	return info, err
}

func (f *fakeDebrid) SelectFiles(_ context.Context, _ string, ids []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selectErr != nil {
		return f.selectErr
	}
	f.selects = append(f.selects, ids)
	f.selected = true
	return nil
}

func (f *fakeDebrid) Unlock(ctx context.Context, link string) (debrid.Unlocked, error) {
	n := int(f.unlockCalls.Add(1)) - 1
	f.mu.Lock()
	fn := f.unlock
	f.mu.Unlock()
	if fn == nil {
		<-ctx.Done()
		return debrid.Unlocked{}, ctx.Err()
	}
	return fn(n, link)
}

func (f *fakeDebrid) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return f.deleteErr
}

func (f *fakeDebrid) Account(context.Context) (debrid.Account, error) {
	return debrid.Account{Username: "test", Premium: true}, nil
}

func (f *fakeDebrid) selectCalls() [][]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.selects)
}

func (f *fakeDebrid) addCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.adds)
}

func (f *fakeDebrid) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deletes)
}

var testRemoteFiles = []debrid.RemoteFile{
	{ID: 1, Path: "Show/S01/e01.mkv", Bytes: 700 << 20},
	{ID: 2, Path: "Show/readme.nfo", Bytes: 1 << 10},
}

// remoteAt builds an answer with the given status and progress.
func remoteAt(status debrid.RemoteStatus, progress float64, selected bool) debrid.RemoteInfo {
	info := debrid.RemoteInfo{Name: "Show", Status: status, Progress: progress, Bytes: 700 << 20, SpeedBps: 4 << 20, Seeders: 9}
	if status != debrid.Converting {
		info.Files = slices.Clone(testRemoteFiles)
		if selected {
			info.Files[0].Selected = true
		}
	}
	if status == debrid.Ready {
		info.Links = []string{"https://real-debrid.com/d/ABC"}
	}
	return info
}

// fastDebrid shrinks every debrid timing for the test.
func fastDebrid(t *testing.T, window time.Duration) {
	t.Helper()
	oldTuning, oldWindow := debridTuning, debridReadyWindow
	debridTuning.fastPoll = 5 * time.Millisecond
	debridTuning.downloadPoll = 5 * time.Millisecond
	debridTuning.queuedPoll = 5 * time.Millisecond
	debridTuning.retryBase = 5 * time.Millisecond
	debridTuning.retryMax = 20 * time.Millisecond
	debridTuning.autoRetryBase = 10 * time.Millisecond
	debridReadyWindow = window
	t.Cleanup(func() { debridTuning, debridReadyWindow = oldTuning, oldWindow })
}

func newDebridTestEngine(t *testing.T, p debrid.Provider) *Engine {
	t.Helper()
	eng := newDirectTestEngine(t)
	eng.SetDebridProvider(p)
	return eng
}

const testMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Show"

func awaitCond(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDebridItemLifecycle(t *testing.T) {
	fastDebrid(t, 40*time.Millisecond)
	var selectedAt atomic.Int32
	selectedAt.Store(-1)
	fake := &fakeDebrid{}
	fake.info = func(n int, selected bool) (debrid.RemoteInfo, error) {
		switch {
		case n < 3:
			return remoteAt(debrid.Converting, 0, false), nil
		case !selected:
			return remoteAt(debrid.WaitingSelection, 0, false), nil
		}
		selectedAt.CompareAndSwap(-1, int32(n))
		step := n - int(selectedAt.Load())
		if step >= 40 {
			return remoteAt(debrid.Ready, 100, true), nil
		}
		return remoteAt(debrid.Downloading, float64(step)*2.5, true), nil
	}
	eng := newDebridTestEngine(t, fake)
	var fired atomic.Int32
	var event DebridReadyEvent
	eng.OnDebridReady(func(ev DebridReadyEvent) {
		event = ev
		fired.Add(1)
	})

	h, owned, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil || !owned {
		t.Fatalf("AddDebridForPreview = %v, owned %v", err, owned)
	}
	if want := metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567"); h != want {
		t.Fatalf("hash = %s, want the magnet's infohash", h.HexString())
	}
	if snap, ok := eng.Snapshot(h); !ok || snap.State != StateFetchingMeta || snap.Via != "realdebrid" {
		t.Fatalf("first snapshot = %+v", snap)
	}
	var files []debrid.RemoteFile
	awaitCond(t, "the remote file list", 5*time.Second, func() bool {
		var ready bool
		files, ready = eng.DebridFiles(h)
		return ready
	})
	if len(files) != 2 || files[0].Path != "Show/S01/e01.mkv" {
		t.Fatalf("files = %+v", files)
	}
	if snap, _ := eng.Snapshot(h); snap.State != StatePreviewing {
		t.Fatalf("preview snapshot state = %v", snap.State)
	}
	// the preview poller stops once the list is known
	calls := fake.infoCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if got := fake.infoCalls.Load(); got != calls {
		t.Fatalf("Info polled %d more times while waiting for the selection", got-calls)
	}

	dir := t.TempDir()
	if err := eng.StartDebrid(h, []int{1}, DebridOptions{DownloadDir: dir}); err != nil {
		t.Fatal(err)
	}
	var states []TorrentState
	var lastRemote int64 = -1
	sawRising := 0
	deadline := time.Now().Add(10 * time.Second)
	for fired.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("ready hook never fired; states %v", states)
		}
		snap, ok := eng.Snapshot(h)
		if !ok {
			t.Fatal("item disappeared")
		}
		if len(states) == 0 || states[len(states)-1] != snap.State {
			states = append(states, snap.State)
		}
		if snap.State == StateRemote {
			if snap.BytesCompleted < lastRemote {
				t.Fatalf("cloud progress went backwards: %d after %d", snap.BytesCompleted, lastRemote)
			}
			if snap.BytesCompleted > lastRemote && lastRemote >= 0 {
				sawRising++
			}
			lastRemote = snap.BytesCompleted
			if snap.Length != 700<<20 || snap.Remote == nil || !strings.HasPrefix(snap.Note, "☁ RD ") {
				t.Fatalf("remote snapshot = %+v", snap)
			}
		}
		time.Sleep(time.Millisecond)
	}
	if len(states) < 2 || states[0] != StateFetchingMeta || !slices.Contains(states, StateRemote) {
		t.Fatalf("state sequence = %v, want fetching metadata then remote", states)
	}
	if i := slices.Index(states, StateRemote); slices.Contains(states[i:], StateFetchingMeta) {
		t.Fatalf("state went back to fetching metadata: %v", states)
	}
	if sawRising < 2 {
		t.Fatalf("cloud progress rose only %d times", sawRising)
	}
	time.Sleep(30 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatalf("ready hook fired %d times, want 1", fired.Load())
	}
	if event.Hash != h || event.Info.Status != debrid.Ready {
		t.Fatalf("ready event = %+v", event)
	}
	if got := fake.selectCalls(); len(got) != 1 || !slices.Equal(got[0], []int{1}) {
		t.Fatalf("SelectFiles calls = %v, want [[1]]", got)
	}
	calls = fake.infoCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if got := fake.infoCalls.Load(); got != calls {
		t.Fatalf("Info polled %d more times after ready", got-calls)
	}
	// the local phase took over (its unlock waits here, see debrid_local_test.go)
	info, ok := eng.DebridItem(h)
	if !ok || info.Phase != DebridLocal || info.RemoteID != "RID1" || info.DownloadDir != dir || !slices.Equal(info.Selected, []int{1}) {
		t.Fatalf("DebridItem = %+v", info)
	}
	if eng.Magnet(h) != testMagnet {
		t.Fatalf("Magnet(h) = %q", eng.Magnet(h))
	}
}

// A cached torrent is ready inside the window: the row never shows the cloud.
func TestDebridReadyInsideWindowNeverShowsRemote(t *testing.T) {
	fastDebrid(t, time.Second)
	fake := &fakeDebrid{}
	fake.info = func(n int, selected bool) (debrid.RemoteInfo, error) {
		switch {
		case n < 2:
			return remoteAt(debrid.Converting, 0, false), nil
		case !selected:
			return remoteAt(debrid.WaitingSelection, 0, false), nil
		case n < 6:
			return remoteAt(debrid.Downloading, 50, true), nil
		}
		return remoteAt(debrid.Ready, 100, true), nil
	}
	eng := newDebridTestEngine(t, fake)
	var fired atomic.Int32
	eng.OnDebridReady(func(DebridReadyEvent) { fired.Add(1) })
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	// start before the file list is known: the selection waits for it
	if err := eng.StartDebrid(h, nil, DebridOptions{}); err != nil {
		t.Fatal(err)
	}
	for fired.Load() == 0 {
		if snap, _ := eng.Snapshot(h); snap.State != StateFetchingMeta {
			t.Fatalf("state %v inside the ready window", snap.State)
		}
		time.Sleep(time.Millisecond)
	}
	if snap, _ := eng.Snapshot(h); snap.State != StateFetchingMeta {
		t.Fatalf("state after a quick ready = %v", snap.State)
	}
	if got := fake.selectCalls(); len(got) != 1 || got[0] != nil {
		t.Fatalf("SelectFiles calls = %v, want one call selecting every file", got)
	}
}

func TestDebridCancelPreviewDeletesRemote(t *testing.T) {
	fastDebrid(t, time.Second)
	fake := &fakeDebrid{info: func(int, bool) (debrid.RemoteInfo, error) {
		return remoteAt(debrid.WaitingSelection, 0, false), nil
	}}
	eng := newDebridTestEngine(t, fake)
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "the file list", 5*time.Second, func() bool { _, ok := eng.DebridFiles(h); return ok })
	eng.CancelDebridPreview(h)
	awaitCond(t, "the remote Delete", 5*time.Second, func() bool { return len(fake.deleted()) == 1 })
	if got := fake.deleted(); got[0] != "RID1" {
		t.Fatalf("deleted %v, want RID1", got)
	}
	if _, ok := eng.Snapshot(h); ok {
		t.Fatal("cancelled preview is still listed")
	}
}

// A started item is not a preview any more: leaving must not delete it.
func TestDebridCancelPreviewIgnoresStartedItem(t *testing.T) {
	fastDebrid(t, time.Second)
	fake := &fakeDebrid{info: func(_ int, selected bool) (debrid.RemoteInfo, error) {
		if selected {
			return remoteAt(debrid.Downloading, 10, true), nil
		}
		return remoteAt(debrid.WaitingSelection, 0, false), nil
	}}
	eng := newDebridTestEngine(t, fake)
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "the file list", 5*time.Second, func() bool { _, ok := eng.DebridFiles(h); return ok })
	if err := eng.StartDebrid(h, []int{1}, DebridOptions{}); err != nil {
		t.Fatal(err)
	}
	eng.CancelDebridPreview(h)
	time.Sleep(30 * time.Millisecond)
	if len(fake.deleted()) != 0 {
		t.Fatal("a started download was deleted on the service")
	}
	if _, ok := eng.Snapshot(h); !ok {
		t.Fatal("a started download was dropped")
	}
}

func TestDebridRefusesHashOfOtherKind(t *testing.T) {
	fastDebrid(t, time.Second)
	fake := &fakeDebrid{info: func(int, bool) (debrid.RemoteInfo, error) {
		return remoteAt(debrid.WaitingSelection, 0, false), nil
	}}
	eng := newDebridTestEngine(t, fake)

	// debrid first, then the same hash as a torrent
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.AddForPreview(testMagnet); !errors.Is(err, ErrAlreadyDebrid) {
		t.Fatalf("torrent preview of a debrid hash: err = %v", err)
	}
	if _, err := eng.Add(testMagnet, nil); !errors.Is(err, ErrAlreadyDebrid) {
		t.Fatalf("torrent add of a debrid hash: err = %v", err)
	}
	if again, owned, err := eng.AddDebridForPreview(t.Context(), testMagnet); err != nil || owned || again != h {
		t.Fatalf("second debrid add = %v, owned %v, err %v", again, owned, err)
	}

	// a torrent first, then the same hash through debrid
	other := "magnet:?xt=urn:btih:89abcdef0123456789abcdef0123456789abcdef"
	oh := metainfo.NewHashFromHex("89abcdef0123456789abcdef0123456789abcdef")
	eng.mu.Lock()
	eng.items[oh] = &item{magnet: other, paused: true, name: "torrent"}
	eng.mu.Unlock()
	if _, _, err := eng.AddDebridForPreview(t.Context(), other); !errors.Is(err, ErrAlreadyTorrent) {
		t.Fatalf("debrid add of a torrent hash: err = %v", err)
	}
	if n := fake.addCount(); n != 1 {
		t.Fatalf("the service got %d adds, want 1", n)
	}
}

func TestDebridRemoteFailuresEndItem(t *testing.T) {
	for _, c := range []struct {
		raw, reason, note string
	}{
		{"dead", "dead torrent", "☁ RD dead torrent"},
		{"virus", "virus detected", "☁ RD virus detected"},
		{"error", "Real-Debrid reported an error", "☁ RD Real-Debrid reported an error"},
	} {
		t.Run(c.raw, func(t *testing.T) {
			fastDebrid(t, 10*time.Millisecond)
			fake := &fakeDebrid{}
			fake.info = func(_ int, selected bool) (debrid.RemoteInfo, error) {
				if !selected {
					return remoteAt(debrid.WaitingSelection, 0, false), nil
				}
				info := remoteAt(debrid.Failed, 0, true)
				info.RawStatus, info.Reason = c.raw, c.reason
				return info, nil
			}
			eng := newDebridTestEngine(t, fake)
			h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
			if err != nil {
				t.Fatal(err)
			}
			if err := eng.StartDebrid(h, nil, DebridOptions{}); err != nil {
				t.Fatal(err)
			}
			awaitCond(t, "the failure", 5*time.Second, func() bool {
				info, _ := eng.DebridItem(h)
				return info.Phase == DebridFailed
			})
			snap, _ := eng.Snapshot(h)
			if snap.State != StatePaused || snap.Note != c.note || snap.Remote == nil || snap.Remote.Status != debrid.Failed {
				t.Fatalf("failed snapshot = state %v note %q remote %+v", snap.State, snap.Note, snap.Remote)
			}
			calls := fake.infoCalls.Load()
			time.Sleep(40 * time.Millisecond)
			if got := fake.infoCalls.Load(); got != calls {
				t.Fatalf("Info polled %d more times after %s", got-calls, c.raw)
			}
		})
	}
}

func TestDebridAccountErrorsEndItem(t *testing.T) {
	for _, kind := range []error{debrid.ErrTooManyActive, debrid.ErrNotPremium, debrid.ErrQuota} {
		t.Run(kind.Error(), func(t *testing.T) {
			fastDebrid(t, 10*time.Millisecond)
			fake := &fakeDebrid{
				info: func(int, bool) (debrid.RemoteInfo, error) {
					return remoteAt(debrid.WaitingSelection, 0, false), nil
				},
				selectErr: &debrid.Error{Provider: "Real-Debrid", Op: "torrents/selectFiles", Kind: kind},
			}
			eng := newDebridTestEngine(t, fake)
			h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
			if err != nil {
				t.Fatal(err)
			}
			awaitCond(t, "the file list", 5*time.Second, func() bool { _, ok := eng.DebridFiles(h); return ok })
			if err := eng.StartDebrid(h, nil, DebridOptions{}); err != nil {
				t.Fatal(err)
			}
			awaitCond(t, "the failure", 5*time.Second, func() bool {
				info, _ := eng.DebridItem(h)
				return info.Phase == DebridFailed
			})
			info, _ := eng.DebridItem(h)
			snap, _ := eng.Snapshot(h)
			if !errors.Is(info.Err, kind) || snap.State != StatePaused || !strings.HasPrefix(snap.Note, "☁ RD ") {
				t.Fatalf("err %v, snapshot state %v note %q", info.Err, snap.State, snap.Note)
			}
		})
	}
}

func TestDebridUnavailableKeepsPolling(t *testing.T) {
	fastDebrid(t, 10*time.Millisecond)
	var fired atomic.Int32
	fake := &fakeDebrid{}
	fake.info = func(n int, selected bool) (debrid.RemoteInfo, error) {
		switch {
		case !selected:
			return remoteAt(debrid.WaitingSelection, 0, false), nil
		case n < 8:
			return debrid.RemoteInfo{}, &debrid.Error{Provider: "Real-Debrid", HTTPStatus: 503, Kind: debrid.ErrUnavailable}
		}
		return remoteAt(debrid.Ready, 100, true), nil
	}
	eng := newDebridTestEngine(t, fake)
	eng.OnDebridReady(func(DebridReadyEvent) { fired.Add(1) })
	h, _, err := eng.AddDebridForPreview(t.Context(), testMagnet)
	if err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "the file list", 5*time.Second, func() bool { _, ok := eng.DebridFiles(h); return ok })
	if err := eng.StartDebrid(h, nil, DebridOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "ready after outages", 5*time.Second, func() bool { return fired.Load() == 1 })
}

func TestDebridAddErrorLeavesNothing(t *testing.T) {
	fake := &fakeDebrid{addErr: &debrid.Error{Provider: "Real-Debrid", Kind: debrid.ErrNotLinked}}
	eng := newDebridTestEngine(t, fake)
	if _, _, err := eng.AddDebridForPreview(t.Context(), testMagnet); !errors.Is(err, debrid.ErrNotLinked) {
		t.Fatalf("err = %v, want ErrNotLinked", err)
	}
	if len(eng.Snapshots()) != 0 {
		t.Fatal("a refused add left a row")
	}
	eng.SetDebridProvider(nil)
	if _, _, err := eng.AddDebridForPreview(t.Context(), testMagnet); !errors.Is(err, ErrNoDebrid) {
		t.Fatalf("err without a provider = %v", err)
	}
}

func TestDebridTorrentFilePreview(t *testing.T) {
	fastDebrid(t, time.Second)
	fake := &fakeDebrid{info: func(int, bool) (debrid.RemoteInfo, error) {
		return remoteAt(debrid.WaitingSelection, 0, false), nil
	}}
	eng := newDebridTestEngine(t, fake)
	infoBytes, err := bencode.Marshal(metainfo.Info{
		Name: "single.iso", PieceLength: 16 << 10, Length: 1000, Pieces: make([]byte, 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	h, name, magnet, owned, err := eng.AddDebridTorrentForPreview(t.Context(), buf.Bytes())
	if err != nil || !owned {
		t.Fatalf("err %v owned %v", err, owned)
	}
	if want := metainfo.HashBytes(infoBytes); h != want || name != "single.iso" || !strings.Contains(magnet, h.HexString()) {
		t.Fatalf("h %s name %q magnet %q", h.HexString(), name, magnet)
	}
	if len(fake.addedData) != 1 || !bytes.Equal(fake.addedData[0], buf.Bytes()) {
		t.Fatal("the service did not get the .torrent bytes")
	}
	if info, _ := eng.DebridItem(h); !bytes.Equal(info.TorrentData, buf.Bytes()) {
		t.Fatal("the item did not keep the .torrent bytes for a re-add")
	}
}

func TestDebridCloseStopsPollers(t *testing.T) {
	fastDebrid(t, 10*time.Millisecond)
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
	fake := &fakeDebrid{info: func(_ int, selected bool) (debrid.RemoteInfo, error) {
		if selected {
			return remoteAt(debrid.Downloading, 10, true), nil // never ready
		}
		return remoteAt(debrid.WaitingSelection, 0, false), nil
	}}
	eng.SetDebridProvider(fake)
	for i := range 4 {
		magnet := fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)
		h, _, err := eng.AddDebridForPreview(t.Context(), magnet)
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.StartDebrid(h, nil, DebridOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(30 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		eng.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not stop the debrid pollers")
	}
	waited := make(chan struct{})
	go func() {
		eng.dwg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("e.dwg still has running goroutines")
	}
	calls := fake.infoCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if got := fake.infoCalls.Load(); got != calls {
		t.Fatalf("Info polled %d times after Close", got-calls)
	}
}

func TestProviderLabels(t *testing.T) {
	if got := providerID("Real-Debrid"); got != "realdebrid" {
		t.Errorf("providerID = %q", got)
	}
	if got := providerShort("Real-Debrid"); got != "RD" {
		t.Errorf("providerShort = %q", got)
	}
	if got := providerShort("Torbox"); got != "Torbox" {
		t.Errorf("providerShort(one word) = %q", got)
	}
}
