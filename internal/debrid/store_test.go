package debrid

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kaanemec/tork-w-debrid/internal/config"
)

func testLink() Link {
	return NewLink("realdebrid", Credentials{ClientID: "C", ClientSecret: "S", AccessToken: "A", RefreshToken: "R", Expiry: time.Now().Add(time.Hour)}, time.Now())
}

func TestStoreRoundTripIsPrivate(t *testing.T) {
	cfg := config.Default(t.TempDir())
	s := NewStore(cfg)
	if _, err := s.Load(); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("missing file: %v", err)
	}
	if err := s.Save(testLink()); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(cfg.DebridPath())
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v %v", info.Mode().Perm(), err)
		}
	}
	got, err := s.Load()
	if err != nil || got.RefreshToken != "R" || got.Provider != "realdebrid" || got.LinkedAt.IsZero() {
		t.Fatalf("got = %+v %v", got, err)
	}
	data, _ := os.ReadFile(cfg.DebridPath())
	for _, key := range []string{"provider", "client_id", "client_secret", "access_token", "refresh_token", "expiry", "linked_at"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("debrid.json lacks %s", key)
		}
	}
	if err := s.Remove(); err != nil || s.Exists() {
		t.Fatalf("remove: %v", err)
	}
	if err := s.Remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestStoreRefusesReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows relies on profile ACLs")
	}
	cfg := config.Default(t.TempDir())
	s := NewStore(cfg)
	if err := s.Save(testLink()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.DebridPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrInsecureFile) {
		t.Fatalf("err = %v", err)
	}
	if Linked(cfg) {
		t.Fatal("Linked trusts a world-readable file")
	}
}

func TestStoreRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	cfg := config.Default(dir)
	target := dir + "/elsewhere.json"
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg.DebridPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(cfg).Load(); err == nil {
		t.Fatal("loaded through a symlink")
	}
	if err := NewStore(cfg).Save(testLink()); err == nil {
		t.Fatal("saved over a symlink")
	}
}

func TestLinkedNeedsFileRefreshTokenAndSwitch(t *testing.T) {
	cfg := config.Default(t.TempDir())
	if Linked(cfg) {
		t.Fatal("linked without a file")
	}
	s := NewStore(cfg)
	l := testLink()
	l.RefreshToken = ""
	_ = s.Save(l)
	if Linked(cfg) {
		t.Fatal("linked without a refresh token")
	}
	_ = s.Save(testLink())
	if !Linked(cfg) {
		t.Fatal("not linked with a good file")
	}
	cfg.Debrid.Enabled = false
	if Linked(cfg) {
		t.Fatal("linked with the switch off")
	}
}

func TestLoadDoesNotEchoSecretsOnBadJSON(t *testing.T) {
	cfg := config.Default(t.TempDir())
	if err := os.WriteFile(cfg.DebridPath(), []byte(`{"access_token":"TOPSECRET"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStore(cfg).Load()
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("err = %v", err)
	}
}

// upstreamConfig is a subset of upstream tork's Config shape: no debrid block.
type upstreamConfig struct {
	DownloadDir string `yaml:"download_dir"`
	Proxy       struct {
		SOCKS5 string `yaml:"socks5,omitempty"`
	} `yaml:"proxy"`
}

func TestUpstreamConfigIgnoresDebridBlock(t *testing.T) {
	data := []byte("download_dir: /tmp/x\ndebrid:\n  enabled: true\n  delete_after_download: false\n  provider: realdebrid\n")
	var up upstreamConfig
	if err := yaml.Unmarshal(data, &up); err != nil || up.DownloadDir != "/tmp/x" {
		t.Fatalf("upstream decode = %+v %v", up, err)
	}
}

func TestMessageAndRedact(t *testing.T) {
	if got := Message(&Error{Provider: "Real-Debrid", Kind: ErrTooManyActive}); got != "Real-Debrid: too many active downloads" {
		t.Fatalf("Message = %q", got)
	}
	if got := Message(ErrNotLinked); got != "debrid: not linked · log in again" {
		t.Fatalf("Message = %q", got)
	}
	if got := RedactURL("https://abc.download.real-debrid.com/d/SECRET/file.mkv"); got != "https://abc.download.real-debrid.com/…" {
		t.Fatalf("RedactURL = %q", got)
	}
}

func TestDaysLeft(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	a := Account{Premium: true, Expiry: now.Add(112*24*time.Hour + time.Hour)}
	if a.DaysLeft(now) != 112 {
		t.Fatalf("days = %d", a.DaysLeft(now))
	}
	if (Account{Premium: true, Expiry: now.Add(-time.Hour)}).DaysLeft(now) != 0 {
		t.Fatal("expired account has days left")
	}
}
