package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFile(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 0 {
		t.Errorf("expected empty state, got %d entries", len(s.Entries))
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC()
	done := now.Add(time.Minute)
	s := &State{}
	s.Upsert(Entry{
		Magnet:         "magnet:?xt=urn:btih:abc",
		Name:           "test",
		AddedAt:        now,
		Done:           true,
		DownloadDir:    "/downloads",
		DataPath:       "/downloads/test",
		NeedsRelink:    true,
		Seed:           Bool(false),
		BytesCompleted: 42,
		Length:         42,
		CompletedAt:    &done,
	})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Name != "test" {
		t.Errorf("round trip mismatch: %+v", got.Entries)
	}
	e := got.Entries[0]
	if e.DownloadDir != "/downloads" || e.DataPath != "/downloads/test" || !e.NeedsRelink || e.Seed == nil || *e.Seed {
		t.Errorf("new fields did not round trip: %+v", e)
	}
	if e.BytesCompleted != 42 || e.Length != 42 || e.CompletedAt == nil {
		t.Errorf("completion fields did not round trip: %+v", e)
	}
}

func TestSeedEnabledDefault(t *testing.T) {
	if !(Entry{}).SeedEnabled(true) {
		t.Fatal("missing seed field should use default true")
	}
	if (Entry{Seed: Bool(false)}).SeedEnabled(true) {
		t.Fatal("explicit false seed should be honored")
	}
}

func TestUpsertReplacesAndRemove(t *testing.T) {
	s := &State{}
	s.Upsert(Entry{Magnet: "m1", Name: "a"})
	s.Upsert(Entry{Magnet: "m1", Name: "b", Done: true})
	if len(s.Entries) != 1 || s.Entries[0].Name != "b" || !s.Entries[0].Done {
		t.Errorf("upsert did not replace: %+v", s.Entries)
	}
	if e := s.Find("m1"); e == nil || e.Name != "b" {
		t.Errorf("Find failed: %+v", e)
	}
	s.Remove("m1")
	if len(s.Entries) != 0 {
		t.Error("remove failed")
	}
}

func TestLoadRecoversFromCorruptState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path) // must NOT error
	if err != nil {
		t.Fatalf("corrupt state should not fail startup: %v", err)
	}
	if len(s.Entries) != 0 {
		t.Errorf("expected empty state, got %d entries", len(s.Entries))
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("corrupt state should be preserved as .bak: %v", err)
	}
}

func TestPinDownloadDirFillsOnlyMissingFolders(t *testing.T) {
	s := &State{Entries: []Entry{
		{Magnet: "a", Name: "a"},
		{Magnet: "b", Name: "b", DownloadDir: "/elsewhere"},
		{Magnet: "c", Name: "c", Paused: true},
	}}
	old := t.TempDir()
	if n := s.PinDownloadDir(old); n != 2 {
		t.Fatalf("pinned %d, want 2", n)
	}
	if s.Entries[0].DownloadDir != old || s.Entries[2].DownloadDir != old || s.Entries[1].DownloadDir != "/elsewhere" {
		t.Fatalf("entries = %+v", s.Entries)
	}
	if s.PinDownloadDir("") != 0 {
		t.Fatal("empty dir pinned entries")
	}
}

// Debrid fields (roadmap J3.4) round-trip; a file from before them loads
// unchanged; upstream tork, which lacks them, still reads a fork-written file.
func TestDebridFieldsAreCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"entries":[{"magnet":"magnet:?xt=urn:btih:abc","name":"old","added_at":"2026-01-02T03:04:05Z","paused":false,"done":false,"excluded":[1]}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil || len(s.Entries) != 1 {
		t.Fatalf("old file: %v, %+v", err, s)
	}
	e := s.Entries[0]
	if e.Name != "old" || e.Via != "" || e.RemoteID != "" || e.Phase != "" || e.RemoteFiles != nil || e.TorrentData != nil || len(e.Excluded) != 1 {
		t.Fatalf("old entry = %+v", e)
	}

	s.Upsert(Entry{
		Magnet: "magnet:?xt=urn:btih:def", Name: "debrid", Via: "realdebrid",
		RemoteID: "RID7", RemoteFiles: []int{2, 5}, Phase: "local", TorrentData: []byte("d4:infod...e"),
	})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	back, _ := Load(path)
	d := back.Find("magnet:?xt=urn:btih:def")
	if d == nil || d.Via != "realdebrid" || d.RemoteID != "RID7" || d.Phase != "local" ||
		len(d.RemoteFiles) != 2 || string(d.TorrentData) != "d4:infod...e" {
		t.Fatalf("debrid entry = %+v", d)
	}

	// upstream's Entry: the same struct without the debrid fields
	type upstreamEntry struct {
		Magnet   string `json:"magnet"`
		Name     string `json:"name"`
		Paused   bool   `json:"paused"`
		Done     bool   `json:"done"`
		Excluded []int  `json:"excluded,omitempty"`
	}
	var upstream struct {
		Entries []upstreamEntry `json:"entries"`
	}
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &upstream); err != nil || len(upstream.Entries) != 2 || upstream.Entries[1].Name != "debrid" {
		t.Fatalf("upstream read: %v, %+v", err, upstream)
	}
}
