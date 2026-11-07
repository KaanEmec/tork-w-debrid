package realdebrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

func TestParseErrorMapsCodesAndStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"error":"bad_token","error_code":8}`, debrid.ErrUnauthorized},
		{401, ``, debrid.ErrUnauthorized},
		{403, `{"error":"permission_denied","error_code":9}`, debrid.ErrNotPremium},
		{403, `{"error":"hoster_not_free","error_code":20}`, debrid.ErrNotPremium},
		{509, `{"error":"too_many_active_downloads","error_code":21}`, debrid.ErrTooManyActive},
		{503, `{"error":"traffic_exhausted","error_code":23}`, debrid.ErrQuota},
		{403, `{"error":"fair_usage_limit","error_code":36}`, debrid.ErrQuota},
		{503, `{"error":"hoster_limit_reached","error_code":18}`, debrid.ErrQuota},
		{404, `{"error":"unknown_ressource","error_code":7}`, debrid.ErrNotFound},
		{404, ``, debrid.ErrNotFound},
		{429, `{"error":"too_many_requests","error_code":34}`, debrid.ErrRateLimited},
		{429, ``, debrid.ErrRateLimited},
		{400, `{"error":"slow_down","error_code":5}`, debrid.ErrRateLimited},
		{503, `{"error":"service_unavailable","error_code":25}`, debrid.ErrUnavailable},
		{500, `{"error":"internal_error","error_code":-1}`, debrid.ErrUnavailable},
		{502, `<html>bad gateway</html>`, debrid.ErrUnavailable},
	}
	sentinels := []error{debrid.ErrUnauthorized, debrid.ErrNotPremium, debrid.ErrTooManyActive, debrid.ErrQuota, debrid.ErrNotFound, debrid.ErrRateLimited, debrid.ErrUnavailable}
	for _, tc := range cases {
		err := parseError("op", tc.status, []byte(tc.body))
		for _, s := range sentinels {
			if got := errors.Is(err, s); got != (s == tc.want) {
				t.Errorf("%d %s: errors.Is(%v) = %v", tc.status, tc.body, s, got)
			}
		}
		if err.HTTPStatus != tc.status {
			t.Errorf("HTTPStatus = %d", err.HTTPStatus)
		}
	}
	// Unclassified codes keep their own message.
	err := parseError("op", 403, []byte(`{"error":"account_locked","error_code":14}`))
	if err.Kind != nil || err.Code != 14 || debrid.Message(err) != "Real-Debrid: account locked" {
		t.Fatalf("err = %#v msg = %q", err, debrid.Message(err))
	}
}

func TestHumanMessages(t *testing.T) {
	cases := map[string]error{
		"Real-Debrid: too many active downloads":                           parseError("op", 509, []byte(`{"error":"too_many_active_downloads","error_code":21}`)),
		"Real-Debrid: premium expired":                                     parseError("op", 403, []byte(`{"error":"permission_denied","error_code":9}`)),
		"Real-Debrid: torrent no longer exists":                            parseError("op", 404, nil),
		"Real-Debrid: service unavailable, try again later":                parseError("op", 503, nil),
		"Real-Debrid: can't reach the server, check your network or proxy": &debrid.Error{Provider: ProviderName, Op: "op", Kind: debrid.ErrUnavailable, Err: errors.New("i/o timeout")},
	}
	for want, err := range cases {
		if got := debrid.Message(fmt.Errorf("wrapped: %w", err)); got != want {
			t.Errorf("Message = %q, want %q", got, want)
		}
	}
}

func TestRetryAfter429ThenSuccess(t *testing.T) {
	var n atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(w, http.StatusTooManyRequests, []byte(`{"error":"too_many_requests","error_code":34}`))
			return
		}
		writeJSON(w, http.StatusOK, fixture(t, "info_single.json"))
	})
	if _, err := testClient(srv, StaticToken(testToken)).Info(context.Background(), "X"); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("requests = %d, want 2", n.Load())
	}
}

func TestRetryHonoursRetryAfter(t *testing.T) {
	var n atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, nil)
			return
		}
		writeJSON(w, http.StatusOK, fixture(t, "user_premium.json"))
	})
	start := time.Now()
	if _, err := testClient(srv, StaticToken(testToken)).Account(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("retried after %v, want Retry-After's 1s", d)
	}
}

func TestRepeated500GivesUpAfterThreeTries(t *testing.T) {
	var n atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		n.Add(1)
		writeJSON(w, http.StatusInternalServerError, nil)
	})
	var waits []time.Duration
	c := testClient(srv, StaticToken(testToken), WithBackoff(func(try int) time.Duration {
		waits = append(waits, time.Duration(1<<(try-1))*time.Second)
		return time.Millisecond
	}))
	_, err := c.Info(context.Background(), "X")
	if !errors.Is(err, debrid.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("requests = %d, want 3", n.Load())
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

func TestDefaultBackoffIsExponential(t *testing.T) {
	c := New(StaticToken("x"))
	for try, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second} {
		if got := c.backoff(try); got != want {
			t.Errorf("backoff(%d) = %v, want %v", try, got, want)
		}
	}
}

func TestAddMagnetIsNeverRetried(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		var n atomic.Int32
		srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
			n.Add(1)
			writeJSON(w, status, nil)
		})
		if _, err := testClient(srv, StaticToken(testToken)).AddMagnet(context.Background(), "magnet:?xt=urn:btih:abc"); err == nil {
			t.Fatal("want an error")
		}
		if n.Load() != 1 {
			t.Fatalf("status %d: requests = %d, want 1", status, n.Load())
		}
	}
}

func TestLimiterBoundsConcurrentInfoCalls(t *testing.T) {
	const (
		calls = 20
		perS  = 40
		burst = 2
	)
	srv, f := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		writeJSON(w, http.StatusOK, fixture(t, "info_single.json"))
	})
	c := testClient(srv, StaticToken(testToken), WithLimiter(rate.NewLimiter(perS, burst)))
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Info(context.Background(), "X"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	reqs := f.requests()
	if len(reqs) != calls {
		t.Fatalf("requests = %d", len(reqs))
	}
	at := make([]time.Time, len(reqs))
	for i, r := range reqs {
		at[i] = r.At
	}
	sort.Slice(at, func(i, j int) bool { return at[i].Before(at[j]) })
	// A token bucket allows at most burst + rate*t requests in any span t.
	slack := 15 * time.Millisecond
	for i := burst; i < len(at); i++ {
		minSpan := time.Duration(float64(i-burst+1) / perS * float64(time.Second))
		if span := at[i].Sub(at[0]); span+slack < minSpan {
			t.Fatalf("request %d came %v after the first, limiter allows no earlier than %v", i, span, minSpan)
		}
	}
}

func TestSharedLimiterStaysUnderRealDebridLimit(t *testing.T) {
	if float64(sharedLimiter.Limit())*60 >= 250 {
		t.Fatalf("shared limiter allows %.0f requests/min, Real-Debrid allows 250", float64(sharedLimiter.Limit())*60)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	var n atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		n.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, nil)
	})
	ctx, cancel := context.WithCancel(context.Background())
	c := testClient(srv, StaticToken(testToken), WithBackoff(func(int) time.Duration { cancel(); return time.Hour }))
	_, err := c.Info(ctx, "X")
	if !errors.Is(err, context.Canceled) || n.Load() != 1 {
		t.Fatalf("err=%v requests=%d", err, n.Load())
	}
}
