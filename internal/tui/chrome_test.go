package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Screen chrome tests for J8.2: the header, rule, footer and toast line at
// 120×36, 80×24 and 50×20, and the three states of the ☁ RD marker.

var chromeSizes = [][2]int{{120, 36}, {80, 24}, {50, 20}}

func plainLines(view string) []string {
	lines := strings.Split(ansi.Strip(view), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func isRule(line string, width int) bool {
	line = strings.TrimSpace(line)
	return line != "" && strings.Trim(line, "─") == "" && lipgloss.Width(line) == width
}

func TestChromeHeaderAndFooterAtEachSize(t *testing.T) {
	for _, size := range chromeSizes {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			a := downloadsGoldenApp(w, h)
			view := a.View()
			assertRenderFits(t, view, w)
			lines := plainLines(view)
			if len(lines) != h {
				t.Fatalf("rendered %d lines, want %d", len(lines), h)
			}
			cw := a.contentWidth()
			header := strings.TrimSpace(lines[0])
			if w < 60 {
				if !strings.HasPrefix(header, "tork  ❯ downloads") {
					t.Errorf("narrow header = %q, want the short form", header)
				}
				if strings.Contains(header, "MiB/s") {
					t.Errorf("narrow header kept the right-hand fact: %q", header)
				}
			} else {
				if !strings.HasPrefix(header, "tork-w-debrid  ›  downloads") {
					t.Errorf("header = %q", header)
				}
				if !strings.HasSuffix(header, "↓ 27.3 MiB/s · ☁︎ 1") {
					t.Errorf("header fact missing: %q", header)
				}
			}
			if !isRule(lines[1], cw) {
				t.Errorf("line 2 is not a %d-cell rule: %q", cw, lines[1])
			}
			if !isRule(lines[h-2], cw) {
				t.Errorf("line above the footer is not a rule: %q", lines[h-2])
			}
			footer := lines[h-1]
			if !strings.Contains(footer, "? keys") {
				t.Errorf("footer lost the ? pointer: %q", footer)
			}
			if !strings.Contains(footer, "☁︎ RD") {
				t.Errorf("footer lost the RD marker: %q", footer)
			}
			strip := strings.TrimSpace(footer[:strings.Index(footer, "? keys")+len("? keys")])
			if n := strings.Count(strip, "   ") + 1; n > 6 {
				t.Errorf("footer carries %d hints, want at most 5 plus ? keys: %q", n, footer)
			}
		})
	}
}

func TestChromeToastLineTakesOneBodyLine(t *testing.T) {
	for _, size := range chromeSizes {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			a := downloadsGoldenApp(w, h)
			before := plainLines(a.View())
			bodyBefore := a.bodyHeight()
			a.showToast("queued · Sintel", toastOK, toastQuick)
			view := a.View()
			assertRenderFits(t, view, w)
			after := plainLines(view)
			if len(after) != h {
				t.Fatalf("with a toast the screen is %d lines, want %d", len(after), h)
			}
			if a.bodyHeight() != bodyBefore-1 {
				t.Fatalf("body height %d with a toast, want %d", a.bodyHeight(), bodyBefore-1)
			}
			if got := strings.TrimSpace(after[h-3]); got != "✓ queued · Sintel" {
				t.Errorf("toast line = %q, want it just above the footer rule", got)
			}
			if !isRule(after[h-2], a.contentWidth()) || after[h-1] != before[h-1] {
				t.Error("the toast moved or replaced the footer")
			}
			// The header and the body's first lines are untouched: the toast
			// takes the body's last line, never covers it.
			for i := 0; i < h-4; i++ {
				if after[i] != before[i] {
					t.Fatalf("line %d changed under the toast:\n%q\n%q", i+1, before[i], after[i])
				}
			}
		})
	}
}

func TestChromeToastTones(t *testing.T) {
	for _, tt := range []struct {
		tone toastTone
		mark string
	}{{toastOK, "✓ "}, {toastWarn, "! "}, {toastRoute, glyphs.cloud + " "}} {
		a := downloadsGoldenApp(80, 24)
		a.showToast("hello", tt.tone, time.Second)
		if got := ansi.Strip(a.toastLine(80)); got != tt.mark+"hello" {
			t.Errorf("tone %d toast = %q, want %q", tt.tone, got, tt.mark+"hello")
		}
	}
}

func TestChromeErrorReplacesFooter(t *testing.T) {
	for _, size := range chromeSizes {
		a := downloadsGoldenApp(size[0], size[1])
		a.errText = "disk full"
		lines := plainLines(a.View())
		footer := strings.TrimSpace(lines[len(lines)-1])
		if !strings.HasPrefix(footer, "disk full") || strings.Contains(footer, "? keys") || strings.Contains(footer, "RD") {
			t.Errorf("%dx%d footer with an error = %q", size[0], size[1], footer)
		}
	}
}

func TestHomeFooterAtEachSize(t *testing.T) {
	for _, size := range chromeSizes {
		a := homeGoldenApp(size[0], size[1])
		view := a.View()
		assertRenderFits(t, view, size[0])
		lines := plainLines(view)
		if len(lines) != size[1] {
			t.Fatalf("home %dx%d rendered %d lines", size[0], size[1], len(lines))
		}
		if strings.Contains(lines[0], "tork-w-debrid  ›") {
			t.Errorf("home %dx%d drew the context header", size[0], size[1])
		}
		if !strings.Contains(lines[len(lines)-1], "? keys") {
			t.Errorf("home %dx%d footer = %q", size[0], size[1], lines[len(lines)-1])
		}
	}
}

// TestRouteMarkerStates covers the three Real-Debrid states: the lavender
// ☁ RD shows only when an account is linked and "use for downloads" is on.
func TestRouteMarkerStates(t *testing.T) {
	marker := ansi.Strip(routeMarker())
	if marker != "☁︎ RD" {
		t.Fatalf("routeMarker() = %q", marker)
	}
	for _, tt := range []struct {
		name            string
		linked, useForD bool
		want            bool
	}{
		{"linked and on", true, true, true},
		{"linked but off", true, false, false},
		{"not linked", false, false, false},
		{"not linked, switch left on", false, true, false},
	} {
		for _, scr := range []screen{screenDownloads, screenSearch} {
			var a *App
			if scr == screenSearch {
				a = homeGoldenApp(120, 36)
			} else {
				a = downloadsGoldenApp(120, 36)
			}
			a.SetDebridStatus(tt.linked, tt.useForD)
			lines := plainLines(a.View())
			footer := lines[len(lines)-1]
			if got := strings.Contains(footer, marker); got != tt.want {
				t.Errorf("%s on screen %d: footer shows RD = %v, want %v (%q)", tt.name, scr, got, tt.want, footer)
			}
			if got := strings.Contains(ansi.Strip(a.footerTail()), marker); got != tt.want {
				t.Errorf("%s: footerTail RD = %v, want %v", tt.name, got, tt.want)
			}
		}
	}
	SetASCIIGlyphs(true)
	defer SetASCIIGlyphs(false)
	if got := ansi.Strip(routeMarker()); got != "RD" {
		t.Fatalf("ASCII routeMarker() = %q", got)
	}
}
