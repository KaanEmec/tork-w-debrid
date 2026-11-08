package engine

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A plain HTTPS download (the Linux ISO shelf) that loses the network stops
// with a "connection lost" note, starts again by itself once the network is
// back, and does not fetch the finished parts again.
func TestDirectDownloadRecoversByItselfAfterNetworkLoss(t *testing.T) {
	scaleDirectTuning(t, 0.01)
	directTuning.autoRetryBase = 300 * time.Millisecond // room to see the stop
	const size = 4 << 20
	srv := newBenchServer(t, benchServerConfig{Size: size, ConnRate: 1 << 20, AcceptRanges: true})
	var down atomic.Bool
	serve := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			panic(http.ErrAbortHandler) // no HTTP answer, as when the network is gone
		}
		serve.ServeHTTP(w, r)
	})
	eng := newDirectTestEngine(t)
	eng.directMinChunkSize = 256 << 10
	eng.directMaxConnections = 4
	h, err := eng.AddDirectDownload(DirectDownload{URL: srv.URL + "/file.bin", Name: "file.bin", ExpectedSize: size})
	if err != nil {
		t.Fatal(err)
	}
	awaitHash(t, eng, h, func(s Snapshot) bool { return s.BytesCompleted >= size/3 }, 20*time.Second)
	down.Store(true)
	srv.CloseClientConnections()
	s := awaitHash(t, eng, h, func(s Snapshot) bool {
		return s.State == StatePaused && strings.Contains(s.Note, "connection lost")
	}, 20*time.Second)
	if !s.Retrying {
		t.Fatal("the stopped row does not say it restarts by itself")
	}
	down.Store(false)
	awaitHash(t, eng, h, isDone, 30*time.Second)
	if served := srv.served.Load(); served > size+size/4 {
		t.Fatalf("served %d bytes for a %d-byte file: finished parts were fetched again", served, size)
	}
}

// A stop that is not network trouble still waits for p.
func TestDirectDownloadServerRefusalWaitsForP(t *testing.T) {
	scaleDirectTuning(t, 0.01)
	srv := newBenchServer(t, benchServerConfig{Size: 1 << 20, AcceptRanges: true})
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	eng := newDirectTestEngine(t)
	h, err := eng.AddDirectDownload(DirectDownload{URL: srv.URL + "/file.bin", Name: "file.bin"})
	if err != nil {
		t.Fatal(err)
	}
	s := awaitHash(t, eng, h, isPaused, 20*time.Second)
	if s.Retrying || strings.Contains(s.Note, "connection lost") {
		t.Fatalf("a 500 answer restarts by itself: %+v", s)
	}
}
