package realdebrid

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

const testToken = "SECRET-TOKEN-123"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// recorded is one request the fake server saw.
type recorded struct {
	Method, Path, Auth, ContentType string
	Body                            string
	At                              time.Time
}

type fakeAPI struct {
	mu   sync.Mutex
	reqs []recorded
}

func (f *fakeAPI) record(r *http.Request) recorded {
	body, _ := io.ReadAll(r.Body)
	rec := recorded{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"), Body: string(body), At: time.Now()}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()
	return rec
}

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// serve starts a server whose handler sees every request after recording.
func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request, rec recorded)) (*httptest.Server, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(w, r, f.record(r))
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func testClient(srv *httptest.Server, ts TokenSource, opts ...Option) *Client {
	all := []Option{
		WithBaseURL(srv.URL + "/rest/1.0/"),
		WithLimiter(rate.NewLimiter(rate.Inf, 0)),
		WithBackoff(func(int) time.Duration { return time.Millisecond }),
	}
	return New(ts, append(all, opts...)...)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func TestAddMagnetSendsFormAndBearer(t *testing.T) {
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusCreated, fixture(t, "add_magnet.json"))
	})
	id, err := testClient(srv, StaticToken(testToken)).AddMagnet(context.Background(), "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a&dn=Big")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ABCDEFGHIJKLM" {
		t.Fatalf("id = %q", id)
	}
	req := f.requests()[0]
	if req.Method != http.MethodPost || req.Path != "/rest/1.0/torrents/addMagnet" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	if req.Auth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", req.Auth)
	}
	if req.ContentType != "application/x-www-form-urlencoded" || !strings.HasPrefix(req.Body, "magnet=magnet%3A%3Fxt%3Durn%3Abtih%3Ac12fe1") {
		t.Fatalf("body = %q (%s)", req.Body, req.ContentType)
	}
}

func TestAddTorrentFilePutsRawBytes(t *testing.T) {
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusCreated, fixture(t, "add_magnet.json"))
	})
	id, err := testClient(srv, StaticToken(testToken)).AddTorrentFile(context.Background(), []byte("d4:infod4:name3:abcee"))
	if err != nil || id != "ABCDEFGHIJKLM" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	req := f.requests()[0]
	if req.Method != http.MethodPut || req.Path != "/rest/1.0/torrents/addTorrent" || req.Body != "d4:infod4:name3:abcee" || req.ContentType != "application/x-bittorrent" {
		t.Fatalf("request = %+v", req)
	}
}

func TestInfoMultiFileReady(t *testing.T) {
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusOK, fixture(t, "info_multi.json"))
	})
	info, err := testClient(srv, StaticToken(testToken)).Info(context.Background(), "ABCDEFGHIJKLM")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.requests()[0]; got.Method != http.MethodGet || got.Path != "/rest/1.0/torrents/info/ABCDEFGHIJKLM" {
		t.Fatalf("request = %s %s", got.Method, got.Path)
	}
	if info.Status != debrid.Ready || info.RawStatus != "downloaded" || info.Progress != 100 {
		t.Fatalf("status = %+v", info)
	}
	if info.Hash != "c12fe1c06bba254a9dc9f519b335aa7c1367a88a" || info.Name != "Big.Show.S01.1080p" {
		t.Fatalf("hash/name = %q %q", info.Hash, info.Name)
	}
	if len(info.Files) != 4 || info.Files[2].Path != "Big.Show.S01.1080p/Sample/sample.mkv" || info.Files[2].Selected {
		t.Fatalf("files = %+v", info.Files)
	}
	sel := info.SelectedFiles()
	if len(sel) != 3 || sel[2].ID != 4 || len(info.Links) != 3 {
		t.Fatalf("selected = %+v links = %v", sel, info.Links)
	}
	if info.Ended.IsZero() || info.Added.IsZero() {
		t.Fatalf("times not parsed: %+v", info)
	}
}

func TestInfoStatuses(t *testing.T) {
	cases := []struct {
		fixture string
		status  debrid.RemoteStatus
		reason  string
	}{
		{"info_single.json", debrid.Downloading, ""},
		{"info_waiting.json", debrid.WaitingSelection, ""},
		{"info_dead.json", debrid.Failed, "dead torrent"},
		{"info_virus.json", debrid.Failed, "virus detected"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
				writeJSON(w, http.StatusOK, fixture(t, tc.fixture))
			})
			info, err := testClient(srv, StaticToken(testToken)).Info(context.Background(), "X1")
			if err != nil {
				t.Fatal(err)
			}
			if info.Status != tc.status || info.Reason != tc.reason {
				t.Fatalf("status = %q reason = %q", info.Status, info.Reason)
			}
		})
	}
}

func TestInfoSingleFileProgress(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusOK, fixture(t, "info_single.json"))
	})
	info, err := testClient(srv, StaticToken(testToken)).Info(context.Background(), "SINGLE1234567")
	if err != nil {
		t.Fatal(err)
	}
	if info.Progress != 43.5 || info.SpeedBps != 5242880 || info.Seeders != 37 || len(info.Files) != 1 || info.Files[0].Path != "debian-13.iso" {
		t.Fatalf("info = %+v", info)
	}
}

func TestMapStatusCoversRealDebridWords(t *testing.T) {
	want := map[string]debrid.RemoteStatus{
		"magnet_conversion": debrid.Converting, "waiting_files_selection": debrid.WaitingSelection,
		"queued": debrid.Queued, "downloading": debrid.Downloading, "compressing": debrid.Downloading,
		"uploading": debrid.Downloading, "downloaded": debrid.Ready, "error": debrid.Failed,
		"magnet_error": debrid.Failed, "virus": debrid.Failed, "dead": debrid.Failed,
	}
	for raw, status := range want {
		got, reason := MapStatus(raw)
		if got != status {
			t.Errorf("%s -> %s, want %s", raw, got, status)
		}
		if (status == debrid.Failed) != (reason != "") {
			t.Errorf("%s reason = %q", raw, reason)
		}
	}
}

func TestSelectFilesBodies(t *testing.T) {
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := testClient(srv, StaticToken(testToken))
	if err := c.SelectFiles(context.Background(), "ABC", []int{1, 4, 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.SelectFiles(context.Background(), "ABC", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if reqs[0].Path != "/rest/1.0/torrents/selectFiles/ABC" || reqs[0].Body != "files=1%2C4%2C7" {
		t.Fatalf("first = %+v", reqs[0])
	}
	if reqs[1].Body != "files=all" {
		t.Fatalf("second = %+v", reqs[1])
	}
}

func TestSelectFilesAlreadyDoneIsSuccess(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusBadRequest, []byte(`{"error":"action_already_done","error_code":31}`))
	})
	if err := testClient(srv, StaticToken(testToken)).SelectFiles(context.Background(), "ABC", nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnlock(t *testing.T) {
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusOK, fixture(t, "unrestrict.json"))
	})
	u, err := testClient(srv, StaticToken(testToken)).Unlock(context.Background(), "https://real-debrid.com/d/AAAAAAAAAAAA1")
	if err != nil {
		t.Fatal(err)
	}
	if u.URL != "https://abc12.download.real-debrid.com/d/UNRESTRICT01/E01.mkv" || u.Filename != "E01.mkv" || u.Bytes != 1073741824 || u.MimeType != "video/x-matroska" {
		t.Fatalf("unlocked = %+v", u)
	}
	req := f.requests()[0]
	if req.Method != http.MethodPost || req.Path != "/rest/1.0/unrestrict/link" || req.Body != "link=https%3A%2F%2Freal-debrid.com%2Fd%2FAAAAAAAAAAAA1" {
		t.Fatalf("request = %+v", req)
	}
}

func TestUnlockAcceptsArrayAnswer(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusOK, []byte("["+string(fixture(t, "unrestrict.json"))+"]"))
	})
	u, err := testClient(srv, StaticToken(testToken)).Unlock(context.Background(), "https://real-debrid.com/d/X")
	if err != nil || u.Filename != "E01.mkv" {
		t.Fatalf("u=%+v err=%v", u, err)
	}
}

func TestDelete(t *testing.T) {
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := testClient(srv, StaticToken(testToken)).Delete(context.Background(), "ABC"); err != nil {
		t.Fatal(err)
	}
	if req := f.requests()[0]; req.Method != http.MethodDelete || req.Path != "/rest/1.0/torrents/delete/ABC" {
		t.Fatalf("request = %+v", req)
	}
}

func TestDeleteMissingIsNotFound(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusNotFound, []byte(`{"error":"unknown_ressource","error_code":7}`))
	})
	err := testClient(srv, StaticToken(testToken)).Delete(context.Background(), "ABC")
	if !errors.Is(err, debrid.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestAccount(t *testing.T) {
	cases := []struct {
		fixture string
		premium bool
	}{{"user_premium.json", true}, {"user_expired.json", false}}
	for _, tc := range cases {
		srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
			writeJSON(w, http.StatusOK, fixture(t, tc.fixture))
		})
		a, err := testClient(srv, StaticToken(testToken)).Account(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if f.requests()[0].Path != "/rest/1.0/user" || a.Username != "kaan" || a.Premium != tc.premium {
			t.Fatalf("%s: account = %+v", tc.fixture, a)
		}
		if tc.premium && !a.Expiry.Equal(time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("expiry = %v", a.Expiry)
		}
		if !tc.premium && a.DaysLeft(time.Now()) != 0 {
			t.Fatal("expired account has days left")
		}
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	c := New(StaticToken(testToken))
	for _, id := range []string{"", "../user", "a/b", "a?b"} {
		if _, err := c.Info(context.Background(), id); err == nil {
			t.Errorf("Info(%q) accepted", id)
		}
	}
}

func TestErrorsNeverContainToken(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusForbidden, []byte(`{"error":"permission_denied","error_code":9}`))
	})
	_, err := testClient(srv, StaticToken(testToken)).Info(context.Background(), "ABC")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
	// A network failure must not leak it either.
	srv.Close()
	_, err = testClient(srv, StaticToken(testToken)).Info(context.Background(), "ABC")
	if err == nil || strings.Contains(err.Error(), testToken) || !errors.Is(err, debrid.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPClientUsesProxyTransport(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(t.TempDir(), "Downloads"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("proxy:\n  socks5: socks5://127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := HTTPClient(cfg)
	tr, ok := h.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("transport = %#v, want the proxy runtime's transport", h.Transport)
	}
	// And the route really goes through the (dead) proxy, never direct.
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusOK, fixture(t, "user_premium.json"))
	})
	_, err = testClient(srv, StaticToken(testToken), WithHTTPClient(h)).Account(context.Background())
	if err == nil || len(f.requests()) != 0 {
		t.Fatalf("err=%v requests=%d: request bypassed the proxy", err, len(f.requests()))
	}

	plain := HTTPClient(config.Default(t.TempDir()))
	if plain.Transport != nil || plain.Timeout == 0 {
		t.Fatalf("no-proxy client = %#v", plain)
	}
}
