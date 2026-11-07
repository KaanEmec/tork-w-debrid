package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/intake"
	"github.com/kaanemec/tork-w-debrid/internal/metacache"
	"github.com/kaanemec/tork-w-debrid/internal/provider"
)

// Roadmap J4.5: every way of starting a torrent download follows the
// debrid path while Real-Debrid is on, and the torrent path while it is off.

// entryApp is a debrid-capable app on the results screen with one release.
func entryApp(t *testing.T) (*App, *engine.Engine, *readyRD) {
	t.Helper()
	fake := newReadyRD(t, time.Hour, 64<<10) // stays in the cloud
	a, eng, cfg := debridPreviewApp(t, fake, "linked")
	a.search = newSearchModel()
	a.results = newResultsModel(cfg.Ranking)
	a.results.curated = true
	a.results.query = "Movie"
	return a, eng, fake
}

func entryMagnet(n int) string {
	return provider.BuildMagnet(fmt.Sprintf("%040x", n), "Movie", nil)
}

func addEntryRelease(a *App, magnet string) {
	a.results.insertRow(provider.Result{Title: "Movie 2024 1080p", Provider: "test", Seeders: 50, SizeBytes: 2 << 30, Magnet: magnet})
	a.results.refreshFilter()
}

// runEntry presses the entry point and plays the messages through, the way
// Bubble Tea would, until the download is queued. It reports the hash.
func runEntry(t *testing.T, a *App, eng *engine.Engine, entry string, raw string) metainfo.Hash {
	t.Helper()
	var cmd tea.Cmd
	switch entry {
	case "D key":
		a.screen = screenResults
		addEntryRelease(a, raw)
		_, cmd = a.Update(key("D"))
	case "preview off":
		a.cfg.PreviewBeforeDownload = false
		a.screen = screenResults
		addEntryRelease(a, raw)
		_, cmd = a.Update(key("enter"))
	case "pasted":
		a.screen = screenSearch
		a.search.input.SetValue(raw)
		_, cmd = a.Update(key("enter"))
	case "command line":
		target, ok, err := intake.DetectCLI(raw)
		if err != nil || !ok {
			t.Fatalf("DetectCLI = %v, %v", ok, err)
		}
		if err := a.OpenTarget(target); err != nil {
			t.Fatal(err)
		}
		cmd, a.startup = a.startup, nil
	default:
		t.Fatalf("unknown entry %q", entry)
	}
	if cmd == nil {
		t.Fatalf("%s: no command", entry)
	}
	switch msg := cmd().(type) {
	case debridStartedMsg: // D and preview off, through Real-Debrid
		a.Update(msg)
		return msg.hash
	case torrentAddedMsg: // D and preview off, as a torrent
		a.Update(msg)
		return msg.hash
	case debridPreviewOpenMsg: // the debrid preview
		a.onDebridPreviewOpen(msg)
		addDebridPreview(t, a, msg)
		waitPreview(t, "Real-Debrid's file list", func() bool { a.preview.refresh(eng); return a.preview.ready })
		a.updatePreview(key("enter"))
		return msg.hash
	case previewReadyMsg: // the torrent preview
		a.onPreviewReady(msg)
		return msg.hash
	default:
		t.Fatalf("%s: unexpected message %T", entry, msg)
	}
	return metainfo.Hash{}
}

// entryInput is what an entry point is given: a magnet for the release
// keys, or any input a user can paste or pass on the command line.
type entryInput struct {
	name  string
	entry string // "D key", "preview off", "pasted" or "command line"
	input func(t *testing.T, n int) (raw string, h metainfo.Hash)
}

func magnetInput(t *testing.T, n int) (string, metainfo.Hash) {
	return entryMagnet(n), metainfo.NewHashFromHex(fmt.Sprintf("%040x", n))
}

func infohashInput(t *testing.T, n int) (string, metainfo.Hash) {
	hex := fmt.Sprintf("%040x", n)
	return hex, metainfo.NewHashFromHex(hex)
}

func torrentFileInput(t *testing.T, n int) (string, metainfo.Hash) {
	mi, _ := testTorrent(t, fmt.Sprintf("Movie%d", n), []metainfo.FileInfo{{Path: []string{"movie.mkv"}, Length: 64 << 10}})
	path := filepath.Join(t.TempDir(), "movie.torrent")
	writeTorrent(t, path, mi)
	return path, mi.HashInfoBytes()
}

func torrentURLInput(t *testing.T, n int) (string, metainfo.Hash) {
	mi, _ := testTorrent(t, fmt.Sprintf("Movie%d", n), []metainfo.FileInfo{{Path: []string{"movie.mkv"}, Length: 64 << 10}})
	var b strings.Builder
	if err := mi.Write(&b); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/movie.torrent", mi.HashInfoBytes()
}

func TestEntryPointsFollowTheDebridSwitch(t *testing.T) {
	cases := []entryInput{
		{"D key", "D key", magnetInput},
		{"preview off", "preview off", magnetInput},
		{"pasted magnet", "pasted", magnetInput},
		{"pasted infohash", "pasted", infohashInput},
		{"pasted torrent URL", "pasted", torrentURLInput},
		{"command-line magnet", "command line", magnetInput},
		{"command-line torrent file", "command line", torrentFileInput},
	}
	for i, tc := range cases {
		for _, on := range []bool{true, false} {
			name := tc.name + " · debrid off"
			if on {
				name = tc.name + " · debrid on"
			}
			t.Run(name, func(t *testing.T) {
				a, eng, fake := entryApp(t)
				a.cfg.Debrid.Enabled = on
				raw, want := tc.input(t, 0x100+i)
				h := runEntry(t, a, eng, tc.entry, raw)
				if h != want {
					t.Fatalf("hash %s, want %s", h.HexString(), want.HexString())
				}
				item, isDebrid := eng.DebridItem(h)
				if !on {
					if isDebrid || len(fake.added()) != 0 {
						t.Fatal("debrid is off but the download went to Real-Debrid")
					}
					if s, ok := eng.Snapshot(h); !ok || s.Via != "" {
						t.Fatalf("snapshot %+v ok=%v, want a torrent item", s, ok)
					}
					return
				}
				if !isDebrid || item.Phase != engine.DebridCloud {
					t.Fatalf("debrid item %+v ok=%v, want one in the cloud phase", item, isDebrid)
				}
				assertNoSwarm(t, eng, h)
				if len(a.st.Entries) != 1 {
					t.Fatalf("state entries %+v", a.st.Entries)
				}
				if e := a.st.Entries[0]; e.Via != "realdebrid" || e.Seed == nil || *e.Seed {
					t.Fatalf("state entry %+v, want a debrid entry with seeding off", e)
				}
				if !strings.HasPrefix(a.toast.text, "queued") {
					t.Fatalf("toast %q, want the queued toast", a.toast.text)
				}
			})
		}
	}
}

// Turning "use for downloads" off sends the next download to the swarm at
// once, while the running debrid download carries on.
func TestDebridSwitchOffAffectsOnlyNewDownloads(t *testing.T) {
	a, eng, _ := entryApp(t)
	first := runEntry(t, a, eng, "D key", entryMagnet(0x201))
	if _, ok := eng.DebridItem(first); !ok {
		t.Fatal("first download did not go through Real-Debrid")
	}
	a.screen = screenSettings
	if handled, _ := (rdUseRow{}).update(a, tea.KeyMsg{Type: tea.KeySpace}); !handled || a.cfg.Debrid.Enabled {
		t.Fatal("the settings switch did not turn Real-Debrid off")
	}
	second := runEntry(t, a, eng, "D key", entryMagnet(0x202))
	if _, ok := eng.DebridItem(second); ok {
		t.Fatal("download after switching off went to Real-Debrid")
	}
	item, ok := eng.DebridItem(first)
	if !ok || item.Phase != engine.DebridCloud || item.Paused {
		t.Fatalf("running debrid download = %+v ok=%v; it must carry on", item, ok)
	}
}

// A linked account that needs a new login stops D with a message rather
// than quietly using the swarm.
func TestDebridDirectNeedsLoginNeverUsesSwarm(t *testing.T) {
	fake := newReadyRD(t, time.Hour, 1<<10)
	a, eng, cfg := debridPreviewApp(t, fake, "needs-login")
	a.results = newResultsModel(cfg.Ranking)
	a.results.curated = true
	addEntryRelease(a, entryMagnet(0x301))
	a.screen = screenResults
	a.Update(key("D"))
	if a.errText == "" || len(eng.Snapshots()) != 0 || len(fake.added()) != 0 {
		t.Fatalf("err %q, %d rows, adds %v; want only a message", a.errText, len(eng.Snapshots()), fake.added())
	}
}

// D on a release whose metadata is cached sends the .torrent, so
// Real-Debrid knows the files at once.
func TestDebridDirectSendsCachedTorrent(t *testing.T) {
	a, eng, fake := entryApp(t)
	mi, magnet := testTorrent(t, "Movie", []metainfo.FileInfo{{Path: []string{"movie.mkv"}, Length: 64 << 10}})
	if err := metacache.New(a.cfg).Store(*mi); err != nil {
		t.Fatal(err)
	}
	h := runEntry(t, a, eng, "D key", magnet)
	if h != mi.HashInfoBytes() {
		t.Fatalf("hash %s", h.HexString())
	}
	if adds := fake.added(); len(adds) != 1 || adds[0] != "torrent" {
		t.Fatalf("adds %v, want the cached .torrent", adds)
	}
	assertNoSwarm(t, eng, h)
}
