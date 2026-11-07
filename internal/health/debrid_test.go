package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
	"github.com/kaanemec/tork-w-debrid/internal/provider"
)

func fakeUserServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func saveTestLink(t *testing.T, path string) {
	t.Helper()
	creds := debrid.Credentials{ClientID: "C", ClientSecret: "S", AccessToken: "A", RefreshToken: "R", Expiry: time.Now().Add(time.Hour)}
	if err := (&debrid.Store{Path: path}).Save(debrid.NewLink("realdebrid", creds, time.Now())); err != nil {
		t.Fatal(err)
	}
}

func runDebridDoctor(t *testing.T, srv *httptest.Server, live bool) Report {
	t.Helper()
	cfg := testConfig(t)
	saveTestLink(t, cfg.DebridPath())
	opts := DoctorOptions{Engine: okEngine}
	if live {
		opts.Debrid = NewDebridProbe(realdebrid.Endpoints{API: srv.URL + "/rest/1.0/", OAuth: srv.URL + "/oauth/v2/"},
			realdebrid.WithLimiter(rate.NewLimiter(rate.Inf, 0)))
	}
	return RunDoctorWith(context.Background(), cfg, []provider.Provider{fakeProvider{name: "good", results: 5}}, Open(cfg.HealthPath()), opts)
}

func TestDoctorDebridNoNetworkWithoutFlag(t *testing.T) {
	srv, hits := fakeUserServer(t, `{"username":"kaan","type":"premium","premium":100,"expiration":"2099-01-01T00:00:00.000Z"}`)
	rep := runDebridDoctor(t, srv, false)
	if c := findCheck(t, rep, "debrid"); c.Status != StatusOK {
		t.Fatalf("debrid check = %+v", c)
	}
	if hits.Load() != 0 {
		t.Fatal("doctor contacted Real-Debrid without --debrid-check")
	}
	for _, c := range rep.Checks {
		if c.Name == "debrid account" {
			t.Fatal("account checked without --debrid-check")
		}
	}
}

func TestDoctorDebridExpiredPremiumFails(t *testing.T) {
	srv, hits := fakeUserServer(t, `{"username":"kaan","type":"free","premium":0,"expiration":"2026-01-01T00:00:00.000Z"}`)
	rep := runDebridDoctor(t, srv, true)
	c := findCheck(t, rep, "debrid account")
	if c.Status != StatusFail || !strings.Contains(c.Detail, "premium expired") || !rep.Failed() {
		t.Fatalf("account check = %+v failed=%v", c, rep.Failed())
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestDoctorDebridPremiumEndingSoonWarns(t *testing.T) {
	exp := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	srv, _ := fakeUserServer(t, `{"username":"kaan","type":"premium","premium":259200,"expiration":"`+exp+`"}`)
	c := findCheck(t, runDebridDoctor(t, srv, true), "debrid account")
	if c.Status != StatusWarn {
		t.Fatalf("account check = %+v", c)
	}
}

func TestDoctorDebridNotLinkedIsInformation(t *testing.T) {
	cfg := testConfig(t)
	c := findCheck(t, RunDoctor(context.Background(), cfg, []provider.Provider{fakeProvider{name: "good", results: 5}}, Open(cfg.HealthPath()), okEngine), "debrid")
	if c.Status != StatusOK || !strings.HasPrefix(c.Detail, "not linked") {
		t.Fatalf("debrid check = %+v", c)
	}
}

func TestDoctorDebridInsecureFileFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows relies on profile ACLs")
	}
	cfg := testConfig(t)
	saveTestLink(t, cfg.DebridPath())
	if err := os.Chmod(cfg.DebridPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	c := findCheck(t, RunDoctor(context.Background(), cfg, []provider.Provider{fakeProvider{name: "good", results: 5}}, Open(cfg.HealthPath()), okEngine), "debrid")
	if c.Status != StatusFail {
		t.Fatalf("debrid check = %+v", c)
	}
}
