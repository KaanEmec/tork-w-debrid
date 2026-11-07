package tui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
)

// fakeRD is a local Real-Debrid: device login and the user call. approve
// makes the first credentials poll succeed; otherwise it stays pending.
type fakeRD struct {
	approve   atomic.Bool
	credPolls atomic.Int32
}

func (f *fakeRD) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/v2/device/code":
			_, _ = w.Write([]byte(`{"device_code":"DEV","user_code":"ABCD1234","interval":1,"expires_in":600,"verification_url":"https://real-debrid.com/device"}`))
		case "/oauth/v2/device/credentials":
			f.credPolls.Add(1)
			if !f.approve.Load() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"authorization_pending","error_code":9}`))
				return
			}
			_, _ = w.Write([]byte(`{"client_id":"CID","client_secret":"SECRET"}`))
		case "/oauth/v2/token":
			_, _ = w.Write([]byte(`{"access_token":"ACCESS","expires_in":3600,"refresh_token":"REFRESH"}`))
		case "/rest/1.0/user":
			_, _ = w.Write([]byte(`{"username":"kaan","type":"premium","premium":9676800,"expiration":"` + time.Now().Add(112*24*time.Hour+time.Hour).UTC().Format(time.RFC3339) + `"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runCmd runs a command and feeds every message it yields back into the app
// (batches included), skipping the timers that would only wait.
func runCmd(t *testing.T, a *App, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("command did not finish")
	}
	switch m := msg.(type) {
	case tea.BatchMsg:
		for _, c := range m {
			runCmd(t, a, c)
		}
	case nil, frameMsg, rdCountdownMsg, clearToastMsg, clearErrMsg:
	default:
		_, next := a.update(m)
		runCmd(t, a, next)
	}
}

func debridTestApp(t *testing.T) (*App, *config.Config, *fakeRD) {
	t.Helper()
	a, cfg, _ := folderTestApp(t)
	f := &fakeRD{}
	srv := f.serve(t)
	a.settings.rd.endpoints = realdebrid.Endpoints{API: srv.URL + "/rest/1.0/", OAuth: srv.URL + "/oauth/v2/"}
	return a, cfg, f
}

func TestLinkRealDebridFromSettings(t *testing.T) {
	a, cfg, f := debridTestApp(t)
	f.approve.Store(true)
	a.openSettings()
	selectSettingRow(t, a, "account")
	if view := ansi.Strip(a.View()); !strings.Contains(view, "not linked") || !strings.Contains(view, "enter link account") {
		t.Fatalf("not-linked state:\n%s", view)
	}
	if strings.Contains(ansi.Strip(a.View()), "RD ") {
		t.Fatal("RD marker shown while not linked")
	}

	_, start := a.updateSettings(key("enter"))
	d, ok := a.settings.overlay.(*rdLinkDialog)
	if !ok {
		t.Fatal("enter did not open the link dialog")
	}
	// Run Start only, to see the code before approving.
	msg := start().(rdCodeMsg)
	wait := msg.applySettings(a)
	if d.phase != rdLinkWaiting {
		t.Fatalf("dialog phase %v after the code, err %q", d.phase, d.err)
	}
	if view := ansi.Strip(a.View()); !strings.Contains(view, "A B C D   1 2 3 4") || !strings.Contains(view, "real-debrid.com/device") {
		t.Fatalf("code not shown:\n%s", view)
	}
	runCmd(t, a, wait)
	if a.settings.overlay != nil {
		t.Fatal("dialog still open after approval")
	}
	if _, err := os.Stat(cfg.DebridPath()); err != nil {
		t.Fatalf("debrid.json not written: %v", err)
	}
	if !debrid.Linked(cfg) {
		t.Fatal("the saved link is not usable")
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"kaan · premium, 112 days left", "x unlink", "use for downloads", "delete cloud copy after download", "RD"} {
		if !strings.Contains(view, want) {
			t.Fatalf("linked view lacks %q:\n%s", want, view)
		}
	}
}

func TestEscStopsLinkPolling(t *testing.T) {
	a, _, f := debridTestApp(t)
	a.openSettings()
	selectSettingRow(t, a, "account")
	_, start := a.updateSettings(key("enter"))
	wait := start().(rdCodeMsg).applySettings(a)
	result := make(chan tea.Msg, 1)
	go func() {
		for _, c := range wait().(tea.BatchMsg) {
			go func(c tea.Cmd) {
				if m, ok := c().(rdLinkDoneMsg); ok {
					result <- m
				}
			}(c)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.credPolls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("login never polled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.updateSettings(key("esc"))
	if a.settings.overlay != nil {
		t.Fatal("esc did not close the dialog")
	}
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("polling did not stop after esc")
	}
	polls := f.credPolls.Load()
	time.Sleep(1500 * time.Millisecond)
	if got := f.credPolls.Load(); got != polls {
		t.Fatalf("still polling after esc: %d → %d", polls, got)
	}
}

func linkedSettingsApp(t *testing.T) (*App, *config.Config) {
	t.Helper()
	a, cfg, _ := debridTestApp(t)
	link := debrid.NewLink(config.DebridProviderRealDebrid, debrid.Credentials{ClientID: "C", ClientSecret: "S", AccessToken: "A", RefreshToken: "R", Expiry: time.Now().Add(time.Hour)}, time.Now())
	if err := debrid.NewStore(cfg).Save(link); err != nil {
		t.Fatal(err)
	}
	a.loadDebridStatus()
	return a, cfg
}

func TestUseForDownloadsSwitchAndRDMarker(t *testing.T) {
	a, cfg := linkedSettingsApp(t)
	if !a.debridRouting() {
		t.Fatal("RD marker off with a linked, enabled account")
	}
	runCmd(t, a, a.openSettings())
	selectSettingRow(t, a, "use for downloads")
	a.updateSettings(key("space"))
	if cfg.Debrid.Enabled || debrid.Linked(cfg) {
		t.Fatal("use for downloads still on in memory")
	}
	if saved, _ := config.LoadReadOnlyFrom(cfg.Dir()); saved.Debrid.Enabled {
		t.Fatal("debrid.enabled not saved")
	}
	if a.debridRouting() || strings.Contains(ansi.Strip(a.footerTail()), "RD") {
		t.Fatal("RD marker still shown with use for downloads off")
	}
	selectSettingRow(t, a, "delete cloud copy after download")
	a.updateSettings(key("space"))
	if saved, _ := config.LoadReadOnlyFrom(cfg.Dir()); !saved.Debrid.DeleteAfterDownload {
		t.Fatal("delete_after_download not saved")
	}
}

func TestUnlinkAsksFirst(t *testing.T) {
	a, cfg := linkedSettingsApp(t)
	runCmd(t, a, a.openSettings())
	selectSettingRow(t, a, "account")
	a.updateSettings(key("x"))
	a.updateSettings(key("n"))
	if !debrid.NewStore(cfg).Exists() {
		t.Fatal("n unlinked anyway")
	}
	a.updateSettings(key("x"))
	a.updateSettings(key("y"))
	if debrid.NewStore(cfg).Exists() || a.debridRouting() || a.settings.rd.phase != rdNotLinked {
		t.Fatal("y did not unlink")
	}
}

var goldenNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func settingsGoldenApp(width, height int) *App {
	a := navigationApp()
	a.width, a.height = width, height
	a.motion.noMotion = true
	a.motion.animations = true // the switch reads on; the run still has no motion
	a.cfg = &config.Config{DownloadDir: "~/Downloads/tork", Debrid: config.DefaultDebridConfig(), UI: config.DefaultUIConfig()}
	a.settings.folder = folderSettings{loaded: true, saved: "~/Downloads/tork", free: 412e9, freeOK: true}
	a.settings.rd = rdSettings{
		loaded: true, phase: rdLinked, haveAcct: true,
		account: debrid.Account{Username: "kaan", Premium: true, Expiry: goldenNow.Add(112*24*time.Hour + time.Hour)},
		now:     func() time.Time { return goldenNow },
	}
	a.SetDebridStatus(true, true)
	a.screen, a.settings.from = screenSettings, screenSearch
	return a
}

func TestGoldenSettingsLinked(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		a := settingsGoldenApp(size[0], size[1])
		view := a.View()
		assertRenderFits(t, view, size[0])
		if size[0] == 120 {
			assertGolden(t, "settings_linked_120x36", view)
		}
	}
}

func TestGoldenLinkDialog(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		a := settingsGoldenApp(size[0], size[1])
		a.settings.rd.phase, a.settings.rd.haveAcct = rdNotLinked, false
		a.SetDebridStatus(false, true)
		a.settings.overlay = &rdLinkDialog{phase: rdLinkWaiting, code: realdebrid.DeviceCode{
			UserCode: "ABCD1234", VerificationURL: "https://real-debrid.com/device",
			Expires: goldenNow.Add(9*time.Minute + 41*time.Second),
		}}
		view := a.View()
		assertRenderFits(t, view, size[0])
		plain := ansi.Strip(view)
		for _, want := range []string{"Link Real-Debrid", "open  real-debrid.com/device", "A B C D   1 2 3 4", "waiting for approval · 9:41", "c copy code", "o open browser", "esc cancel"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%dx%d dialog lacks %q:\n%s", size[0], size[1], want, plain)
			}
		}
		if strings.Contains(plain, "? keys") {
			t.Fatal("dialog footer shows more than its own keys")
		}
		if size[0] == 120 {
			assertGolden(t, "settings_link_dialog_120x36", view)
		}
		// With animations off the spinner and the pulse hold still.
		a.motion.frame = 7
		if a.View() != view {
			t.Fatal("dialog moved with motion off")
		}
	}
}
