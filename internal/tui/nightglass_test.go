package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/kaanemec/tork-w-debrid/internal/aggregator"
	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/health"
	"github.com/kaanemec/tork-w-debrid/internal/provider"
	"github.com/kaanemec/tork-w-debrid/internal/sources"
)

// Golden screens for the Nightglass restyle (J8.4–J8.6), rendered with motion
// off (frame 0). The goldens hold the plain text, so they pin layout and copy
// on every platform; colour is checked separately in theme_test.go.
//
// Regenerate after an intended change: go test ./internal/tui -run Golden -update
var updateGolden = flag.Bool("update", false, "rewrite the golden screens in testdata/")

func assertGolden(t *testing.T, name, view string) {
	t.Helper()
	got := ansi.Strip(view)
	lines := strings.Split(got, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	got = strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update): %v", path, err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("%s changed:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func goldenProviders(n int) []provider.Provider {
	names := []string{"yts", "knaben", "1337x", "tpb-movies", "tpb-tv", "eztv", "nyaa", "limetorrents", "torznab"}
	out := make([]provider.Provider, n)
	for i := range out {
		out[i] = resultStatusProvider(names[i])
	}
	return out
}

func homeGoldenApp(width, height int) *App {
	a := navigationApp()
	a.width, a.height = width, height
	a.cfg = &config.Config{Providers: map[string]config.ProviderConfig{}}
	for i := 0; i < 7; i++ {
		a.cfg.Providers[fmt.Sprint("p", i)] = config.ProviderConfig{Enabled: true}
	}
	a.downloads.items = []downloadItem{
		{Name: "Sintel", State: engine.StateDownloading, Length: 100, BytesCompleted: 40},
		{Name: "Elephants Dream", State: engine.StateDownloading, Length: 100, BytesCompleted: 10},
		{Name: "Tears of Steel", Route: routeCloud, CloudProgress: 0.43},
	}
	a.downloads.metrics = summarizeDownloads(a.downloads.items)
	a.downloads.itemsReady = true
	a.SetDebridStatus(true, true)
	a.focusSearch()
	return a
}

func TestGoldenHome(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}, {50, 20}} {
		a := homeGoldenApp(size[0], size[1])
		view := a.View()
		assertRenderFits(t, view, size[0])
		assertGolden(t, fmt.Sprintf("home_%dx%d", size[0], size[1]), view)
	}
}

func resultsGoldenApp(searching bool) *App {
	a := picksApp()
	a.width, a.height = 120, 36
	a.results.query = "sintel"
	a.agg = aggregator.New(goldenProviders(7), 0, 0)
	for i, title := range []string{
		"Sintel (2010) 2160p WEB-DL x265 HDR", "Sintel 2010 2160p BluRay x264", "Sintel 2010 2160p UHD REMUX HEVC",
		"Sintel (2010) 1080p BluRay x264", "Sintel 2010 1080p WEB-DL AAC", "Sintel 2010 1080p BluRay x265",
		"Sintel 2010 1080p HDTV", "Sintel 2010 1080p WEBRip",
	} {
		seeds := []int{212, 48, 3, 640, 188, 22, 0, 9}[i]
		addPick(a, i+1, title, seeds, int64(i+1)<<30)
	}
	a.results.searching = searching
	for i, p := range a.agg.Providers() {
		if i < 5 {
			a.results.status[p.Name()] = aggregator.StatusEvent{Provider: p.Name(), State: aggregator.StateDone, Count: 3}
		}
	}
	a.SetDebridStatus(true, true)
	return a
}

func TestGoldenResults(t *testing.T) {
	for _, tt := range []struct {
		name      string
		searching bool
	}{{"results_searching", true}, {"results_done", false}} {
		a := resultsGoldenApp(tt.searching)
		view := a.View()
		assertRenderFits(t, view, a.width)
		assertGolden(t, tt.name, view)
	}
	a := resultsGoldenApp(false)
	a.width, a.height = 80, 24
	assertGolden(t, "results_80x24", a.View())
}

func TestResultsHeaderStatesAndFooter(t *testing.T) {
	a := resultsGoldenApp(true)
	view := ansi.Strip(a.View())
	for _, want := range []string{"tork-w-debrid  ›  sintel", "searching · 5 of 7 sources", "esc stop", "v all results", "☁\uFE0E via Real-Debrid", "★"} {
		if !strings.Contains(view, want) {
			t.Errorf("searching results missing %q", want)
		}
	}
	a.results.searching = false
	view = ansi.Strip(a.View())
	if !strings.Contains(view, "8 results · 7 sources") || strings.Contains(view, "esc stop") {
		t.Fatalf("finished search header/footer wrong:\n%s", view)
	}
}

func previewGoldenApp() *App {
	files := []engine.FileInfo{
		{Index: 0, Path: "Sintel (2010) 1080p/Sintel.2010.1080p.BluRay.x264.mkv", Length: 1800 << 20},
		{Index: 1, Path: "Sintel (2010) 1080p/Subs/English.srt", Length: 48 << 10},
		{Index: 2, Path: "Sintel (2010) 1080p/Subs/Deutsch.srt", Length: 47 << 10},
		{Index: 3, Path: "Sintel (2010) 1080p/Subs/Español.srt", Length: 49 << 10},
	}
	excluded := map[int]bool{2: true, 3: true}
	p := previewModel{name: "Sintel (2010) 1080p BluRay x264", ready: true, files: files, excluded: excluded, tree: buildPreviewTree(files, excluded), from: screenResults, debrid: true, list: listFromRemote}
	p.rebuildRows()
	p.rebuildStats()
	a := &App{screen: screenPreview, width: 120, height: 36, cfg: &config.Config{DownloadDir: "~/Downloads/tork"}, preview: p, results: newTestResults()}
	a.results.query = "sintel"
	a.SetDebridStatus(true, true)
	return a
}

func TestGoldenPreview(t *testing.T) {
	a := previewGoldenApp()
	view := a.View()
	assertRenderFits(t, view, a.width)
	assertGolden(t, "preview_mixed", view)
	plain := ansi.Strip(view)
	for _, want := range []string{"sintel  ›  preview", "2 of 4 files", "via Real-Debrid", "file list from Real-Debrid · no swarm contacted", "[✓]", "[ ]", "enter download", "space choose", "a all", "esc back"} {
		if !strings.Contains(plain, want) {
			t.Errorf("preview missing %q", want)
		}
	}
}

func downloadsGoldenApp(width, height int) *App {
	a := &App{width: width, height: height, screen: screenDownloads, downloads: newDownloadsModel()}
	a.downloads.items = []downloadItem{
		{Name: "Sintel (2010) 1080p BluRay x264", State: engine.StateDownloading, Length: 100 << 20, BytesCompleted: 64 << 20, SpeedBps: 24e6, ETA: 62 * time.Second,
			Segments: []segmentCell{2, 2, 1, 1, 0, 0, 2, 2, 1, 0, 0, 0}, ConnSpeeds: []float64{6.1e6, 5.9e6, 6.0e6, 0.4e6}},
		{Name: "Tears of Steel (2012) 2160p", Route: routeCloud, CloudProgress: 0.43, CloudNote: "Real-Debrid 43 % · 12 MB/s · 8 seeders"},
		{Name: "Elephants Dream (2006) 1080p", Route: routeStarting},
		{Name: "debian-13.1.0-amd64-netinst.iso", State: engine.StateDownloading, Length: 100, BytesCompleted: 88, SpeedBps: 4.4 * (1 << 20), ETA: 12 * time.Second},
		{Name: "Big Buck Bunny (2008) 1080p", State: engine.StatePaused, Length: 100, BytesCompleted: 31, Note: "paused"},
		{Name: "ubuntu-24.04.3-desktop-amd64.iso", State: engine.StateSeeding, Length: 6 << 30},
		{Name: "Cosmos Laundromat (2015) 1080p", State: engine.StateDone, Length: 2400 << 20, ViaDebrid: true},
	}
	a.downloads.metrics = summarizeDownloads(a.downloads.items)
	a.downloads.itemsReady = true
	a.SetDebridStatus(true, true)
	return a
}

func TestGoldenDownloads(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		a := downloadsGoldenApp(size[0], size[1])
		view := a.View()
		assertRenderFits(t, view, size[0])
		assertGolden(t, fmt.Sprintf("downloads_%dx%d", size[0], size[1]), view)
	}
	plain := ansi.Strip(downloadsGoldenApp(120, 36).View())
	for _, want := range []string{"↓ 27.3 MiB/s · ☁\uFE0E 1", "Real-Debrid 43 % · 12 MB/s · 8 seeders", "starting", "Ⅱ", "done", "4 connections · 6.1 · 5.9 · 6.0 · 0.4 MB/s", "via Real-Debrid", "64%"} {
		if !strings.Contains(plain, want) {
			t.Errorf("downloads missing %q", want)
		}
	}
}

func TestDownloadsRailShrinksOnNarrowTerminals(t *testing.T) {
	if downloadRailWidth(112) != 48 || downloadRailWidth(54) != 24 {
		t.Fatalf("rail widths = %d / %d, want 48 / 24", downloadRailWidth(112), downloadRailWidth(54))
	}
	for _, w := range []int{40, 61, 62, 80} {
		a := downloadsGoldenApp(w, 24)
		assertRenderFits(t, a.View(), w)
	}
}

func TestSourcesFitNineAt80x24(t *testing.T) {
	a := sourcesTestApp(t)
	a.width, a.height = 80, 24
	a.screen = screenSources
	a.ensureSources()
	list := 0
	for id := range a.cfg.Providers {
		a.sources.readings[id] = sourceReading{probe: health.ProviderProbe{OK: true, Results: 12, LatencyMS: 340}, at: time.Now().Add(-5 * time.Minute)}
		list++
	}
	view := a.View()
	assertRenderFits(t, view, 80)
	if got := len(strings.Split(view, "\n")); got != 24 {
		t.Fatalf("sources rendered %d lines", got)
	}
	plain := ansi.Strip(view)
	for _, want := range []string{"tork-w-debrid  ›  sources", "12 results · 340ms", "+ add a source", "checked 5m ago", "enter on/off"} {
		if !strings.Contains(plain, want) {
			t.Errorf("sources missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "[on ]") || strings.Contains(plain, "[off]") {
		t.Fatal("sources still draw [on ] toggles")
	}
	if list < 9 {
		t.Logf("config ships %d sources", list)
	}
}

func TestSlowSourceIsAmberText(t *testing.T) {
	a := sourcesTestApp(t)
	a.ensureSources()
	for _, s := range sources.List(a.cfg) {
		if s.Config.Enabled {
			a.sources.readings[s.ID] = sourceReading{probe: health.ProviderProbe{OK: true, Results: 3, LatencyMS: 1900}, at: time.Now()}
			if got := ansi.Strip(a.sourceRow(s, false, 100)); !strings.Contains(got, "slow · 1.9s") {
				t.Fatalf("slow source row = %q", got)
			}
			return
		}
	}
	t.Skip("no enabled source in the default config")
}
