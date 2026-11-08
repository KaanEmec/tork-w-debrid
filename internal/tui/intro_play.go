package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// When the intro plays (J8.8):
//
//   - once after each new version: state.json's intro_seen_version differs
//     from the running version (SetVersion), and Settings › Look › intro after
//     each update is on (SetIntroAfterUpdate). The version is written when the
//     intro ends.
//   - on replay: Settings › Look › play the intro sends replayIntroMsg, and
//     `torkwd intro` runs NewIntro on its own.
//
// It never plays by itself when motion is off (NO_COLOR, TERM=dumb, stdout not
// a terminal, --no-motion, animations off), on ASCII terminals, or when the
// terminal is too small. A replay with motion off shows the still poster frame
// until a key. Any key skips it; it ends by itself after about 8 s. It is an
// overlay: the screen underneath keeps its state, so the user returns to
// where they were.

// replayIntroMsg plays the intro now (Settings › Look › play the intro).
type replayIntroMsg struct{}

type introState struct {
	active  bool
	poster  bool // motion off: the still frame until a key
	frame   int
	version string // the running version: the version line and the once-per-update check
	off     bool   // ui.intro is off: no intro after updates (replay still works)
}

// SetVersion tells the UI which version is running.
func (a *App) SetVersion(v string) { a.intro.version = v }

// SetIntroAfterUpdate applies Settings › Look › intro after each update
// (ui.intro, default on).
func (a *App) SetIntroAfterUpdate(on bool) { a.intro.off = !on }

// IntroPlaying reports whether the intro is on screen.
func (a *App) IntroPlaying() bool { return a.intro.active }

// introDue reports whether this start should open with the intro.
func (a *App) introDue() bool {
	in := a.intro
	switch {
	case in.active || in.off || in.version == "" || a.st == nil:
		return false
	case a.st.IntroSeenVersion == in.version:
		return false
	case !a.motionOn() || a.motion.detached || glyphs.ascii:
		return false
	case a.width > 0 && !introFits(a.width, a.height):
		return false
	}
	return true
}

// maybeStartIntro starts the once-per-update intro when it is due.
func (a *App) maybeStartIntro() tea.Cmd {
	if !a.introDue() {
		return nil
	}
	return a.startIntro()
}

func (a *App) startIntro() tea.Cmd {
	a.intro.active = true
	a.intro.frame = 0
	a.intro.poster = !a.motionOn() || a.motion.detached
	return tea.HideCursor
}

// replayIntro is Settings › Look › play the intro: it plays even when the
// intro after updates is off, and shows the poster frame when motion is off.
func (a *App) replayIntro() tea.Cmd {
	if glyphs.ascii {
		return a.showError("the intro needs a terminal with block characters")
	}
	if a.intro.active {
		return nil
	}
	return a.startIntro()
}

// endIntro closes the intro and remembers that this version has been seen.
func (a *App) endIntro() tea.Cmd {
	a.intro.active = false
	v := a.intro.version
	if v == "" || a.st == nil || a.st.IntroSeenVersion == v {
		return nil
	}
	a.st.IntroSeenVersion = v
	if a.cfg == nil {
		return nil
	}
	return a.saveState()
}

// updateIntro handles the messages the intro owns while it plays. Everything
// else (download ticks, search results, resizes) reaches the app as usual.
func (a *App) updateIntro(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case frameMsg:
		a.onFrame()
		if a.intro.poster {
			return true, nil
		}
		if a.width > 0 && !introFits(a.width, a.height) {
			a.intro.active = false // too small to show; try again next start
			return true, nil
		}
		a.intro.frame++
		if a.intro.frame >= introFrames {
			return true, a.endIntro()
		}
		return true, nil
	case tea.KeyMsg:
		cmd := a.endIntro()
		if msg.String() == "ctrl+c" {
			return false, cmd // ctrl+c still quits
		}
		return true, cmd
	case tea.MouseMsg, replayIntroMsg:
		return true, nil
	}
	return false, nil
}

func (a *App) viewIntro() string {
	n := a.intro.frame
	if a.intro.poster {
		n = 0
	}
	return introFrame(n, a.width, a.height, currentIntroTheme(), a.intro.version)
}

// IntroModel plays the intro on its own (`torkwd intro`) and quits on any key
// or when it ends. With motion off it shows the poster frame until a key.
type IntroModel struct {
	version       string
	motion        bool
	frame         int
	width, height int
}

// NewIntro returns the standalone intro for version. motion false shows the
// still poster frame.
func NewIntro(version string, motion bool) *IntroModel {
	return &IntroModel{version: version, motion: motion}
}

func (m *IntroModel) Init() tea.Cmd {
	if !m.motion {
		return tea.HideCursor
	}
	return tea.Batch(tea.HideCursor, frameCmd(introFPS))
}

func (m *IntroModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m, tea.Quit
	case frameMsg:
		if !m.motion {
			return m, nil
		}
		m.frame++
		if m.frame >= introFrames {
			return m, tea.Quit
		}
		return m, frameCmd(introFPS)
	}
	return m, nil
}

func (m *IntroModel) View() string {
	n := m.frame
	if !m.motion {
		n = 0
	}
	return introFrame(n, m.width, m.height, currentIntroTheme(), m.version)
}
