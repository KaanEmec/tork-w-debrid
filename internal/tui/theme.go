package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Nightglass is the v0.2.0 palette (E8, design.md § Tokens). Every colour
// means one thing, in dark and light terminals alike:
//
//   - accent (mint) is tork's own voice: focus, keys, local progress, ✓
//   - route (lavender) means Real-Debrid and nothing else
//   - amber is caution: paused, slow, under 5 seeds, ★ best
//   - sky is seeding (and the second colour of the logo cycle)
//   - rose is failed and errors
//
// Words use fg, text or muted only; faint is decoration (empty bar cells,
// dots, separators) and never carries words. The ground is the terminal's own
// background and is never painted; only the selected row fills raised.
//
// Each token is a lipgloss.CompleteAdaptiveColor: lipgloss picks the light or
// dark column from the terminal background, and the truecolor, ANSI-256 or
// basic ANSI value from the colour profile, so a 256-colour terminal gets the
// hand-picked fallback rather than a nearest-colour guess.
type token int

const (
	tokGround token = iota
	tokRaised
	tokLine
	tokFg
	tokText
	tokMuted
	tokFaint
	tokAccent
	tokRoute
	tokAmber
	tokSky
	tokRose
	tokenCount
)

// tokenSpec is one row of design.md's token table.
type tokenSpec struct {
	name                string
	dark, light         string // truecolor
	dark256, light256   string // ANSI-256 fallback
	darkANSI, lightANSI string // basic 16-colour fallback
	words               bool   // carries text, so must reach 4.5:1 on ground
}

var tokenTable = [tokenCount]tokenSpec{
	tokGround: {"ground", "#0E1015", "#F6F7F9", "233", "255", "0", "15", false},
	tokRaised: {"raised", "#1C212C", "#E3E8EF", "235", "254", "8", "7", false},
	tokLine:   {"line", "#262C3A", "#D5DBE5", "236", "253", "8", "7", false},
	tokFg:     {"fg", "#E8EBF1", "#11141B", "255", "233", "15", "0", true},
	tokText:   {"text", "#C5CAD6", "#2B3140", "251", "236", "7", "0", true},
	tokMuted:  {"muted", "#8B93A6", "#555D6E", "246", "241", "8", "8", true},
	tokFaint:  {"faint", "#4E5568", "#A3AAB8", "239", "248", "8", "7", false},
	tokAccent: {"accent", "#7CE8B0", "#087A4B", "115", "29", "10", "2", true},
	tokRoute:  {"lavender", "#B4A5FF", "#5F4BD0", "147", "62", "13", "5", true},
	tokAmber:  {"amber", "#F2C46D", "#8F5B00", "221", "94", "11", "3", true},
	tokSky:    {"sky", "#7FC3FF", "#1D66B8", "117", "25", "14", "4", true},
	tokRose:   {"rose", "#FF8E9E", "#B8304A", "210", "125", "9", "1", true},
}

// color returns the token as a lipgloss colour with all six values.
func (t token) color() lipgloss.CompleteAdaptiveColor {
	s := tokenTable[t]
	return lipgloss.CompleteAdaptiveColor{
		Dark:  lipgloss.CompleteColor{TrueColor: s.dark, ANSI256: s.dark256, ANSI: s.darkANSI},
		Light: lipgloss.CompleteColor{TrueColor: s.light, ANSI256: s.light256, ANSI: s.lightANSI},
	}
}

var (
	colGround = tokGround.color()
	colRaised = tokRaised.color()
	colLine   = tokLine.color()
	colFg     = tokFg.color()
	colText   = tokText.color()
	colMuted  = tokMuted.color()
	colFaint  = tokFaint.color()
	colAccent = tokAccent.color()
	colRoute  = tokRoute.color()
	colAmber  = tokAmber.color()
	colSky    = tokSky.color()
	colRose   = tokRose.color()

	colWhite = lipgloss.CompleteAdaptiveColor{
		Dark:  lipgloss.CompleteColor{TrueColor: "#FFFFFF", ANSI256: "231", ANSI: "15"},
		Light: lipgloss.CompleteColor{TrueColor: "#FFFFFF", ANSI256: "231", ANSI: "15"},
	}

	// Derived shades used by the motion primitives and the segment map.
	colAccentShine = mixColor(colAccent, colWhite, 0.55)  // mint mixed 45 % with white
	colAccentDim   = mixColor(colAccent, colGround, 0.40) // segment "in progress"
	colRouteDim    = mixColor(colRoute, colGround, 0.45)  // cloud-phase progress rail
)

// mixColor blends b into a by t (0 = a, 1 = b) for both themes. The truecolor
// value is mixed exactly; the ANSI-256 value is the nearest xterm colour to the
// mix, and the basic ANSI value falls back to whichever end dominates.
func mixColor(a, b lipgloss.CompleteAdaptiveColor, t float64) lipgloss.CompleteAdaptiveColor {
	return lipgloss.CompleteAdaptiveColor{
		Dark:  mixComplete(a.Dark, b.Dark, t),
		Light: mixComplete(a.Light, b.Light, t),
	}
}

func mixComplete(a, b lipgloss.CompleteColor, t float64) lipgloss.CompleteColor {
	hex := mixHex(a.TrueColor, b.TrueColor, t)
	ansi := a.ANSI
	if t > 0.5 {
		ansi = b.ANSI
	}
	return lipgloss.CompleteColor{TrueColor: hex, ANSI256: nearest256(hex), ANSI: ansi}
}

func parseHex(hex string) (r, g, b float64) {
	v, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil {
		return 0, 0, 0
	}
	return float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)
}

func mixHex(a, b string, t float64) string {
	t = max(0, min(1, t))
	ar, ag, ab := parseHex(a)
	br, bg, bb := parseHex(b)
	mix := func(x, y float64) int { return int(math.Round(x + (y-x)*t)) }
	return fmt.Sprintf("#%02X%02X%02X", mix(ar, br), mix(ag, bg), mix(ab, bb))
}

// nearest256 maps a truecolor value onto the xterm-256 cube or grey ramp.
// Only derived shades use it; the base tokens carry hand-picked values.
func nearest256(hex string) string {
	r, g, b := parseHex(hex)
	levels := [6]float64{0, 95, 135, 175, 215, 255}
	nearestLevel := func(v float64) int {
		best := 0
		for i := range levels {
			if math.Abs(levels[i]-v) < math.Abs(levels[best]-v) {
				best = i
			}
		}
		return best
	}
	ri, gi, bi := nearestLevel(r), nearestLevel(g), nearestLevel(b)
	dist := func(x, y, z float64) float64 { return (r-x)*(r-x) + (g-y)*(g-y) + (b-z)*(b-z) }
	cube := 16 + 36*ri + 6*gi + bi
	cubeDist := dist(levels[ri], levels[gi], levels[bi])
	grey := int(math.Round(((r+g+b)/3 - 8) / 10))
	grey = max(0, min(23, grey))
	gv := float64(8 + 10*grey)
	if dist(gv, gv, gv) < cubeDist {
		return strconv.Itoa(232 + grey)
	}
	return strconv.Itoa(cube)
}

// relativeLuminance and contrastRatio follow WCAG 2.x; the token tests use
// them to hold every word-carrying token at 4.5:1 or better on the ground.
func relativeLuminance(hex string) float64 {
	r, g, b := parseHex(hex)
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// glyphSet holds every non-ASCII glyph the UI draws, so terminals without
// Unicode blocks can swap them in one place (design.md § Fallbacks, J8.9).
type glyphSet struct {
	ascii    bool
	cloud    string // Real-Debrid marker
	cursor   string // selected-row gutter
	prompt   string
	filled   string // progress rail, done part
	empty    string // progress rail, remaining part
	star     string
	ellipsis string // truncation mark
	sep      string // header breadcrumb separator
	spinner  []string
	segOpen  string
	segDone  string
	segPart  string
	segTodo  string
	segClose string
}

// The cloud is printed with U+FE0E so terminals keep it to text width.
var unicodeGlyphs = glyphSet{
	cloud: "☁︎", cursor: "▍", prompt: "❯", filled: "━", empty: "─", star: "★", ellipsis: "…", sep: "›",
	spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	segOpen: "▕", segDone: "█", segPart: "▓", segTodo: "░", segClose: "▏",
}

var asciiGlyphs = glyphSet{
	ascii: true,
	cloud: "RD", cursor: ">", prompt: ">", filled: "=", empty: "-", star: "*", ellipsis: "...", sep: ">",
	spinner: []string{"|", "/", "-", "\\"},
	segOpen: "[", segDone: "#", segPart: "=", segTodo: ".", segClose: "]",
}

// glyphs is the active set. J8.9 decides when a terminal needs the ASCII set
// and calls SetASCIIGlyphs; everything that draws a glyph reads it from here.
var glyphs = unicodeGlyphs

// SetASCIIGlyphs switches every glyph the UI draws to its ASCII fallback.
func SetASCIIGlyphs(on bool) {
	if on {
		glyphs = asciiGlyphs
	} else {
		glyphs = unicodeGlyphs
	}
}

// Real-Debrid styles. Lavender is reserved for these: cloud rows, the ☁ RD
// marker, "via Real-Debrid", the link dialog and the account badge.
var (
	styleRoute     = lipgloss.NewStyle().Foreground(colRoute)
	styleRouteBold = lipgloss.NewStyle().Foreground(colRoute).Bold(true)
	styleRouteDim  = lipgloss.NewStyle().Foreground(colRouteDim) // cloud-phase rail
)

// routeMarker is the footer's "☁ RD": shown only when an account is linked and
// "use for downloads" is on.
func routeMarker() string {
	if glyphs.ascii {
		return styleRoute.Render("RD")
	}
	return styleRoute.Render(glyphs.cloud + " RD")
}

// cloudGlyph is the downloads-row glyph for an item in the cloud phase.
func cloudGlyph() string { return styleRoute.Render(glyphs.cloud) }

// viaRoute is the lavender "☁ via Real-Debrid" used on results and preview.
func viaRoute() string {
	if glyphs.ascii {
		return styleRoute.Render("via Real-Debrid")
	}
	return styleRoute.Render(glyphs.cloud + " via Real-Debrid")
}

// routeBadge renders a Real-Debrid account fact (for example "linked as
// kaan · premium until 2027-03-01") in bold lavender for Settings.
func routeBadge(text string) string { return styleRouteBold.Render(text) }

// cloudRowStyle colours a cloud-phase activity line such as
// "Real-Debrid 43 % · 12 MB/s · 8 seeders".
func cloudRowStyle() lipgloss.Style { return styleRoute }
