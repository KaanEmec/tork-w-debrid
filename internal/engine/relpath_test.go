package engine

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSafeRelPath(t *testing.T) {
	deep := strings.Repeat("d/", maxRelPathParts-1) + "f.mkv"
	tooDeep := strings.Repeat("d/", maxRelPathParts) + "f.mkv"
	long := strings.Repeat(strings.Repeat("x", 200)+"/", 6) + "f.mkv" // > 1,024 bytes
	cases := []struct {
		rel  string
		want bool
	}{
		{"a/b/c.mkv", true},
		{"Show (2024)/S01/e01.mkv", true},
		{"single.iso", true},
		{deep, true},
		{"../x", false},
		{"a/../../x", false},
		{"a/./b", false},
		{"/etc/x", false},
		{"C:/x", false},
		{"c:x", false},
		{`a\b`, false},
		{`a/b\..\c`, false},
		{"a//b", false},
		{"a/b/", false},
		{"", false},
		{"CON/x", false},
		{"a/nul.txt", false},
		{"a/b.", false},
		{"a /b", false},
		{"a/\u202egnp.exe", false},
		{"a/\u2066b", false},
		{"a/b\x00c", false},
		{"a/b\nc", false},
		{tooDeep, false},
		{long, false},
	}
	for _, c := range cases {
		if got := safeRelPath(c.rel); got != c.want {
			t.Errorf("safeRelPath(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestSafeRelDataPathStaysInsideDir(t *testing.T) {
	dir := t.TempDir()
	got, ok := safeRelDataPath(dir, "Show (2024)/S01/e01.mkv")
	if !ok {
		t.Fatal("a plain nested path was refused")
	}
	if want := filepath.Join(dir, "Show (2024)", "S01", "e01.mkv"); got != want {
		t.Fatalf("safeRelDataPath = %q, want %q", got, want)
	}
	if !safePathWithin(dir, got) {
		t.Fatalf("%q is not inside %q", got, dir)
	}
	for _, rel := range []string{"../x", "a/../../x", "/etc/x", "C:/x"} {
		if p, ok := safeRelDataPath(dir, rel); ok {
			t.Errorf("safeRelDataPath(%q) = %q, want refusal", rel, p)
		}
	}
}

// linkDir makes link point at target: a symlink, or on Windows without
// symlink rights a junction. It skips the test when neither works.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("neither symlinks nor junctions available: %v %s", err, out)
		}
	}
	t.Skip("symlinks unavailable")
}

func TestRelPathRefusesLinkedSubfolder(t *testing.T) {
	eng := newDirectTestEngine(t)
	outside := t.TempDir()
	if err := os.MkdirAll(eng.cfg.DownloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir(t, outside, filepath.Join(eng.cfg.DownloadDir, "Show"))

	if _, ok := safeRelDataPath(eng.cfg.DownloadDir, "Show/S01/e01.mkv"); ok {
		t.Error("a path through a linked folder was accepted")
	}
	if err := ensureRelParents(eng.cfg.DownloadDir, "Show/S01/e01.mkv"); err == nil {
		t.Error("ensureRelParents walked through a linked folder")
	}
	if _, err := eng.AddDirect("https://example.com/e01.mkv", "Show/S01/e01.mkv", ""); err == nil {
		t.Error("AddDirect accepted a path through a linked folder")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written outside the download dir: %v", entries)
	}
}

// The link may appear after the download was added; the transfer must still
// refuse it before opening the .part file.
func TestRelPathRefusesLinkCreatedBeforeTransfer(t *testing.T) {
	eng := newDirectTestEngine(t)
	outside := t.TempDir()
	dir := eng.cfg.DownloadDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir(t, outside, filepath.Join(dir, "Show"))
	it := &directItem{
		url: "http://127.0.0.1:1/e01.mkv", name: "Show/S01/e01.mkv",
		downloadDir: dir, dataPath: filepath.Join(dir, "Show", "S01", "e01.mkv"),
		state: StateDownloading,
	}
	eng.mu.Lock()
	eng.direct[directHash(it.url)] = it
	eng.startDirectLocked(it)
	done := it.runDone
	eng.mu.Unlock()
	<-done
	eng.mu.Lock()
	state, note := it.state, it.note
	eng.mu.Unlock()
	if state != StatePaused || !strings.Contains(note, "link") {
		t.Fatalf("state = %v, note = %q; want a paused refusal", state, note)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written outside the download dir: %v", entries)
	}
}

func TestDirectDownloadIntoSubfolders(t *testing.T) {
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ts := serveISO(t, payload, nil)
	eng := newDirectTestEngine(t)

	name := "Show (2024)/Season 1/ep01.mkv"
	h, err := eng.AddDirect(ts.URL+"/ep01.mkv", name, sumHex(payload))
	if err != nil {
		t.Fatal(err)
	}
	snap := awaitDirect(t, eng, func(s Snapshot) bool { return s.Hash == h && s.State == StateDone }, 10*time.Second)
	want := filepath.Join(eng.cfg.DownloadDir, "Show (2024)", "Season 1", "ep01.mkv")
	if snap.DataPath != want {
		t.Fatalf("DataPath = %q, want %q", snap.DataPath, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("downloaded bytes differ from payload")
	}
	fi, err := os.Stat(filepath.Dir(want))
	if err != nil || !fi.IsDir() {
		t.Fatalf("parent folder missing: %v", err)
	}
}
