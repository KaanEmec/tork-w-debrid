// Package realdebrid is the Real-Debrid implementation of debrid.Provider,
// plus its device-code login and token refresh.
package realdebrid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

const (
	// ProviderName is the display name used in errors and the UI.
	ProviderName = "Real-Debrid"
	// DefaultBaseURL is the REST API root.
	DefaultBaseURL = "https://api.real-debrid.com/rest/1.0/"

	maxTries        = 3
	maxRetryAfter   = 30 * time.Second
	maxResponseBody = 4 << 20
)

// sharedLimiter keeps every client in the process under Real-Debrid's limit of
// 250 requests per minute (about 4/s): 3/s with a small burst leaves room for
// the login flow and for a second tork process.
var sharedLimiter = rate.NewLimiter(3, 5)

// TokenSource supplies the bearer token for each request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Refresher is a TokenSource that can renew a token the API rejected. On a
// 401 the client calls Refresh once with the rejected token and retries once.
type Refresher interface {
	TokenSource
	Refresh(ctx context.Context, rejected string) (string, error)
}

// StaticToken is a fixed token, for tests and for the first call right after
// a login.
type StaticToken string

func (t StaticToken) Token(context.Context) (string, error) {
	if t == "" {
		return "", debrid.ErrNotLinked
	}
	return string(t), nil
}

// TokenOnly hides a Refresher's Refresh, so a 401 is reported instead of
// triggering a refresh. Doctor uses it to stay read-only.
func TokenOnly(ts TokenSource) TokenSource { return tokenOnly{ts} }

type tokenOnly struct{ ts TokenSource }

func (t tokenOnly) Token(ctx context.Context) (string, error) { return t.ts.Token(ctx) }

// Client is a Real-Debrid REST client. It is safe for concurrent use.
type Client struct {
	base    *url.URL
	http    *http.Client
	tokens  TokenSource
	limiter *rate.Limiter
	backoff func(try int) time.Duration
}

var _ debrid.Provider = (*Client)(nil)

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another API root (tests).
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if !strings.HasSuffix(raw, "/") {
			raw += "/"
		}
		if u, err := url.Parse(raw); err == nil {
			c.base = u
		}
	}
}

// WithHTTPClient sets the HTTP client. Use HTTPClient(cfg) in production so
// API calls and downloads leave through the same route.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithLimiter replaces the process-wide rate limiter (tests).
func WithLimiter(l *rate.Limiter) Option { return func(c *Client) { c.limiter = l } }

// WithBackoff replaces the retry delay before try n+1 (n starts at 1). The
// default is 1 s, 2 s, 4 s. A Retry-After header still wins.
func WithBackoff(f func(try int) time.Duration) Option { return func(c *Client) { c.backoff = f } }

// New returns a client that authenticates with tokens.
func New(tokens TokenSource, opts ...Option) *Client {
	base, _ := url.Parse(DefaultBaseURL)
	c := &Client{
		base:    base,
		http:    &http.Client{Timeout: 30 * time.Second},
		tokens:  tokens,
		limiter: sharedLimiter,
		backoff: func(try int) time.Duration { return time.Duration(1<<(try-1)) * time.Second },
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// HTTPClient returns the client debrid traffic must use: the strict proxy
// client when a SOCKS5 proxy is configured, else a plain client with a
// timeout. Real-Debrid ties unlocked links to the IP that unlocked them.
func HTTPClient(cfg *config.Config) *http.Client {
	if h := cfg.ProxyHTTPClient(); h != nil {
		return h
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) Name() string { return ProviderName }

// call is one API request.
type call struct {
	method string
	// op names the endpoint in errors without IDs, e.g. "torrents/info".
	op string
	// path is relative to the base URL, IDs already escaped.
	path        string
	form        url.Values
	body        []byte
	contentType string
	// retry allows retries on 429, 5xx and network errors. Only for calls
	// that cannot create anything twice.
	retry bool
}

// do runs a call with rate limiting, retries and one token refresh on 401,
// and decodes a JSON answer into out when out is non-nil.
func (c *Client) do(ctx context.Context, cl call, out any) error {
	refreshed := false
	for try := 1; ; try++ {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return err
		}
		status, data, retryAfter, err := c.send(ctx, cl, token)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e := &debrid.Error{Provider: ProviderName, Op: cl.op, Kind: debrid.ErrUnavailable, Err: err}
			if !cl.retry || try >= maxTries {
				return e
			}
			if err := c.sleep(ctx, c.backoff(try)); err != nil {
				return err
			}
			continue
		}
		if status >= 200 && status < 300 {
			if out == nil || len(bytes.TrimSpace(data)) == 0 {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return &debrid.Error{Provider: ProviderName, Op: cl.op, HTTPStatus: status, Message: "unexpected response", Err: err}
			}
			return nil
		}
		apiErr := parseError(cl.op, status, data)
		if status == http.StatusUnauthorized {
			r, ok := c.tokens.(Refresher)
			if !ok {
				return apiErr
			}
			if refreshed {
				// A freshly refreshed token was rejected too: the link is
				// no longer usable.
				apiErr.Kind = debrid.ErrNotLinked
				return apiErr
			}
			refreshed = true
			if _, err := r.Refresh(ctx, token); err != nil {
				return err
			}
			try-- // a refresh is not a retry
			continue
		}
		retryable := errors.Is(apiErr, debrid.ErrRateLimited) || errors.Is(apiErr, debrid.ErrUnavailable)
		if !cl.retry || !retryable || try >= maxTries {
			return apiErr
		}
		wait := c.backoff(try)
		if retryAfter > 0 {
			wait = retryAfter
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// send performs one HTTP exchange. Transport errors are stripped of the URL,
// which could carry an ID; the token is only ever in a header.
func (c *Client) send(ctx context.Context, cl call, token string) (status int, data []byte, retryAfter time.Duration, err error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return 0, nil, 0, err
		}
	}
	u := c.base.ResolveReference(&url.URL{Path: cl.path})
	var body io.Reader
	contentType := cl.contentType
	switch {
	case cl.form != nil:
		body = strings.NewReader(cl.form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case cl.body != nil:
		body = bytes.NewReader(cl.body)
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, u.String(), body)
	if err != nil {
		return 0, nil, 0, errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, 0, stripURL(err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return 0, nil, 0, stripURL(err)
	}
	return resp.StatusCode, data, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", strings.ToLower(ue.Op), ue.Err)
	}
	return err
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = time.Until(at)
	}
	if d < 0 {
		return 0
	}
	return min(d, maxRetryAfter)
}
