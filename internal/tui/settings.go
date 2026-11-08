package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Settings (J5.1, styled by J8.7): a list of sections, each a list of rows.
// A row is a label, its current value and a short right-aligned hint. Each
// section lives in its own file (settings_debrid.go, settings_downloads.go,
// settings_look.go) and registers itself, so this file never grows a case per
// setting.
//
// Keys: ↑↓ move, enter edits or acts, space toggles a switch, ←→ steps a
// choice, esc goes back (or cancels an edit). A row being edited opens an
// overlay (the folder editor, the link dialog) that owns the keyboard until it
// closes.

// settingLabelWidth is the label column: a label padded to 34 cells, then the
// value (design.md › Settings).
const settingLabelWidth = 34

// settingsLayout is the label column and whether the hint column shows. Below
// 62 columns the hints give way (the footer strip carries the keys) and the
// label column shrinks, so the values stay readable on a 50-column terminal.
func settingsLayout(width int) (labelW int, hints bool) {
	if width >= settingLabelWidth+28 {
		return settingLabelWidth, true
	}
	return max(12, min(settingLabelWidth, width*11/20)), false
}

// settingRow is one line of a settings section.
type settingRow interface {
	label() string
	// value is the styled current value: on/off, a path, an account.
	value(a *App) string
	// hint is the short muted key hint drawn on the right ("space toggle").
	hint(a *App) string
	// keys lists the keys this row answers to, for the footer and the ? card.
	keys(a *App) []keyHint
	// update handles a key while the row is selected. handled=false lets the
	// screen treat the key (for example esc going back).
	update(a *App, key tea.KeyMsg) (handled bool, cmd tea.Cmd)
}

// settingNoter is a row with a muted line under it ("up to 8 per torrent",
// "this run uses -d D:\Media").
type settingNoter interface {
	note(a *App) string
}

// settingSection is a titled group of rows ("Real-Debrid", "Downloads").
type settingSection struct {
	title string
	rows  []settingRow
}

type settingSectionBuilder struct {
	order int
	build func(a *App) settingSection
}

// settingSectionBuilders holds the registered sections, in screen order.
var settingSectionBuilders []settingSectionBuilder

// registerSettingSection adds a section to the settings screen. order sorts
// the sections (Real-Debrid 10, Downloads 20, Look 30); build is called on
// every render, so a section can change its rows with the state it shows.
func registerSettingSection(order int, build func(a *App) settingSection) {
	settingSectionBuilders = append(settingSectionBuilders, settingSectionBuilder{order: order, build: build})
	sort.SliceStable(settingSectionBuilders, func(i, j int) bool {
		return settingSectionBuilders[i].order < settingSectionBuilders[j].order
	})
}

// settingOverlay is an editor or dialog opened from a row. While it is open
// it owns the keyboard; it closes itself by calling a.closeSettingOverlay.
type settingOverlay interface {
	// crumb is the last part of the header breadcrumb ("default folder").
	crumb() string
	update(a *App, msg tea.KeyMsg) tea.Cmd
	// view draws the overlay. A dialog is drawn as a box over the dimmed
	// settings list; anything else replaces the list.
	view(a *App, width, height int) string
	keys(a *App) []keyHint
	// typing reports whether a text field owns the keyboard.
	typing() bool
	// dialog reports whether view is a centered box over the list.
	dialog() bool
	animating(a *App) bool
	// close releases what the overlay holds (a running login) when it is
	// dismissed or the user leaves the screen.
	close(a *App)
}

// settingConfirm is a y/N question under the list ("unlink Real-Debrid?").
type settingConfirm struct {
	question string
	yes      func(a *App) tea.Cmd
	no       func(a *App) tea.Cmd // optional
}

type settingsModel struct {
	from    screen // where esc goes back to
	cursor  int
	offset  int // first visible body line
	overlay settingOverlay
	confirm *settingConfirm

	rd     rdSettings     // Real-Debrid section (settings_debrid.go)
	folder folderSettings // Downloads section (settings_downloads.go)
}

// settingsMsg is a message the settings code handles wherever the user is,
// so a slow answer (an account lookup, a login) can't land on another screen.
type settingsMsg interface {
	applySettings(a *App) tea.Cmd
}

// openSettings opens Settings (F5 and the home menu row).
func (a *App) openSettings() tea.Cmd {
	if a.screen == screenSettings {
		return nil
	}
	from := a.screen
	cmd := a.navigate(screenSettings)
	a.settings.from = from
	a.settings.cursor = min(a.settings.cursor, max(0, len(a.settingRows())-1))
	return tea.Batch(cmd, a.onSettingsOpen())
}

// settingsOpeners run when the screen opens (load the account, read the
// saved folder). Sections add theirs in init.
var settingsOpeners []func(a *App) tea.Cmd

func (a *App) onSettingsOpen() tea.Cmd {
	var cmds []tea.Cmd
	for _, open := range settingsOpeners {
		cmds = append(cmds, open(a))
	}
	return tea.Batch(cmds...)
}

// leaveSettings drops any open editor and cancels work it started, for
// example a Real-Debrid login that is still polling.
func (a *App) leaveSettings() {
	a.closeSettingOverlay()
	a.settings.confirm = nil
}

func (a *App) closeSettingOverlay() {
	if a.settings.overlay != nil {
		a.settings.overlay.close(a)
		a.settings.overlay = nil
	}
}

func (a *App) settingsTyping() bool {
	return a.settings.overlay != nil && a.settings.overlay.typing()
}

// settingsLocked keeps F-keys and tab from leaving mid-edit; esc closes the
// editor first.
func (a *App) settingsLocked() bool {
	return a.settings.overlay != nil || a.settings.confirm != nil
}

func (a *App) settingsAnimating() bool {
	return a.settings.overlay != nil && a.settings.overlay.animating(a)
}

func (a *App) settingSections() []settingSection {
	var out []settingSection
	for _, b := range settingSectionBuilders {
		if s := b.build(a); len(s.rows) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func (a *App) settingRows() []settingRow {
	var rows []settingRow
	for _, s := range a.settingSections() {
		rows = append(rows, s.rows...)
	}
	return rows
}

func (a *App) selectedSettingRow() settingRow {
	rows := a.settingRows()
	if len(rows) == 0 {
		return nil
	}
	a.settings.cursor = max(0, min(a.settings.cursor, len(rows)-1))
	return rows[a.settings.cursor]
}

func (a *App) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return a, nil
	}
	s := &a.settings
	if s.overlay != nil {
		return a, s.overlay.update(a, key)
	}
	if s.confirm != nil {
		c := s.confirm
		switch key.String() {
		case "y", "Y":
			s.confirm = nil
			return a, c.yes(a)
		case "n", "N", "esc", "enter":
			s.confirm = nil
			if c.no != nil {
				return a, c.no(a)
			}
		}
		return a, nil
	}
	rows := a.settingRows()
	switch key.String() {
	case "up", "k":
		if s.cursor > 0 {
			s.cursor--
		}
		return a, nil
	case "down", "j":
		if s.cursor < len(rows)-1 {
			s.cursor++
		}
		return a, nil
	case "home", "g":
		s.cursor = 0
		return a, nil
	case "end", "G":
		s.cursor = max(0, len(rows)-1)
		return a, nil
	}
	if row := a.selectedSettingRow(); row != nil {
		if handled, cmd := row.update(a, key); handled {
			return a, cmd
		}
	}
	if key.String() == "esc" {
		back := s.from
		if back == screenSettings {
			back = screenSearch
		}
		return a, a.navigate(back)
	}
	return a, nil
}

// settingsKeys is the keymap for the settings screen: the overlay's when one
// is open, otherwise the selected row's keys plus moving and going back.
func (a *App) settingsKeys() []keyHint {
	s := &a.settings
	if s.overlay != nil {
		return s.overlay.keys(a)
	}
	if s.confirm != nil {
		return []keyHint{onStrip("y", "yes"), onStrip("n / esc", "no")}
	}
	keys := []keyHint{onStrip("↑↓", "move")}
	if row := a.selectedSettingRow(); row != nil {
		keys = append(keys, row.keys(a)...)
	}
	keys = append(keys,
		onStrip("esc", "back"),
		inCard("enter", "change or act on the selected row"),
		inCard("space", "toggle a switch"),
		inCard("←→", "step through choices"),
		inCard("g/G", "top / bottom"),
	)
	return dedupeKeys(keys)
}

// dedupeKeys keeps the first entry of each key, so a row's own wording wins
// over the screen's generic one.
func dedupeKeys(keys []keyHint) []keyHint {
	seen := map[string]bool{}
	out := keys[:0:0]
	for _, k := range keys {
		if seen[k.key] {
			continue
		}
		seen[k.key] = true
		out = append(out, k)
	}
	return out
}

// settingsFact is the header's right side: where the settings are saved.
func (a *App) settingsFact() string {
	return styleDim.Render("saved in " + a.configPathLabel())
}

// configPathLabel is config.yaml's path with the home folder written as ~.
func (a *App) configPathLabel() string {
	if a.cfg == nil || a.cfg.Dir() == "" {
		return "~/.tork/config.yaml"
	}
	return tildePath(a.cfg.ConfigPath())
}

// tildePath writes a path under the home folder as ~/… (with the platform's
// separator after the ~).
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(p))
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return "~" + string(filepath.Separator) + rel
}

func (a *App) settingsContext() string {
	ctx := "settings"
	if o := a.settings.overlay; o != nil && !o.dialog() {
		ctx += crumbSep + o.crumb()
	}
	return ctx
}

func (a *App) viewSettings() string {
	width := a.contentWidth()
	help := a.keyStrip(a.helpBudget(width))
	if f, ok := a.settings.overlay.(interface{ footer(*App) string }); ok {
		help = f.footer(a)
	}
	if o := a.settings.overlay; o != nil && !o.dialog() {
		return a.chrome(a.settingsContext(), o.view(a, width, a.bodyHeight()), help)
	}
	body := a.settingsList(width, a.bodyHeight())
	if o := a.settings.overlay; o != nil {
		body = overlayDialog(body, o.view(a, width, a.bodyHeight()), width, a.bodyHeight())
	}
	return a.chrome(a.settingsContext(), body, help)
}

// settingsList renders the sections, scrolled so the selected row (and the
// line under it) stays visible.
func (a *App) settingsList(width, height int) string {
	sections := a.settingSections()
	if len(sections) == 0 {
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, styleDim.Render("nothing to set yet"))
	}
	var lines []string
	cursorLine, idx := 0, 0
	for si, sec := range sections {
		if si > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "  "+styleDim.Render(sec.title))
		for _, row := range sec.rows {
			selected := idx == a.settings.cursor
			if selected {
				cursorLine = len(lines)
			}
			lines = append(lines, a.settingRowLine(row, selected, width))
			if n, ok := row.(settingNoter); ok {
				if note := n.note(a); note != "" {
					labelW, _ := settingsLayout(width)
					lines = append(lines, truncate(strings.Repeat(" ", 2+labelW)+note, width))
					if selected {
						cursorLine = len(lines) - 1
					}
				}
			}
			idx++
		}
	}
	if c := a.settings.confirm; c != nil {
		lines = append(lines, "", "  "+styleWarn.Render(c.question)+"  "+hints(hint("y", "yes"), hint("n", "no")))
		cursorLine = len(lines) - 1
	}
	lines = append([]string{""}, lines...)
	cursorLine++
	off := a.settings.offset
	if cursorLine < off+1 {
		off = max(0, cursorLine-2)
	}
	if cursorLine >= off+height {
		off = cursorLine - height + 1
	}
	off = max(0, min(off, max(0, len(lines)-height)))
	a.settings.offset = off
	end := min(len(lines), off+height)
	return strings.Join(lines[off:end], "\n")
}

// settingRowLine is "▍ label(34) value … hint", the hint right-aligned in
// muted; the selected row gets the accent gutter and the raised background.
func (a *App) settingRowLine(row settingRow, selected bool, width int) string {
	inner := width - 1
	labelW, showHints := settingsLayout(width)
	hintText := ""
	if h := row.hint(a); h != "" && showHints {
		hintText = styleDim.Render(h)
	}
	label := truncate(row.label(), labelW-1)
	left := " " + padRight(styleText.Render(label), labelW) + row.value(a)
	if selected {
		left = " " + padRight(styleFg.Bold(true).Render(label), labelW) + row.value(a)
	}
	content := left
	if hintText != "" {
		hw := lipgloss.Width(hintText)
		left = truncate(left, max(1, inner-hw-2))
		gap := max(2, inner-lipgloss.Width(left)-hw-1)
		content = left + strings.Repeat(" ", gap) + hintText
	}
	content = truncate(content, inner)
	if selected {
		return styleSelBar.Background(colRaised).Render(glyphs.cursor) + selectedRow(content, inner)
	}
	return " " + content
}

// overlayDialog draws box centered over back (which fills width×height),
// with everything behind the box redrawn in muted.
func overlayDialog(back, box string, width, height int) string {
	bg := strings.Split(padLines(ansi.Strip(back), height), "\n")
	fg := strings.Split(box, "\n")
	bw := lipgloss.Width(box)
	top := max(0, (height-len(fg))/2)
	left := max(0, (width-bw)/2)
	for y := range bg {
		plain := padRight(bg[y], width)
		i := y - top
		if i < 0 || i >= len(fg) {
			bg[y] = styleDim.Render(plain)
			continue
		}
		bg[y] = styleDim.Render(ansi.Truncate(plain, left, "")) + padRight(fg[i], bw) +
			styleDim.Render(ansi.TruncateLeft(plain, left+bw, ""))
	}
	return strings.Join(bg, "\n")
}

// onOff is a switch's value: "on" in accent, "off" in muted.
func onOff(on bool) string {
	if on {
		return styleOK.Render("on")
	}
	return styleDim.Render("off")
}
