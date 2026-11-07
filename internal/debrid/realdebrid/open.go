package realdebrid

import (
	"context"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// Endpoints overrides the API roots; empty fields keep the defaults. Tests
// point both at an httptest server.
type Endpoints struct {
	API   string
	OAuth string
}

// NewFromConfig returns a client for the account linked in cfg's
// debrid.json, with automatic token refresh and cfg's proxy route. It makes
// no request; with no usable link the first call fails with
// debrid.ErrNotLinked. Check debrid.Linked(cfg) first to pick the flow.
func NewFromConfig(cfg *config.Config, ep Endpoints, opts ...Option) (*Client, *StoreTokenSource) {
	h := HTTPClient(cfg)
	ts := NewStoreTokenSource(debrid.NewStore(cfg), NewOAuth(h, ep.OAuth))
	all := []Option{WithHTTPClient(h)}
	if ep.API != "" {
		all = append(all, WithBaseURL(ep.API))
	}
	return New(ts, append(all, opts...)...), ts
}

// NewLogin returns a device login over cfg's proxy route.
func NewLogin(cfg *config.Config, ep Endpoints) *DeviceLogin {
	return NewDeviceLogin(NewOAuth(HTTPClient(cfg), ep.OAuth))
}

// SaveLogin stores the credentials from DeviceLogin.Wait in debrid.json
// (mode 0600) and then reads the account with them. The link is saved even
// when the account lookup fails; the error is returned for display.
func SaveLogin(ctx context.Context, cfg *config.Config, ep Endpoints, creds debrid.Credentials, opts ...Option) (debrid.Account, error) {
	if err := debrid.NewStore(cfg).Save(debrid.NewLink(config.DebridProviderRealDebrid, creds, time.Now())); err != nil {
		return debrid.Account{}, err
	}
	all := []Option{WithHTTPClient(HTTPClient(cfg))}
	if ep.API != "" {
		all = append(all, WithBaseURL(ep.API))
	}
	return New(StaticToken(creds.AccessToken), append(all, opts...)...).Account(ctx)
}
