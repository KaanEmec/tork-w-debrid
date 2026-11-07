package tui

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// Token tests for J8.1: every token resolves to the value design.md lists for
// each theme and colour profile, NO_COLOR strips every colour, and every token
// that carries words reaches 4.5:1 on the ground.

// withProfile points the default renderer (which every package style uses) at
// a fixed colour profile and background for one test.
func withProfile(t *testing.T, p termenv.Profile, dark bool) {
	t.Helper()
	oldProfile, oldDark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(p)
	lipgloss.SetHasDarkBackground(dark)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(oldProfile)
		lipgloss.SetHasDarkBackground(oldDark)
	})
}

// fgSequence is the SGR parameter a foreground colour value produces.
func fgSequence(t *testing.T, p termenv.Profile, value string) string {
	t.Helper()
	switch p {
	case termenv.TrueColor:
		r, g, b := parseHex(value)
		return fmt.Sprintf("38;2;%d;%d;%d", int(r), int(g), int(b))
	case termenv.ANSI256:
		return "38;5;" + value
	case termenv.ANSI:
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 15 {
			t.Fatalf("basic ANSI value %q is not 0-15", value)
		}
		if n < 8 {
			return strconv.Itoa(30 + n)
		}
		return strconv.Itoa(90 + n - 8)
	}
	t.Fatalf("no sequence for profile %v", p)
	return ""
}

// truecolorNear reports whether out sets a truecolor foreground within one
// step per channel of hex.
func truecolorNear(out, hex string) bool {
	i := strings.Index(out, "38;2;")
	if i < 0 {
		return false
	}
	rest := out[i+len("38;2;"):]
	end := strings.IndexByte(rest, 'm')
	if end < 0 {
		return false
	}
	parts := strings.Split(rest[:end], ";")
	if len(parts) != 3 {
		return false
	}
	r, g, b := parseHex(hex)
	for i, want := range []float64{r, g, b} {
		got, err := strconv.Atoi(parts[i])
		if err != nil || got < int(want)-1 || got > int(want)+1 {
			return false
		}
	}
	return true
}

func TestTokenLookupPerTheme(t *testing.T) {
	seen := map[string]bool{}
	for tok := token(0); tok < tokenCount; tok++ {
		spec := tokenTable[tok]
		if spec.name == "" || seen[spec.name] {
			t.Fatalf("token %d has an empty or duplicate name %q", tok, spec.name)
		}
		seen[spec.name] = true
		for _, theme := range []struct {
			dark                    bool
			truecolor, c256, cBasic string
		}{
			{true, spec.dark, spec.dark256, spec.darkANSI},
			{false, spec.light, spec.light256, spec.lightANSI},
		} {
			for _, p := range []struct {
				profile termenv.Profile
				value   string
			}{
				{termenv.TrueColor, theme.truecolor},
				{termenv.ANSI256, theme.c256},
				{termenv.ANSI, theme.cBasic},
			} {
				r := lipgloss.NewRenderer(io.Discard)
				r.SetColorProfile(p.profile)
				r.SetHasDarkBackground(theme.dark)
				out := r.NewStyle().Foreground(tok.color()).Render("x")
				if p.profile == termenv.TrueColor {
					// termenv converts through floating point, so a channel
					// may land one step off the table's hex.
					if !truecolorNear(out, p.value) {
						t.Errorf("%s (dark=%v, truecolor) rendered %q, want %s", spec.name, theme.dark, out, p.value)
					}
					continue
				}
				want := "\x1b[" + fgSequence(t, p.profile, p.value) + "m"
				if !strings.Contains(out, want) {
					t.Errorf("%s (dark=%v, profile %v) rendered %q, want %q", spec.name, theme.dark, p.profile, out, want)
				}
			}
		}
	}
}

func TestTokenAliasesUseTheTable(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  lipgloss.CompleteAdaptiveColor
		tok  token
	}{
		{"colAccent", colAccent, tokAccent},
		{"colRoute", colRoute, tokRoute},
		{"colAmber", colAmber, tokAmber},
		{"colSky", colSky, tokSky},
		{"colRose", colRose, tokRose},
		{"colMuted", colMuted, tokMuted},
		{"colFaint", colFaint, tokFaint},
		{"colRaised", colRaised, tokRaised},
	} {
		if tt.got != tt.tok.color() {
			t.Errorf("%s = %+v, want the %s token", tt.name, tt.got, tokenTable[tt.tok].name)
		}
	}
	// Meanings stay exclusive: no two meaning colours share a value.
	meaning := []token{tokAccent, tokRoute, tokAmber, tokSky, tokRose}
	for i := range meaning {
		for j := i + 1; j < len(meaning); j++ {
			a, b := tokenTable[meaning[i]], tokenTable[meaning[j]]
			if a.dark == b.dark || a.light == b.light || a.dark256 == b.dark256 || a.light256 == b.light256 {
				t.Errorf("%s and %s share a colour", a.name, b.name)
			}
		}
	}
}

func TestTokenContrastOnGround(t *testing.T) {
	ground := tokenTable[tokGround]
	raised := tokenTable[tokRaised]
	for tok := token(0); tok < tokenCount; tok++ {
		spec := tokenTable[tok]
		if !spec.words {
			continue
		}
		if r := contrastRatio(spec.dark, ground.dark); r < 4.5 {
			t.Errorf("%s on the dark ground is %.2f:1, want at least 4.5:1", spec.name, r)
		}
		if r := contrastRatio(spec.light, ground.light); r < 4.5 {
			t.Errorf("%s on the light ground is %.2f:1, want at least 4.5:1", spec.name, r)
		}
	}
	// The selected row draws bold fg on raised.
	fg := tokenTable[tokFg]
	if r := contrastRatio(fg.dark, raised.dark); r < 4.5 {
		t.Errorf("fg on dark raised is %.2f:1", r)
	}
	if r := contrastRatio(fg.light, raised.light); r < 4.5 {
		t.Errorf("fg on light raised is %.2f:1", r)
	}
	// faint is decoration only, so it must stay out of the words set.
	if tokenTable[tokFaint].words || tokenTable[tokLine].words {
		t.Fatal("faint and line are decoration, not text tokens")
	}
	if got := contrastRatio("#FFFFFF", "#000000"); got < 20.9 || got > 21.1 {
		t.Fatalf("contrastRatio(white, black) = %.2f, want 21", got)
	}
}

// nightglassScreens renders every restyled screen with the golden fixtures.
func nightglassScreens() map[string]string {
	return map[string]string{
		"home":      homeGoldenApp(120, 36).View(),
		"results":   resultsGoldenApp(true).View(),
		"preview":   previewGoldenApp().View(),
		"downloads": downloadsGoldenApp(120, 36).View(),
	}
}

func TestNoColorHasNoEscapes(t *testing.T) {
	// NO_COLOR wins even over CLICOLOR_FORCE: the renderer picks no colour.
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "1")
	if p := lipgloss.NewRenderer(io.Discard).ColorProfile(); p != termenv.Ascii {
		t.Fatalf("NO_COLOR renderer profile = %v, want Ascii", p)
	}

	withProfile(t, termenv.TrueColor, true)
	colored := nightglassScreens()
	lipgloss.SetColorProfile(termenv.Ascii)
	plain := nightglassScreens()
	for name, view := range plain {
		if strings.Contains(view, "\x1b") {
			t.Errorf("%s under NO_COLOR still has escape codes: %q", name, view)
		}
		if !strings.Contains(colored[name], "\x1b[") {
			t.Errorf("%s has no colour in truecolor, so this test proves nothing", name)
		}
		if got := ansi.Strip(colored[name]); got != view {
			t.Errorf("%s layout differs without colour:\n--- colour ---\n%s\n--- NO_COLOR ---\n%s", name, got, view)
		}
	}
}

func TestDerivedShadesStayInGamut(t *testing.T) {
	for _, c := range []lipgloss.CompleteAdaptiveColor{colAccentShine, colAccentDim, colRouteDim} {
		for _, cc := range []lipgloss.CompleteColor{c.Dark, c.Light} {
			if len(cc.TrueColor) != 7 || cc.TrueColor[0] != '#' {
				t.Errorf("derived truecolor %q is not #RRGGBB", cc.TrueColor)
			}
			if n, err := strconv.Atoi(cc.ANSI256); err != nil || n < 16 || n > 255 {
				t.Errorf("derived ANSI-256 value %q is not a cube or grey colour", cc.ANSI256)
			}
		}
	}
	if got := nearest256("#000000"); got != "16" {
		t.Errorf("nearest256(black) = %s, want 16", got)
	}
	if got := nearest256("#808080"); got != "244" {
		t.Errorf("nearest256(grey 128) = %s, want 244", got)
	}
}
