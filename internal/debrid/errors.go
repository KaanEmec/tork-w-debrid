package debrid

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Sentinel errors callers check with errors.Is. A provider's *Error matches at
// most one of them through Kind.
var (
	// ErrUnauthorized: the access token is bad or expired (HTTP 401). The
	// token source refreshes once on it.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNotLinked: no account is linked, or the link was revoked and the
	// user has to log in again.
	ErrNotLinked = errors.New("not linked")
	// ErrNotPremium: the account is not (or no longer) premium.
	ErrNotPremium = errors.New("premium required")
	// ErrTooManyActive: the account has too many active cloud downloads.
	ErrTooManyActive = errors.New("too many active downloads")
	// ErrQuota: traffic or fair-use quota is exhausted.
	ErrQuota = errors.New("quota exhausted")
	// ErrNotFound: the remote torrent no longer exists on the service.
	ErrNotFound = errors.New("not found")
	// ErrRateLimited: the service asked tork to slow down (HTTP 429).
	ErrRateLimited = errors.New("rate limited")
	// ErrUnavailable: a 5xx answer or a network failure.
	ErrUnavailable = errors.New("service unavailable")
	// ErrCodeExpired: a device login code expired before approval.
	ErrCodeExpired = errors.New("code expired")
)

// Error is a failed provider call. It never contains a token or a full
// download URL.
type Error struct {
	// Provider is the display name, such as "Real-Debrid".
	Provider string
	// Op names the call, such as "torrents/info".
	Op string
	// HTTPStatus is the response status, 0 for a network failure.
	HTTPStatus int
	// Code is the provider's numeric error code, 0 when none was sent.
	Code int
	// Message is the provider's error word or text (e.g. "bad_token").
	Message string
	// Kind is the matching sentinel, or nil for an unclassified failure.
	Kind error
	// Err is the underlying cause (a network error), if any.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Provider != "" {
		b.WriteString(e.Provider)
		b.WriteString(": ")
	}
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	switch {
	case e.Message != "":
		b.WriteString(e.Message)
	case e.Kind != nil:
		b.WriteString(e.Kind.Error())
	default:
		b.WriteString("request failed")
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, " (code %d)", e.Code)
	}
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, " [HTTP %d]", e.HTTPStatus)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes both the sentinel and the cause to errors.Is / errors.As.
func (e *Error) Unwrap() []error {
	var out []error
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// Message is a short, human sentence for the TUI, such as
// "Real-Debrid: too many active downloads". It falls back to err's text.
func Message(err error) string {
	if err == nil {
		return ""
	}
	name := "debrid"
	var de *Error
	if errors.As(err, &de) && de.Provider != "" {
		name = de.Provider
	}
	var text string
	switch {
	case errors.Is(err, ErrCodeExpired):
		text = "code expired, start again"
	case errors.Is(err, ErrNotLinked):
		text = "not linked · log in again"
	case errors.Is(err, ErrUnauthorized):
		text = "login rejected · log in again"
	case errors.Is(err, ErrNotPremium):
		text = "premium expired"
	case errors.Is(err, ErrTooManyActive):
		text = "too many active downloads"
	case errors.Is(err, ErrQuota):
		text = "traffic quota exhausted"
	case errors.Is(err, ErrNotFound):
		text = "torrent no longer exists"
	case errors.Is(err, ErrRateLimited):
		text = "too many requests, slowing down"
	case errors.Is(err, ErrUnavailable) && de != nil && de.HTTPStatus == 0:
		text = "can't reach the server, check your network or proxy"
	case errors.Is(err, ErrUnavailable):
		text = "service unavailable, try again later"
	case de != nil && de.Message != "":
		text = strings.ReplaceAll(de.Message, "_", " ")
	default:
		return err.Error()
	}
	return name + ": " + text
}

// RedactURL keeps a URL's scheme and host and hides the rest, so logs and
// errors never carry an unlocked download link.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	return u.Scheme + "://" + u.Host + "/…"
}
