package tui

import (
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/health"
)

// Settings › Downloads (J5.3): the default download folder for this machine.

// folderSettings is what the Downloads section shows about the folder.
type folderSettings struct {
	loaded   bool
	saved    string // download_dir in config.yaml, absolute
	free     int64
	freeOK   bool
	override string // the -d folder this run uses instead; "" when none
}

// settingsFreeBytes is the free-space check (doctor's); tests replace it.
var settingsFreeBytes = health.FreeBytes

func init() {
	registerSettingSection(20, func(a *App) settingSection {
		if a.cfg == nil {
			return settingSection{}
		}
		return settingSection{title: "Downloads", rows: a.downloadSettingRows()}
	})
	settingsOpeners = append(settingsOpeners, func(a *App) tea.Cmd {
		a.loadFolderSettings()
		return nil
	})
}

// downloadSettingRows lists the Downloads rows: the folder, then the
// connections per debrid download (J7.5).
func (a *App) downloadSettingRows() []settingRow {
	return []settingRow{folderRow{}, connectionsRow{}}
}

// loadFolderSettings reads download_dir as saved in config.yaml. When the
// run's folder differs, the run was started with -d.
func (a *App) loadFolderSettings() {
	f := &a.settings.folder
	f.saved, f.override = a.cfg.DownloadDir, ""
	if dir := a.cfg.Dir(); dir != "" {
		if saved, err := config.LoadReadOnlyFrom(dir); err == nil && saved.DownloadDir != "" {
			f.saved = filepath.Clean(saved.DownloadDir)
		}
	}
	if run := a.cfg.DownloadDir; run != "" && !samePath(run, f.saved) {
		f.override = run
	}
	f.free, f.freeOK = 0, false
	if n, err := settingsFreeBytes(f.saved); err == nil {
		f.free, f.freeOK = n, true
	}
	f.loaded = true
}

func (a *App) folderSettings() *folderSettings {
	if !a.settings.folder.loaded && a.cfg != nil {
		a.loadFolderSettings()
	}
	return &a.settings.folder
}

// folderRow is "default folder  ~/Downloads/tork · 412 GB free".
type folderRow struct{}

func (folderRow) label() string { return "default folder" }

func (folderRow) value(a *App) string {
	f := a.folderSettings()
	v := styleFg.Render(tildePath(f.saved))
	if f.freeOK {
		v += styleDim.Render(" · " + freeText(f.free))
	}
	return v
}

func (folderRow) hint(*App) string { return "enter change" }

func (folderRow) keys(*App) []keyHint {
	return []keyHint{onStrip("enter", "change folder"), inCard("r", "reset to the OS default")}
}

func (folderRow) note(a *App) string {
	if o := a.folderSettings().override; o != "" {
		return styleDim.Render("this run uses -d " + tildePath(o))
	}
	return ""
}

func (folderRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	switch key.String() {
	case "enter":
		return true, a.openFolderEditor()
	case "r":
		def := config.OSDefaultDownloadDir()
		a.settings.confirm = &settingConfirm{
			question: "reset the default folder to " + tildePath(def) + "?",
			yes: func(a *App) tea.Cmd {
				cmd, err := a.saveDownloadFolder(def)
				if err != nil {
					return tea.Batch(cmd, a.showError("folder not saved: "+err.Error()))
				}
				return cmd
			},
		}
		return true, nil
	}
	return false, nil
}

// saveDownloadFolder makes dir the default folder: saved entries without a
// folder are pinned to the old one first, then config.yaml, the engine and
// the in-memory config change, so nothing that exists appears to move. With
// -d in force the run keeps using that folder.
func (a *App) saveDownloadFolder(dir string) (tea.Cmd, error) {
	f := a.folderSettings()
	pinned := 0
	if a.st != nil && f.saved != "" {
		pinned = a.st.PinDownloadDir(f.saved)
	}
	abs, err := a.cfg.WriteDownloadDir(dir)
	var cmds []tea.Cmd
	if pinned > 0 {
		cmds = append(cmds, a.saveState())
	}
	if err != nil {
		return tea.Batch(cmds...), err
	}
	f.saved = abs
	if n, err := settingsFreeBytes(abs); err == nil {
		f.free, f.freeOK = n, true
	} else {
		f.free, f.freeOK = 0, false
	}
	msg := "New downloads go to " + tildePath(abs)
	if f.override == "" {
		if a.eng != nil {
			a.eng.SetDefaultDownloadDir(abs)
		}
		a.cfg.DownloadDir = abs
	} else {
		msg = "Saved · this run still uses -d " + tildePath(f.override)
	}
	return tea.Batch(append(cmds, a.showToast(msg, toastOK, toastLong))...), nil
}

// freeText writes free space the way a disk tool does: "412 GB free",
// "1.2 TB free".
func freeText(n int64) string {
	const (
		gb = 1e9
		tb = 1e12
	)
	f := float64(n)
	switch {
	case f >= tb:
		return trimFloat(f/tb) + " TB free"
	case f >= gb:
		return trimFloat(f/gb) + " GB free"
	default:
		return trimFloat(f/1e6) + " MB free"
	}
}

func trimFloat(v float64) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f", v)
	}
	s := fmt.Sprintf("%.1f", v)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s
}

// samePath compares two folders the way the file system would.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if filepath.Separator == '\\' {
		return equalFoldASCII(a, b)
	}
	return a == b
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// connectionsRow is "connections per download  1  2  [4]  8  16" (J7.5):
// how many connections one Real-Debrid file downloads with. ←→ steps through
// the choices and saves at once; the line under it is the per-torrent cap
// (debrid.max_total_connections).
type connectionsRow struct{}

func (connectionsRow) label() string { return "connections per download" }

func (connectionsRow) value(a *App) string {
	cur := a.cfg.Debrid.Connections
	parts := make([]string, 0, len(config.DebridConnectionChoices))
	for _, n := range config.DebridConnectionChoices {
		if n == cur {
			parts = append(parts, styleMatch.Render(fmt.Sprintf("[%d]", n)))
		} else {
			parts = append(parts, styleDim.Render(fmt.Sprint(n)))
		}
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "  "
		}
		out += p
	}
	if !containsInt(config.DebridConnectionChoices, cur) {
		out += styleDim.Render(fmt.Sprintf("  (now %d)", cur))
	}
	return out
}

func (connectionsRow) hint(*App) string { return "←→" }

func (connectionsRow) keys(*App) []keyHint {
	return []keyHint{onStrip("←→", "connections")}
}

func (connectionsRow) note(a *App) string {
	return styleDim.Render(fmt.Sprintf("up to %d per torrent", a.cfg.Debrid.MaxTotalConnections))
}

func (connectionsRow) update(a *App, key tea.KeyMsg) (bool, tea.Cmd) {
	var step int
	switch key.String() {
	case "left", "h":
		step = -1
	case "right", "l":
		step = 1
	default:
		return false, nil
	}
	n := stepChoice(config.DebridConnectionChoices, a.cfg.Debrid.Connections, step)
	if n == a.cfg.Debrid.Connections {
		return true, nil
	}
	if err := a.cfg.WriteDebridConnections(n); err != nil {
		return true, a.showError("not saved: " + err.Error())
	}
	// New debrid transfers use it, and running ones pick it up the next time
	// a connection claims a segment.
	a.cfg.Debrid.Connections = n
	if a.eng != nil {
		a.eng.SetDebridSettings(a.cfg.Debrid)
	}
	return true, nil
}

// stepChoice moves from cur to the next choice in direction step, also from
// a value that isn't one of the choices.
func stepChoice(choices []int, cur, step int) int {
	if step > 0 {
		for _, c := range choices {
			if c > cur {
				return c
			}
		}
		return cur
	}
	for i := len(choices) - 1; i >= 0; i-- {
		if choices[i] < cur {
			return choices[i]
		}
	}
	return cur
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
