package tui

import (
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

// ASCII fallback (E8, design.md § Fallbacks, J8.9). Terminals without Unicode
// block or box characters get the asciiGlyphs set, and asciiFold turns the
// remaining one-cell symbols that screens draw directly into printable ASCII.

// DetectASCII reports whether this process's terminal needs the ASCII glyph
// set. TORKWD_ASCII=1 or 0 forces the answer. Otherwise it is true for
// TERM=dumb or linux (the kernel console lacks box and block glyphs), for a
// locale that is not UTF-8, and for a terminal whose colour profile is plain
// ASCII (unless that is only NO_COLOR, which asks for no colour, not for no
// Unicode).
func DetectASCII() bool {
	tty := term.IsTerminal(os.Stdout.Fd())
	profile := termenv.Ascii
	if tty {
		profile = termenv.NewOutput(os.Stdout).EnvColorProfile()
	}
	return detectASCIIEnv(os.Getenv, tty, profile, runtime.GOOS)
}

func detectASCIIEnv(getenv func(string) string, stdoutTTY bool, profile termenv.Profile, goos string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("TORKWD_ASCII"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv("TERM"))) {
	case "dumb", "linux":
		return true
	}
	if goos != "windows" {
		for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
			if v := strings.TrimSpace(getenv(k)); v != "" {
				l := strings.ToLower(v)
				return !(strings.Contains(l, "utf-8") || strings.Contains(l, "utf8"))
			}
		}
	}
	return stdoutTTY && profile == termenv.Ascii && getenv("NO_COLOR") == ""
}

// asciiFolder maps every one-cell symbol the screens draw to ASCII, keeping
// widths so layout does not move. User text (titles, paths) is left alone.
var asciiFolder = strings.NewReplacer(
	"☁︎", "RD", "☁", "RD", "︎", "",
	"─", "-", "━", "=", "│", "|", "┃", "|", "┌", "+", "┐", "+", "└", "+", "┘", "+",
	"╭", "+", "╮", "+", "╯", "+", "╰", "+", "├", "+", "┤", "+", "┬", "+", "┴", "+", "┼", "+",
	"═", "=", "║", "|", "╔", "+", "╗", "+", "╚", "+", "╝", "+",
	"·", "-", "•", "*", "—", "-", "–", "-", "…", ".", "›", ">", "❯", ">", "‹", "<", "»", ">", "«", "<",
	"↓", "v", "↑", "^", "←", "<", "→", ">", "↵", "~", "⏎", "~",
	"✓", "+", "✔", "+", "✗", "x", "✘", "x", "×", "x", "Ⅱ", "p", "★", "*", "☆", "*",
	"▾", "v", "▸", ">", "▴", "^", "▶", "*", "◀", "<", "●", "*", "○", "o", "◆", "*", "◇", "o",
	"▍", ">", "▌", ">", "█", "#", "▓", "=", "▒", "=", "░", ".", "▕", "[", "▏", "]",
	"▀", "#", "▄", "#", "▮", "#", "▯", ".", "▤", "#", "◉", "o",
	"✉", "@", "⚠", "!", "⚙", "*", "♪", "~", "’", "'", "≤", "<", "∞", "~", "±", "+",
	"⠋", "|", "⠙", "/", "⠹", "-", "⠸", "\\", "⠼", "|", "⠴", "/", "⠦", "-", "⠧", "\\", "⠇", "|", "⠏", "/",
)

func asciiFold(view string) string { return asciiFolder.Replace(view) }
