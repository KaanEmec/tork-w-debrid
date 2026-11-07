package engine

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSetDefaultDownloadDirAppliesToNewDownloadsOnly(t *testing.T) {
	payload := make([]byte, 32<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ts := serveISO(t, payload, nil)
	eng := newDirectTestEngine(t)
	oldDir := eng.DefaultDownloadDir()

	first, err := eng.AddDirect(ts.URL+"/a.iso", "a.iso", sumHex(payload))
	if err != nil {
		t.Fatal(err)
	}
	newDir := t.TempDir()
	eng.SetDefaultDownloadDir(newDir)
	second, err := eng.AddDirect(ts.URL+"/b.iso", "b.iso", sumHex(payload))
	if err != nil {
		t.Fatal(err)
	}
	a := awaitDirect(t, eng, func(s Snapshot) bool { return s.Hash == first && s.State == StateDone }, 10*time.Second)
	b := awaitDirect(t, eng, func(s Snapshot) bool { return s.Hash == second && s.State == StateDone }, 10*time.Second)
	if a.DownloadDir != oldDir {
		t.Fatalf("existing download moved to %q, want %q", a.DownloadDir, oldDir)
	}
	if b.DownloadDir != newDir {
		t.Fatalf("new download went to %q, want %q", b.DownloadDir, newDir)
	}
	if _, err := os.Stat(filepath.Join(newDir, "b.iso")); err != nil {
		t.Fatal(err)
	}
}

// Run with -race: changing the default while downloads are added must not
// race with normalizeOptions.
func TestSetDefaultDownloadDirConcurrentWithAdd(t *testing.T) {
	payload := make([]byte, 4<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ts := serveISO(t, payload, nil)
	eng := newDirectTestEngine(t)
	dirs := []string{t.TempDir(), t.TempDir()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			eng.SetDefaultDownloadDir(dirs[i%2])
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 5 {
			name := string(rune('a'+i)) + ".iso"
			if _, err := eng.AddDirect(ts.URL+"/"+name, name, sumHex(payload)); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
}
