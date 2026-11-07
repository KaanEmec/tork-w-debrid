package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDebridDefaults(t *testing.T) {
	d := Default(t.TempDir()).Debrid
	if !d.Enabled || d.DeleteAfterDownload || d.Provider != "realdebrid" {
		t.Fatalf("defaults = %+v", d)
	}
}

func TestDebridBlockMissingKeepsDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("download_dir: "+filepath.ToSlash(t.TempDir())+"\ndebrid:\n  delete_after_download: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadReadOnlyFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Debrid.Enabled || !cfg.Debrid.DeleteAfterDownload || cfg.Debrid.Provider != "realdebrid" {
		t.Fatalf("debrid = %+v", cfg.Debrid)
	}
	if cfg.DebridPath() != filepath.Join(dir, "debrid.json") {
		t.Fatalf("DebridPath = %q", cfg.DebridPath())
	}
}

func TestWriteDebridSwitchesPreserveComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "# my settings\nfuture_option: stay-put\ndebrid:\n  # route downloads\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(data), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := Default(dir)
	if err := cfg.WriteDebridEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.WriteDebridDeleteAfterDownload(true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	text := string(got)
	for _, want := range []string{"# my settings", "future_option: stay-put", "# route downloads", "enabled: false", "delete_after_download: true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lost %q:\n%s", want, text)
		}
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %v", info.Mode().Perm())
		}
	}
	loaded, err := LoadReadOnlyFrom(dir)
	if err != nil || loaded.Debrid.Enabled || !loaded.Debrid.DeleteAfterDownload {
		t.Fatalf("reloaded = %+v %v", loaded.Debrid, err)
	}
}

func TestWriteDebridEnabledCreatesConfigWhenMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".tork")
	cfg := Default(dir)
	if err := cfg.WriteDebridEnabled(false); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReadOnlyFrom(dir)
	if err != nil || loaded.Debrid.Enabled {
		t.Fatalf("reloaded = %+v %v", loaded.Debrid, err)
	}
}

func TestWriteDebridRefusesMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("debrid: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Default(dir).WriteDebridEnabled(false); err == nil {
		t.Fatal("edited malformed config")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "debrid: [unclosed\n" {
		t.Fatal("malformed config was rewritten")
	}
}
