package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/kaanemec/tork-w-debrid/internal/config"
)

// directWant is the connection count a download asks for now.
func (e *Engine) directWant(it *directItem) int {
	e.mu.Lock()
	n := it.connections
	if it.debridConns {
		n = e.debridConns // read on every claim, so a settings change applies at once (J7.5)
	}
	e.mu.Unlock()
	if n <= 0 {
		n = e.directMaxConnections
	}
	return min(max(n, 1), config.MaxDirectConnections)
}

func (e *Engine) directCanRefresh(it *directItem) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return it.refreshURL != nil
}

// SetDirectConnections changes a download's connection count (0 = the
// direct.max_connections default). A running download applies it the next
// time a connection claims a segment.
func (e *Engine) SetDirectConnections(h metainfo.Hash, n int) error {
	if n < 0 || n > MaxDownloadConnections {
		return fmt.Errorf("connections must be between 0 and %d", MaxDownloadConnections)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.direct[h]
	if !ok {
		return errors.New("unknown download")
	}
	it.connections = n
	return nil
}

// Segments returns the live segment map of a running segmented download,
// for a segment bar (roadmap J7.6). For a debrid item it is the map of its
// first running file. It reports false for torrents, finished or paused
// downloads, and single-connection transfers.
func (e *Engine) Segments(h metainfo.Hash) (SegmentMap, bool) {
	e.mu.Lock()
	it, ok := e.direct[h]
	e.mu.Unlock()
	if !ok {
		return e.debridSegments(h)
	}
	return e.directSegments(it)
}

// directSegments is Segments for an item that may not be in e.direct (a
// child of a debrid item).
func (e *Engine) directSegments(it *directItem) (SegmentMap, bool) {
	e.mu.Lock()
	run := it.seg
	e.mu.Unlock()
	if run == nil {
		return SegmentMap{}, false
	}
	return run.snapshot(time.Now()), true
}
