package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
)

// Starting a debrid download and the ready check (roadmap J4.3). The engine
// gives Real-Debrid 5 seconds after the file selection: a cached torrent
// goes straight to the HTTPS download and its row looks like any download
// ("starting", then ↓); otherwise the row shows Real-Debrid's progress and
// the local download starts by itself once Real-Debrid is done. Nothing here
// waits on the network: enter returns at once and the tick follows the rows.

// The ready check's two toasts, in lavender with a leading ☁.
const (
	notCachedToast = "Not cached on Real-Debrid yet, downloading there first. tork will fetch it when it's ready."
	debridToastFor = 5 * time.Second
)

// debridReadyToast is the second toast; done is for a file small enough to
// have come down between two ticks.
func debridReadyToast(name string, done bool) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "?" {
		name = "The download"
	}
	if done {
		return truncate(name, 44) + " was ready on Real-Debrid and is downloaded"
	}
	return truncate(name, 44) + " is ready on Real-Debrid, downloading now"
}

// recordDebridStart saves a debrid download that just started (StartDebrid
// went through) in state.json and confirms it with the usual "queued"
// toast. The entry's Via, RemoteID, RemoteFiles and Phase come from the
// engine, with seeding off (D6).
func (a *App) recordDebridStart(h metainfo.Hash, name string) tea.Cmd {
	var save tea.Cmd
	if entry, ok := a.eng.DebridEntry(h); ok && a.st != nil {
		a.st.Upsert(entry)
		save = a.saveState()
		if name == "" {
			name = entry.Name
		}
	}
	a.downloads.snaps = a.eng.Snapshots()
	a.refreshDownloadItems()
	return tea.Batch(save, a.ensureTick(), a.showToast(queuedToast(name), toastOK, toastQuick))
}

// debridNotices turns the ready check's changes into toasts: the window
// ended before Real-Debrid had the files (the row switched to the cloud),
// or a download that waited in the cloud is now coming down. They come from
// the rows the tick just read, not from engine callbacks, so the background
// session needs no terminal. While no terminal is attached nothing is
// taken: the engine keeps the notice until someone can see it, and gives
// each one once.
func (a *App) debridNotices() tea.Cmd {
	if a.eng == nil || a.motion.detached {
		return nil
	}
	var cmd tea.Cmd
	for _, s := range a.downloads.snaps {
		if s.Via == "" {
			continue
		}
		switch s.State {
		case engine.StateRemote, engine.StateFetchingMeta, engine.StateDownloading, engine.StateDone:
		default:
			continue
		}
		switch a.eng.TakeDebridNotice(s.Hash) {
		case engine.DebridNoticeNotCached:
			cmd = a.showToast(notCachedToast, toastRoute, debridToastFor)
		case engine.DebridNoticeReady:
			cmd = a.showToast(debridReadyToast(s.Name, s.State == engine.StateDone), toastRoute, debridToastFor)
		}
	}
	return cmd
}

// Other ways to start a download (roadmap J4.5). `D` in the results ("download
// now, skipping the preview") and `preview_before_download: false` both end
// in launchCmd with preview off. With Real-Debrid on, they go through it
// with every file chosen, and the same ready check follows. Pasted magnets,
// infohashes, .torrent files and URLs, on the home screen or the command
// line, already open the debrid preview. The ISO shelf (HTTPS already) and
// autopilot (torrent-only in v0.2.0) are left as they are on purpose.

// debridStartedMsg reports a download started without the preview.
type debridStartedMsg struct {
	hash metainfo.Hash
	name string
	err  error
}

// launchDebridDownload starts magnet through Real-Debrid with every file,
// or reports false when downloads don't go through Real-Debrid. It never
// touches the swarm: when the account can't be used right now it says so
// instead of quietly falling back to a torrent.
func (a *App) launchDebridDownload(magnet, name string) (tea.Cmd, bool) {
	on, problem := a.debridPreviewRoute()
	if !on {
		return nil, false
	}
	if problem != "" {
		return a.showError(problem), true
	}
	if m, err := metainfo.ParseMagnetUri(magnet); err == nil {
		if e := a.listedEntry(m.InfoHash); e != nil {
			return a.showListedDownload(e), true
		}
	}
	a.ensureDebridProvider()
	a.pendingAdds++
	eng := a.eng
	dir := a.cfg.DownloadDir
	return func() (msg tea.Msg) {
		defer guard(&msg, func(r any) tea.Msg {
			return debridStartedMsg{name: name, err: fmt.Errorf("add panicked: %v", r)}
		})
		ctx, cancel := context.WithTimeout(context.Background(), debridAddTimeout)
		defer cancel()
		h, owned, err := addDebridMagnet(ctx, eng, magnet)
		if err != nil {
			return debridStartedMsg{hash: h, name: name, err: err}
		}
		if err := eng.StartDebrid(h, nil, engine.DebridOptions{DownloadDir: dir}); err != nil {
			if owned {
				eng.CancelDebridPreview(h)
			}
			return debridStartedMsg{hash: h, name: name, err: err}
		}
		return debridStartedMsg{hash: h, name: name}
	}, true
}

// addDebridMagnet sends a magnet to Real-Debrid, as the cached .torrent
// when the metadata cache has it (the service then knows the files at once).
func addDebridMagnet(ctx context.Context, eng *engine.Engine, magnet string) (metainfo.Hash, bool, error) {
	mag, err := metainfo.ParseMagnetUri(magnet)
	if err != nil {
		return metainfo.Hash{}, false, err
	}
	data, ok := eng.CachedTorrent(mag.InfoHash)
	if !ok {
		return eng.AddDebridForPreview(ctx, magnet)
	}
	if h, _, _, err := torrentFileList(data); err != nil || h != mag.InfoHash {
		return eng.AddDebridForPreview(ctx, magnet) // a bad cache entry
	}
	h, _, _, owned, err := eng.AddDebridTorrentForPreview(ctx, data)
	return h, owned, err
}

func (a *App) onDebridStarted(msg debridStartedMsg) tea.Cmd {
	if a.pendingAdds > 0 {
		a.pendingAdds--
	}
	if msg.err != nil {
		return a.showError("add failed: " + debridProblem(msg.err))
	}
	return a.recordDebridStart(msg.hash, msg.name)
}
