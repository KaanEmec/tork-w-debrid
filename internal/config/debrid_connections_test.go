package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebridConnectionDefaults(t *testing.T) {
	d := Default(t.TempDir()).Debrid
	if d.Connections != 4 || d.MaxTotalConnections != 8 {
		t.Fatalf("defaults = %+v, want 4 per file and 8 per torrent", d)
	}
}

func TestOutOfRangeDebridConnectionsAreRejectedAtLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	for _, tc := range []struct{ yaml, want string }{
		{"debrid:\n  connections: 0\n", "debrid.connections must be between 1 and 16"},
		{"debrid:\n  connections: -1\n", "debrid.connections must be between 1 and 16"},
		{"debrid:\n  connections: 64\n", "debrid.connections must be between 1 and 16"},
		{"debrid:\n  max_total_connections: 0\n", "debrid.max_total_connections must be between 1 and 32"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFrom(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoadFrom(%q) err = %v, want %q", tc.yaml, err, tc.want)
		}
		if _, err := LoadReadOnlyFrom(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoadReadOnlyFrom(%q) err = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

func TestWriteDebridConnections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("# mine\ndebrid:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default(dir)
	if err := cfg.WriteDebridConnections(8); err != nil {
		t.Fatal(err)
	}
	if err := cfg.WriteDebridConnections(17); err == nil {
		t.Fatal("17 connections accepted")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# mine") || !strings.Contains(string(raw), "connections: 8") {
		t.Fatalf("config.yaml:\n%s", raw)
	}
	again, err := LoadReadOnlyFrom(dir)
	if err != nil || again.Debrid.Connections != 8 {
		t.Fatalf("reloaded connections = %d (err %v)", again.Debrid.Connections, err)
	}
}
