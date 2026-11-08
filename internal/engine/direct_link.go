package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// This file holds what both HTTPS download paths share for recovering from
// errors (roadmap J7.2 and J7.3): the current URL and its refresh hook, the
// classification of transfer errors, and the retry timings.

// URLRefresher returns a fresh download URL for the same remote file, for
// example by unlocking a Real-Debrid link again. The engine calls it when the
// current URL answers 403, 404 or 410, at most directTuning.maxRefreshes times
// per run, and never from two workers at once.
type URLRefresher func(ctx context.Context) (string, error)

// directTuning holds the timings of retries, link refreshes, adaptive
// connection lowering and the slow-connection check. Tests shrink them.
var directTuning = struct {
	retryBase       time.Duration // first back-off; doubles per try up to 8x
	maxTries        int           // failed attempts allowed per segment
	progressReset   int64         // an attempt that wrote this much resets its segment's tries
	maxRefreshes    int           // link refreshes per run
	checkpointBytes int64         // save the manifest after this much new data
	throttleRecover time.Duration // quiet time before a lowered connection count goes back up
	maxRetryAfter   time.Duration // longest Retry-After honoured
	tick            time.Duration // supervisor tick (speed samples, limits)
	healthInterval  time.Duration // slow-connection check interval
	healthWindow    time.Duration // speed window the check compares
	restartCooldown time.Duration // minimum gap between restarts of one connection
	slowRatio       float64       // slower than this share of the average is slow
	slowChecks      int           // checks in a row before a restart
}{
	retryBase:       time.Second,
	maxTries:        5,
	progressReset:   1 << 20,
	maxRefreshes:    3,
	checkpointBytes: 8 << 20,
	throttleRecover: 60 * time.Second,
	maxRetryAfter:   5 * time.Minute,
	tick:            250 * time.Millisecond,
	healthInterval:  5 * time.Second,
	healthWindow:    10 * time.Second,
	restartCooldown: time.Minute,
	slowRatio:       0.3,
	slowChecks:      2,
}

// directBackoff is the wait before retry number try (1-based): 1 s, 2 s, 4 s,
// then 8 s, each with up to 20 % jitter either way.
func directBackoff(try int) time.Duration {
	d := directTuning.retryBase << min(max(try-1, 0), 3)
	jitter := (rand.Float64()*0.4 - 0.2) * float64(d)
	return d + time.Duration(jitter)
}

// directStatusError is an HTTP answer the transfer could not use.
type directStatusError struct {
	code       int
	retryAfter time.Duration
}

func (e directStatusError) Error() string {
	return fmt.Sprintf("server answered %d %s", e.code, http.StatusText(e.code))
}

func newDirectStatusError(resp *http.Response) directStatusError {
	return directStatusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
}

// directFatalError stops the transfer at once (disk errors, refused redirects).
type directFatalError struct{ error }

func (e directFatalError) Unwrap() error { return e.error }

var (
	errUntrustedRedirect = errors.New("refusing a download redirect outside the trusted origin")
	errTooManyRedirects  = errors.New("too many redirects")
	errRestartSegment    = errors.New("restarting a slow connection")
)

type directErrKind int

const (
	errKindRetry    directErrKind = iota // network reset, timeout, 5xx: retry the segment
	errKindExpired                       // 403, 404, 410: refresh the link
	errKindBusy                          // 429, 503: honour Retry-After, use fewer connections
	errKindProtocol                      // ranges are broken: fall back to one connection
	errKindFatal                         // give up now
	errKindRestart                       // slow-connection restart: no try counted
)

// directNetError marks a transfer that gave up because the network kept
// failing (refused, reset or timed-out connections, lookups that fail), not
// because the server answered no. Debrid items retry those by themselves
// (holdDebridLocked), so a short outage needs no key press.
type directNetError struct{ err error }

func (e directNetError) Error() string { return e.err.Error() }
func (e directNetError) Unwrap() error { return e.err }

// isNetworkErr reports a transport failure that got no HTTP answer.
func isNetworkErr(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	var status directStatusError
	if errors.As(err, &status) {
		return false
	}
	return errors.As(err, &op) || errors.As(err, &dns) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded)
}

// giveUpErr wraps the error a transfer stops with, marking a network outage.
func giveUpErr(last error, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	if isNetworkErr(last) {
		return directNetError{err}
	}
	return err
}

func classifyDirectErr(err error) directErrKind {
	var status directStatusError
	var protocol directProtocolError
	var fatal directFatalError
	switch {
	case errors.Is(err, errRestartSegment):
		return errKindRestart
	case errors.As(err, &protocol):
		return errKindProtocol
	case errors.As(err, &fatal), errors.Is(err, errUntrustedRedirect), errors.Is(err, errTooManyRedirects):
		return errKindFatal
	case errors.As(err, &status):
		switch status.code {
		case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return errKindExpired
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return errKindBusy
		case http.StatusRequestedRangeNotSatisfiable:
			return errKindProtocol
		}
	}
	return errKindRetry
}

// parseRetryAfter reads delta-seconds or an HTTP date, capped at
// directTuning.maxRetryAfter. Missing or invalid values give 0.
func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(raw); err == nil {
		d = at.Sub(now)
	}
	return min(max(d, 0), directTuning.maxRetryAfter)
}

// directURL returns the item's current URL and its generation, waiting while
// a refresh is in flight so no request goes out with a link being replaced.
func (e *Engine) directURL(ctx context.Context, it *directItem) (string, int, error) {
	e.mu.Lock()
	for it.refreshing != nil {
		ch := it.refreshing
		e.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
		e.mu.Lock()
	}
	defer e.mu.Unlock()
	return it.url, it.urlGen, nil
}

// refreshDirectURL replaces a link that stopped working. gen is the URL
// generation the failed request used: when another worker already replaced
// that URL, it returns at once, and while a refresh is running it waits for
// it, so concurrent failures cause one call to the refresher.
func (e *Engine) refreshDirectURL(ctx context.Context, it *directItem, gen int) error {
	e.mu.Lock()
	for {
		if it.urlGen != gen {
			e.mu.Unlock()
			return nil
		}
		ch := it.refreshing
		if ch == nil {
			break
		}
		e.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		e.mu.Lock()
		if it.urlGen == gen && it.refreshing == nil && it.refreshErr != nil {
			err := it.refreshErr
			e.mu.Unlock()
			return err
		}
	}
	if it.refreshURL == nil {
		e.mu.Unlock()
		return errors.New("link expired and cannot be refreshed")
	}
	if it.refreshes >= directTuning.maxRefreshes {
		e.mu.Unlock()
		return fmt.Errorf("link expired again after %d refreshes", it.refreshes)
	}
	it.refreshes++
	ch := make(chan struct{})
	it.refreshing, it.refreshErr = ch, nil
	it.note = "refreshing link"
	refresh, lockedOrigin := it.refreshURL, it.lockedOrigin
	e.mu.Unlock()

	fresh, err := refresh(ctx)
	if err == nil {
		err = checkRefreshedURL(fresh, lockedOrigin)
	}
	if err != nil {
		err = fmt.Errorf("refresh link: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		it.url = fresh
		it.urlGen++
		if it.note == "refreshing link" {
			it.note = ""
		}
	}
	it.refreshErr = err
	it.refreshing = nil
	close(ch)
	return err
}

func checkRefreshedURL(raw, lockedOrigin string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("refreshed link is not a valid HTTP(S) URL")
	}
	if lockedOrigin != "" && directURLOrigin(parsed) != lockedOrigin {
		return errUntrustedRedirect
	}
	return nil
}

// setDirectNote shows a transient status on the row while it downloads.
func (e *Engine) setDirectNote(it *directItem, note string) {
	e.mu.Lock()
	if it.state == StateDownloading {
		it.note = note
	}
	e.mu.Unlock()
}

// sleepCtx waits for d or until ctx ends, reporting whether the wait finished.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
