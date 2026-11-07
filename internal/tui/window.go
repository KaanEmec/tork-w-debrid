package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// listWindow owns the cursor+offset pair of a scrolling list, replacing the
// per-view copies of the same clamp-and-follow logic.
type listWindow struct {
	cursor int
	offset int
}

// move shifts the cursor by delta within [0,total) and keeps it visible.
func (w *listWindow) move(delta, total, visible int) {
	w.cursor += delta
	w.clamp(total, visible)
}

func (w *listWindow) home() { w.cursor, w.offset = 0, 0 }

func (w *listWindow) end(total, visible int) {
	w.cursor = total - 1
	w.clamp(total, visible)
}

// clamp normalizes cursor and offset against the current list and window
// sizes, returning the visible range [start, end).
func (w *listWindow) clamp(total, visible int) (start, end int) {
	if visible < 1 {
		visible = 1
	}
	w.cursor = max(0, min(total-1, w.cursor))
	if w.cursor < w.offset {
		w.offset = w.cursor
	}
	if w.cursor >= w.offset+visible {
		w.offset = w.cursor - visible + 1
	}
	w.offset = max(0, min(w.offset, max(0, total-visible)))
	return w.offset, min(total, w.offset+visible)
}

// renderWindow draws the rows visible through w, one line each: a ▍ gutter and
// the selection background on the cursor row, padded to exactly visible lines
// so screen bodies keep a stable height. render must not include the gutter.
func renderWindow(w *listWindow, total, visible, width int, render func(i int, selected bool) string) string {
	start, end := w.clamp(total, visible)
	rowWidth := max(0, width-1) // every row reserves one display cell for its gutter
	var b strings.Builder
	for i := start; i < end; i++ {
		content := truncate(render(i, i == w.cursor), rowWidth)
		if i == w.cursor {
			b.WriteString(styleSelBar.Background(colRaised).Render(glyphs.cursor))
			b.WriteString(selectedRow(content, rowWidth))
		} else {
			b.WriteString(" " + padRight(content, rowWidth))
		}
		b.WriteString("\n")
	}
	for i := end - start; i < visible; i++ {
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// selectedRow fills a row with the raised selection background and makes its
// plain text bold fg, while cells rendered with their own colour (a ★, amber
// seeds, a rail) keep it. Nested styles reset the background at every span
// end, so the selection style is re-applied after each reset.
func selectedRow(content string, width int) string {
	body := padRight(content, width)
	prefix := selectionPrefix()
	if prefix == "" {
		return styleSelected.Render(ansi.Strip(body))
	}
	return prefix + strings.ReplaceAll(body, ansiReset, ansiReset+prefix) + ansiReset
}

const ansiReset = "\x1b[0m"

// selectionPrefix is the escape sequence that opens styleSelected in the
// current colour profile ("" when the profile emits none).
func selectionPrefix() string {
	s := styleSelected.Render("x")
	if i := strings.Index(s, "x"); i > 0 {
		return s[:i]
	}
	return ""
}
