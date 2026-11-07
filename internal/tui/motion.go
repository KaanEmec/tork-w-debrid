package tui

import (
	"math"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

// Motion (E8, design.md § Motion). One frame clock drives every animation, and
// it only runs while something on the current screen is actually moving.
//
// Every primitive below is a pure function of (frame, width) that returns
// styled text, so a test can render any frame exactly:
//
//   - spinnerGlyph    braille spinner, one step per frame (any wait)
//   - copperRule      a bright segment sweeps along the header rule, 1.6 s a
//     pass (Results, only while a search is streaming)
//   - shineRail       a lighter band runs along the filled part of the active
//     progress bar, 3 s a pass (Downloads)
//   - logoCycleStyle  colour cycle across the Home logo's columns, 8 s a loop
//   - cursorVisible   block cursor blink, 0.5 s on and 0.5 s off (Home)
//   - catEyes         the cat blinks: -.- for 0.2 s every 5 s (Home)
//   - fadeStyle       faint → muted → normal over 3 frames (new result rows,
//     toasts)
//
// Rule: at most ONE moving effect per screen, plus a spinner. Motion moves in
// whole cells, never smoothly. With motion off every primitive draws frame 0,
// the still poster frame.

const (
	appFPS     = 10 // the app's frame rate
	introFPS   = 30 // the intro's rate (J8.8); never go above it
	fadeFrames = 3
)

// frameMsg advances the animation clock by one frame.
type frameMsg struct{}

func frameCmd(fps int) tea.Cmd {
	fps = max(1, min(introFPS, fps))
	return tea.Tick(time.Second/time.Duration(fps), func(time.Time) tea.Msg { return frameMsg{} })
}

// motionState decides whether anything may move, and holds the frame clock.
type motionState struct {
	noMotion   bool // this run: --no-motion, NO_COLOR, TERM=dumb or stdout not a terminal
	animations bool // Settings › Look › animations (ui.motion in config.yaml)
	frame      int  // frames since start; only advances while the clock runs
	running    bool // a frameMsg is in flight
	detached   bool // background session with no terminal attached
}

func newMotionState() motionState {
	return motionState{noMotion: !MotionAllowed(), animations: true}
}

// MotionAllowed reports whether this process's terminal allows motion: not
// when NO_COLOR is set, TERM is dumb, or stdout is not a terminal. A
// background session's terminal belongs to the client, which sends the answer
// along when it attaches.
func MotionAllowed() bool {
	return motionAllowedEnv(os.Getenv, term.IsTerminal(os.Stdout.Fd()))
}

func motionAllowedEnv(getenv func(string) string, stdoutTTY bool) bool {
	if getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(getenv("TERM")), "dumb") {
		return false
	}
	return stdoutTTY
}

// SetNoMotion turns motion off (or back on) for this run: the --no-motion
// flag, or the attaching terminal's environment.
func (a *App) SetNoMotion(off bool) { a.motion.noMotion = off }

// SetAnimations applies the Settings › Look › animations switch (ui.motion).
func (a *App) SetAnimations(on bool) { a.motion.animations = on }

// MotionEnabled reports whether animations currently run.
func (a *App) MotionEnabled() bool { return a.motionOn() }

func (a *App) motionOn() bool { return a.motion.animations && !a.motion.noMotion }

// vframe is the frame every primitive draws: the live frame, or the still
// frame 0 when motion is off.
func (a *App) vframe() int {
	if !a.motionOn() {
		return 0
	}
	return a.motion.frame
}

// age is how many frames ago something born at frame born appeared. With
// motion off everything is fully grown.
func (a *App) age(born int) int {
	if !a.motionOn() {
		return math.MaxInt32
	}
	return a.motion.frame - born
}

// animating reports whether the visible screen has something moving. The
// clock stops as soon as this turns false; nothing moves when nothing is
// happening. Later screens (the Real-Debrid link dialog, the intro) add
// their own case here.
func (a *App) animating() bool {
	if !a.motionOn() || a.motion.detached {
		return false
	}
	if a.toast.text != "" && a.age(a.toast.born) < fadeFrames {
		return true
	}
	if a.showHelp {
		return false
	}
	switch a.screen {
	case screenSearch:
		return true // logo colour cycle, cursor and cat blink
	case screenResults:
		r := &a.results
		return r.searching || (r.lastBorn >= 0 && a.age(r.lastBorn) < fadeFrames)
	case screenPreview:
		return !a.preview.ready
	case screenDownloads:
		return a.downloadsAnimating()
	}
	return false
}

func (a *App) frameRate() int { return appFPS }

// ensureFrame arms the clock when something starts moving. Exactly one
// frameMsg is ever in flight.
func (a *App) ensureFrame() tea.Cmd {
	if a.motion.running || !a.animating() {
		return nil
	}
	a.motion.running = true
	return frameCmd(a.frameRate())
}

// onFrame advances the clock; Update re-arms it through ensureFrame while the
// screen still animates.
func (a *App) onFrame() {
	a.motion.running = false
	a.motion.frame++
}

// --- primitives -----------------------------------------------------------

// spinnerGlyph is the wait spinner in accent.
func spinnerGlyph(frame int) string {
	s := glyphs.spinner
	return styleOK.Render(s[posMod(frame, len(s))])
}

// spinnerGlyphIn renders the spinner in another style (lavender for
// Real-Debrid waits).
func spinnerGlyphIn(frame int, st lipgloss.Style) string {
	s := glyphs.spinner
	return st.Render(s[posMod(frame, len(s))])
}

const sweepFrames = 16 // 1.6 s per pass at 10 fps

// copperRule is the header rule with the bright sweep. Frame 0 puts the
// segment just off the left edge, so the still frame is a plain rule.
func copperRule(frame, width int) string {
	if width < 1 {
		return ""
	}
	seg := max(2, width/4)
	start := posMod(frame, sweepFrames)*(width+seg)/sweepFrames - seg
	if start+seg <= 0 || start >= width {
		return rule(width)
	}
	var b strings.Builder
	lo, hi := max(0, start), min(width, start+seg)
	if lo > 0 {
		b.WriteString(styleRule.Render(strings.Repeat("─", lo)))
	}
	edge := seg / 4
	for x := lo; x < hi; x++ {
		st := styleOK
		if x-start < edge || start+seg-1-x < edge {
			st = styleAccentDim
		}
		b.WriteString(st.Render("─"))
	}
	if hi < width {
		b.WriteString(styleRule.Render(strings.Repeat("─", width-hi)))
	}
	return b.String()
}

const shineFrames = 30 // 3 s per pass at 10 fps

// shineRail is a progress rail whose filled part carries a lighter band that
// runs left to right. Frame 0 keeps the band off the rail.
func shineRail(progress float64, width, frame int, fill lipgloss.Style) string {
	width = max(1, width)
	filled := int(clampProgress(progress)*float64(width) + 0.5)
	band := 3
	start := posMod(frame, shineFrames)*(filled+band)/shineFrames - band
	var b strings.Builder
	if filled > 0 {
		lo, hi := max(0, start), min(filled, start+band)
		if hi <= lo {
			b.WriteString(fill.Render(strings.Repeat(glyphs.filled, filled)))
		} else {
			if lo > 0 {
				b.WriteString(fill.Render(strings.Repeat(glyphs.filled, lo)))
			}
			b.WriteString(styleShine.Render(strings.Repeat(glyphs.filled, hi-lo)))
			if hi < filled {
				b.WriteString(fill.Render(strings.Repeat(glyphs.filled, filled-hi)))
			}
		}
	}
	if width > filled {
		b.WriteString(styleFaint.Render(strings.Repeat(glyphs.empty, width-filled)))
	}
	return b.String()
}

const logoCycleFrames = 80 // 8 s per loop at 10 fps
const logoSteps = 8

// logoCycleStyle colours column x of a word span cells wide: the colour runs
// base → sky → base across twice the span and slides one loop every 8 s.
func logoCycleStyle(ramp []lipgloss.Style, frame, x, span int) lipgloss.Style {
	span = max(1, span)
	t := float64(x)/float64(2*span) + float64(posMod(frame, logoCycleFrames))/logoCycleFrames
	t -= math.Floor(t)
	tri := 1 - math.Abs(2*t-1)
	return ramp[int(math.Round(tri*float64(len(ramp)-1)))]
}

// colourRamp is steps bold styles blending from to into.
func colourRamp(from, to lipgloss.CompleteAdaptiveColor, steps int) []lipgloss.Style {
	out := make([]lipgloss.Style, steps)
	for i := range out {
		c := from
		if i > 0 {
			c = mixColor(from, to, float64(i)/float64(steps-1))
		}
		out[i] = lipgloss.NewStyle().Bold(true).Foreground(c)
	}
	return out
}

var (
	rampTork   = colourRamp(colAccent, colSky, logoSteps)
	rampDebrid = colourRamp(colRoute, colSky, logoSteps)

	styleShine     = lipgloss.NewStyle().Foreground(colAccentShine)
	styleAccentDim = lipgloss.NewStyle().Foreground(colAccentDim)
)

// cursorVisible blinks the Home cursor: on for 0.5 s, off for 0.5 s.
func cursorVisible(frame int) bool { return posMod(frame, appFPS) < appFPS/2 }

const catBlinkFrames = 50 // 5 s

// catEyes closes the cat's eyes for 0.2 s of every 5 s.
func catEyes(frame int) string {
	if posMod(frame, catBlinkFrames) >= catBlinkFrames-2 {
		return "-.-"
	}
	return "^.^"
}

// fadeStyle fades something in over three frames: faint, muted, then its own
// style.
func fadeStyle(age int, final lipgloss.Style) lipgloss.Style {
	switch {
	case age <= 0:
		return styleFaint
	case age == 1:
		return styleDim
	default:
		return final
	}
}

func posMod(a, n int) int {
	if n <= 0 {
		return 0
	}
	return ((a % n) + n) % n
}
