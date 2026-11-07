package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// How long a toast sits on screen. A confirmation of something the user just
// asked for needs very little dwell; a verification result is worth reading.
const (
	toastQuick = 1500 * time.Millisecond
	toastLong  = 3 * time.Second
)

// toastTone picks the colour: accent for "that worked", amber for a result
// that deserves a second look, lavender for Real-Debrid news. Errors are not
// toasts - they take over the footer, where they stay put long enough to act on.
type toastTone int

const (
	toastOK toastTone = iota
	toastWarn
	toastRoute // Real-Debrid messages (J4.3), drawn in lavender
)

// toastState is the app's single transient-confirmation slot. Yanks, finished
// verifications, and queued downloads all pass through it, so only one box is
// ever on screen and the newest message always wins.
type toastState struct {
	text string
	tone toastTone
	gen  int // invalidates stale clear timers when toasts overlap
	born int // frame the toast appeared on, for its fade-in
}

// showToast replaces whatever is on screen and returns the timer that clears
// it. The generation bump is what stops an older toast's timer from cutting a
// newer one short.
func (a *App) showToast(text string, tone toastTone, dwell time.Duration) tea.Cmd {
	a.toast.text = text
	a.toast.tone = tone
	a.toast.gen++
	a.toast.born = a.motion.frame
	return clearToastCmd(a.toast.gen, dwell)
}

// toastLine is the one-line toast drawn just above the footer, fading in over
// three frames. It replaces the old bottom-right box, so a toast never covers
// content: the body gives up one line while it shows.
func (a *App) toastLine(width int) string {
	t := a.toast
	st, mark := styleOK, "✓ "
	switch t.tone {
	case toastWarn:
		st, mark = styleWarn, "! "
	case toastRoute:
		st, mark = styleRoute, glyphs.cloud+" "
	}
	text := t.text
	if !strings.HasPrefix(text, mark) {
		text = mark + text
	}
	return truncate(fadeStyle(a.age(t.born), st).Render(text), width)
}

// queuedToast names what just started, so a download that no longer yanks the
// user to the downloads screen still says plainly that it took.
func queuedToast(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "?" {
		return "queued"
	}
	return "queued · " + truncate(name, 44)
}
