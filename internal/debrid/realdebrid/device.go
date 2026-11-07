package realdebrid

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

const (
	// slowDownStep is added to the poll interval when Real-Debrid asks tork
	// to slow down (RFC 8628 uses the same step).
	slowDownStep = 5 * time.Second
	// maxPollFailures ends the wait after this many network or 5xx failures
	// in a row; a pending answer resets the count.
	maxPollFailures = 5
)

// DeviceCode is what the user needs to approve the login.
type DeviceCode struct {
	// UserCode is the short code the user types at VerificationURL.
	UserCode        string
	VerificationURL string
	// DirectURL opens the approval page with the code already filled in.
	DirectURL string
	// Expires is when the code stops working.
	Expires time.Time
	// Interval is how often Wait polls.
	Interval time.Duration
}

// DeviceLogin runs Real-Debrid's device-code login without any UI: Start
// fetches a code to show, and Wait polls until the user approves it, then
// exchanges it for credentials. It writes nothing to disk; save the result
// with SaveLogin (or debrid.Store). A UI runs Start and then Wait in a
// background command and cancels Wait's context when the user backs out.
//
// A DeviceLogin is for one attempt; call Start again (or make a new one) to
// retry after an expiry.
type DeviceLogin struct {
	oauth *OAuth

	deviceCode string
	code       DeviceCode

	after func(time.Duration) <-chan time.Time // test hook
}

// NewDeviceLogin returns a login that uses oauth's endpoints and HTTP client.
func NewDeviceLogin(oauth *OAuth) *DeviceLogin {
	return &DeviceLogin{oauth: oauth, after: time.After}
}

// Start asks Real-Debrid for a device code.
func (d *DeviceLogin) Start(ctx context.Context) (DeviceCode, error) {
	var r struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		Interval        int64  `json:"interval"`
		ExpiresIn       int64  `json:"expires_in"`
		VerificationURL string `json:"verification_url"`
		DirectURL       string `json:"direct_verification_url"`
	}
	q := url.Values{"client_id": {OpenSourceClientID}, "new_credentials": {"yes"}}
	if _, err := d.oauth.request(ctx, http.MethodGet, "device code", "device/code", q, nil, &r); err != nil {
		return DeviceCode{}, err
	}
	if r.DeviceCode == "" || r.UserCode == "" {
		return DeviceCode{}, &debrid.Error{Provider: ProviderName, Op: "device code", Message: "incomplete device code response"}
	}
	interval := time.Duration(r.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(r.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 10 * time.Minute
	}
	verification := r.VerificationURL
	if verification == "" {
		verification = "https://real-debrid.com/device"
	}
	d.deviceCode = r.DeviceCode
	d.code = DeviceCode{
		UserCode: r.UserCode, VerificationURL: verification, DirectURL: r.DirectURL,
		Expires: d.oauth.now().Add(expiresIn), Interval: interval,
	}
	return d.code, nil
}

// Code returns the code from the last Start.
func (d *DeviceLogin) Code() DeviceCode { return d.code }

// Wait polls every interval until the user approves, then returns full
// credentials. Cancelling ctx stops it at once with ctx's error. An expired
// code returns an error matching debrid.ErrCodeExpired.
func (d *DeviceLogin) Wait(ctx context.Context) (debrid.Credentials, error) {
	if d.deviceCode == "" {
		return debrid.Credentials{}, errors.New("realdebrid: Wait called before Start")
	}
	interval := d.code.Interval
	failures := 0
	for {
		if !d.oauth.now().Before(d.code.Expires) {
			return debrid.Credentials{}, d.expired()
		}
		select {
		case <-ctx.Done():
			return debrid.Credentials{}, ctx.Err()
		case <-d.after(interval):
		}
		if ctx.Err() != nil {
			return debrid.Credentials{}, ctx.Err()
		}
		if !d.oauth.now().Before(d.code.Expires) {
			return debrid.Credentials{}, d.expired()
		}
		var r struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		q := url.Values{"client_id": {OpenSourceClientID}, "code": {d.deviceCode}}
		_, err := d.oauth.request(ctx, http.MethodGet, "device credentials", "device/credentials", q, nil, &r)
		if ctx.Err() != nil {
			return debrid.Credentials{}, ctx.Err()
		}
		var de *debrid.Error
		switch {
		case err == nil && r.ClientID != "" && r.ClientSecret != "":
			return d.oauth.exchange(ctx, "token", r.ClientID, r.ClientSecret, d.deviceCode)
		case err == nil:
			failures = 0 // approved answer without credentials yet: keep waiting
		case errors.Is(err, debrid.ErrRateLimited):
			interval += slowDownStep
		case errors.Is(err, debrid.ErrUnavailable):
			failures++
			if failures >= maxPollFailures {
				return debrid.Credentials{}, err
			}
		case errors.As(err, &de) && de.HTTPStatus >= 400 && de.HTTPStatus < 500:
			// "authorization pending": Real-Debrid answers with an error
			// until the user enters the code.
			failures = 0
		default:
			return debrid.Credentials{}, err
		}
	}
}

func (d *DeviceLogin) expired() error {
	return &debrid.Error{Provider: ProviderName, Op: "device login", Message: "code expired, start again", Kind: debrid.ErrCodeExpired}
}
