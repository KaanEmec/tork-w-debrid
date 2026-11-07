package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

var introTestTheme = introTheme{dark: true, profile: termenv.TrueColor}

func TestIntroLogoMatchesDesign(t *testing.T) {
	if bigIntroLogo.width != 103 || len(bigIntroLogo.pix) != 10 {
		t.Fatalf("bold logo is %d×%d half rows, want 103×10", bigIntroLogo.width, len(bigIntroLogo.pix))
	}
	if bigIntroLogo.torkEnd != 34 || bigIntroLogo.wEnd != 60 {
		t.Fatalf("colour spans %d/%d, want 34/60", bigIntroLogo.torkEnd, bigIntroLogo.wEnd)
	}
	if smallIntroLogo.width != homeLogoWidth || len(smallIntroLogo.pix) != 4 {
		t.Fatalf("small logo is %d×%d", smallIntroLogo.width, len(smallIntroLogo.pix))
	}
}

func TestGoldenIntro(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		for _, n := range []int{0, 30} {
			view := introFrame(n, size[0], size[1], introTestTheme, "v0.2.0")
			assertIntroShape(t, view, size[0], size[1])
			assertGolden(t, fmt.Sprintf("intro_%dx%d_f%d", size[0], size[1], n), view)
		}
	}
}

func assertIntroShape(t *testing.T, view string, w, h int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Fatalf("intro is %d rows, want %d", len(lines), h)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got != w {
			t.Fatalf("row %d is %d cells, want %d", i, got, w)
		}
	}
}

func TestIntroFrameIsDeterministic(t *testing.T) {
	for _, n := range []int{0, 1, 17, 30, 239} {
		a := introFrame(n, 120, 36, introTestTheme, "v0.2.0")
		b := introFrame(n, 120, 36, introTestTheme, "v0.2.0")
		if a != b {
			t.Fatalf("frame %d differs between two renders", n)
		}
	}
	if introFrame(0, 120, 36, introTestTheme, "v0.2.0") == introFrame(1, 120, 36, introTestTheme, "v0.2.0") {
		t.Fatal("frames 0 and 1 are identical; nothing moves")
	}
}

func TestIntroLayersAtBothSizes(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}, {200, 60}} {
		plain := ansi.Strip(introFrame(0, size[0], size[1], introTestTheme, "v0.2.0"))
		for _, want := range []string{"v 0 . 2 . 0", "( ^.^ )", "any key skip"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%dx%d: missing %q", size[0], size[1], want)
			}
		}
		if !strings.ContainsAny(plain, "▀▄█") {
			t.Fatalf("%dx%d: no logo", size[0], size[1])
		}
		L, _ := layoutIntro(size[0], size[1])
		band := strings.Join(strings.Split(plain, "\n")[L.scrollTop:L.scrollTop+introScrollRows], "")
		if letters := strings.Count(band, "R") + strings.Count(band, "e"); letters < 3 {
			t.Fatalf("%dx%d: the scroller is missing", size[0], size[1])
		}
	}
	// The cat blinks for 0.2 s every 4 s.
	if !strings.Contains(introFrame(112, 120, 36, introTestTheme, "v0.2.0"), "-.-") {
		t.Fatal("the cat never blinks")
	}
}

func TestIntroColoursUseThePalette(t *testing.T) {
	view := introFrame(0, 120, 36, introTestTheme, "v0.2.0")
	// ground plate, mint, lavender
	for _, rgb := range []string{"48;2;14;16;21", "38;2;124;232;176", "165;255"} {
		if !strings.Contains(view, rgb) {
			t.Fatalf("frame lacks colour %s", rgb)
		}
	}
	plain := introFrame(0, 120, 36, introTheme{dark: true, profile: termenv.Ascii}, "v0.2.0")
	if strings.Contains(plain, "\x1b[") {
		t.Fatal("the ASCII profile must not emit escape sequences")
	}
	light := introFrame(0, 120, 36, introTheme{dark: false, profile: termenv.TrueColor}, "v0.2.0")
	if !strings.Contains(light, "48;2;246;247;249") {
		t.Fatal("light theme plate must use the light ground")
	}
}

func TestIntroTooSmallShowsTheName(t *testing.T) {
	if introFits(79, 23) || introFits(40, 30) {
		t.Fatal("the intro must not play in tiny terminals")
	}
	plain := ansi.Strip(introFrame(0, 40, 12, introTestTheme, "v0.2.0"))
	if !strings.Contains(plain, "tork-w-debrid  v0.2.0") {
		t.Fatalf("too-small frame: %q", plain)
	}
}

// The budget: a frame builds in under 4 ms at 120×36.
func TestIntroFrameBudget(t *testing.T) {
	introFrame(0, 120, 36, introTestTheme, "v0.2.0") // warm the palette
	const frames = 60
	start := time.Now()
	for n := 0; n < frames; n++ {
		introFrame(n, 120, 36, introTestTheme, "v0.2.0")
	}
	if per := time.Since(start) / frames; per > 4*time.Millisecond {
		t.Fatalf("a frame takes %v, budget is 4ms", per)
	}
}

func BenchmarkIntroFrame(b *testing.B) {
	for i := 0; i < b.N; i++ {
		introFrame(i, 120, 36, introTestTheme, "v0.2.0")
	}
}

func introApp(t *testing.T, version, seen string) *App {
	t.Helper()
	cfg, err := config.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := navigationApp()
	a.cfg = cfg
	a.st = &state.State{IntroSeenVersion: seen}
	a.width, a.height = 120, 36
	a.motion = motionState{animations: true}
	a.SetVersion(version)
	return a
}

func TestIntroPlaysOncePerVersion(t *testing.T) {
	a := introApp(t, "v0.2.0", "v0.1.0")
	if a.maybeStartIntro() == nil || !a.IntroPlaying() {
		t.Fatal("a new version must open with the intro")
	}
	if !strings.Contains(a.View(), "any key") {
		t.Fatal("the intro is not on screen")
	}
	if a.frameRate() != introFPS || !a.animating() {
		t.Fatal("the intro runs the clock at 30 fps")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if a.IntroPlaying() {
		t.Fatal("any key skips the intro")
	}
	if a.st.IntroSeenVersion != "v0.2.0" {
		t.Fatalf("seen version = %q", a.st.IntroSeenVersion)
	}
	saved, err := state.Load(a.cfg.StatePath())
	if err != nil || saved.IntroSeenVersion != "v0.2.0" {
		t.Fatalf("state.json not written: %+v %v", saved, err)
	}
	if a.search.input.Value() != "" || a.screen != screenSearch {
		t.Fatal("the skip key must not reach the screen underneath")
	}

	// The next start goes straight home.
	b := introApp(t, "v0.2.0", saved.IntroSeenVersion)
	if b.maybeStartIntro() != nil || b.IntroPlaying() {
		t.Fatal("the intro played twice for one version")
	}
}

func TestIntroEndsByItself(t *testing.T) {
	a := introApp(t, "v0.2.0", "")
	a.maybeStartIntro()
	for i := 0; i < introFrames-1; i++ {
		a.Update(frameMsg{})
	}
	if !a.IntroPlaying() {
		t.Fatal("the intro ended early")
	}
	a.Update(frameMsg{})
	if a.IntroPlaying() || a.st.IntroSeenVersion != "v0.2.0" {
		t.Fatal("the intro must end after about 8 s and be marked seen")
	}
}

func TestIntroNeverAutoplaysWithoutMotion(t *testing.T) {
	cases := map[string]func(*App){
		"motion off":     func(a *App) { a.SetNoMotion(true) },
		"animations off": func(a *App) { a.SetAnimations(false) },
		"ui.intro off":   func(a *App) { a.SetIntroAfterUpdate(false) },
		"no version":     func(a *App) { a.SetVersion("") },
		"tiny terminal":  func(a *App) { a.width, a.height = 60, 20 },
	}
	for name, setup := range cases {
		a := introApp(t, "v0.2.0", "")
		setup(a)
		if a.maybeStartIntro() != nil || a.IntroPlaying() {
			t.Fatalf("%s: the intro must not play", name)
		}
	}
}

func TestIntroReplay(t *testing.T) {
	// Replay plays even with ui.intro off and the version already seen, and
	// returns to the screen the user was on.
	a := introApp(t, "v0.2.0", "v0.2.0")
	a.SetIntroAfterUpdate(false)
	a.screen = screenDownloads
	a.Update(replayIntroMsg{})
	if !a.IntroPlaying() || a.intro.poster {
		t.Fatal("replay must play the intro")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.IntroPlaying() || a.screen != screenDownloads {
		t.Fatal("any key returns to where the user was")
	}

	// With motion off, replay shows the still poster until a key.
	a.SetNoMotion(true)
	a.Update(replayIntroMsg{})
	if !a.IntroPlaying() || !a.intro.poster || a.animating() {
		t.Fatal("motion off: poster frame, no clock")
	}
	poster := a.View()
	a.Update(frameMsg{})
	if a.View() != poster || !a.IntroPlaying() {
		t.Fatal("the poster must stay still until a key")
	}
	a.Update(tea.KeyMsg{Type: tea.KeySpace})
	if a.IntroPlaying() {
		t.Fatal("a key closes the poster")
	}
}

func TestIntroCtrlCStillQuits(t *testing.T) {
	a := introApp(t, "v0.2.0", "")
	a.maybeStartIntro()
	_, cmd := a.update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if a.IntroPlaying() || cmd == nil {
		t.Fatal("ctrl+c must end the intro and quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c must quit")
	}
}

func TestStandaloneIntroModel(t *testing.T) {
	m := NewIntro("v0.2.0", true)
	if m.Init() == nil {
		t.Fatal("the standalone intro needs its clock")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	m.Update(frameMsg{})
	if m.frame != 1 || !strings.Contains(m.View(), "any key") {
		t.Fatal("the standalone intro does not advance")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("any key quits")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("any key quits")
	}

	still := NewIntro("v0.2.0", false)
	still.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	before := still.View()
	if _, cmd := still.Update(frameMsg{}); cmd != nil || still.View() != before {
		t.Fatal("motion off: the poster stays still")
	}
}
