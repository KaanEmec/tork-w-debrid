package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIDefaultsAndMissingBlock(t *testing.T) {
	if ui := Default(t.TempDir()).UI; !ui.Motion || !ui.Intro {
		t.Fatalf("defaults = %+v, want both on", ui)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("ui:\n  intro: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadReadOnlyFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UI.Motion || cfg.UI.Intro {
		t.Fatalf("ui = %+v, want motion kept on and intro off", cfg.UI)
	}
}

func TestWriteUISwitchesSurviveReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("# mine\nui:\n  # look\n  motion: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default(dir)
	if err := cfg.WriteUIMotion(false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.WriteUIIntro(false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"# mine", "# look", "motion: false", "intro: false"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lost %q:\n%s", want, raw)
		}
	}
	again, err := LoadReadOnlyFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.UI.Motion || again.UI.Intro {
		t.Fatalf("reloaded ui = %+v, want both off", again.UI)
	}
}
