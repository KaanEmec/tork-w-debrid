package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// withSettingSections swaps the registered sections for the length of a test.
func withSettingSections(t *testing.T, builders []settingSectionBuilder) {
	t.Helper()
	saved, savedOpeners := settingSectionBuilders, settingsOpeners
	settingSectionBuilders, settingsOpeners = builders, nil
	t.Cleanup(func() { settingSectionBuilders, settingsOpeners = saved, savedOpeners })
}

// testSwitchRow is a minimal on/off row for the screen tests.
type testSwitchRow struct {
	name string
	on   *bool
}

func (r testSwitchRow) label() string { return r.name }
func (r testSwitchRow) value(*App) string {
	return onOff(*r.on)
}
func (r testSwitchRow) hint(*App) string    { return "space toggle" }
func (r testSwitchRow) keys(*App) []keyHint { return []keyHint{onStrip("space", "toggle")} }
func (r testSwitchRow) update(_ *App, k tea.KeyMsg) (bool, tea.Cmd) {
	if k.String() == " " {
		*r.on = !*r.on
		return true, nil
	}
	return false, nil
}

func TestSettingsOpensFromHomeMenuAndF5AndEscGoesBack(t *testing.T) {
	withSettingSections(t, nil)
	a := navigationApp()
	a.focusSearch()
	// Home menu: tab to the menu, then down to "settings".
	a.Update(tea.KeyMsg{Type: tea.KeyTab})
	for a.homeDestinations()[a.search.menu].name != "settings" {
		a.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.screen != screenSettings {
		t.Fatalf("home menu opened %v, want settings", a.screen)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.screen != screenSearch {
		t.Fatalf("esc went to %v, want home", a.screen)
	}

	a.navigate(screenDownloads)
	a.Update(tea.KeyMsg{Type: tea.KeyF5})
	if a.screen != screenSettings {
		t.Fatalf("F5 opened %v, want settings", a.screen)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.screen != screenDownloads {
		t.Fatalf("esc went to %v, want downloads (where F5 was pressed)", a.screen)
	}
}

func TestSettingsWithNoSectionsSaysNothingToSet(t *testing.T) {
	withSettingSections(t, nil)
	a := navigationApp()
	a.openSettings()
	for _, k := range []tea.KeyType{tea.KeyDown, tea.KeyUp, tea.KeyEnter, tea.KeySpace, tea.KeyLeft} {
		a.Update(tea.KeyMsg{Type: k})
	}
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "nothing to set yet") {
		t.Fatalf("empty settings view:\n%s", view)
	}
	if !strings.Contains(view, "saved in") || !strings.Contains(view, "config.yaml") {
		t.Fatalf("header does not say where settings are saved:\n%s", view)
	}
}

func TestSettingsRowsMoveToggleAndFit(t *testing.T) {
	var one, two bool
	withSettingSections(t, []settingSectionBuilder{{order: 1, build: func(*App) settingSection {
		return settingSection{title: "Look", rows: []settingRow{testSwitchRow{"first switch", &one}, testSwitchRow{"second switch", &two}}}
	}}})
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		one, two = false, false
		a := navigationApp()
		a.width, a.height = size[0], size[1]
		a.openSettings()
		a.Update(tea.KeyMsg{Type: tea.KeyDown})
		a.Update(tea.KeyMsg{Type: tea.KeySpace})
		if one || !two {
			t.Fatalf("space toggled one=%v two=%v, want only the selected row", one, two)
		}
		view := a.View()
		assertRenderFits(t, view, size[0])
		plain := ansi.Strip(view)
		for _, want := range []string{"Look", "first switch", "second switch", "space toggle", glyphs.cursor} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%dx%d settings view lacks %q:\n%s", size[0], size[1], want, plain)
			}
		}
		// Label column: the value starts 34 cells after the label.
		for _, line := range strings.Split(plain, "\n") {
			if i := strings.Index(line, "second switch"); i >= 0 {
				if got := line[i+settingLabelWidth:]; !strings.HasPrefix(got, "on ") {
					t.Fatalf("value is not 34 cells after the label: %q", line)
				}
			}
		}
	}
}

func TestSettingsHelpCardListsSettingsKeys(t *testing.T) {
	var on bool
	withSettingSections(t, []settingSectionBuilder{{order: 1, build: func(*App) settingSection {
		return settingSection{title: "Look", rows: []settingRow{testSwitchRow{"switch", &on}}}
	}}})
	a := navigationApp()
	a.openSettings()
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if !a.showHelp {
		t.Fatal("? did not open the key card on settings")
	}
	card := ansi.Strip(strings.Join(a.helpLines(), "\n"))
	for _, want := range []string{"settings", "toggle", "esc", "F5"} {
		if !strings.Contains(card, want) {
			t.Fatalf("key card lacks %q:\n%s", want, card)
		}
	}
}
