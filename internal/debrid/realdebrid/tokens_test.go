package realdebrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// tokenServer is a fake Real-Debrid that issues ACCESS-n on the n-th refresh
// and accepts only the newest access token on /user.
type tokenServer struct {
	refreshes  atomic.Int32
	userCalls  atomic.Int32
	rejectAll  bool // /user always answers 401
	revoked    bool // refreshes are rejected
	refreshLag time.Duration
	current    atomic.Value
}

func (s *tokenServer) handler(t *testing.T) func(http.ResponseWriter, *http.Request, recorded) {
	s.current.Store("ACCESS-0")
	return func(w http.ResponseWriter, r *http.Request, rec recorded) {
		switch r.URL.Path {
		case "/oauth/v2/token":
			form, _ := url.ParseQuery(rec.Body)
			if form.Get("client_id") != "CID" || form.Get("client_secret") != "CSECRET" || form.Get("grant_type") != deviceGrantType {
				t.Errorf("refresh form = %v", form)
			}
			if s.revoked {
				writeJSON(w, http.StatusForbidden, []byte(`{"error":"invalid_grant"}`))
				return
			}
			time.Sleep(s.refreshLag)
			n := s.refreshes.Add(1)
			tok := fmt.Sprintf("ACCESS-%d", n)
			s.current.Store(tok)
			writeJSON(w, http.StatusOK, []byte(fmt.Sprintf(`{"access_token":%q,"expires_in":3600,"refresh_token":"REFRESH-%d"}`, tok, n)))
		case "/rest/1.0/user":
			s.userCalls.Add(1)
			if s.rejectAll || rec.Auth != "Bearer "+s.current.Load().(string) {
				writeJSON(w, http.StatusUnauthorized, []byte(`{"error":"bad_token","error_code":8}`))
				return
			}
			writeJSON(w, http.StatusOK, fixture(t, "user_premium.json"))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}
}

func linkedConfig(t *testing.T, expiry time.Time) (*config.Config, *debrid.Store) {
	t.Helper()
	cfg := config.Default(t.TempDir())
	store := debrid.NewStore(cfg)
	link := debrid.Link{Provider: "realdebrid", ClientID: "CID", ClientSecret: "CSECRET", AccessToken: "ACCESS-0", RefreshToken: "REFRESH-0", Expiry: expiry, LinkedAt: time.Now()}
	if err := store.Save(link); err != nil {
		t.Fatal(err)
	}
	return cfg, store
}

func TestTokenRefreshesBeforeExpiryOnceForManyCallers(t *testing.T) {
	ts := &tokenServer{refreshLag: 20 * time.Millisecond}
	srv, _ := serve(t, ts.handler(t))
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	_, store := linkedConfig(t, clock.Add(time.Hour))
	src := NewStoreTokenSource(store, NewOAuth(nil, srv.URL+"/oauth/v2/"))
	var mu sync.Mutex
	src.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }

	if tok, err := src.Token(context.Background()); err != nil || tok != "ACCESS-0" {
		t.Fatalf("fresh token = %q %v", tok, err)
	}
	mu.Lock()
	clock = clock.Add(59 * time.Minute) // inside the 2-minute skew, not yet expired
	mu.Unlock()

	var wg sync.WaitGroup
	tokens := make([]string, 10)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := src.Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			tokens[i] = tok
		}()
	}
	wg.Wait()
	if ts.refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", ts.refreshes.Load())
	}
	for _, tok := range tokens {
		if tok != "ACCESS-1" {
			t.Fatalf("tokens = %v", tokens)
		}
	}
	saved, err := store.Load()
	if err != nil || saved.AccessToken != "ACCESS-1" || saved.RefreshToken != "REFRESH-1" {
		t.Fatalf("saved = %+v %v", saved, err)
	}
}

func TestUnauthorizedRefreshesOnceAndRetriesOnce(t *testing.T) {
	ts := &tokenServer{}
	srv, _ := serve(t, ts.handler(t))
	cfg, _ := linkedConfig(t, time.Now().Add(time.Hour))
	ts.current.Store("ACCESS-SERVER-SIDE-ROTATED") // the stored token is no longer accepted
	c, src := NewFromConfig(cfg, Endpoints{API: srv.URL + "/rest/1.0/", OAuth: srv.URL + "/oauth/v2/"}, WithLimiter(nil))
	a, err := c.Account(context.Background())
	if err != nil || a.Username != "kaan" {
		t.Fatalf("account = %+v %v", a, err)
	}
	if ts.refreshes.Load() != 1 || ts.userCalls.Load() != 2 || src.Refreshes() != 1 {
		t.Fatalf("refreshes = %d user calls = %d", ts.refreshes.Load(), ts.userCalls.Load())
	}
}

func TestSecond401IsNotLinked(t *testing.T) {
	ts := &tokenServer{rejectAll: true}
	srv, _ := serve(t, ts.handler(t))
	cfg, _ := linkedConfig(t, time.Now().Add(time.Hour))
	c, _ := NewFromConfig(cfg, Endpoints{API: srv.URL + "/rest/1.0/", OAuth: srv.URL + "/oauth/v2/"}, WithLimiter(nil))
	_, err := c.Account(context.Background())
	if !errors.Is(err, debrid.ErrNotLinked) {
		t.Fatalf("err = %v", err)
	}
	if ts.refreshes.Load() != 1 || ts.userCalls.Load() != 2 {
		t.Fatalf("refreshes = %d user calls = %d", ts.refreshes.Load(), ts.userCalls.Load())
	}
}

func TestRevokedRefreshMarksNeedsLoginButKeepsFile(t *testing.T) {
	ts := &tokenServer{revoked: true}
	srv, _ := serve(t, ts.handler(t))
	cfg, store := linkedConfig(t, time.Now().Add(-time.Minute))
	c, _ := NewFromConfig(cfg, Endpoints{API: srv.URL + "/rest/1.0/", OAuth: srv.URL + "/oauth/v2/"}, WithLimiter(nil))
	_, err := c.Account(context.Background())
	if !errors.Is(err, debrid.ErrNotLinked) {
		t.Fatalf("err = %v", err)
	}
	saved, err := store.Load()
	if err != nil || !saved.NeedsLogin || saved.RefreshToken != "REFRESH-0" {
		t.Fatalf("saved = %+v %v", saved, err)
	}
	if debrid.Linked(cfg) {
		t.Fatal("Linked is true for a link that needs login")
	}
	if ts.userCalls.Load() != 0 {
		t.Fatal("called the API without a token")
	}
}

func TestNotLinkedWithoutFile(t *testing.T) {
	cfg := config.Default(t.TempDir())
	c, _ := NewFromConfig(cfg, Endpoints{API: "http://127.0.0.1:1/"})
	if _, err := c.Info(context.Background(), "X"); !errors.Is(err, debrid.ErrNotLinked) {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveLoginWritesPrivateFileNotConfig(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		if rec.Auth != "Bearer ACCESSTOKEN1" {
			t.Errorf("auth = %q", rec.Auth)
		}
		writeJSON(w, http.StatusOK, fixture(t, "user_premium.json"))
	})
	dir := t.TempDir()
	cfg := config.Default(dir)
	creds := debrid.Credentials{ClientID: "C", ClientSecret: "S", AccessToken: "ACCESSTOKEN1", RefreshToken: "R", Expiry: time.Now().Add(time.Hour)}
	a, err := SaveLogin(context.Background(), cfg, Endpoints{API: srv.URL + "/rest/1.0/"}, creds, WithLimiter(nil))
	if err != nil || a.Username != "kaan" {
		t.Fatalf("account = %+v %v", a, err)
	}
	info, err := os.Stat(cfg.DebridPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("login wrote config.yaml")
	}
	cfg.Debrid.Enabled = true
	if !debrid.Linked(cfg) {
		t.Fatal("Linked is false after login")
	}
}
