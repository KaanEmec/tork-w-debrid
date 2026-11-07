package tui

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

func folderTestApp(t *testing.T) (*App, *config.Config, *engine.Engine) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	cfg, err := config.LoadFrom(filepath.Join(t.TempDir(), ".tork"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	a := New(cfg, eng, nil, &state.State{}, nil)
	a.width, a.height = 100, 30
	return a, cfg, eng
}

// selectSettingRow moves the cursor to the row with label.
func selectSettingRow(t *testing.T, a *App, label string) {
	t.Helper()
	for i, r := range a.settingRows() {
		if r.label() == label {
			a.settings.cursor = i
			return
		}
	}
	t.Fatalf("no settings row %q", label)
}

func typeInto(a *App, text string) {
	a.updateSettings(tea.KeyMsg{Type: tea.KeyCtrlU})
	a.updateSettings(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestFolderEditorSavesNewFolderForConfigPreviewAndDownloads(t *testing.T) {
	a, cfg, eng := folderTestApp(t)
	a.openSettings()
	selectSettingRow(t, a, "default folder")
	a.updateSettings(key("enter"))
	if _, ok := a.settings.overlay.(*folderEditor); !ok || !a.typing() {
		t.Fatalf("enter did not open the folder editor as a text field (overlay %T)", a.settings.overlay)
	}
	target := filepath.Join(t.TempDir(), "Media", "films")
	typeInto(a, target)
	a.updateSettings(key("enter"))
	ed := a.settings.overlay.(*folderEditor)
	if !ed.confirm || !strings.Contains(ansi.Strip(a.View()), "will be created") {
		t.Fatalf("missing folder did not ask before creating it:\n%s", ansi.Strip(a.View()))
	}
	// n leaves everything as it was.
	a.updateSettings(key("n"))
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("n created the folder anyway (stat err %v)", err)
	}
	if saved, _ := config.LoadReadOnlyFrom(cfg.Dir()); samePath(saved.DownloadDir, target) {
		t.Fatal("n saved the folder anyway")
	}

	a.updateSettings(key("enter"))
	a.updateSettings(key("y"))
	if a.settings.overlay != nil {
		t.Fatalf("editor still open after saving: %s", ed.refused)
	}
	saved, err := config.LoadReadOnlyFrom(cfg.Dir())
	if err != nil || !samePath(saved.DownloadDir, target) {
		t.Fatalf("config download_dir = %q (err %v), want %q", saved.DownloadDir, err, target)
	}
	if raw, _ := os.ReadFile(cfg.ConfigPath()); !strings.Contains(string(raw), "download_dir:") {
		t.Fatalf("config.yaml has no download_dir:\n%s", raw)
	}
	if got := a.previewDestination(); !samePath(got, target) {
		t.Fatalf("preview saves to %q, want %q", got, target)
	}
	if !strings.Contains(a.toast.text, "New downloads go to") {
		t.Fatalf("toast = %q", a.toast.text)
	}

	payload := []byte("a new download lands in the new folder")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.iso", time.Time{}, strings.NewReader(string(payload)))
	}))
	defer ts.Close()
	h, err := eng.AddDirect(ts.URL+"/x.iso", "x.iso", fmt.Sprintf("%x", sha256.Sum256(payload)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s, ok := eng.Snapshot(h); ok && s.State == engine.StateDone {
			if !samePath(s.DownloadDir, target) {
				t.Fatalf("download went to %q, want %q", s.DownloadDir, target)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(target, "x.iso")); err != nil {
		t.Fatal(err)
	}
}

func TestFolderEditorRefusesUnwritableFolder(t *testing.T) {
	a, cfg, _ := folderTestApp(t)
	folderWritable = func(string) bool { return false }
	t.Cleanup(func() { folderWritable = writableDir })
	before, _ := os.ReadFile(cfg.ConfigPath())

	a.openSettings()
	selectSettingRow(t, a, "default folder")
	a.updateSettings(key("enter"))
	typeInto(a, t.TempDir())
	a.updateSettings(key("enter"))
	ed, ok := a.settings.overlay.(*folderEditor)
	if !ok {
		t.Fatal("editor closed on an unwritable folder")
	}
	if view := ansi.Strip(a.View()); !strings.Contains(view, "✗ not writable") {
		t.Fatalf("no inline error:\n%s", view)
	}
	if ed.confirm {
		t.Fatal("asked to create a folder that exists")
	}
	if after, _ := os.ReadFile(cfg.ConfigPath()); string(after) != string(before) {
		t.Fatal("config changed although saving was refused")
	}
	a.updateSettings(key("esc"))
	if a.settings.overlay != nil || a.screen != screenSettings {
		t.Fatal("esc did not cancel the editor back to settings")
	}
}

func TestCheckFolderVerdicts(t *testing.T) {
	tork := filepath.Join(t.TempDir(), ".tork")
	existing := t.TempDir()
	file := filepath.Join(existing, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw, want string
		create    bool
	}{
		{"", "choose a folder", false},
		{"relative/folder", "absolute", false},
		{filepath.Join(tork, "downloads"), "tork's own config folder", false},
		{file, "not a folder", false},
		{filepath.Join(file, "below"), "is not a folder", false},
		{existing, "", false},
		{filepath.Join(existing, "new", "deeper"), "", true},
	} {
		c := checkFolder(tc.raw, tork)
		if tc.want == "" && c.err != "" || tc.want != "" && !strings.Contains(c.err, tc.want) {
			t.Errorf("checkFolder(%q) err = %q, want %q", tc.raw, c.err, tc.want)
		}
		if c.create != tc.create {
			t.Errorf("checkFolder(%q) create = %v, want %v", tc.raw, c.create, tc.create)
		}
	}
}

// Windows path forms that don't touch the file system.
func TestFolderPathFormsOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path forms")
	}
	if c := checkFolder(`Media\films`, ""); !strings.Contains(c.err, "absolute") {
		t.Fatalf(`Media\films: %q`, c.err)
	}
	if c := checkFolder(`\Media`, ""); !strings.Contains(c.err, "absolute") {
		t.Fatalf(`\Media (no drive): %q`, c.err)
	}
	if !samePath(`C:\Users\Kaan\Downloads`, `c:\users\kaan\downloads\`) {
		t.Fatal("Windows paths compare case-insensitively")
	}
	if got, ok := completeFolder("D:"); !ok || got != `D:\` {
		t.Fatalf(`completeFolder("D:") = %q, %v`, got, ok)
	}
	home, _ := os.UserHomeDir()
	if got := expandTilde(`~\Media`); got != filepath.Join(home, "Media") {
		t.Fatalf(`~\Media expanded to %q`, got)
	}
}

func TestCompleteFolderNames(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"Movies", "Music", "Shows"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "Shopping.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	if got, ok := completeFolder(filepath.Join(root, "Sh")); !ok || got != filepath.Join(root, "Shows")+sep {
		t.Fatalf("Sh completed to %q, %v", got, ok)
	}
	// Movies and Music share only "M": nothing to add.
	if _, ok := completeFolder(filepath.Join(root, "M")); ok {
		t.Fatal("M has no longer common prefix, but completion reported progress")
	}
	if got, ok := completeFolder(filepath.Join(root, "Mus")); !ok || got != filepath.Join(root, "Music")+sep {
		t.Fatalf("Mus completed to %q", got)
	}
}

func TestFolderPicksListUsedFoldersOnce(t *testing.T) {
	a := navigationApp()
	a.st = &state.State{}
	media := filepath.Join(t.TempDir(), "Media")
	for i := 0; i < 3; i++ {
		a.st.Upsert(state.Entry{Magnet: fmt.Sprint("m", i), DownloadDir: media})
	}
	picks := a.folderPicks()
	if picks[0].note != "OS default" {
		t.Fatalf("first pick = %+v, want the OS default", picks[0])
	}
	found := 0
	for _, p := range picks {
		if samePath(p.path, media) {
			found++
			if p.note != "used by 3 downloads" {
				t.Fatalf("pick note = %q", p.note)
			}
		}
	}
	if found != 1 {
		t.Fatalf("media folder listed %d times in %+v", found, picks)
	}
}

func TestFolderRowShowsDashDOverride(t *testing.T) {
	a, cfg, _ := folderTestApp(t)
	run := t.TempDir()
	cfg.DownloadDir = run
	a.openSettings()
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "this run uses -d") {
		t.Fatalf("no -d note:\n%s", view)
	}
	a.loadFolderSettings()
	if a.settings.folder.override != run {
		t.Fatalf("override = %q, want %q", a.settings.folder.override, run)
	}
}
