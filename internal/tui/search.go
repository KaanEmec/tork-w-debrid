package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kaanemec/tork-w-debrid/internal/intake"
)

// homeDest is a destination on the front-page menu. The latest results are
// not on it: F2 goes back there.
type homeDest struct {
	name     string
	screen   screen
	settings bool // opens Settings rather than a screen of its own
}

func (a *App) homeDestinations() []homeDest {
	return []homeDest{
		{name: "downloads", screen: screenDownloads},
		{name: "linux isos", screen: screenISOs},
		{name: "sources", screen: screenSources},
		{name: "settings", settings: true},
	}
}

// openSettings opens Settings (F5). The settings screen lands with the
// settings jobs (J5.1, J8.7); until then the menu row says so.
func (a *App) openSettings() tea.Cmd {
	return a.showToast("settings are coming soon", toastWarn, toastQuick)
}

// openHomeDest acts on a front-page menu row.
func (a *App) openHomeDest(d homeDest) tea.Cmd {
	if d.settings {
		return a.openSettings()
	}
	return a.navigate(d.screen)
}

// Tab moves through the controls on home; arrows work the same way.
func (a *App) cycleHomeFocus(reverse bool) tea.Cmd {
	n := len(a.homeDestinations())
	if !a.search.menuFocused {
		a.search.menu = 0
		if reverse {
			a.search.menu = n - 1
		}
		a.search.menuFocused = true
		a.search.input.Blur()
		return nil
	}
	if reverse {
		a.search.menu--
	} else {
		a.search.menu++
	}
	if a.search.menu < 0 || a.search.menu >= n {
		return a.focusSearch()
	}
	return nil
}

type searchModel struct {
	input       textinput.Model
	menu        int // highlighted front-page destination
	menuFocused bool
}

func newSearchModel() searchModel {
	ti := textinput.New()
	// The leading space carries the block cursor on an empty field, so the
	// placeholder reads "█ search or paste a torrent link".
	ti.Placeholder = " search or paste a torrent link"
	ti.CharLimit = 4096 // tracker-rich magnets and long local paths are valid inputs
	ti.Width = 50
	ti.Prompt = glyphs.prompt + " "
	ti.PromptStyle = styleBrand
	ti.TextStyle = styleFg
	ti.PlaceholderStyle = styleDim
	ti.Cursor.Style = styleBrand
	// The frame clock blinks the cursor (J8.3), not the text input's own timer.
	ti.Cursor.SetMode(cursor.CursorStatic)
	return searchModel{input: ti}
}

// OpenTorrent arranges for any supported explicit CLI input to open in the
// same quiet preview flow as pasting it on the home screen.
func (a *App) OpenTorrent(raw string) error {
	target, ok, err := intake.DetectCLI(raw)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("expected a magnet link, infohash, torrent URL, or local torrent file")
	}
	return a.OpenTarget(target)

}

// OpenTarget queues a previously classified input for the first TUI frame.
func (a *App) OpenTarget(target intake.Target) error {
	a.search.input.SetValue(target.Value)
	switch target.Kind {
	case intake.Magnet, intake.InfoHash:
		a.startup = a.launchCmd(target.Value, target.Name, true)
	case intake.TorrentURL:
		a.startup = a.launchTorrentURLPreviewCmd(target.Value, target.Name)
	case intake.TorrentFile:
		a.startup = a.launchTorrentFilePreviewCmd(target.Value, target.Name)
	default:
		return fmt.Errorf("unsupported torrent input")
	}
	return nil
}

func (a *App) updateSearch(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		a.search.input, cmd = a.search.input.Update(msg)
		return a, cmd
	}

	// Only the visibly focused control handles Enter. Typing from the menu
	// returns focus to search without losing the saved query.
	switch key.String() {
	case "enter":
		if a.search.menuFocused {
			return a, a.openHomeDest(a.homeDestinations()[a.search.menu])
		}
		if query := strings.TrimSpace(a.search.input.Value()); query != "" {
			target, detected, err := intake.DetectHome(query)
			if err != nil {
				return a, a.showError(err.Error())
			}
			if detected {
				switch target.Kind {
				case intake.TorrentURL:
					return a, a.launchTorrentURLPreviewCmd(target.Value, target.Name)
				case intake.TorrentFile:
					return a, a.launchTorrentFilePreviewCmd(target.Value, target.Name)
				default:
					return a, a.launchCmd(target.Value, target.Name, true)
				}
			}
			return a, a.startSearch(query)
		}
		return a, nil
	case "up":
		if a.search.menuFocused && a.search.menu > 0 {
			a.search.menu--
			return a, nil
		}
		return a, a.focusSearch()
	case "down":
		if a.search.menuFocused {
			a.search.menu = min(len(a.homeDestinations())-1, a.search.menu+1)
		} else {
			a.search.menu = 0
			a.search.menuFocused = true
			a.search.input.Blur()
		}
		return a, nil
	case "esc":
		if a.search.menuFocused {
			return a, a.focusSearch()
		}
		a.search.input.SetValue("")
		return a, nil
	}
	if a.search.menuFocused {
		if key.Type != tea.KeyRunes && key.Type != tea.KeySpace && key.Type != tea.KeyBackspace && key.Type != tea.KeyCtrlV {
			return a, nil
		}
		focus := a.focusSearch()
		var cmd tea.Cmd
		a.search.input, cmd = a.search.input.Update(msg)
		return a, tea.Batch(focus, cmd)
	}
	var cmd tea.Cmd
	a.search.input, cmd = a.search.input.Update(msg)
	return a, cmd
}

// startSearch cancels any in-flight search and fans a new one out.
func (a *App) startSearch(query string) tea.Cmd {
	agg := a.agg
	if a.nextAgg != nil {
		agg = a.nextAgg
	}
	if len(agg.Providers()) == 0 {
		return a.showError("no sources enabled · open sources from home")
	}
	if a.results.cancel != nil {
		a.results.cancel()
	}
	a.cancelResolve()
	a.agg, a.nextAgg = agg, nil
	ctx, cancel := context.WithCancel(context.Background())
	resultCh, statusCh := a.agg.Search(ctx, query)
	a.searchSeq++

	prefs := a.results.pickPrefs
	category, hideDead, sort := a.results.category, a.results.hideDead, a.results.sort
	a.results = newResultsModel(a.cfg.Ranking)
	// Results open on the grouped top picks (J8.5); v shows every result and
	// c the category browser.
	a.results.curated = true
	a.results.category, a.results.hideDead, a.results.sort = category, hideDead, sort
	a.results.pickPrefs = prefs
	a.results.searchID = a.searchSeq
	a.results.query = query
	a.results.cancel = cancel
	a.results.resultCh = resultCh
	a.results.statusCh = statusCh
	a.results.searching = true
	a.screen = screenResults

	return tea.Batch(waitForResult(a.searchSeq, resultCh), waitForStatus(a.searchSeq, statusCh))
}

// viewSearch is the front page: the logo with the cat on the "w", the slogan,
// the search field (the screen's one panel) and four menu rows, above the
// footer. Small terminals get a one-line title instead of the logo.
func (a *App) viewSearch() string {
	th := a.termHeight()
	frame := a.vframe()

	fieldW := max(8, min(56, a.contentWidth()))
	a.search.input.Width = max(1, fieldW-6)
	a.setCursorBlink(frame)
	border := colAccent
	if a.search.menuFocused {
		border = colLine
	}
	field := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(max(1, fieldW-2)).
		Render(a.search.input.View())

	roomy := th >= 25 // 80x24 drops the slogan and the menu notes
	var parts []string
	if a.contentWidth() < 60 || a.bodyHeight() < 13 {
		parts = []string{brandMark(), "", field, "", a.homeMenuView(fieldW, roomy)}
	} else {
		parts = []string{homeLogo(frame), ""}
		if roomy {
			parts = append(parts, styleDim.Render(homeSlogan), "")
		}
		parts = append(parts, field, "", a.homeMenuView(fieldW, roomy))
	}
	hero := fitBlockWidth(lipgloss.JoinVertical(lipgloss.Center, parts...), a.contentWidth())
	body := lipgloss.Place(a.contentWidth(), a.bodyHeight(), lipgloss.Center, lipgloss.Center, hero)
	return a.chrome("home", body, a.keyStrip(a.helpBudget(a.contentWidth())))
}

const homeSlogan = "you name it, the cat fetches it"

// setCursorBlink shows the field's block cursor for half of every second.
func (a *App) setCursorBlink(frame int) {
	mode := cursor.CursorStatic
	if !cursorVisible(frame) {
		mode = cursor.CursorHide
	}
	if a.search.input.Cursor.Mode() != mode {
		a.search.input.Cursor.SetMode(mode)
	}
}

// homeMenuView renders the destination rows under the search field, as wide
// as the field so the block stays aligned.
func (a *App) homeMenuView(width int, notes bool) string {
	descs := []string{a.downloadsSummary(), styleDim.Render("official images"), styleDim.Render(a.sourcesSummary()), styleDim.Render("F5")}
	menu := a.homeDestinations()
	rows := make([]string, len(menu))
	for i, d := range menu {
		name := padRight(d.name, 17)
		desc := ""
		if notes {
			desc = descs[i]
		}
		if a.search.menuFocused && i == a.search.menu {
			rows[i] = styleSelBar.Render(glyphs.cursor+" ") + styleFg.Bold(true).Render(name) + desc
		} else {
			rows[i] = "  " + styleText.Render(name) + desc
		}
		rows[i] = padRight(truncate(rows[i], width), width)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// downloadsSummary is the downloads row's note: "2 active · 1 on Real-Debrid"
// with the Real-Debrid part in lavender.
func (a *App) downloadsSummary() string {
	m := a.downloads.metrics
	if !a.downloads.itemsReady {
		m = summarizeDownloads(a.downloadItems())
	}
	if m.Active > 0 || m.Cloud > 0 {
		out := styleDim.Render(fmt.Sprintf("%d active", m.Active))
		if m.Cloud > 0 {
			out += styleDim.Render(" · ") + styleRoute.Render(fmt.Sprintf("%d on Real-Debrid", m.Cloud))
		}
		return out
	}
	switch m.Total {
	case 0:
		return styleDim.Render("nothing downloading yet")
	case 1:
		return styleDim.Render("1 saved download")
	default:
		return styleDim.Render(fmt.Sprintf("%d saved downloads", m.Total))
	}
}

func (a *App) sourcesSummary() string {
	if a.cfg == nil {
		return "choose search indexes"
	}
	enabled := 0
	for _, p := range a.cfg.Providers {
		if p.Enabled {
			enabled++
		}
	}
	return fmt.Sprintf("%d enabled", enabled)
}
