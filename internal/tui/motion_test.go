package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/kaanemec/tork-w-debrid/internal/engine"
)

// Motion tests for J8.3: the frame clock only runs while something moves,
// motion off draws the same frame forever, and every primitive has golden
// frames. BenchmarkFrame120x36 holds the 2 ms frame budget.

func withMotion(a *App) *App {
	a.motion = motionState{animations: true}
	return a
}

// idleDownloadsApp has nothing in flight: one paused and one finished item.
func idleDownloadsApp() *App {
	a := withMotion(&App{width: 120, height: 36, screen: screenDownloads, downloads: newDownloadsModel()})
	a.downloads.items = []downloadItem{
		{Name: "Big Buck Bunny (2008) 1080p", State: engine.StatePaused, Length: 100, BytesCompleted: 31, Note: "paused"},
		{Name: "Cosmos Laundromat (2015) 1080p", State: engine.StateDone, Length: 100, BytesCompleted: 100},
	}
	a.downloads.metrics = summarizeDownloads(a.downloads.items)
	a.downloads.itemsReady = true
	return a
}

func TestIdleScreenSchedulesNoFrame(t *testing.T) {
	idle := map[string]*App{
		"downloads paused/done": idleDownloadsApp(),
		"results finished":      pastFade(withMotion(resultsGoldenApp(false))),
		"preview ready":         withMotion(previewGoldenApp()),
	}
	for name, a := range idle {
		if a.animating() {
			t.Errorf("%s: animating() = true on an idle screen", name)
		}
		_, cmd := a.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		if cmd != nil || a.motion.running {
			t.Errorf("%s: an idle screen armed the frame clock", name)
		}
	}
}

func TestBusyScreenRunsOneClock(t *testing.T) {
	a := withMotion(resultsGoldenApp(true))
	_, cmd := a.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	if cmd == nil || !a.motion.running {
		t.Fatal("a streaming search did not start the frame clock")
	}
	// A second message while a frame is in flight never schedules another.
	if _, cmd := a.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); cmd != nil {
		t.Fatal("a second frame tick was scheduled while one was in flight")
	}
	if _, ok := cmd().(frameMsg); !ok {
		t.Fatal("the clock's command did not deliver a frameMsg")
	}
	_, next := a.Update(frameMsg{})
	if a.motion.frame != 1 || next == nil || !a.motion.running {
		t.Fatalf("frame %d, re-armed %v: the clock must advance and keep running", a.motion.frame, next != nil)
	}
	// The search finishes and the last rows have faded in: the frame in
	// flight lands and the clock stops.
	a.results.searching = false
	a.motion.frame = 10
	if _, cmd := a.Update(frameMsg{}); cmd != nil || a.motion.running {
		t.Fatal("the clock kept running after the search finished")
	}
}

func TestMotionOffSchedulesNoFrame(t *testing.T) {
	for name, off := range map[string]func(*App){
		"--no-motion":       func(a *App) { a.SetNoMotion(true) },
		"animations off":    func(a *App) { a.SetAnimations(false) },
		"no terminal (bg)":  func(a *App) { a.motion.detached = true },
		"motion on, sanity": func(*App) {},
	} {
		a := withMotion(resultsGoldenApp(true))
		off(a)
		_, cmd := a.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		want := name == "motion on, sanity"
		if got := cmd != nil; got != want {
			t.Errorf("%s: frame scheduled = %v, want %v", name, got, want)
		}
	}
}

func TestDetachStopsAndAttachRestartsClock(t *testing.T) {
	a := withMotion(resultsGoldenApp(true))
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	if !a.motion.running {
		t.Fatal("clock did not start")
	}
	a.DetachSession()
	if _, cmd := a.Update(frameMsg{}); cmd != nil || a.motion.running {
		t.Fatal("the clock kept ticking with no terminal attached")
	}
	if _, err := a.AttachSession(nil); err != nil {
		t.Fatal(err)
	}
	if _, cmd := a.Update(tea.WindowSizeMsg{Width: 120, Height: 36}); cmd == nil || !a.motion.running {
		t.Fatal("reattaching did not restart the clock")
	}
}

func TestMotionAllowedEnv(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	for _, tt := range []struct {
		name string
		get  func(string) string
		tty  bool
		want bool
	}{
		{"terminal", env("TERM", "xterm-256color"), true, true},
		{"NO_COLOR", env("NO_COLOR", "1", "TERM", "xterm"), true, false},
		{"TERM=dumb", env("TERM", "dumb"), true, false},
		{"TERM=DUMB", env("TERM", " DUMB "), true, false},
		{"not a terminal", env("TERM", "xterm"), false, false},
	} {
		if got := motionAllowedEnv(tt.get, tt.tty); got != tt.want {
			t.Errorf("%s: motionAllowedEnv = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// motionScreens is one app per screen that has an animation.
func motionScreens() map[string]*App {
	toast := downloadsGoldenApp(120, 36)
	toast.showToast("queued · Sintel", toastOK, toastQuick)
	return map[string]*App{
		"home":              homeGoldenApp(120, 36),
		"results searching": resultsGoldenApp(true),
		"downloads":         downloadsGoldenApp(120, 36),
		"toast":             toast,
	}
}

func TestMotionOffFramesAreIdentical(t *testing.T) {
	withProfile(t, termenv.TrueColor, true)
	for name, a := range motionScreens() {
		withMotion(a)
		a.SetNoMotion(true)
		a.motion.frame = 0
		still := a.View()
		for _, f := range []int{1, 3, 7, 16, 23, 49, 51, 80} {
			a.motion.frame = f
			if got := a.View(); got != still {
				t.Errorf("%s: motion off, frame %d differs from frame 0", name, f)
				break
			}
		}
		// Sanity: with motion on the same screen does move.
		a.SetNoMotion(false)
		moved := false
		for _, f := range []int{1, 3, 7, 16, 23, 49} {
			a.motion.frame = f
			if a.View() != still {
				moved = true
				break
			}
		}
		if !moved && name != "toast" {
			t.Errorf("%s: no frame differs with motion on, so the test proves nothing", name)
		}
	}
}

// --- golden frames ----------------------------------------------------------

// styledCell is one printed rune with its truecolor foreground and boldness.
type styledCell struct {
	r    rune
	fg   string // "#RRGGBB" or "" for the terminal default
	bold bool
}

// parseStyled splits truecolor SGR output into cells.
func parseStyled(t *testing.T, s string) []styledCell {
	t.Helper()
	var out []styledCell
	fg, bold := "", false
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			end := strings.IndexByte(s[i:], 'm')
			if end < 0 {
				t.Fatalf("unterminated escape in %q", s)
			}
			params := strings.Split(s[i+2:i+end], ";")
			for p := 0; p < len(params); p++ {
				switch params[p] {
				case "", "0":
					fg, bold = "", false
				case "1":
					bold = true
				case "22":
					bold = false
				case "39":
					fg = ""
				case "38":
					if p+4 < len(params) && params[p+1] == "2" {
						var rgb [3]int
						for c := range rgb {
							rgb[c], _ = strconv.Atoi(params[p+2+c])
						}
						fg = fmt.Sprintf("#%02X%02X%02X", rgb[0], rgb[1], rgb[2])
						p += 4
					}
				case "48":
					if p+1 < len(params) && params[p+1] == "2" {
						p += 4
					}
				}
			}
			i += end + 1
			continue
		}
		r, size := []rune(s[i:])[0], len(string([]rune(s[i:])[0]))
		out = append(out, styledCell{r: r, fg: fg, bold: bold})
		i += size
	}
	return out
}

// colourNames names the dark-theme colours the primitives draw with.
func colourNames() map[string]string {
	names := map[string]string{}
	for tok := token(0); tok < tokenCount; tok++ {
		names[tokenTable[tok].dark] = tokenTable[tok].name
	}
	names[colAccentShine.Dark.TrueColor] = "accent-shine"
	names[colAccentDim.Dark.TrueColor] = "accent-dim"
	names[colRouteDim.Dark.TrueColor] = "lavender-dim"
	for i, st := range rampTork {
		if c, ok := st.GetForeground().(lipgloss.CompleteAdaptiveColor); ok {
			if _, taken := names[c.Dark.TrueColor]; !taken {
				names[c.Dark.TrueColor] = fmt.Sprintf("tork-ramp-%d", i)
			}
		}
	}
	for i, st := range rampDebrid {
		if c, ok := st.GetForeground().(lipgloss.CompleteAdaptiveColor); ok {
			if _, taken := names[c.Dark.TrueColor]; !taken {
				names[c.Dark.TrueColor] = fmt.Sprintf("debrid-ramp-%d", i)
			}
		}
	}
	return names
}

// colourName finds the named colour within one step per channel, since
// termenv rounds through floating point.
func colourName(names map[string]string, hex string) string {
	if hex == "" {
		return "default"
	}
	r, g, b := parseHex(hex)
	for k, name := range names {
		kr, kg, kb := parseHex(k)
		if abs(kr-r) <= 1 && abs(kg-g) <= 1 && abs(kb-b) <= 1 {
			return name
		}
	}
	return hex
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// goldenSheet writes styled lines as text plus a colour row: each cell's
// colour is a letter from the sheet's legend, upper case when bold.
type goldenSheet struct {
	t       *testing.T
	names   map[string]string
	letters map[string]byte
	order   []string
	body    strings.Builder
}

func newGoldenSheet(t *testing.T) *goldenSheet {
	return &goldenSheet{t: t, names: colourNames(), letters: map[string]byte{}}
}

func (g *goldenSheet) letter(name string) byte {
	if name == "default" {
		return '.'
	}
	if l, ok := g.letters[name]; ok {
		return l
	}
	l := byte('a' + len(g.order))
	g.letters[name] = l
	g.order = append(g.order, name)
	return l
}

func (g *goldenSheet) add(label, styled string) {
	fmt.Fprintf(&g.body, "%s\n", label)
	for _, line := range strings.Split(styled, "\n") {
		cells := parseStyled(g.t, line)
		var text, colour strings.Builder
		for _, c := range cells {
			text.WriteRune(c.r)
			l := g.letter(colourName(g.names, c.fg))
			if c.bold && l != '.' {
				l -= 'a' - 'A'
			}
			colour.WriteByte(l)
		}
		fmt.Fprintf(&g.body, "  |%s|\n  |%s|\n", text.String(), colour.String())
	}
}

func (g *goldenSheet) String() string {
	var b strings.Builder
	b.WriteString("legend (upper case = bold, . = default):\n")
	for _, name := range g.order {
		fmt.Fprintf(&b, "  %c = %s\n", g.letters[name], name)
	}
	b.WriteString("\n")
	b.WriteString(g.body.String())
	return b.String()
}

func TestGoldenMotionPrimitives(t *testing.T) {
	withProfile(t, termenv.TrueColor, true)
	g := newGoldenSheet(t)
	for f := 0; f <= 10; f++ {
		g.add(fmt.Sprintf("spinner frame %d", f), spinnerGlyph(f))
	}
	for _, f := range []int{0, 1, 3, 5, 8, 12, 15, 16} {
		g.add(fmt.Sprintf("copper sweep frame %d width 40", f), copperRule(f, 40))
	}
	for _, f := range []int{0, 1, 5, 10, 15, 20, 29, 30} {
		g.add(fmt.Sprintf("shine frame %d, 60%% of 30", f), shineRail(0.6, 30, f, styleOK))
	}
	for _, f := range []int{0, 20, 40, 60, 79, 80} {
		g.add(fmt.Sprintf("logo colour cycle frame %d", f), homeLogo(f))
	}
	var blink, cat strings.Builder
	for f := 0; f < 2*appFPS; f++ {
		if cursorVisible(f) {
			blink.WriteString("█")
		} else {
			blink.WriteString("_")
		}
	}
	g.add("cursor blink frames 0-19", blink.String())
	for _, f := range []int{0, 1, 47, 48, 49, 50, 98, 99} {
		fmt.Fprintf(&cat, "%d:%s ", f, catEyes(f))
	}
	g.add("cat eyes", strings.TrimSpace(cat.String()))
	for age := 0; age <= 3; age++ {
		g.add(fmt.Sprintf("fade age %d", age), fadeStyle(age, styleOK).Render("queued"))
	}
	assertGolden(t, "motion_primitives", g.String())
}

// --- frame budget -----------------------------------------------------------

func benchmarkFrame(b *testing.B, a *App) {
	withProfile(b, termenv.TrueColor, true)
	withMotion(a)
	a.View() // warm caches the first render builds
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.motion.frame++
		_ = a.View()
	}
}

// BenchmarkFrame120x36 measures building one frame at 120×36 on each moving
// screen. The budget is 2 ms per frame (design.md § Motion).
func BenchmarkFrame120x36(b *testing.B) {
	b.Run("home", func(b *testing.B) { benchmarkFrame(b, homeGoldenApp(120, 36)) })
	b.Run("results-searching", func(b *testing.B) { benchmarkFrame(b, resultsGoldenApp(true)) })
	b.Run("downloads", func(b *testing.B) { benchmarkFrame(b, downloadsGoldenApp(120, 36)) })
}

// pastFade moves the clock past the newest result row's fade-in.
func pastFade(a *App) *App {
	a.motion.frame = a.results.lastBorn + fadeFrames
	return a
}
