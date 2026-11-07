package engine

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// J6.2 hand test 10: the network drops for a while during a multi-connection
// download. Every connection fails, the item is held with a "connection lost"
// note, starts again by itself once the network is back, and does not fetch
// the finished parts again.
func TestDebridLocalRecoversByItselfAfterNetworkLoss(t *testing.T) {
	const size = 2 << 20
	files := []rdFile{{1, "big.bin", testPayload(size, 11)}}
	host := newRDHost(t, hostFiles(files))
	host.rate = 1 << 20
	var down atomic.Bool
	serve := host.Config.Handler
	host.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			panic(http.ErrAbortHandler) // the connection drops, no HTTP answer
		}
		serve.ServeHTTP(w, r)
	})
	fake := &fakeDebrid{info: readyTorrent("big.bin", files, rdLinks(1)), unlock: unlockTo(host, files)}
	eng := newLocalTestEngine(t, fake)
	debridTuning.autoRetryBase = 300 * time.Millisecond // room to see the hold
	eng.SetDebridSettings(config.DebridConfig{Connections: 4, MaxTotalConnections: 8})
	dir := t.TempDir()
	h := startLocal(t, eng, dir)

	awaitCond(t, "a third of the file", 10*time.Second, func() bool { return host.served.Load() > size/3 })
	down.Store(true)
	host.CloseClientConnections() // transfers in flight break too
	awaitCond(t, "the hold", 10*time.Second, func() bool {
		info, _ := eng.DebridItem(h)
		return info.Paused && strings.Contains(info.Note, "connection lost")
	})
	before := host.served.Load()
	down.Store(false)

	awaitHash(t, eng, h, isDone, 20*time.Second)
	checkFile(t, filepath.Join(dir, "big.bin"), files[0].data)
	// Bytes in flight at the drop may be lost; nothing near the file again.
	if served := host.served.Load(); served > size+size/4 {
		t.Fatalf("served %d bytes for a %d-byte file (%d before the network came back): finished parts were fetched again", served, size, before)
	}
}

// A hold the user resumes or pauses is not restarted by a timer that was
// set for an earlier hold, and holds that are not network trouble wait for p.
func TestDebridAutoRestartOnlyForNetworkLoss(t *testing.T) {
	fastDebrid(t, 20*time.Millisecond)
	netErr := directNetError{errors.New("every connection keeps failing - connection refused")}
	if !lostNetwork(netErr) || !lostNetwork(fmt.Errorf("file: %w", netErr)) {
		t.Fatal("a network give-up is not seen as lost network")
	}
	if !lostNetwork(&debrid.Error{Provider: "Real-Debrid", Kind: debrid.ErrUnavailable}) {
		t.Fatal("an unreachable service is not seen as lost network")
	}
	for _, err := range []error{
		errors.New("partial download is larger than expected"),
		&debrid.Error{Provider: "Real-Debrid", Kind: debrid.ErrNotFound},
		giveUpErr(directStatusError{code: http.StatusInternalServerError}, "failed 5 times"),
	} {
		if lostNetwork(err) {
			t.Fatalf("%v is seen as lost network", err)
		}
	}
	if isNetworkErr(directStatusError{code: http.StatusBadGateway}) {
		t.Fatal("an HTTP answer is not a network error")
	}
}
