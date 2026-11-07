package realdebrid

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// apiError is Real-Debrid's error body.
type apiError struct {
	Error   string `json:"error"`
	Code    *int   `json:"error_code"`
	Details string `json:"error_details"`
}

// codeKinds maps Real-Debrid's documented error codes onto the provider-neutral
// sentinels. Codes not listed stay unclassified and keep their own message.
var codeKinds = map[int]error{
	-1: debrid.ErrUnavailable,   // internal error
	5:  debrid.ErrRateLimited,   // slow down
	6:  debrid.ErrUnavailable,   // resource unreachable
	7:  debrid.ErrNotFound,      // resource not found
	8:  debrid.ErrUnauthorized,  // bad token
	9:  debrid.ErrNotPremium,    // permission denied (not premium, locked)
	17: debrid.ErrUnavailable,   // hoster in maintenance
	18: debrid.ErrQuota,         // hoster limit reached
	19: debrid.ErrUnavailable,   // hoster temporarily unavailable
	20: debrid.ErrNotPremium,    // hoster not available for free users
	21: debrid.ErrTooManyActive, // too many active downloads
	23: debrid.ErrQuota,         // traffic exhausted
	25: debrid.ErrUnavailable,   // service unavailable
	34: debrid.ErrRateLimited,   // too many requests
	36: debrid.ErrQuota,         // fair usage limit
}

// parseError turns a non-2xx answer into a *debrid.Error. The body is only
// read for its error word and code; nothing else from it is echoed.
func parseError(op string, status int, body []byte) *debrid.Error {
	e := &debrid.Error{Provider: ProviderName, Op: op, HTTPStatus: status}
	var a apiError
	if json.Unmarshal(body, &a) == nil {
		e.Message = sanitizeMessage(a.Error)
		if a.Code != nil {
			e.Code = *a.Code
			e.Kind = codeKinds[*a.Code]
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		e.Kind = debrid.ErrUnauthorized
	case e.Kind != nil:
	case status == http.StatusNotFound:
		e.Kind = debrid.ErrNotFound
	case status == http.StatusTooManyRequests:
		e.Kind = debrid.ErrRateLimited
	case status >= 500:
		e.Kind = debrid.ErrUnavailable
	}
	return e
}

// sanitizeMessage keeps a short, single-line error word.
func sanitizeMessage(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
