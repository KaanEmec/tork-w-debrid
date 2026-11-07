package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
)

// readyRD is a scripted Real-Debrid for the ready check (J4.3): the file
// list is known at once, and the selected files are ready readyAfter after
// the selection. Unlock points at a local file host.
type readyRD struct {
	readyAfter time.Duration
	host       *httptest.Server
	payload    []byte

	mu        sync.Mutex
	selectAt  time.Time
	adds      []string
	selects   int
	infoCalls int
}

func newReadyRD(t *testing.T, readyAfter time.Duration, size int) *readyRD {
	t.Helper()
	r := &readyRD{readyAfter: readyAfter, payload: make([]byte, size)}
	for i := range r.payload {
		r.payload[i] = byte(i * 7)
	}
	r.host = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.ServeContent(w, req, "movie.mkv", time.Time{}, strings.NewReader(string(r.payload)))
	}))
	t.Cleanup(r.host.Close)
	return r
}

func (r *readyRD) Name() string { return "Real-Debrid" }

func (r *readyRD) AddMagnet(_ context.Context, magnet string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adds = append(r.adds, magnet)
	return "RID" + strconv.Itoa(len(r.adds)), nil
}

func (r *readyRD) AddTorrentFile(context.Context, []byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adds = append(r.adds, "torrent")
	return "RID" + strconv.Itoa(len(r.adds)), nil
}

func (r *readyRD) Info(_ context.Context, id string) (debrid.RemoteInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.infoCalls++
	size := int64(len(r.payload))
	info := debrid.RemoteInfo{ID: id, Name: "Movie", Status: debrid.WaitingSelection, Bytes: size,
		Files: []debrid.RemoteFile{{ID: 1, Path: "movie.mkv", Bytes: size, Selected: r.selects > 0}}}
	switch {
	case r.selects == 0:
	case time.Since(r.selectAt) < r.readyAfter:
		info.Status, info.Progress, info.SpeedBps, info.Seeders = debrid.Downloading, 43, 12<<20, 8
	default:
		info.Status, info.Progress, info.Links = debrid.Ready, 100, []string{"rd/0"}
	}
	return info, nil
}

func (r *readyRD) SelectFiles(context.Context, string, []int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selects++
	r.selectAt = time.Now()
	return nil
}

func (r *readyRD) Unlock(_ context.Context, link string) (debrid.Unlocked, error) {
	return debrid.Unlocked{URL: r.host.URL + "/l0/movie.mkv", Filename: "movie.mkv", Bytes: int64(len(r.payload)), Link: link}, nil
}

func (r *readyRD) Delete(context.Context, string) error { return nil }

func (r *readyRD) Account(context.Context) (debrid.Account, error) {
	return debrid.Account{Username: "test", Premium: true}, nil
}

func (r *readyRD) added() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.adds)
}

// openReadyPreview opens a debrid preview of the test magnet from the
// results screen and waits until Real-Debrid's file list is shown.
func openReadyPreview(t *testing.T, a *App, eng *engine.Engine) {
	t.Helper()
	const magnet = "magnet:?xt=urn:btih:abababababababababababababababababababab&dn=Movie"
	open := openDebridPreview(t, a, a.launchCmd(magnet, "Movie", true))
	addDebridPreview(t, a, open)
	waitPreview(t, "Real-Debrid's file list", func() bool { a.preview.refresh(eng); return a.preview.ready })
}

// toastLog ticks the app until done reports true, keeping every toast.
func toastLog(t *testing.T, a *App, what string, done func() bool) []string {
	t.Helper()
	var log []string
	gen := a.toast.gen
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; toasts %q", what, log)
		}
		a.Update(tickMsg(time.Now()))
		if a.toast.gen != gen {
			gen = a.toast.gen
			log = append(log, a.toast.text)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return log
}

// tickFor ticks the app for d and returns the toasts shown meanwhile.
func tickFor(t *testing.T, a *App, d time.Duration) []string {
	t.Helper()
	until := time.Now().Add(d)
	return toastLog(t, a, "the ticks", func() bool { return time.Now().After(until) })
}

func debridRow(a *App) (downloadItem, bool) {
	for _, it := range a.downloads.items {
		if it.Name == "Movie" || it.Name == "movie.mkv" {
			return it, true
		}
	}
	return downloadItem{}, false
}

// Enter in the preview returns to the results at once, with the real
// 5-second window, whether Real-Debrid has the files cached or not. The row
// reads "starting" meanwhile.
func TestDebridEnterReturnsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		readyAfter time.Duration
	}{{"cached", 800 * time.Millisecond}, {"uncached", time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newReadyRD(t, tc.readyAfter, 256<<10)
			a, eng, _ := debridPreviewApp(t, fake, "linked")
			openReadyPreview(t, a, eng)

			began := time.Now()
			_, cmd := a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
			if took := time.Since(began); took > time.Second {
				t.Fatalf("enter took %v", took)
			}
			if a.screen != screenResults || cmd == nil {
				t.Fatalf("after enter: screen %v, cmd %v; want the results at once", a.screen, cmd)
			}
			if a.toast.text != queuedToast("Movie") {
				t.Fatalf("toast %q, want the queued toast", a.toast.text)
			}
			if len(a.st.Entries) != 1 || a.st.Entries[0].Via != "realdebrid" {
				t.Fatalf("state entries %+v", a.st.Entries)
			}
			a.screen = screenDownloads
			view := ansi.Strip(a.View())
			if !strings.Contains(view, "starting") || strings.Contains(view, "Real-Debrid 43") {
				t.Fatalf("row inside the window:\n%s", view)
			}
		})
	}
}

// Cached path through the TUI: the row goes from starting to downloading
// without ever showing the cloud, and no "not cached" toast appears.
func TestDebridCachedNoCloudNoToast(t *testing.T) {
	defer engine.SetDebridTimings(500*time.Millisecond, 100*time.Millisecond)()
	fake := newReadyRD(t, 80*time.Millisecond, 4<<20)
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	openReadyPreview(t, a, eng)
	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})

	var states []engine.TorrentState
	toasts := toastLog(t, a, "local bytes", func() bool {
		it, ok := debridRow(a)
		if ok && (len(states) == 0 || states[len(states)-1] != it.State) {
			states = append(states, it.State)
		}
		return ok && (it.State == engine.StateDone || (it.State == engine.StateDownloading && it.BytesCompleted > 0))
	})
	if slices.Contains(states, engine.StateRemote) {
		t.Fatalf("a cached torrent showed the cloud: %v", states)
	}
	if len(toasts) != 0 {
		t.Fatalf("toasts %q for a cached torrent", toasts)
	}
	// and none later either
	if more := tickFor(t, a, 300*time.Millisecond); len(more) != 0 {
		t.Fatalf("late toasts %q", more)
	}
}

// Uncached path through the TUI: after the window the row shows
// Real-Debrid's progress with the "not cached" toast, then the local phase
// starts by itself with the "ready" toast. Each toast shows once.
func TestDebridUncachedToastsOnce(t *testing.T) {
	defer engine.SetDebridTimings(100*time.Millisecond, 20*time.Millisecond)()
	fake := newReadyRD(t, 600*time.Millisecond, 256<<10)
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	openReadyPreview(t, a, eng)
	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})

	sawCloud := false
	toasts := toastLog(t, a, "the download to finish", func() bool {
		it, ok := debridRow(a)
		if ok && it.State == engine.StateRemote {
			sawCloud = true
		}
		return ok && it.State == engine.StateDone
	})
	toasts = append(toasts, tickFor(t, a, 200*time.Millisecond)...)
	if !sawCloud {
		t.Fatal("the row never showed Real-Debrid's progress")
	}
	want := []string{notCachedToast, debridReadyToast("Movie", false)}
	if len(toasts) == 2 && toasts[1] == debridReadyToast("Movie", true) {
		want[1] = toasts[1] // the file came down between two ticks
	}
	if !slices.Equal(toasts, want) {
		t.Fatalf("toasts %q, want %q", toasts, want)
	}
	if a.toast.tone != toastRoute {
		t.Fatalf("toast tone %v, want the Real-Debrid lavender", a.toast.tone)
	}
}

// With no terminal attached the notice waits, and the next attached tick
// shows it.
func TestDebridNoticeWaitsWhileDetached(t *testing.T) {
	defer engine.SetDebridTimings(100*time.Millisecond, 20*time.Millisecond)()
	fake := newReadyRD(t, time.Hour, 64<<10)
	a, eng, _ := debridPreviewApp(t, fake, "linked")
	openReadyPreview(t, a, eng)
	a.updatePreview(tea.KeyMsg{Type: tea.KeyEnter})
	a.DetachSession()
	toasts := toastLog(t, a, "the cloud row", func() bool {
		it, ok := debridRow(a)
		return ok && it.State == engine.StateRemote
	})
	if len(toasts) != 0 {
		t.Fatalf("toasts %q while detached", toasts)
	}
	a.motion.detached = false // what AttachSession does
	toasts = toastLog(t, a, "one tick", func() bool { return a.toast.text == notCachedToast })
	if !slices.Equal(toasts, []string{notCachedToast}) {
		t.Fatalf("toasts after attach %q", toasts)
	}
}
