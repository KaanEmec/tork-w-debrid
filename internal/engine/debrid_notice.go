package engine

import (
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// The ready check's notices (roadmap J4.3). After the file selection a
// debrid item gets debridReadyWindow to have its files ready. Inside the
// window the row reads "starting" like any download; when the window ends
// first the row switches to the cloud's progress, and the UI says so once.
// When that cloud download is later ready, the UI says so once more.
//
// The UI finds these by polling TakeDebridNotice from its tick for the
// debrid rows it shows, never through a callback, so the background
// session keeps working with no terminal attached. The "already told" flags
// live in the item, so a notice is given once even when the UI attaches
// after the change happened.

// NoteDebridStarting is Snapshot.Note while a debrid item waits inside its
// ready window, or for its first local bytes after it.
const NoteDebridStarting = "starting"

// NoteDebridUnreachable is Snapshot.Note while the poller cannot reach the
// service (a network error or a busy answer) and keeps retrying: the cloud
// progress on the row is the last one seen.
func NoteDebridUnreachable(short string) string {
	return "☁ can't reach " + short + " · retrying"
}

// DebridStarting reports whether a snapshot is a debrid item in its ready
// window (or about to receive its first local bytes): the row reads
// "starting", with a spinner and an empty rail.
func (s Snapshot) DebridStarting() bool {
	return s.Via != "" && s.State == StateFetchingMeta && s.Note == NoteDebridStarting
}

// DebridNotice is a one-time message about a debrid item.
type DebridNotice int

const (
	// DebridNoticeNone: nothing new to say.
	DebridNoticeNone DebridNotice = iota
	// DebridNoticeNotCached: the ready window ended before the service had
	// the files, so it downloads them first; the row shows its progress.
	DebridNoticeNotCached
	// DebridNoticeReady: a download that waited in the cloud is ready, and
	// the local phase has started (or, for a small file, already finished).
	DebridNoticeReady
)

// TakeDebridNotice returns the item's notice that has not been given yet
// and marks it given, so each notice is returned at most once per item.
// When both are due (the UI was away while the item went through the cloud),
// only the ready notice is returned (the item may even be done by then).
func (e *Engine) TakeDebridNotice(h metainfo.Hash) DebridNotice {
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.debrid[h]
	if !ok || it.paused {
		return DebridNoticeNone
	}
	switch it.phase {
	case DebridLocal, DebridDone: // not DebridReady: the row still shows the cloud then
		if it.cloudFirst && !it.toldReady {
			it.toldReady, it.toldNotCached = true, true
			return DebridNoticeReady
		}
	case DebridCloud:
		if !it.toldNotCached && it.remoteVisible(time.Now()) {
			it.toldNotCached = true
			return DebridNoticeNotCached
		}
	}
	return DebridNoticeNone
}

// SetDebridTimings changes the ready window and the debrid poll intervals
// (zero keeps a value) and returns a function that puts them back. It is
// for tests in other packages, which must not wait 5 seconds; call it
// before creating the engine.
func SetDebridTimings(window, poll time.Duration) (restore func()) {
	oldTuning, oldWindow := debridTuning, debridReadyWindow
	if window > 0 {
		debridReadyWindow = window
	}
	if poll > 0 {
		debridTuning.fastPoll = poll
		debridTuning.downloadPoll = poll
		debridTuning.queuedPoll = poll
		debridTuning.retryBase = poll
		debridTuning.retryMax = 4 * poll
	}
	return func() { debridTuning, debridReadyWindow = oldTuning, oldWindow }
}
