package realdebrid

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

const (
	// DefaultOAuthURL is the OAuth2 root for the device flow and refreshes.
	DefaultOAuthURL = "https://api.real-debrid.com/oauth/v2/"
	// OpenSourceClientID is Real-Debrid's public client ID for open-source
	// apps, from the "Workflow for opensource apps" section of
	// https://api.real-debrid.com/. The device flow trades it for a
	// per-user client ID and secret.
	OpenSourceClientID = "X245A4XAIBGVM"
	// deviceGrantType is the grant for both the first exchange and refreshes.
	deviceGrantType = "http://oauth.net/grant_type/device/1.0"
)

// OAuth talks to Real-Debrid's OAuth2 endpoints.
type OAuth struct {
	base *url.URL
	http *http.Client
	now  func() time.Time
}

// NewOAuth returns an OAuth client using h (use HTTPClient(cfg)). baseURL may
// be empty for DefaultOAuthURL.
func NewOAuth(h *http.Client, baseURL string) *OAuth {
	if baseURL == "" {
		baseURL = DefaultOAuthURL
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		base, _ = url.Parse(DefaultOAuthURL)
	}
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	return &OAuth{base: base, http: h, now: time.Now}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// Refresh renews the access token with the stored refresh token. A rejected
// refresh (access revoked) matches debrid.ErrNotLinked.
func (o *OAuth) Refresh(ctx context.Context, c debrid.Credentials) (debrid.Credentials, error) {
	if c.RefreshToken == "" || c.ClientID == "" || c.ClientSecret == "" {
		return debrid.Credentials{}, debrid.ErrNotLinked
	}
	out, err := o.exchange(ctx, "token refresh", c.ClientID, c.ClientSecret, c.RefreshToken)
	var de *debrid.Error
	if errors.As(err, &de) && de.HTTPStatus >= 400 && de.HTTPStatus < 500 && de.HTTPStatus != http.StatusTooManyRequests {
		de.Kind = debrid.ErrNotLinked
	}
	return out, err
}

// exchange posts to the token endpoint with code (a device code or a refresh
// token).
func (o *OAuth) exchange(ctx context.Context, op, clientID, secret, code string) (debrid.Credentials, error) {
	form := url.Values{
		"client_id": {clientID}, "client_secret": {secret},
		"code": {code}, "grant_type": {deviceGrantType},
	}
	var t tokenResponse
	status, err := o.request(ctx, http.MethodPost, op, "token", nil, form, &t)
	if err != nil {
		return debrid.Credentials{}, err
	}
	if t.AccessToken == "" {
		return debrid.Credentials{}, &debrid.Error{Provider: ProviderName, Op: op, HTTPStatus: status, Message: "no access token in response"}
	}
	refresh := t.RefreshToken
	if refresh == "" {
		refresh = code // keep the old refresh token when none is sent
	}
	return debrid.Credentials{
		ClientID: clientID, ClientSecret: secret,
		AccessToken: t.AccessToken, RefreshToken: refresh,
		Expiry: o.now().Add(time.Duration(t.ExpiresIn) * time.Second).UTC(),
	}, nil
}

// request performs one OAuth call and decodes a 2xx JSON answer into out. A
// non-2xx answer is a *debrid.Error; secrets in the query or form never
// appear in it.
func (o *OAuth) request(ctx context.Context, method, op, path string, query, form url.Values, out any) (int, error) {
	u := o.base.ResolveReference(&url.URL{Path: path})
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, &debrid.Error{Provider: ProviderName, Op: op, Message: "build request"}
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := o.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &debrid.Error{Provider: ProviderName, Op: op, Kind: debrid.ErrUnavailable, Err: stripURL(err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &debrid.Error{Provider: ProviderName, Op: op, Kind: debrid.ErrUnavailable, Err: stripURL(err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := parseError(op, resp.StatusCode, data)
		// The REST error codes mean something else here: 9 on an OAuth
		// endpoint is a refused client, not a lapsed premium.
		if errors.Is(e.Kind, debrid.ErrNotPremium) {
			e.Kind = nil
		}
		return resp.StatusCode, e
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, &debrid.Error{Provider: ProviderName, Op: op, HTTPStatus: resp.StatusCode, Message: "unexpected response"}
		}
	}
	return resp.StatusCode, nil
}
