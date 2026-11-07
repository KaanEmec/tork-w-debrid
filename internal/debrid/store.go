package debrid

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/config"
)

// ErrInsecureFile: debrid.json can be read by other users, so tork refuses to
// use the credentials in it.
var ErrInsecureFile = errors.New("debrid.json must be readable by its owner only (chmod 600)")

// Credentials are what a device login produces and a refresh renews.
type Credentials struct {
	// ClientID and ClientSecret are the per-user OAuth client issued by the
	// device flow.
	ClientID     string
	ClientSecret string
	AccessToken  string
	RefreshToken string
	// Expiry is when AccessToken stops working.
	Expiry time.Time
}

// Link is the content of ~/.tork/debrid.json: the provider and its secrets.
// It never goes into config.yaml.
type Link struct {
	Provider     string    `json:"provider"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	LinkedAt     time.Time `json:"linked_at"`
	// NeedsLogin is set when the provider rejected a refresh (access
	// revoked). The file is kept until the user unlinks or logs in again.
	NeedsLogin bool `json:"needs_login,omitempty"`
}

// NewLink builds a fresh link from a completed login.
func NewLink(provider string, c Credentials, now time.Time) Link {
	return Link{
		Provider: provider, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
		AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, Expiry: c.Expiry,
		LinkedAt: now.UTC(),
	}
}

// Credentials returns the secrets part of the link.
func (l Link) Credentials() Credentials {
	return Credentials{ClientID: l.ClientID, ClientSecret: l.ClientSecret, AccessToken: l.AccessToken, RefreshToken: l.RefreshToken, Expiry: l.Expiry}
}

// WithCredentials returns l with renewed tokens; LinkedAt is kept and
// NeedsLogin cleared.
func (l Link) WithCredentials(c Credentials) Link {
	l.ClientID, l.ClientSecret = c.ClientID, c.ClientSecret
	l.AccessToken, l.RefreshToken, l.Expiry = c.AccessToken, c.RefreshToken, c.Expiry
	l.NeedsLogin = false
	return l
}

// Usable reports whether the link can produce tokens without a new login.
func (l Link) Usable() bool { return l.RefreshToken != "" && !l.NeedsLogin }

// Store reads and writes debrid.json. The zero value is unusable; use
// NewStore or set Path.
type Store struct {
	Path string
}

// NewStore returns the store for cfg's debrid.json.
func NewStore(cfg *config.Config) *Store { return &Store{Path: cfg.DebridPath()} }

// Exists reports whether debrid.json is present, without reading it.
func (s *Store) Exists() bool {
	_, err := os.Lstat(s.Path)
	return err == nil
}

// Secure reports whether the file is private to its owner. On Windows the
// file mode cannot express that, so privacy is left to %USERPROFILE% ACLs.
func (s *Store) Secure() (bool, error) {
	info, err := os.Lstat(s.Path)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s must be a regular file", s.Path)
	}
	if goruntime.GOOS == "windows" {
		return true, nil
	}
	return info.Mode().Perm()&0o077 == 0, nil
}

// Load reads the link. A missing file is ErrNotLinked; a group- or
// world-readable file is ErrInsecureFile and is not parsed.
func (s *Store) Load() (Link, error) {
	secure, err := s.Secure()
	if os.IsNotExist(err) {
		return Link{}, ErrNotLinked
	}
	if err != nil {
		return Link{}, err
	}
	if !secure {
		return Link{}, ErrInsecureFile
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return Link{}, err
	}
	var l Link
	if err := json.Unmarshal(data, &l); err != nil {
		// Never echo the content: it holds secrets.
		return Link{}, fmt.Errorf("%s is not valid JSON; run torkwd debrid logout and log in again", s.Path)
	}
	return l, nil
}

// Save writes the link atomically with mode 0600.
func (s *Store) Save(l Link) (err error) {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if info, lerr := os.Lstat(s.Path); lerr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("refusing to replace non-regular %s", s.Path)
	}
	tmp, err := os.CreateTemp(dir, ".debrid-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && goruntime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.Path)
}

// Remove deletes debrid.json. A missing file is not an error.
func (s *Store) Remove() error {
	err := os.Remove(s.Path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Linked is the cheap check the TUI uses to pick the download flow: a usable
// link exists and the "use for downloads" switch (debrid.enabled) is on. It
// reads one small file and makes no network request.
func Linked(cfg *config.Config) bool {
	if cfg == nil || !cfg.Debrid.Enabled {
		return false
	}
	l, err := NewStore(cfg).Load()
	return err == nil && l.Usable() && providerMatches(cfg, l)
}

func providerMatches(cfg *config.Config, l Link) bool {
	want := strings.TrimSpace(cfg.Debrid.Provider)
	if want == "" {
		want = config.DebridProviderRealDebrid
	}
	return l.Provider == "" || l.Provider == want
}
