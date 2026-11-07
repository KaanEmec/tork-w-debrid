package tui

import (
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
