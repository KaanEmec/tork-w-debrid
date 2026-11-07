package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kaanemec/tork-w-debrid/internal/config"
)

// The default-folder editor (J5.3, look from J8.7): a path input with a live
// check line under it and quick picks below.
//
//	default folder
//	❯ ~/Media/films█
//	✓ will be created · writable · 1.2 TB free
//
//	quick picks
//	▍ ~/Downloads/tork              OS default
//	  D:\Media                      used by 3 downloads
//	  E:\                           other drive · 1.2 TB free

const folderPickWidth = 30

// folderCheck is the live verdict on what is typed.
type folderCheck struct {
	input  string
	dir    string // absolute and cleaned
	err    string // why the folder can't be used; "" when it can
	create bool   // missing, and would be created
	free   int64
	freeOK bool
}

type folderPick struct {
	path string
	note string
}

type folderEditor struct {
	input   textinput.Model
	check   folderCheck
	seq     int
	picks   []folderPick
	pick    int // highlighted quick pick, -1 for none
	confirm bool
	refused string // why the last enter didn't save
}

// folderCheckMsg carries a finished check; stale ones (seq) are dropped.
type folderCheckMsg struct {
	seq   int
	check folderCheck
}

func (m folderCheckMsg) applySettings(a *App) tea.Cmd {
	if ed, ok := a.settings.overlay.(*folderEditor); ok && ed.seq == m.seq {
		ed.check = m.check
	}
	return nil
}

func (a *App) openFolderEditor() tea.Cmd {
	f := a.folderSettings()
	ti := newPathPrompt(pathActionNone, downloadItem{}, glyphs.prompt+" ", tildePath(f.saved)).input
	ti.Cursor.SetMode(cursor.CursorStatic) // the frame clock blinks it
	ti.CursorEnd()
	ti.Focus()
	ed := &folderEditor{input: ti, pick: -1, picks: a.folderPicks()}
	a.settings.overlay = ed
	return ed.recheck(a)
}

// recheck starts a check of the current input.
func (ed *folderEditor) recheck(a *App) tea.Cmd {
	ed.seq++
	ed.refused = ""
	seq, raw, torkDir := ed.seq, ed.input.Value(), ""
	if a.cfg != nil {
		torkDir = a.cfg.Dir()
	}
	ed.check = folderCheck{input: raw}
	return func() tea.Msg { return folderCheckMsg{seq: seq, check: checkFolder(raw, torkDir)} }
}

func (ed *folderEditor) crumb() string         { return "default folder" }
func (ed *folderEditor) typing() bool          { return true }
func (ed *folderEditor) dialog() bool          { return false }
func (ed *folderEditor) close(*App)            {}
func (ed *folderEditor) animating(a *App) bool { return true } // cursor blink

func (ed *folderEditor) keys(*App) []keyHint {
	if ed.confirm {
		return []keyHint{onStrip("y", "create it and save"), onStrip("n / esc", "don't create")}
	}
	return []keyHint{
		onStrip("enter", "save"),
		onStrip("tab", "complete"),
		onStrip("↑↓", "quick picks"),
		onStrip("esc", "cancel"),
		inCard("~", "your home folder"),
	}
}

func (ed *folderEditor) update(a *App, key tea.KeyMsg) tea.Cmd {
	if ed.confirm {
		switch key.String() {
		case "y", "Y":
			ed.confirm = false
			return ed.save(a)
		case "n", "N", "esc":
			ed.confirm = false
		}
		return nil
	}
	switch key.String() {
	case "esc":
		a.closeSettingOverlay()
		return nil
	case "?":
		a.showHelp, a.helpOffset = true, 0
		return nil
	case "enter":
		if ed.check.input != ed.input.Value() || ed.check.dir == "" && ed.check.err == "" {
			ed.check = checkFolder(ed.input.Value(), a.cfg.Dir())
		}
		switch {
		case ed.check.err != "":
			ed.refused = ed.check.err
			return nil
		case ed.check.create:
			ed.confirm = true
			return nil
		}
		return ed.save(a)
	case "tab":
		if done, ok := completeFolder(ed.input.Value()); ok {
			ed.input.SetValue(done)
			ed.input.CursorEnd()
			ed.pick = -1
			return ed.recheck(a)
		}
		return nil
	case "up", "down":
		if len(ed.picks) == 0 {
			return nil
		}
		if key.String() == "down" {
			ed.pick = min(ed.pick+1, len(ed.picks)-1)
		} else {
			ed.pick = max(ed.pick-1, 0)
		}
		ed.input.SetValue(tildePath(ed.picks[ed.pick].path))
		ed.input.CursorEnd()
		return ed.recheck(a)
	}
	before := ed.input.Value()
	var cmd tea.Cmd
	ed.input, cmd = ed.input.Update(key)
	if ed.input.Value() != before {
		ed.pick = -1
		return tea.Batch(cmd, ed.recheck(a))
	}
	return cmd
}

func (ed *folderEditor) save(a *App) tea.Cmd {
	cmd, err := a.saveDownloadFolder(ed.check.dir)
	if err != nil {
		ed.refused = err.Error()
		return cmd
	}
	a.closeSettingOverlay()
	return cmd
}

func (ed *folderEditor) view(a *App, width, height int) string {
	mode := cursor.CursorStatic
	if !cursorVisible(a.vframe()) {
		mode = cursor.CursorHide
	}
	if ed.input.Cursor.Mode() != mode {
		ed.input.Cursor.SetMode(mode)
	}
	ed.input.Width = max(10, width-6)

	lines := []string{"", "  " + styleDim.Render("default folder"), "  " + ed.input.View(), "  " + ed.checkLine()}
	if ed.confirm {
		lines = append(lines, "  "+styleWarn.Render("create "+tildePath(ed.check.dir)+"?")+"  "+hints(hint("y", "yes"), hint("n", "no")))
	}
	if len(ed.picks) > 0 {
		lines = append(lines, "", "  "+styleDim.Render("quick picks"))
		for i, p := range ed.picks {
			content := " " + padRight(styleFg.Render(truncate(tildePath(p.path), folderPickWidth)), folderPickWidth) + "  " + styleDim.Render(p.note)
			if i == ed.pick {
				lines = append(lines, " "+styleSelBar.Background(colRaised).Render(glyphs.cursor)+selectedRow(content, width-2))
			} else {
				lines = append(lines, "  "+content)
			}
		}
	}
	if o := a.folderSettings().override; o != "" {
		body := padLines(strings.Join(lines, "\n"), max(len(lines), height-1))
		return body + "\n  " + styleDim.Render("this run uses -d "+tildePath(o))
	}
	return strings.Join(lines, "\n")
}

// checkLine is "✓ will be created · writable · 1.2 TB free", or the reason
// in rose.
func (ed *folderEditor) checkLine() string {
	c := ed.check
	if ed.refused != "" {
		return styleErr.Render("✗ " + ed.refused)
	}
	switch {
	case c.err != "":
		return styleErr.Render("✗ " + c.err)
	case c.dir == "":
		return styleDim.Render("checking…")
	}
	var parts []string
	if c.create {
		parts = append(parts, "will be created")
	}
	parts = append(parts, "writable")
	if c.freeOK {
		parts = append(parts, freeText(c.free))
	}
	return styleOK.Render("✓") + " " + styleDim.Render(strings.Join(parts, " · "))
}

// checkFolder decides whether raw can be the default folder, without
// creating anything: it must be absolute, outside tork's own folder, a
// folder (or not exist yet, with a writable folder above it) and writable.
func checkFolder(raw, torkDir string) folderCheck {
	c := folderCheck{input: raw}
	if strings.TrimSpace(raw) == "" {
		c.err = config.ErrDownloadDirEmpty.Error()
		return c
	}
	dir := expandTilde(strings.TrimSpace(raw))
	if !filepath.IsAbs(dir) {
		c.err = config.ErrDownloadDirRelative.Error()
		return c
	}
	dir = filepath.Clean(dir)
	c.dir = dir
	if torkDir != "" && pathInside(dir, torkDir) {
		c.err = config.ErrDownloadDirInsideTork.Error()
		return c
	}
	existing := dir
	for {
		info, err := os.Stat(existing)
		if err == nil {
			if !info.IsDir() {
				c.create = false
				if existing == dir {
					c.err = "not a folder"
				} else {
					c.err = tildePath(existing) + " is not a folder"
				}
				return c
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			c.err = "can't read it: " + firstLine(err)
			return c
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			c.err = "drive or volume not found"
			return c
		}
		existing = parent
		c.create = true
	}
	if !folderWritable(existing) {
		c.err = "not writable"
		return c
	}
	if n, err := settingsFreeBytes(existing); err == nil {
		c.free, c.freeOK = n, true
	}
	return c
}

func firstLine(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return strings.SplitN(err.Error(), "\n", 2)[0]
}

// folderWritable is the writability probe; tests replace it.
var folderWritable = writableDir

// writableDir proves a folder is writable with a probe file it removes again.
func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".tork-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name) == nil
}

// pathInside reports whether path is base or inside it.
func pathInside(path, base string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// expandTilde expands a leading ~ (followed by / or, on Windows, \).
func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !(filepath.Separator == '\\' && strings.HasPrefix(p, `~\`)) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p[1:])
}

// completeFolder completes the last part of raw to a folder name, as far as
// the matching folders agree. A single match gets a trailing separator.
func completeFolder(raw string) (string, bool) {
	expanded := expandTilde(raw)
	sep := "/"
	if filepath.Separator == '\\' && !strings.Contains(raw, "/") {
		sep = `\`
	}
	var parent, prefix string
	switch {
	case raw == "~":
		return raw + sep, true
	case isDriveOnly(raw):
		return raw + `\`, true
	case strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, string(filepath.Separator)):
		parent = expanded
	default:
		parent, prefix = filepath.Dir(expanded), filepath.Base(expanded)
	}
	if parent == "" || !filepath.IsAbs(parent) {
		return raw, false
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return raw, false
	}
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			if info, err := os.Stat(filepath.Join(parent, name)); err != nil || !info.IsDir() {
				continue
			}
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if hasPrefix(name, prefix, fold) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return raw, false
	}
	sort.Strings(names)
	done := names[0]
	if len(names) > 1 {
		done = commonPrefix(names, fold)
		if len(done) <= len(prefix) {
			return raw, false
		}
	} else {
		done += sep
	}
	return raw[:len(raw)-len(prefix)] + done, true
}

func isDriveOnly(s string) bool {
	return runtime.GOOS == "windows" && len(s) == 2 && s[1] == ':' &&
		(('a' <= s[0] && s[0] <= 'z') || ('A' <= s[0] && s[0] <= 'Z'))
}

func hasPrefix(s, prefix string, fold bool) bool {
	if len(s) < len(prefix) {
		return false
	}
	if fold {
		return strings.EqualFold(s[:len(prefix)], prefix)
	}
	return strings.HasPrefix(s, prefix)
}

func commonPrefix(names []string, fold bool) string {
	p := names[0]
	for _, n := range names[1:] {
		i := 0
		for i < len(p) && i < len(n) && (p[i] == n[i] || fold && strings.EqualFold(p[i:i+1], n[i:i+1])) {
			i++
		}
		p = p[:i]
	}
	return p
}

// folderPicks are the editor's quick picks: the OS default, folders existing
// downloads use, and on Windows the roots of the other fixed drives.
func (a *App) folderPicks() []folderPick {
	def := config.OSDefaultDownloadDir()
	used := map[string]int{}
	var order []string
	if a.st != nil {
		for _, e := range a.st.Entries {
			if e.DownloadDir == "" {
				continue
			}
			dir := filepath.Clean(e.DownloadDir)
			key := pathKey(dir)
			if used[key] == 0 {
				order = append(order, dir)
			}
			used[key]++
		}
	}
	usedBy := func(n int) string {
		if n == 1 {
			return "used by 1 download"
		}
		return fmt.Sprintf("used by %d downloads", n)
	}
	picks := []folderPick{{path: def, note: "OS default"}}
	if n := used[pathKey(def)]; n > 0 {
		picks[0].note += " · " + usedBy(n)
	}
	sort.SliceStable(order, func(i, j int) bool { return used[pathKey(order[i])] > used[pathKey(order[j])] })
	for _, dir := range order {
		if samePath(dir, def) {
			continue
		}
		picks = append(picks, folderPick{path: dir, note: usedBy(used[pathKey(dir)])})
	}
	defVol := strings.ToUpper(filepath.VolumeName(def))
	for _, root := range fixedDriveRoots() {
		if strings.EqualFold(filepath.VolumeName(root), defVol) {
			continue
		}
		note := "other drive"
		if n, err := settingsFreeBytes(root); err == nil {
			note += " · " + freeText(n)
		}
		picks = append(picks, folderPick{path: root, note: note})
	}
	if len(picks) > 6 {
		picks = picks[:6]
	}
	return picks
}

func pathKey(p string) string {
	p = filepath.Clean(p)
	if filepath.Separator == '\\' {
		return strings.ToLower(p)
	}
	return p
}
