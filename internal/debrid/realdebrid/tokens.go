package realdebrid

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// DefaultRefreshSkew is how long before expiry the token is renewed.
const DefaultRefreshSkew = 2 * time.Minute

// StoreTokenSource hands out a valid access token from debrid.json and renews
// it with the refresh token shortly before it expires, or when the API rejects
// it. Refreshes are serialised, so parallel jobs refresh only once, and every
// refresh is saved. It is safe for concurrent use.
type StoreTokenSource struct {
	store *debrid.Store
	oauth *OAuth
	// Skew is how long before expiry Token refreshes. Zero means refresh only
	// once the token has actually expired (doctor uses that).
	Skew time.Duration

	mu        sync.Mutex
	link      debrid.Link
	loaded    bool
	refreshes int
	now       func() time.Time
}

var _ Refresher = (*StoreTokenSource)(nil)

// NewStoreTokenSource returns a token source backed by store.
func NewStoreTokenSource(store *debrid.Store, oauth *OAuth) *StoreTokenSource {
	return &StoreTokenSource{store: store, oauth: oauth, Skew: DefaultRefreshSkew, now: time.Now}
}

// Token returns a valid access token, refreshing it first when it is about to
// expire. With no usable link it returns debrid.ErrNotLinked.
func (s *StoreTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", err
	}
	if s.link.AccessToken == "" || !s.now().Add(s.Skew).Before(s.link.Expiry) {
		if err := s.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return s.link.AccessToken, nil
}

// Refresh renews the token after the API rejected rejected. When another
// caller already replaced that token, the newer one is returned without a
// second refresh.
func (s *StoreTokenSource) Refresh(ctx context.Context, rejected string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", err
	}
	if s.link.AccessToken != rejected && s.link.AccessToken != "" {
		return s.link.AccessToken, nil
	}
	if err := s.refreshLocked(ctx); err != nil {
		return "", err
	}
	return s.link.AccessToken, nil
}

// Refreshes is how many refreshes this source performed (and saved).
func (s *StoreTokenSource) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Link returns the link as last loaded or refreshed.
func (s *StoreTokenSource) Link() debrid.Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.link
}

func (s *StoreTokenSource) loadLocked() error {
	if s.loaded {
		if s.link.NeedsLogin {
			return debrid.ErrNotLinked
		}
		return nil
	}
	l, err := s.store.Load()
	if err != nil {
		return err
	}
	if !l.Usable() {
		return debrid.ErrNotLinked
	}
	s.link, s.loaded = l, true
	return nil
}

func (s *StoreTokenSource) refreshLocked(ctx context.Context) error {
	// Another tork process (the CLI beside a background session) may have
	// refreshed already; adopt its token instead of refreshing again.
	if disk, err := s.store.Load(); err == nil && disk.Usable() && disk.AccessToken != s.link.AccessToken &&
		s.now().Add(s.Skew).Before(disk.Expiry) {
		s.link = disk
		return nil
	}
	creds, err := s.oauth.Refresh(ctx, s.link.Credentials())
	if err != nil {
		if errors.Is(err, debrid.ErrNotLinked) {
			// Access was revoked. Keep the file, but remember that the user
			// has to log in again.
			s.link.NeedsLogin = true
			_ = s.store.Save(s.link)
		}
		return err
	}
	s.link = s.link.WithCredentials(creds)
	s.refreshes++
	return s.store.Save(s.link)
}
