package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/cursor"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

func TestAnimationsSwitchStopsMotionAtOnceAndSurvivesRestart(t *testing.T) {
	a, cfg, _ := folderTestApp(t)
	a.motion.noMotion = false // this test terminal allows motion
	a.openSettings()
	selectSettingRow(t, a, "animations")
	if !a.MotionEnabled() {
		t.Fatal("animations start off")
	}
	a.updateSettings(key("space"))
	if a.MotionEnabled() || a.animating() || a.ensureFrame() != nil {
		t.Fatal("turning animations off left the clock running")
	}
	again, err := config.LoadFrom(cfg.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if again.UI.Motion {
		t.Fatal("ui.motion was not saved")
	}
	b := New(again, nil, nil, &state.State{}, nil)
	b.motion.noMotion = false
	if b.MotionEnabled() {
		t.Fatal("animations came back on after a restart")
	}
	a.updateSettings(key("space"))
	a.navigate(screenSearch) // home always animates
	if !a.MotionEnabled() || a.ensureFrame() == nil {
		t.Fatal("turning animations on did not bring motion back")
	}
}

func TestIntroSwitchSavesUIIntro(t *testing.T) {
	a, cfg, _ := folderTestApp(t)
	a.openSettings()
	selectSettingRow(t, a, "intro after each update")
	a.updateSettings(key("space"))
	if cfg.UI.Intro {
		t.Fatal("in-memory ui.intro still on")
	}
	if again, _ := config.LoadReadOnlyFrom(cfg.Dir()); again.UI.Intro {
		t.Fatal("ui.intro was not saved")
	}
}

func TestPlayTheIntroSendsReplayEvenWithIntroOff(t *testing.T) {
	a, cfg, _ := folderTestApp(t)
	cfg.UI.Intro = false
	a.openSettings()
	selectSettingRow(t, a, "play the intro")
	_, cmd := a.updateSettings(key("enter"))
	if cmd == nil {
		t.Fatal("enter on play the intro did nothing")
	}
	msg := cmd()
	if _, ok := msg.(replayIntroMsg); !ok {
		t.Fatalf("play the intro sent %T, want replayIntroMsg", msg)
	}

	a.width, a.height = 120, 36
	a.Update(msg)
	if !a.intro.active {
		t.Fatal("replayIntroMsg did not start the intro")
	}
}

func folderEditorGoldenApp() *App {
	a := navigationApp()
	a.width, a.height = 120, 36
	a.motion.noMotion = true
	a.cfg = &config.Config{DownloadDir: `D:\Media`}
	a.settings.folder = folderSettings{loaded: true, saved: "~/Downloads/tork", free: 412e9, freeOK: true, override: `D:\Media`}
	a.screen = screenSettings
	a.settings.from = screenSearch
	ti := newPathPrompt(pathActionNone, downloadItem{}, glyphs.prompt+" ", "~/Media/films").input
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.CursorEnd()
	ti.Focus()
	a.settings.overlay = &folderEditor{
		input: ti,
		check: folderCheck{input: "~/Media/films", dir: "/x", create: true, free: 1.2e12, freeOK: true},
		pick:  -1,
		picks: []folderPick{
			{path: "~/Downloads/tork", note: "OS default"},
			{path: `D:\Media`, note: "used by 3 downloads"},
			{path: `E:\`, note: "other drive · 1.2 TB free"},
		},
	}
	return a
}

func TestGoldenFolderEditor(t *testing.T) {
	a := folderEditorGoldenApp()
	view := a.View()
	assertRenderFits(t, view, 120)
	assertGolden(t, "settings_folder_120x36", view)
	a.width, a.height = 80, 24
	assertRenderFits(t, a.View(), 80)
}
