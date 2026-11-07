package tui

import (
	"math"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The demoscene intro (J8.8, design.md § Intro): an Amiga/C64 cracktro that
// plays once after each update at 30 fps, for about 8 s or until any key.
//
// introFrame is a pure function of (frame, size, theme, version), so every
// frame is testable. Layers, back to front:
//
//  1. starfield: three layers drifting left at 5, 10 and 20 cells/s
//  2. copper bars: mint, sky and lavender, swinging on a sine in half rows
//  3. the logo on a ground-coloured plate, each column riding a sine wave
//  4. the version, letter-spaced
//  5. a sine scroller, 5 rows high, 15 cells/s
//  6. a perspective checkerboard floor, 6 rows
//  7. the cat, sitting on the floor and blinking
//
// A frame is built in a cell buffer and written out with one SGR sequence per
// colour run, not through lipgloss per cell, to stay well inside the 4 ms
// budget at 120×36.

const (
	introSeconds    = 8
	introFrames     = introFPS * introSeconds // the intro ends by itself after this
	introMinWidth   = 50                      // below this (or introMinHeight) it never plays
	introMinHeight  = 24
	introStarPeriod = 130 // the starfield repeats every 130 columns
	introWave       = 44  // logo wave: wavelength in columns
	introWaveFrames = 66  // logo wave: 2.2 s per period
	introPulse      = 96  // logo colour pulse: 3.2 s
	introPulseSteps = 24
	introBarFrames  = 72 // copper bars: 2.4 s per swing
	introScrollCyc  = 90 // scroller colours: mint → sky → lavender over 3 s
	introFloorPair  = 36 // floor: one tile pair every 1.2 s
	introCatBlink   = 120
)

// introTheme is what the intro needs to know about the terminal: the light or
// dark token column, and how many colours it can show.
type introTheme struct {
	dark    bool
	profile termenv.Profile
}

func currentIntroTheme() introTheme {
	return introTheme{dark: lipgloss.HasDarkBackground(), profile: lipgloss.ColorProfile()}
}

// --- logo -------------------------------------------------------------------

// introLogo is a wordmark as a pixel grid: each pixel is half a terminal row.
type introLogo struct {
	width   int
	pix     [][]bool
	torkEnd int // columns [0, torkEnd) are "tork"
	wEnd    int // columns [torkEnd, wEnd) are "-w-", the rest "debrid"
}

// rows is how many terminal rows the logo takes with its ±1 row wave.
func (l *introLogo) rows() int { return (len(l.pix) + 4 + 1) / 2 }

// The bold letters (design.md § Logos): the solid strokes of the ANSI Shadow
// face, 5 rows, each glyph row including its trailing gap.
var introGlyphTable = map[rune][5]string{
	'T': {"████████ ", "   ██    ", "   ██    ", "   ██    ", "   ██    "},
	'O': {" ██████  ", "██    ██ ", "██    ██ ", "██    ██ ", " ██████  "},
	'R': {"██████  ", "██   ██ ", "██████  ", "██   ██ ", "██   ██ "},
	'K': {"██   ██ ", "██  ██  ", "█████   ", "██  ██  ", "██   ██ "},
	'D': {"██████  ", "██   ██ ", "██   ██ ", "██   ██ ", "██████  "},
	'E': {"███████ ", "██      ", "█████   ", "██      ", "███████ "},
	'B': {"██████  ", "██   ██ ", "██████  ", "██   ██ ", "██████  "},
	'I': {"██ ", "██ ", "██ ", "██ ", "██ "},
	'w': {"          ", "          ", "██  █  ██ ", "██ ███ ██ ", " ███ ███  "},
	'-': {"      ", "      ", "      ", "█████ ", "      "},
	' ': {"  ", "  ", "  ", "  ", "  "},
}

func introWord(word string, row int) []rune {
	var out []rune
	for _, r := range word {
		out = append(out, []rune(introGlyphTable[r][row])...)
	}
	return out
}

// bigIntroLogo is TORK -w- DEBRID, 103 columns and 10 half rows.
var bigIntroLogo = func() introLogo {
	l := introLogo{}
	for row := 0; row < 5; row++ {
		tork, w, debrid := introWord("TORK", row), introWord(" -w- ", row), introWord("DEBRID", row)
		line := append(append(append([]rune{}, tork...), w...), debrid...)
		l.torkEnd, l.wEnd, l.width = len(tork), len(tork)+len(w), len(line)
		px := make([]bool, len(line))
		for i, r := range line {
			px[i] = r == '█'
		}
		l.pix = append(l.pix, px, px) // a full block is two half rows
	}
	return l
}()

// smallIntroLogo is the Home wordmark (logo.go), 43 columns, for terminals
// too small for the bold letters. The cat's feet leave the "w" gap empty.
var smallIntroLogo = func() introLogo {
	rows := [2]string{
		logoTork[0] + "       " + logoDebrid[0],
		logoTork[1] + logoW[1] + logoDebrid[1],
	}
	l := introLogo{torkEnd: len([]rune(logoTork[0])), wEnd: len([]rune(logoTork[0])) + len([]rune(logoW[1]))}
	for _, line := range rows {
		runes := []rune(line)
		l.width = len(runes)
		top, bot := make([]bool, len(runes)), make([]bool, len(runes))
		for i, r := range runes {
			top[i] = r == '▀' || r == '█'
			bot[i] = r == '▄' || r == '█'
		}
		l.pix = append(l.pix, top, bot)
	}
	return l
}()

// --- layout -----------------------------------------------------------------

type introLayout struct {
	logo       *introLogo
	plateTop   int
	plateLeft  int
	plateW     int
	plateRows  int
	versionRow int
	scrollTop  int // the scroller band is 5 rows: scrollTop … scrollTop+4
	catTop     int
	floorTop   int
	ruleRow    int
	footerRow  int
}

const (
	introScrollRows = 5
	introFloorRows  = 6
	introCatRows    = 3
)

// introFits reports whether the intro can play at this size.
func introFits(width, height int) bool {
	_, ok := layoutIntro(width, height)
	return ok
}

func layoutIntro(w, h int) (introLayout, bool) {
	if w < introMinWidth || h < introMinHeight {
		return introLayout{}, false
	}
	bottom := introCatRows + introFloorRows + 2 // cat, floor, rule, footer
	body := func(l *introLogo) int { return l.rows() + 2 + 2 + introScrollRows }
	logo := &bigIntroLogo
	if w < logo.width+4 || h < bottom+body(logo)+1 {
		logo = &smallIntroLogo
	}
	if w < logo.width+4 || h < bottom+body(logo) {
		return introLayout{}, false
	}
	L := introLayout{logo: logo, footerRow: h - 1, ruleRow: h - 2}
	L.floorTop = L.ruleRow - introFloorRows
	L.catTop = L.floorTop - introCatRows
	L.plateRows = logo.rows() + 2
	L.plateW = logo.width + 4
	L.plateLeft = (w - L.plateW) / 2
	if h >= bottom+body(logo)+1 {
		L.plateTop = 1
	}
	L.versionRow = L.plateTop + L.plateRows + 1
	avail := L.catTop - (L.versionRow + 1)
	L.scrollTop = L.versionRow + 1 + (avail-introScrollRows)/2
	return L, true
}

// --- palette ----------------------------------------------------------------

// Colour ids in an introPalette.
const (
	icGround = iota
	icLine
	icMuted
	icFaint
	icAccent
	icRoute
	icBar0                                 // 3 bars × 3 shades
	icFloor0  = icBar0 + 9                 // 6 rows × 2 tiles
	icScroll0 = icFloor0 + 12              // scroller colour cycle
	icTork0   = icScroll0 + introScrollCyc // "tork" pulse
	icW0      = icTork0 + introPulseSteps + 1
	icDebrid0 = icW0 + introPulseSteps + 1
	icCount   = icDebrid0 + introPulseSteps + 1
)

const icNone int16 = -1

// introPalette holds the SGR parameters for every colour id: fg[id] for the
// foreground, bg[id] for the background. plain means the profile has no
// colour, so the frame is written without any escape sequence.
type introPalette struct {
	fg, bg []string
	plain  bool
}

var introPalettes sync.Map // introTheme → *introPalette

var (
	introBarShades  = [6]int{0, 1, 2, 2, 1, 0}
	introBarMix     = [3]float64{0.10, 0.22, 0.40}
	introFloorTiles = [introFloorRows]int{2, 3, 4, 6, 8, 11}
	introFloorLight = [introFloorRows]float64{0.30, 0.45, 0.60, 0.75, 0.88, 1}
	introScrollBob  = [12]int{0, -1, -2, -2, -2, -1, 0, 1, 2, 2, 2, 1}
)

func introPaletteFor(th introTheme) *introPalette {
	if p, ok := introPalettes.Load(th); ok {
		return p.(*introPalette)
	}
	hex := func(t token) string {
		if th.dark {
			return tokenTable[t].dark
		}
		return tokenTable[t].light
	}
	ground, mint, sky, lav := hex(tokGround), hex(tokAccent), hex(tokSky), hex(tokRoute)
	const white = "#FFFFFF"
	cols := make([]string, icCount)
	cols[icGround], cols[icLine], cols[icMuted], cols[icFaint], cols[icAccent], cols[icRoute] =
		ground, hex(tokLine), hex(tokMuted), hex(tokFaint), mint, lav
	for b, c := range [3]string{mint, sky, lav} {
		for s, m := range introBarMix {
			cols[icBar0+b*3+s] = mixHex(ground, c, m)
		}
	}
	tiles := [2]string{mixHex(ground, mint, 0.30), mixHex(ground, lav, 0.16)}
	for row, light := range introFloorLight {
		for k, tile := range tiles {
			cols[icFloor0+row*2+k] = mixHex(ground, tile, light)
		}
	}
	cycle := [3]string{mint, sky, lav}
	third := introScrollCyc / 3
	for i := 0; i < introScrollCyc; i++ {
		seg := i / third
		cols[icScroll0+i] = mixHex(cycle[seg], cycle[(seg+1)%3], float64(i%third)/float64(third))
	}
	for k := 0; k <= introPulseSteps; k++ {
		f := float64(k) / introPulseSteps
		cols[icTork0+k] = mixHex(mint, white, 0.35*f)
		cols[icDebrid0+k] = mixHex(lav, white, 0.35*f)
		cols[icW0+k] = mixHex(mint, lav, f)
	}
	p := &introPalette{fg: make([]string, icCount), bg: make([]string, icCount), plain: th.profile == termenv.Ascii}
	for i, c := range cols {
		col := th.profile.Color(c)
		if col == nil {
			continue
		}
		p.fg[i], p.bg[i] = col.Sequence(false), col.Sequence(true)
	}
	actual, _ := introPalettes.LoadOrStore(th, p)
	return actual.(*introPalette)
}

// --- cell buffer ------------------------------------------------------------

type introCell struct {
	ch     rune
	fg, bg int16
	bold   bool
}

type introCanvas struct {
	w, h  int
	cells []introCell
}

func newIntroCanvas(w, h int) *introCanvas {
	c := &introCanvas{w: w, h: h, cells: make([]introCell, w*h)}
	for i := range c.cells {
		c.cells[i] = introCell{ch: ' ', fg: icNone, bg: icNone}
	}
	return c
}

func (c *introCanvas) set(x, y int, ch rune, fg, bg int16, bold bool) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	c.cells[y*c.w+x] = introCell{ch: ch, fg: fg, bg: bg, bold: bold}
}

// text writes s at (x, y) in one colour on the terminal's own background.
func (c *introCanvas) text(x, y int, s string, fg int16, bold bool) {
	for _, r := range s {
		if r != ' ' {
			c.set(x, y, r, fg, icNone, bold)
		}
		x++
	}
}

// halves draws one cell from its top and bottom half colours (icNone = clear),
// on bg where both are clear or for the clear half.
func (c *introCanvas) halves(x, y int, top, bot, bg int16) {
	switch {
	case top == icNone && bot == icNone:
		c.set(x, y, ' ', icNone, bg, false)
	case top == bot:
		c.set(x, y, '█', top, bg, false)
	case bot == icNone:
		c.set(x, y, '▀', top, bg, false)
	case top == icNone:
		c.set(x, y, '▄', bot, bg, false)
	default:
		c.set(x, y, '▀', top, bot, false)
	}
}

func (c *introCanvas) render(p *introPalette) string {
	var b strings.Builder
	b.Grow(c.w * c.h * 8)
	for y := 0; y < c.h; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		styled := false
		cf, cb, cbold := icNone, icNone, false
		for _, cell := range c.cells[y*c.w : (y+1)*c.w] {
			if !p.plain {
				same := cell.bg == cb && (cell.ch == ' ' || (cell.fg == cf && cell.bold == cbold))
				if !same {
					b.WriteString("\x1b[0")
					if cell.fg != icNone && p.fg[cell.fg] != "" {
						b.WriteByte(';')
						b.WriteString(p.fg[cell.fg])
					}
					if cell.bg != icNone && p.bg[cell.bg] != "" {
						b.WriteByte(';')
						b.WriteString(p.bg[cell.bg])
					}
					if cell.bold {
						b.WriteString(";1")
					}
					b.WriteByte('m')
					cf, cb, cbold = cell.fg, cell.bg, cell.bold
					styled = true
				}
			}
			b.WriteRune(cell.ch)
		}
		if styled {
			b.WriteString("\x1b[0m")
		}
	}
	return b.String()
}

// --- the frame --------------------------------------------------------------

type introStarRow struct {
	layer int
	cols  []int
}

var (
	introStarGlyph = [3]rune{'.', '·', '*'}
	introStarColor = [3]int16{icFaint, icMuted, icMuted}
	introStarSpeed = [3]int{5, 10, 20} // cells per second
	introStarRows  = func() []introStarRow {
		seed := int64(23) // Park–Miller, as on the canvas, so the sky is the same every run
		rnd := func() float64 {
			seed = seed * 16807 % 2147483647
			return float64(seed-1) / 2147483646
		}
		rows := make([]introStarRow, 128)
		for i := range rows {
			rows[i].layer = int(rnd() * 3)
			n := 1 + int(rnd()*3)
			for k := 0; k < n; k++ {
				rows[i].cols = append(rows[i].cols, int(rnd()*introStarPeriod))
			}
		}
		return rows
	}()
)

// introScrollText is the scroller message with the running version.
func introScrollText(version string) []rune {
	return []rune("TORK-W-DEBRID " + version + "   ···   torrents in, Real-Debrid out   ···   " +
		"4 connections per file   ···   link your account with a code, no password   ···   " +
		"greetings to melqtx and everyone seeding linux isos   ···   " +
		"settings › play the intro to see this again   ···   press any key   ···   ")
}

const (
	introFooterLeftKey   = "any key"
	introFooterLeftLabel = " skip"
	introFooterRight     = "30 fps · replay any time from settings › play the intro"
)

// introFrame draws frame n (30 per second) of the intro at width×height.
// Frame 0 is the still poster shown when motion is off.
func introFrame(n, width, height int, th introTheme, version string) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	c := newIntroCanvas(width, height)
	L, ok := layoutIntro(width, height)
	if !ok {
		introTooSmall(c, version)
		return c.render(introPaletteFor(th))
	}
	// 1. starfield
	for y, i := 0, 0; y < L.floorTop; y, i = y+2, i+1 {
		row := introStarRows[i%len(introStarRows)]
		shift := floorDiv(n*introStarSpeed[row.layer], introFPS)
		for _, col := range row.cols {
			for x := posMod(col-shift, introStarPeriod); x < width; x += introStarPeriod {
				c.set(x, y, introStarGlyph[row.layer], introStarColor[row.layer], icNone, false)
			}
		}
	}

	// 2. copper bars, full width, swinging behind the plate in half rows
	half := make([]int16, 2*height)
	for i := range half {
		half[i] = icNone
	}
	travel := float64(2*L.plateRows - 6)
	for b := 0; b < 3; b++ {
		phase := float64(n)/introBarFrames + float64(b)/3
		top := 2*L.plateTop + int(math.Round(travel/2+travel/2*math.Sin(2*math.Pi*phase)))
		for j, shade := range introBarShades {
			if hy := top + j; hy >= 0 && hy < len(half) {
				half[hy] = int16(icBar0 + b*3 + shade)
			}
		}
	}
	for y := 0; y < height; y++ {
		top, bot := half[2*y], half[2*y+1]
		if top == icNone && bot == icNone {
			continue
		}
		for x := 0; x < width; x++ {
			c.halves(x, y, top, bot, icNone)
		}
	}

	// 3. the logo on its plate
	for y := L.plateTop; y < L.plateTop+L.plateRows; y++ {
		for x := L.plateLeft; x < L.plateLeft+L.plateW; x++ {
			c.set(x, y, ' ', icNone, icGround, false)
		}
	}
	pulse := 0.5 - 0.5*math.Cos(2*math.Pi*float64(n)/introPulse)
	k := int16(math.Round(pulse * introPulseSteps))
	logo := L.logo
	lx, ly := L.plateLeft+2, L.plateTop+1
	for x := 0; x < logo.width; x++ {
		off := int(math.Round(2 + 2*math.Sin(2*math.Pi*(float64(x)/introWave-float64(n)/introWaveFrames))))
		col := icTork0 + k
		switch {
		case x >= logo.wEnd:
			col = icDebrid0 + k
		case x >= logo.torkEnd:
			col = icW0 + k
		}
		px := func(hy int) int16 {
			if p := hy - off; p >= 0 && p < len(logo.pix) && logo.pix[p][x] {
				return col
			}
			return icNone
		}
		for r := 0; r < logo.rows(); r++ {
			if top, bot := px(2*r), px(2*r+1); top != icNone || bot != icNone {
				c.halves(lx+x, ly+r, top, bot, icGround)
			}
		}
	}

	// 4. version, letter-spaced
	if version != "" {
		spaced := strings.Join(strings.Split(version, ""), " ")
		c.text((width-len([]rune(spaced)))/2, L.versionRow, spaced, icMuted, false)
	}

	// 5. sine scroller
	text := introScrollText(version)
	scroll := floorDiv(n*15, introFPS)
	mid := L.scrollTop + introScrollRows/2
	for x := 0; x < width; x++ {
		i := x + scroll
		ch := text[posMod(i, len(text))]
		if ch == ' ' {
			continue
		}
		bob := introScrollBob[posMod(floorDiv(2*n-3*i, 6), len(introScrollBob))]  // 0.1 s a step, 0.05 s per letter
		col := int16(icScroll0 + posMod(floorDiv(10*n-18*i, 10), introScrollCyc)) // 0.06 s per letter
		c.set(x, mid+bob, ch, col, icNone, true)
	}

	// 6. floor
	for row, tile := range introFloorTiles {
		shift := floorDiv(n*2*tile, introFloorPair)
		for x := 0; x < width; x++ {
			u := x - width/2 + (row%2)*tile + shift
			c.set(x, L.floorTop+row, ' ', icNone, int16(icFloor0+row*2+posMod(floorDiv(u, tile), 2)), false)
		}
	}

	// 7. the cat, blinking every 4 s
	cx := (width - 7) / 2
	c.text(cx, L.catTop, ` /\_/\`, icMuted, false)
	c.text(cx, L.catTop+1, "(     )", icMuted, false)
	eyes := "^.^"
	if f := posMod(n, introCatBlink); f >= 110 && f < 116 {
		eyes = "-.-"
	}
	c.text(cx+2, L.catTop+1, eyes, icAccent, false)
	c.text(cx, L.catTop+2, " > ^ <", icMuted, false)

	introFooter(c, L.ruleRow, L.footerRow)
	return c.render(introPaletteFor(th))
}

func introFooter(c *introCanvas, ruleRow, footerRow int) {
	for x := 0; x < c.w; x++ {
		c.set(x, ruleRow, '─', icLine, icNone, false)
	}
	c.text(0, footerRow, introFooterLeftKey, icAccent, false)
	c.text(len(introFooterLeftKey), footerRow, introFooterLeftLabel, icMuted, false)
	left := len(introFooterLeftKey + introFooterLeftLabel)
	right := len([]rune(introFooterRight))
	if left+3+right <= c.w {
		c.text(c.w-right, footerRow, introFooterRight, icMuted, false)
	}
}

// introTooSmall is the still frame for terminals too small for the intro
// (a replay with motion off still shows something until a key).
func introTooSmall(c *introCanvas, version string) {
	width := len("tork-w-debrid")
	if version != "" {
		width += 2 + len([]rune(version))
	}
	y := max(0, (c.h-1)/2)
	x := max(0, (c.w-width)/2)
	c.text(x, y, "tork", icAccent, true)
	c.text(x+4, y, "-w-", icMuted, false)
	c.text(x+7, y, "debrid", icRoute, true)
	if version != "" {
		c.text(x+15, y, version, icMuted, false)
	}
	if c.h >= 3 {
		introFooter(c, c.h-2, c.h-1)
	}
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
