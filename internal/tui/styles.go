package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/kaanemec/tork-w-debrid/internal/provider"
)

// Styles are drawn from the Nightglass tokens in theme.go. The older names
// stay as aliases so the screens keep reading naturally.
var (
	styleBrand = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(colFg)
	styleFg    = lipgloss.NewStyle().Foreground(colFg)
	styleText  = lipgloss.NewStyle().Foreground(colText)
	styleDim   = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint = lipgloss.NewStyle().Foreground(colFaint) // decoration only, never words
	styleRule  = lipgloss.NewStyle().Foreground(colLine)

	styleSelected = lipgloss.NewStyle().Background(colRaised).Foreground(colFg).Bold(true)
	styleSelBar   = lipgloss.NewStyle().Foreground(colAccent)

	styleSeeders  = lipgloss.NewStyle().Foreground(colText)
	styleLeechers = lipgloss.NewStyle().Foreground(colMuted)
	styleErr      = lipgloss.NewStyle().Foreground(colRose).Bold(true)
	styleOK       = lipgloss.NewStyle().Foreground(colAccent)
	styleMatch    = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleBest     = lipgloss.NewStyle().Foreground(colAmber).Bold(true) // ★ best and caution
	styleWarn     = lipgloss.NewStyle().Foreground(colAmber)
	styleSeeding  = lipgloss.NewStyle().Foreground(colSky)
	styleStateTag = lipgloss.NewStyle().Foreground(colMuted)

	styleKey   = lipgloss.NewStyle().Foreground(colAccent)
	styleKeyLb = lipgloss.NewStyle().Foreground(colMuted)
)

var (
	styleHealthGood = lipgloss.NewStyle().Foreground(colAccent)
	styleHealthMid  = lipgloss.NewStyle().Foreground(colAmber)
	styleHealthBad  = lipgloss.NewStyle().Foreground(colRose)
)

// healthDot returns a swarm-health indicator colored by seeder count.
func healthDot(seeders int) string {
	switch {
	case seeders >= 50:
		return styleHealthGood.Render("●")
	case seeders >= 5:
		return styleHealthMid.Render("●")
	case seeders >= 1:
		return styleHealthBad.Render("●")
	}
	return styleFaint.Render("○")
}

// providerTag renders a provider name. Providers used to carry their own
// colours; colour now carries meaning, so every name renders in text.
func providerTag(name string) string {
	return styleText.Render(name)
}

// providerBracket renders a provider as a small tag: [knaben].
func providerBracket(name string) string {
	return styleFaint.Render("[") + providerTag(name) + styleFaint.Render("]")
}

// sourceTag names the index a row came from, with a faint "+N" when other
// indexes listed the same infohash. The count reads as corroboration rather
// than clutter: a release three indexes agree on is rarely the risky pick, and
// its magnet is carrying all three tracker sets.
func sourceTag(res provider.Result, plain bool) string {
	tag := providerTag(res.Provider)
	if plain {
		tag = res.Provider
	}
	return tag + mergeSuffix(res, plain)
}

// sourceCol is sourceTag in the graph view's bracketed form: [knaben]+2.
func sourceCol(res provider.Result, plain bool) string {
	return providerCol(res.Provider, plain) + mergeSuffix(res, plain)
}

func mergeSuffix(res provider.Result, plain bool) string {
	if len(res.AlsoOn) == 0 {
		return ""
	}
	return colorize(plain, styleDim, fmt.Sprintf("+%d", len(res.AlsoOn)))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanSpeed(bps float64) string {
	if bps <= 0 {
		return "0 B/s"
	}
	return humanBytes(int64(bps)) + "/s"
}

func fmtETA(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	if d > 99*time.Hour {
		return "∞"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// truncate shortens s to at most w display cells (ANSI- and wide-rune-aware),
// ending with an ellipsis when anything was cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return ansi.Truncate(s, 1, "")
	}
	return ansi.Truncate(s, w, glyphs.ellipsis)
}
