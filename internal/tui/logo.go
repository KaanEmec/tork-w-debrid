package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The Home logo (design.md § Logos): two rows of half blocks, 43 cells wide.
// "tork" cycles mint, "debrid" cycles lavender, the small "w" zigzag is sky,
// and the cat sits on the "w" with its feet in the gap of the top row.
//
//	                /\_/\
//	               ( ^.^ )
//	▀█▀ █▀█ █▀█ █▄▀ > ^ < █▀▄ █▀▀ █▀▄ █▀█ █ █▀▄
//	 █  █▄█ █▀▄ █▀▄ ▀▄▀▄▀ █▄▀ ██▄ ██▀ █▀▄ █ █▄▀
const homeLogoWidth = 43

var (
	logoTork   = [2]string{`▀█▀ █▀█ █▀█ █▄▀`, ` █  █▄█ █▀▄ █▀▄`}
	logoW      = [2]string{` > ^ < `, ` ▀▄▀▄▀ `}
	logoDebrid = [2]string{`█▀▄ █▀▀ █▀▄ █▀█ █ █▀▄`, `█▄▀ ██▄ ██▀ █▀▄ █ █▄▀`}
)

// homeLogo draws the cat and the wordmark at the given frame. Frame 0 is the
// still poster: eyes open, the colour cycle at its start.
func homeLogo(frame int) string {
	if glyphs.ascii {
		return brandMark()
	}
	lines := []string{
		padRight(strings.Repeat(" ", 16)+styleDim.Render(`/\_/\`), homeLogoWidth),
		padRight(strings.Repeat(" ", 15)+styleDim.Render("( ")+styleOK.Render(catEyes(frame))+styleDim.Render(" )"), homeLogoWidth),
	}
	for row := 0; row < 2; row++ {
		w := styleDim.Render(logoW[row]) // the cat's feet
		if row == 1 {
			w = styleSeeding.Render(logoW[row])
		}
		lines = append(lines, cycleWord(logoTork[row], rampTork, frame)+w+cycleWord(logoDebrid[row], rampDebrid, frame))
	}
	return strings.Join(lines, "\n")
}

// cycleWord colours each column of word from the ramp, joining runs of the
// same colour so a frame costs a handful of renders rather than one per cell.
func cycleWord(word string, ramp []lipgloss.Style, frame int) string {
	runes := []rune(word)
	var b strings.Builder
	start := 0
	cur := logoCycleStyle(ramp, frame, 0, len(runes))
	for x := 1; x <= len(runes); x++ {
		var next lipgloss.Style
		if x < len(runes) {
			next = logoCycleStyle(ramp, frame, x, len(runes))
			if sameStyle(next, cur) {
				continue
			}
		}
		b.WriteString(cur.Render(string(runes[start:x])))
		start, cur = x, next
	}
	return b.String()
}

func sameStyle(a, b lipgloss.Style) bool {
	return a.GetForeground() == b.GetForeground()
}
