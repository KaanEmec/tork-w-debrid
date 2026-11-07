package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

const premiumUser = `{"id":1,"username":"kaan","type":"premium","premium":9676800,"expiration":"2027-03-01T12:00:00.000Z"}`

func testDebridCLI(t *testing.T, srvURL string) (*debridCLI, string, *bytes.Buffer) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".tork")
	out := &bytes.Buffer{}
	return &debridCLI{
		out: out, errOut: &bytes.Buffer{}, in: strings.NewReader(""),
		ep:         realdebrid.Endpoints{API: srvURL + "/rest/1.0/", OAuth: srvURL + "/oauth/v2/"},
		opts:       []realdebrid.Option{realdebrid.WithLimiter(rate.NewLimiter(rate.Inf, 0))},
		openURL:    func(string) error { return nil },
		now:        func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
		loadConfig: func() (*config.Config, error) { return config.LoadReadOnlyFrom(dir) },
	}, dir, out
}

func fakeRD(t *testing.T, user string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/rest/1.0/user" || r.Header.Get("Authorization") != "Bearer ACCESS" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(user))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func linkAccount(t *testing.T, dir string) {
	t.Helper()
	store := &debrid.Store{Path: filepath.Join(dir, "debrid.json")}
	creds := debrid.Credentials{ClientID: "C", ClientSecret: "S", AccessToken: "ACCESS", RefreshToken: "R", Expiry: time.Now().Add(time.Hour)}
	if err := store.Save(debrid.NewLink("realdebrid", creds, time.Now())); err != nil {
		t.Fatal(err)
	}
}

func TestDebridStatusNotLinked(t *testing.T) {
	srv, hits := fakeRD(t, premiumUser)
	cli, _, out := testDebridCLI(t, srv.URL)
	code, err := cli.run(context.Background(), []string{"status"})
	if err != nil || code != 1 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out.String()) != "not linked · run: torkwd debrid login" {
		t.Fatalf("output = %q", out.String())
	}
	if hits.Load() != 0 {
		t.Fatal("status contacted Real-Debrid without a link")
	}
}

func TestDebridStatusLinkedPremium(t *testing.T) {
	srv, _ := fakeRD(t, premiumUser)
	cli, dir, out := testDebridCLI(t, srv.URL)
	linkAccount(t, dir)
	code, err := cli.run(context.Background(), []string{"status"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%s", code, err, out)
	}
	for _, want := range []string{"linked as kaan", "premium until 2027-03-01 (114 days left)", "use for downloads: on", "token: refreshes automatically"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDebridStatusNotPremiumExitsOne(t *testing.T) {
	srv, _ := fakeRD(t, `{"username":"kaan","type":"free","premium":0}`)
	cli, dir, out := testDebridCLI(t, srv.URL)
	linkAccount(t, dir)
	code, err := cli.run(context.Background(), []string{"status"})
	if err != nil || code != 1 || !strings.Contains(out.String(), "not premium") {
		t.Fatalf("code=%d err=%v out=%s", code, err, out)
	}
}

func TestDebridLogout(t *testing.T) {
	srv, _ := fakeRD(t, premiumUser)
	cli, dir, out := testDebridCLI(t, srv.URL)
	linkAccount(t, dir)
	path := filepath.Join(dir, "debrid.json")

	cli.in = strings.NewReader("n\n")
	if code, _ := cli.run(context.Background(), []string{"logout"}); code != 1 {
		t.Fatalf("declined logout code = %d", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("declined logout removed the link")
	}
	cli.in = strings.NewReader("y\n")
	if code, err := cli.run(context.Background(), []string{"logout"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout kept debrid.json")
	}
	if !strings.Contains(out.String(), "revoked on real-debrid.com") {
		t.Fatalf("output = %s", out)
	}

	linkAccount(t, dir)
	cli.in = strings.NewReader("")
	if code, err := cli.run(context.Background(), []string{"logout", "-y"}); code != 0 || err != nil {
		t.Fatalf("-y: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout -y kept debrid.json")
	}
}

func TestDebridOnOffEditsConfig(t *testing.T) {
	srv, _ := fakeRD(t, premiumUser)
	cli, dir, out := testDebridCLI(t, srv.URL)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("# mine\nfuture_option: keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := cli.run(context.Background(), []string{"off"}); code != 0 || err != nil {
		t.Fatalf("off: code=%d err=%v", code, err)
	}
	cfg, err := config.LoadReadOnlyFrom(dir)
	if err != nil || cfg.Debrid.Enabled {
		t.Fatalf("after off: %+v %v", cfg.Debrid, err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "# mine") || !strings.Contains(string(data), "future_option: keep") {
		t.Fatalf("config lost content:\n%s", data)
	}
	if code, err := cli.run(context.Background(), []string{"on"}); code != 0 || err != nil {
		t.Fatalf("on: code=%d err=%v", code, err)
	}
	if cfg, _ := config.LoadReadOnlyFrom(dir); !cfg.Debrid.Enabled {
		t.Fatal("on did not enable")
	}
	if !strings.Contains(out.String(), "no account is linked yet") {
		t.Fatalf("output = %s", out)
	}
}

func TestDebridUsage(t *testing.T) {
	cli, _, _ := testDebridCLI(t, "http://127.0.0.1:1")
	for _, args := range [][]string{nil, {"nope"}, {"status", "extra"}} {
		if code, err := cli.run(context.Background(), args); code != 1 || err == nil {
			t.Errorf("%v: code=%d err=%v", args, code, err)
		}
	}
}

// J5.2: an entry pinned to the old default keeps resuming from there after
// the default folder changes.
func TestPinnedEntryResumesFromOldFolder(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	st := &state.State{Entries: []state.Entry{{Magnet: "magnet:?xt=urn:btih:abc", Name: "payload.dat", Paused: true}}}
	if err := os.WriteFile(filepath.Join(oldDir, "payload.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.PinDownloadDir(oldDir)
	e := st.Entries[0]
	normalizeEntryPaths(&e, newDir) // newDir is now the default
	if e.DownloadDir != oldDir || e.DataPath != filepath.Join(oldDir, "payload.dat") || e.NeedsRelink {
		t.Fatalf("entry = %+v", e)
	}
}
