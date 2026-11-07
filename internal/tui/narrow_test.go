package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
	"github.com/kaanemec/tork-w-debrid/internal/health"
)

// J8.9: every screen at 80×24 and 50×20, dark and light, with Unicode and in
// ASCII mode. Screens are built by sizeScreens; tests loop over all of them.

var narrowSizes = [][2]int{{80, 24}, {50, 20}}

type sizedScreen struct {
	name  string
	build func(t *testing.T, w, h int) *App
}

func sizeScreens() []sizedScreen {
	return []sizedScreen{
		{"home", func(t *testing.T, w, h int) *App { return homeGoldenApp(w, h) }},
		{"results", func(t *testing.T, w, h int) *App {
			a := resultsGoldenApp(false)
			a.width, a.height = w, h
			return a
		}},
		{"results-searching", func(t *testing.T, w, h int) *App {
			a := resultsGoldenApp(true)
			a.width, a.height = w, h
			return a
		}},
		{"preview", func(t *testing.T, w, h int) *App {
			a := previewGoldenApp()
			a.width, a.height = w, h
			return a
		}},
		{"downloads", func(t *testing.T, w, h int) *App { return downloadsGoldenApp(w, h) }},
		{"sources", func(t *testing.T, w, h int) *App {
			a := sourcesTestApp(t)
			a.width, a.height = w, h
			a.screen = screenSources
			a.ensureSources()
			for id := range a.cfg.Providers {
				a.sources.readings[id] = sourceReading{probe: health.ProviderProbe{OK: true, Results: 12, LatencyMS: 340}, at: time.Now().Add(-5 * time.Minute)}
			}
			return a
		}},
		{"settings", func(t *testing.T, w, h int) *App { return settingsGoldenApp(w, h) }},
		{"link-dialog", func(t *testing.T, w, h int) *App {
			a := settingsGoldenApp(w, h)
			a.settings.rd.phase, a.settings.rd.haveAcct = rdNotLinked, false
			a.SetDebridStatus(false, true)
			a.settings.overlay = &rdLinkDialog{phase: rdLinkWaiting, code: realdebrid.DeviceCode{
				UserCode: "ABCD1234", VerificationURL: "https://real-debrid.com/device",
				Expires: goldenNow.Add(9*time.Minute + 41*time.Second),
			}}
			return a
		}},
		{"folder-editor", func(t *testing.T, w, h int) *App {
			a := folderEditorGoldenApp()
			a.width, a.height = w, h
			return a
		}},
		{"help", func(t *testing.T, w, h int) *App {
			a := homeGoldenApp(w, h)
			a.showHelp = true
			return a
		}},
	}
}

// nonASCII returns the first character outside printable ASCII on any line.
func nonASCII(view string) (line int, r rune, ok bool) {
	for i, l := range strings.Split(ansi.Strip(view), "\n") {
		for _, c := range l {
			if c < 0x20 || c > 0x7e {
				return i + 1, c, true
			}
		}
	}
	return 0, 0, false
}

func TestEveryScreenFitsNarrowTerminals(t *testing.T) {
	for _, s := range sizeScreens() {
		for _, size := range narrowSizes {
			w, h := size[0], size[1]
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, w, h), func(t *testing.T) {
				a := s.build(t, w, h)
				view := a.View()
				assertRenderFits(t, view, w)
				if got := len(strings.Split(view, "\n")); got > h {
					t.Fatalf("%d lines, terminal has %d", got, h)
				}
			})
		}
	}
}

func TestEveryScreenInASCIIMode(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	for _, s := range sizeScreens() {
		for _, size := range append(narrowSizes, [2]int{120, 36}) {
			w, h := size[0], size[1]
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, w, h), func(t *testing.T) {
				a := s.build(t, w, h)
				view := a.View()
				assertRenderFits(t, view, w)
				if i, r, bad := nonASCII(view); bad {
					t.Fatalf("line %d has %q (U+%04X):\n%s", i, r, r, ansi.Strip(view))
				}
			})
		}
	}
}

var _ = lipgloss.Width

// sgrRun is a stretch of visible text and the truecolor it is drawn in.
type sgrRun struct {
	text   string
	fg, bg string // "#RRGGBB", or "" for the terminal's own colour
}

// sgrRuns splits a truecolor-rendered view into runs with their colours.
func sgrRuns(view string) []sgrRun {
	var runs []sgrRun
	var fg, bg string
	hex := func(r, g, b int) string { return fmt.Sprintf("#%02X%02X%02X", r, g, b) }
	for len(view) > 0 {
		i := strings.Index(view, "\x1b[")
		if i < 0 {
			runs = append(runs, sgrRun{view, fg, bg})
			break
		}
		if i > 0 {
			runs = append(runs, sgrRun{view[:i], fg, bg})
		}
		view = view[i+2:]
		j := strings.IndexAny(view, "mABCDHJK")
		if j < 0 {
			break
		}
		if view[j] == 'm' {
			p := strings.Split(view[:j], ";")
			for k := 0; k < len(p); k++ {
				n := func(o int) int {
					if k+o >= len(p) {
						return 0
					}
					v := 0
					fmt.Sscan(p[k+o], &v)
					return v
				}
				switch p[k] {
				case "", "0":
					fg, bg = "", ""
				case "38", "48":
					if n(1) == 2 {
						c := hex(n(2), n(3), n(4))
						if p[k] == "38" {
							fg = c
						} else {
							bg = c
						}
						k += 4
					} else {
						k += 2
					}
				case "39":
					fg = ""
				case "49":
					bg = ""
				}
			}
		}
		view = view[j+1:]
	}
	return runs
}

// TestLightThemeOnEveryScreen draws every screen on a light terminal in
// truecolor: no dark-column token may appear, only light raised may fill, and
// every run of words must reach 4.5:1 on the light ground (or on raised).
func TestLightThemeOnEveryScreen(t *testing.T) {
	withProfile(t, termenv.TrueColor, false)
	var darkHex []string
	for tok := token(0); tok < tokenCount; tok++ {
		darkHex = append(darkHex, tokenTable[tok].dark)
	}
	ground, raised := tokenTable[tokGround].light, tokenTable[tokRaised].light
	for _, s := range sizeScreens() {
		for _, size := range append(narrowSizes, [2]int{120, 36}) {
			w, h := size[0], size[1]
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, w, h), func(t *testing.T) {
				a := s.build(t, w, h)
				view := a.View()
				assertRenderFits(t, view, w)
				if !strings.Contains(view, "\x1b[") {
					t.Fatal("no colour in the render; the test proves nothing")
				}
				for _, r := range sgrRuns(view) {
					for _, d := range darkHex {
						if strings.EqualFold(r.fg, d) || strings.EqualFold(r.bg, d) {
							t.Fatalf("dark token %s on a light terminal, text %q", d, r.text)
						}
					}
					if r.bg != "" && !strings.EqualFold(r.bg, raised) && !strings.EqualFold(r.bg, ground) {
						t.Fatalf("background %s is neither raised nor ground, text %q", r.bg, r.text)
					}
					if r.fg == "" || !strings.ContainsFunc(r.text, func(c rune) bool {
						return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
					}) {
						continue
					}
					on := ground
					if r.bg != "" {
						on = r.bg
					}
					if c := contrastRatio(r.fg, on); c < 4.5 {
						t.Errorf("%q in %s on %s is %.2f:1, want 4.5:1", r.text, r.fg, on, c)
					}
				}
			})
		}
	}
}

func TestGoldenASCIIScreensAt80x24(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	for _, s := range sizeScreens() {
		a := s.build(t, 80, 24)
		view := a.View()
		assertRenderFits(t, view, 80)
		assertGolden(t, "ascii_"+s.name+"_80x24", view)
	}
}

func TestASCIIFallbackCloud(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	for name, got := range map[string]string{"marker": routeMarker(), "glyph": cloudGlyph(), "via": viaRoute()} {
		if strings.Contains(ansi.Strip(got), "☁") || !strings.Contains(ansi.Strip(got), "RD") && name != "via" {
			t.Errorf("%s = %q, want RD in place of the cloud", name, ansi.Strip(got))
		}
	}
	if got := asciiFold("☁︎ 1"); got != "RD 1" {
		t.Errorf("asciiFold of a stray cloud = %q", got)
	}
}

func TestASCIIFallbackSegmentMap(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	got := ansi.Strip(segmentMap([]segmentCell{segmentDone, segmentDone, segmentPart, segmentTodo, segmentTodo}))
	if got != "[##=..]" {
		t.Fatalf("segment map = %q, want [##=..]", got)
	}
}

func TestASCIIFallbackSpinner(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	var got string
	for i := 0; i < 4; i++ {
		got += ansi.Strip(spinnerGlyph(i))
	}
	if got != `|/-\` {
		t.Fatalf("spinner frames = %q", got)
	}
}

func TestASCIIFallbackRails(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	if got := ansi.Strip(shineRail(0.5, 10, 0, styleOK)); got != "=====-----" {
		t.Errorf("progress rail = %q, want =====-----", got)
	}
	if got := ansi.Strip(asciiFold(rule(6))); got != "------" {
		t.Errorf("rule = %q", got)
	}
	if got := ansi.Strip(asciiFold(copperRule(5, 20))); got != strings.Repeat("-", 20) {
		t.Errorf("copper rule = %q", got)
	}
}

func TestASCIIFallbackCursorAndTruncation(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	a := downloadsGoldenApp(80, 24)
	if !strings.Contains(ansi.Strip(a.View()), "> ") {
		t.Error("selected row lacks the > cursor")
	}
	if got := ansi.Strip(truncate("a long title that will be cut", 10)); got != "a long ..." {
		t.Errorf("truncate = %q", got)
	}
	if strings.ContainsRune(ansi.Strip(a.View()), '▍') {
		t.Error("the ▍ cursor leaked into ASCII mode")
	}
}

func TestASCIIFallbackLogoAndIntro(t *testing.T) {
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	if got := ansi.Strip(homeLogo(0)); got != "tork-w-debrid" {
		t.Errorf("home logo = %q, want a plain title", got)
	}
	a := introApp(t, "v0.2.0", "")
	if a.introDue() {
		t.Error("the intro is due in ASCII mode")
	}
	if cmd := a.replayIntro(); a.intro.active || cmd == nil {
		t.Error("replay started the intro in ASCII mode")
	}
}

func TestIntroOnNarrowAndLightTerminals(t *testing.T) {
	a := introApp(t, "v0.2.0", "")
	a.width, a.height = 50, 20
	if a.introDue() {
		t.Error("the intro is due at 50x20, where it cannot fit")
	}
	var darkHex []string
	for tok := token(0); tok < tokenCount; tok++ {
		d := tokenTable[tok].dark
		r, g, b := parseHex(d)
		darkHex = append(darkHex, fmt.Sprintf("2;%d;%d;%d", int(r), int(g), int(b)))
	}
	light := introTheme{dark: false, profile: termenv.TrueColor}
	for _, size := range [][2]int{{80, 24}, {120, 36}} {
		for _, n := range []int{0, 30, 90} {
			view := introFrame(n, size[0], size[1], light, "v0.2.0")
			assertIntroShape(t, view, size[0], size[1])
			for _, d := range darkHex {
				if strings.Contains(view, "38;"+d) || strings.Contains(view, "48;"+d) {
					t.Fatalf("%dx%d frame %d uses a dark token (%s) on a light terminal", size[0], size[1], n, d)
				}
			}
		}
	}
}

func TestDetectASCII(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name    string
		env     func(string) string
		tty     bool
		profile termenv.Profile
		goos    string
		want    bool
	}{
		{"utf-8 terminal", env("TERM", "xterm-256color", "LANG", "en_US.UTF-8"), true, termenv.ANSI256, "linux", false},
		{"utf8 spelling", env("LC_ALL", "C.utf8"), true, termenv.TrueColor, "linux", false},
		{"C locale", env("TERM", "xterm", "LANG", "C"), true, termenv.ANSI, "linux", true},
		{"LC_ALL beats LANG", env("LC_ALL", "POSIX", "LANG", "en_US.UTF-8"), true, termenv.ANSI, "linux", true},
		{"latin-1", env("LANG", "de_DE.ISO-8859-1"), true, termenv.ANSI, "linux", true},
		{"linux console", env("TERM", "linux", "LANG", "en_US.UTF-8"), true, termenv.ANSI, "linux", true},
		{"dumb", env("TERM", "dumb"), true, termenv.Ascii, "linux", true},
		{"no locale, no colour", env("TERM", "vt100"), true, termenv.Ascii, "linux", true},
		{"NO_COLOR only", env("TERM", "xterm", "NO_COLOR", "1", "LANG", "en_US.UTF-8"), true, termenv.Ascii, "linux", false},
		{"NO_COLOR, no locale", env("NO_COLOR", "1"), true, termenv.Ascii, "linux", false},
		{"windows ignores LANG", env("LANG", "C"), true, termenv.TrueColor, "windows", false},
		{"not a terminal", env(), false, termenv.Ascii, "linux", false},
		{"forced on", env("TORKWD_ASCII", "1", "LANG", "en_US.UTF-8"), true, termenv.TrueColor, "linux", true},
		{"forced off", env("TORKWD_ASCII", "0", "TERM", "dumb"), true, termenv.Ascii, "linux", false},
	} {
		if got := detectASCIIEnv(tc.env, tc.tty, tc.profile, tc.goos); got != tc.want {
			t.Errorf("%s: detectASCIIEnv = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionLaunchCarriesASCII(t *testing.T) {
	// The background worker has no terminal; the attaching client's answer
	// reaches it through session.Launch.ASCII (see cmd/torkwd/session.go).
	SetASCIIGlyphs(true)
	if !glyphs.ascii {
		t.Fatal("SetASCIIGlyphs(true) did not switch the set")
	}
	SetASCIIGlyphs(false)
	if glyphs.ascii {
		t.Fatal("SetASCIIGlyphs(false) did not restore the set")
	}
}

// Every downloads row, selected in turn, fits narrow terminals and ASCII
// mode: the cloud, starting, queued, failed and segment rows included.
func TestEveryDownloadsRowFitsWhenSelected(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		SetASCIIGlyphs(ascii)
		for _, size := range append(narrowSizes, [2]int{120, 36}) {
			base := downloadsGoldenApp(size[0], size[1])
			for i := range base.downloads.items {
				a := downloadsGoldenApp(size[0], size[1])
				a.downloads.win.cursor = i
				view := a.View()
				assertRenderFits(t, view, size[0])
				if got := len(strings.Split(view, "\n")); got > size[1] {
					t.Errorf("ascii=%v %dx%d row %d: %d lines", ascii, size[0], size[1], i, got)
				}
				if ascii {
					if l, r, bad := nonASCII(view); bad {
						t.Errorf("row %d line %d has %q", i, l, r)
					}
				}
			}
		}
	}
	SetASCIIGlyphs(false)
}
