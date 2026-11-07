package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveDebridProbe measures how a real debrid download host behaves
// (roadmap J7.1). It never runs in CI. To run it, unlock one large file by
// hand on real-debrid.com (or with `torkwd debrid`) and pass the link:
//
//	TORKWD_LIVE_DEBRID=1 TORKWD_LIVE_DEBRID_URL=https://... \
//	  [TORKWD_LIVE_DEBRID_URL2=<a second unlock of the same file>] \
//	  go test ./internal/engine -run TestLiveDebridProbe -v -timeout 10m
//
// It logs the validator headers, whether If-Range is honoured, the
// connection count where speed stops rising or 429/503 starts, and whether
// the link still works after a minute and after a new unlock. Copy the log
// into library-research.md ("Still to measure").
func TestLiveDebridProbe(t *testing.T) {
	if os.Getenv("TORKWD_LIVE_DEBRID") != "1" {
		t.Skip("set TORKWD_LIVE_DEBRID=1 and TORKWD_LIVE_DEBRID_URL to probe a real debrid link")
	}
	link := os.Getenv("TORKWD_LIVE_DEBRID_URL")
	if link == "" {
		t.Fatal("TORKWD_LIVE_DEBRID_URL is empty")
	}
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}
	ctx := context.Background()
	t.Logf("probe date: %s", time.Now().UTC().Format(time.RFC3339))

	head := liveProbeRequest(t, client, ctx, http.MethodHead, link, nil)
	t.Logf("HEAD %d  ETag=%q Last-Modified=%q Accept-Ranges=%q Content-Length=%q",
		head.StatusCode, head.Header.Get("ETag"), head.Header.Get("Last-Modified"),
		head.Header.Get("Accept-Ranges"), head.Header.Get("Content-Length"))
	probe := liveProbeRequest(t, client, ctx, http.MethodGet, link, map[string]string{"Range": "bytes=0-0"})
	_, _, total, ok := parseContentRange(probe.Header.Get("Content-Range"))
	t.Logf("GET 0-0 %d  Content-Range=%q ETag=%q Last-Modified=%q",
		probe.StatusCode, probe.Header.Get("Content-Range"), probe.Header.Get("ETag"), probe.Header.Get("Last-Modified"))
	if !ok {
		t.Fatalf("no usable Content-Range; ranged downloads are impossible on this host")
	}
	validator := responseValidator(probe)
	if validator != "" {
		match := liveProbeRequest(t, client, ctx, http.MethodGet, link, map[string]string{"Range": "bytes=0-0", "If-Range": validator})
		miss := liveProbeRequest(t, client, ctx, http.MethodGet, link, map[string]string{"Range": "bytes=0-0", "If-Range": `"torkwd-mismatch"`})
		t.Logf("If-Range: matching validator -> %d, wrong validator -> %d (honoured when 206 then 200)", match.StatusCode, miss.StatusCode)
	} else {
		t.Log("no validator: If-Range cannot be used")
	}

	best, bestSpeed := 0, 0.0
	for _, conns := range []int{1, 2, 4, 8, 16} {
		speed, statuses := liveProbeSpeed(client, link, total, conns, 8*time.Second)
		t.Logf("%2d connections: %.1f MB/s  statuses=%v", conns, speed/(1<<20), statuses)
		if statuses[http.StatusTooManyRequests] > 0 || statuses[http.StatusServiceUnavailable] > 0 {
			t.Logf("host pushed back at %d connections", conns)
			break
		}
		if speed < bestSpeed*1.1 {
			t.Logf("speed stopped rising at %d connections", conns)
			break
		}
		best, bestSpeed = conns, speed
	}
	t.Logf("best connection count: %d (%.1f MB/s)", best, bestSpeed/(1<<20))

	t.Log("waiting one minute to check the link still works")
	time.Sleep(time.Minute)
	again := liveProbeRequest(t, client, ctx, http.MethodGet, link, map[string]string{"Range": "bytes=0-0"})
	t.Logf("after 1 min: %d  validator=%q", again.StatusCode, responseValidator(again))
	if second := os.Getenv("TORKWD_LIVE_DEBRID_URL2"); second != "" {
		fresh := liveProbeRequest(t, client, ctx, http.MethodGet, second, map[string]string{"Range": "bytes=0-0"})
		t.Logf("new unlock: %d  Content-Range=%q validator=%q (same validator: %v)", fresh.StatusCode,
			fresh.Header.Get("Content-Range"), responseValidator(fresh), responseValidator(fresh) == validator)
		old := liveProbeRequest(t, client, ctx, http.MethodGet, link, map[string]string{"Range": "bytes=0-0"})
		t.Logf("old link after the new unlock: %d", old.StatusCode)
	}
}

func liveProbeRequest(t *testing.T, client *http.Client, ctx context.Context, method, link string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", directUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<10)
	resp.Body.Close()
	return resp
}

// liveProbeSpeed reads from conns evenly spaced offsets for d and returns the
// combined speed and the count of each response status.
func liveProbeSpeed(client *http.Client, link string, total int64, conns int, d time.Duration) (float64, map[int]int) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var read atomic.Int64
	var mu sync.Mutex
	statuses := map[int]int{}
	var wg sync.WaitGroup
	began := time.Now()
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", directUserAgent)
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", total/int64(conns)*int64(i)))
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			statuses[resp.StatusCode]++
			mu.Unlock()
			buf := make([]byte, 128<<10)
			for {
				n, err := resp.Body.Read(buf)
				read.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	return float64(read.Load()) / time.Since(began).Seconds(), statuses
}
