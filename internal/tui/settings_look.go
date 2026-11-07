package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Settings › Look (J8.7): turn animations off, choose whether the intro
// plays after an update, and replay it.
//
//	Look
//	  animations                        on                          space toggle
//	  intro after each update           on                          space toggle
//	  play the intro                    about 8 s · any key stops it  enter play

func init() {
	registerSettingSection(30, func(a *App) settingSection {
		if a.cfg == nil {
			return settingSection{}
		}
		return settingSection{title: "Look", rows: []settingRow{motionRow{}, introRow{}, playIntroRow{}}}
	})
}

// motionRow is "animations on/off" (ui.motion). It takes effect at once.
type motionRow struct{}

func (motionRow) label() string       { return "animations" }
func (motionRow) value(a *App) string { return onOff(a.motion.animations) }
func (motionRow) hint(*App) string    { return "space toggle" }
func (motionRow) keys(*App) []keyHint { return []keyHint{onStrip("space", "toggle")} }
func (motionRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	if !isToggleKey(key) {
		return false, nil
	}
	on := !a.motion.animations
	if err := a.cfg.WriteUIMotion(on); err != nil {
		return true, a.showError("not saved: " + err.Error())
	}
	a.cfg.UI.Motion = on
	a.SetAnimations(on)
	return true, nil
}

// introRow is "intro after each update on/off" (ui.intro), read by J8.8.
type introRow struct{}

func (introRow) label() string       { return "intro after each update" }
func (introRow) value(a *App) string { return onOff(a.cfg.UI.Intro) }
func (introRow) hint(*App) string    { return "space toggle" }
func (introRow) keys(*App) []keyHint { return []keyHint{onStrip("space", "toggle")} }
func (introRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	if !isToggleKey(key) {
		return false, nil
	}
	on := !a.cfg.UI.Intro
	if err := a.cfg.WriteUIIntro(on); err != nil {
		return true, a.showError("not saved: " + err.Error())
	}
	a.cfg.UI.Intro = on
	return true, nil
}

// playIntroRow replays the intro at once.
type playIntroRow struct{}

func (playIntroRow) label() string { return "play the intro" }
func (playIntroRow) value(*App) string {
	return styleDim.Render("about 8 s · any key stops it")
}
func (playIntroRow) hint(*App) string    { return "enter play" }
func (playIntroRow) keys(*App) []keyHint { return []keyHint{onStrip("enter", "play the intro")} }
func (playIntroRow) update(_ *App, key tea.KeyMsg) (bool, tea.Cmd) {
	if key.String() != "enter" {
		return false, nil
	}
	return true, func() tea.Msg { return replayIntroMsg{} }
}

// isToggleKey: space toggles a switch; enter does too, so a switch never
// ignores the key that acts on every other row.
func isToggleKey(key tea.KeyMsg) bool {
	return key.String() == " " || key.String() == "enter"
}
