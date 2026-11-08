package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// keyHint is one documented shortcut. `strip` marks the few that earn a place
// on the always-visible footer; everything else lives behind `?`.
//
// Both the footer and the help card read this one table, so the two can never
// drift apart - or away from what the key actually does. The downloads screen
// is why this exists: its footer had grown to twelve hints, ran past the
// content column, and collided with the proxy badge sharing that line.
type keyHint struct {
	key   string
	label string
	strip bool
}

func onStrip(key, label string) keyHint { return keyHint{key: key, label: label, strip: true} }
func inCard(key, label string) keyHint  { return keyHint{key: key, label: label} }

// globalKeys work from anywhere and are listed once, in the card, rather than
// eating room on every screen's footer.
var globalKeys = []keyHint{
	inCard("F1 / F2", "home / latest results"),
	inCard("F3 / F4", "downloads / sources"),
	inCard("F5", "settings"),
	inCard("tab / shift+tab", "home: focus controls; elsewhere: downloads / back"),
	inCard("esc", "back or cancel current step"),
	inCard("^d", "downloads"),
	inCard("H", "health history"),
	inCard("?", "this card"),
	inCard("^c", "quit"),
}

// screenKeys is the keymap for whatever is on screen right now.
func (a *App) screenKeys() []keyHint {
	switch a.screen {
	case screenSearch:
		choose := inCard("↑↓", "choose")
		if a.search.menuFocused {
			choose.strip = true
		}
		return []keyHint{
			onStrip("enter", a.homeEnterLabel()),
			choose,
			onStrip("tab", "menu"),
			inCard("tab / shift+tab", "next / previous control"),
			inCard("paste", "a magnet, infohash, .torrent path or URL"),
			inCard("esc", "focus search, then clear the field"),
		}

	case screenISOs:
		return []keyHint{
			onStrip("↑↓", "move"),
			onStrip("enter", "download"),
			onStrip("tab", "downloads"),
			onStrip("esc", "back"),
			inCard("g/G", "top / bottom"),
			inCard("pgup/pgdn", "page"),
		}

	case screenResults:
		if a.results.browser && !a.results.pickPanel && !a.results.sourcePanel {
			return []keyHint{onStrip("enter", "preview"), onStrip("/", "search"), onStrip("tab", "browse"), onStrip("f", "filter"), inCard("c / ←", "categories"), inCard("o", "sort: relevance / seeders / size"), inCard("z", "hide zero seeders"), inCard("r", "reset filters"), inCard("ctrl+f", "resolution and size filters"), inCard("s", "source status"), inCard("D", "download now"), inCard("Y", "copy magnet"), onStrip("esc", "home"), inCard("pgup/pgdn", "page"), inCard("g/G", "top / bottom")}
		}

		if a.results.sourcePanel {
			return []keyHint{onStrip("↑↓", "choose source"), onStrip("t", "retry source"), onStrip("s / esc", "back to results"), inCard("pgup/pgdn", "page"), inCard("g/G", "top / bottom")}
		}
		if a.results.curated {
			esc := inCard("esc", "back")
			if a.results.searching {
				esc = onStrip("esc", "stop")
			}
			return []keyHint{onStrip("enter", a.resultEnterLabel()), onStrip("/", "filter"), onStrip("v", "all results"), esc, inCard("tab", "downloads"), inCard("↑↓", "choose a release"), inCard("s", "source status for this search"), inCard("c", "categories"), inCard("f", "advanced text filter"), inCard("D", "download now, skipping the preview"), inCard("Y", "copy magnet")}
		}
		if a.results.grouped {
			return []keyHint{
				inCard("s", "source status for this search"),
				inCard("↑↓", "move"),
				inCard("←→", "fold"),
				onStrip("enter", a.resultEnterLabel()),
				onStrip("tab", "downloads"),
				inCard("v", "top picks"),
				onStrip("esc", "back"),
				inCard("space", "fold this group"),
				inCard("D", "download now, skipping the preview"),
				inCard("Y", "copy magnet"),
				inCard("o", "sort: "+a.results.sort.String()),
				inCard("/", "smart filter: res:1080p seeders:>20 size:<8gb is:trusted"),
				inCard("g/G", "top / bottom"),
			}
		}
		return []keyHint{
			inCard("s", "source status for this search"),
			inCard("↑↓", "move"),
			onStrip("enter", a.resultEnterLabel()),
			onStrip("tab", "downloads"),
			onStrip("/", "filter"),
			inCard("o", "sort: "+a.results.sort.String()),
			inCard("v", "top picks"),
			inCard("V", "release groups"),
			inCard("f", "advanced text filter"),
			onStrip("esc", "back"),
			inCard("D", "download now, skipping the preview"),
			inCard("Y", "copy magnet"),
			inCard("g/G", "top / bottom"),
			inCard("pgup/pgdn", "page"),
		}

	case screenPreview:
		if a.preview.debrid && a.preview.debridErr != "" {
			return []keyHint{onStrip("t", "normal torrent"), onStrip("esc", "back")}
		}
		if a.preview.startPending {
			return []keyHint{onStrip("esc", "cancel")}
		}
		toggle := "choose"
		if n := a.preview.currentNode(); n != nil && n.fileIdx < 0 {
			toggle = "choose folder"
		}
		return []keyHint{
			onStrip("enter", a.previewDownloadLabel()),
			onStrip("space", toggle),
			onStrip("a", "all"),
			inCard("←→", "fold"),
			onStrip("esc", "back"),
			inCard("n", "select none"),
			inCard("g/G", "top / bottom"),
		}

	case screenSources:
		if a.sources.catalog != nil {
			return []keyHint{onStrip("↑↓", "choose"), onStrip("enter", "add & check"), onStrip("esc", "back")}
		}
		if a.sources.editor != nil {
			return []keyHint{onStrip("^t", "test"), onStrip("^s", "test & save"), onStrip("tab", "next field"), onStrip("esc", "cancel"), inCard("←→ / space", "change source type")}
		}
		if a.sources.removing != "" {
			return []keyHint{onStrip("y", "remove source"), onStrip("esc", "cancel")}
		}
		return []keyHint{onStrip("enter", "on/off"), onStrip("a", "add"), onStrip("r", "check"), onStrip("esc", "back"), inCard("t", "test selected source"), inCard("↑↓", "choose"), inCard("space", "enable / disable"), inCard("e", "edit custom source"), inCard("x", "remove custom source"), inCard("pgup/pgdn", "page"), inCard("g/G", "top / bottom")}

	case screenSettings:
		return a.settingsKeys()

	case screenHealth:
		return []keyHint{
			onStrip("↑↓", "move"),
			onStrip("r", "re-check"),
			onStrip("esc", "back"),
			onStrip("q", "quit"),
		}

	default:
		keys := []keyHint{
			onStrip("↑↓", "move"),
			onStrip("enter", "open"),
		}
		if it, ok := a.selectedDownload(a.downloadItems()); ok && it.Live && it.Trouble != "" {
			// a Real-Debrid row in trouble: the way out goes up front
			keys = append(keys, onStrip("t", "torrent"))
		}
		keys = append(keys,
			onStrip("p", "pause"),
			onStrip("x", "remove"),
			onStrip("d", "delete"),
			onStrip("esc", "back"),
			inCard("x / d / t", "in the dialog, c toggles the Real-Debrid cloud copy: delete / keep"),
			inCard("p on a held row", "retry it"),
			inCard("s", "seed on or off (torrents)"),
			inCard("t", "download a Real-Debrid row as a normal torrent instead"),
			inCard("v", "verify completed data"),
			inCard("m", "move to another folder"),
			inCard("r", "relink to existing files"),
			inCard("y", "copy full path"),
			inCard("Y", "copy magnet"),
			inCard("x", "remove from the list, keep the files"),
			inCard("d", "stop and delete the downloaded files, partial ones too"),
			inCard("g/G", "top / bottom"),
		)
		if revealAvailable {
			keys = append(keys, inCard("o", revealLabel))
		}
		return keys
	}
}

// keyStrip renders the footer line for the current screen in the room it
// actually has, always ending with the `?` pointer so the full list is
// reachable from everywhere. On a narrow terminal the tail is dropped rather
// than run off the edge - the card holds every key anyway.
func (a *App) keyStrip(width int) string {
	const sep = 3 // hints() joins with three spaces
	const maxHints = 5
	var parts []string
	for _, kh := range a.screenKeys() {
		if kh.strip && len(parts) < maxHints {
			parts = append(parts, hint(kh.key, kh.label))
		}
	}
	parts = append(parts, hint("?", "keys"))
	for len(parts) > 2 && stripWidth(parts, sep) > width {
		parts = append(parts[:len(parts)-2], parts[len(parts)-1])
	}
	// On the narrowest terminals the screen's main action beats the pointer
	// to the card.
	if len(parts) == 2 && stripWidth(parts, sep) > width {
		if lipgloss.Width(parts[0]) <= width {
			parts = parts[:1]
		} else {
			parts = parts[1:]
		}
	}
	return hints(parts...)
}

func stripWidth(parts []string, sep int) int {
	total := 0
	for i, p := range parts {
		total += lipgloss.Width(p)
		if i > 0 {
			total += sep
		}
	}
	return total
}

// helpBudget is the room the strip has on the footer line once the proxy badge
// sharing that line is paid for.
func (a *App) helpBudget(width int) int {
	if tail := a.footerTail(); tail != "" {
		width -= lipgloss.Width(truncate(tail, max(1, width/2))) + 2
	}
	return max(20, width)
}

func (a *App) screenTitle() string {
	switch a.screen {
	case screenSearch:
		return "home"
	case screenISOs:
		return "linux isos"
	case screenResults:
		if a.results.grouped {
			return "results · grouped"
		}
		return "results"
	case screenPreview:
		return "preview"
	case screenSources:
		return "sources"
	case screenHealth:
		return "health"
	case screenSettings:
		return "settings"
	default:
		return "downloads"
	}
}

// helpLines wraps the key reference into columns when space allows. Smaller
// windows can scroll the complete reference instead of clipping shortcuts.
func (a *App) helpLines() []string {
	navigation := append([]keyHint(nil), globalKeys...)
	if a.background {
		navigation[len(navigation)-1].label = "close UI; transfers keep running"
	}
	width := a.contentWidth()
	var body string
	if width >= 80 {
		leftWidth := (width - 4) / 2
		here := strings.Join(helpSection(a.screenTitle(), a.screenKeys(), leftWidth), "\n")
		everywhere := strings.Join(helpSection("navigation", navigation, width-leftWidth-4), "\n")
		left := lipgloss.NewStyle().Width(leftWidth).Render(here)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, "    ", everywhere)
	} else {
		here := helpSection(a.screenTitle(), a.screenKeys(), width)
		everywhere := helpSection("navigation", navigation, width)
		body = strings.Join(append(append(here, ""), everywhere...), "\n")
	}
	return strings.Split(body, "\n")
}

func (a *App) viewHelp() string {
	lines := a.helpLines()
	offset := min(a.helpOffset, max(0, len(lines)-a.bodyHeight()))
	help := "esc close"
	if len(lines) > a.bodyHeight() {
		help = "↑↓ / pgup/pgdn scroll · esc close"
	}
	return a.chrome("keys", strings.Join(lines[offset:], "\n"), styleDim.Render(help))
}

// helpSection lists keys under a title in a column width cells wide. A label
// that does not fit wraps under itself (a hanging indent), never back under
// the keys.
func helpSection(title string, keys []keyHint, width int) []string {
	const keyCol = 2 + 16
	lines := []string{styleTitle.Render(title), ""}
	labelWidth := max(width-keyCol, 8)
	indent := strings.Repeat(" ", keyCol)
	for _, kh := range keys {
		label := strings.Split(ansi.Wrap(kh.label, labelWidth, ""), "\n")
		lines = append(lines, "  "+padRight(styleKey.Render(kh.key), 16)+styleDim.Render(label[0]))
		for _, more := range label[1:] {
			lines = append(lines, indent+styleDim.Render(more))
		}
	}
	return lines
}

func (a *App) homeEnterLabel() string {
	if a.search.menuFocused {
		return "open " + a.homeDestinations()[a.search.menu].name
	}
	return "search"
}

func (a *App) resultEnterLabel() string {
	if a.cfg != nil && !a.cfg.PreviewBeforeDownload {
		return "download"
	}
	return "preview"
}

func (a *App) previewDownloadLabel() string {
	if !a.preview.ready {
		if a.preview.debrid {
			return "download all now"
		}
		return "queue all"
	}
	if a.preview.selectedBytes() == 0 {
		return "select files first"
	}
	return "download"
}
