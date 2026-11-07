package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The golden file is written in the YAML encoder's own layout, so an edit of
// one key must leave every other byte as it was.
func TestWriteDownloadDirChangesOnlyThatKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".tork")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := "# tork config\n" +
		"download_dir: /old/place # where files go\n" +
		"seed_after_complete: true\n" +
		"providers:\n" +
		"  knaben:\n" +
		"    enabled: true # default source\n" +
		"future_option: stay-put\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default(dir)
	target := filepath.Join(home, "Media", "Downloads")
	got, err := cfg.WriteDownloadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("returned %q, want %q", got, target)
	}
	after, _ := os.ReadFile(path)
	want := strings.Replace(before, "/old/place", "~/Media/Downloads", 1)
	if string(after) != want {
		t.Fatalf("config changed beyond download_dir:\n--- got\n%s--- want\n%s", after, want)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("folder not created: %v", err)
	}
	loaded, err := LoadReadOnlyFrom(dir)
	if err != nil || loaded.DownloadDir != target {
		t.Fatalf("reloaded download_dir = %q %v", loaded.DownloadDir, err)
	}
}

func TestWriteDownloadDirOutsideHomeStaysAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".tork")
	cfg := Default(dir)
	target := t.TempDir() // not under the fake home
	if _, err := cfg.WriteDownloadDir(target); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReadOnlyFrom(dir)
	if err != nil || loaded.DownloadDir != target {
		t.Fatalf("download_dir = %q %v", loaded.DownloadDir, err)
	}
}

func TestWriteDownloadDirRejectsBadChoices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".tork")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	const original = "download_dir: /old/place\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "  ", ErrDownloadDirEmpty},
		{"relative", "downloads/here", ErrDownloadDirRelative},
		{"inside tork", filepath.Join(dir, "metainfo"), ErrDownloadDirInsideTork},
		{"tork itself", "~/.tork", ErrDownloadDirInsideTork},
		{"a file", file, ErrDownloadDirNotDir},
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		ro := filepath.Join(home, "readonly")
		if err := os.Mkdir(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct {
			name string
			raw  string
			want error
		}{"read-only", ro, ErrDownloadDirNotWritable})
	}
	cfg := Default(dir)
	for _, tc := range cases {
		if _, err := cfg.WriteDownloadDir(tc.raw); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("config written on a rejected choice:\n%s", got)
	}
}

func TestHomeRelative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := homeRelative(filepath.Join(home, "a", "b")); got != "~/a/b" {
		t.Fatalf("got %q", got)
	}
	if got := homeRelative(home); got != home {
		t.Fatalf("home itself = %q", got)
	}
	other := filepath.Join(filepath.Dir(home), "elsewhere")
	if got := homeRelative(other); got != other {
		t.Fatalf("outside = %q", got)
	}
}
