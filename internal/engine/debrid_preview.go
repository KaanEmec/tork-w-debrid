package engine

import (
	"bytes"
	"context"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// These helpers let the debrid preview (roadmap J4.1) find a torrent's file
// list without the swarm: the local metadata cache and a .torrent download
// over tork's HTTP route. Neither adds a torrent to the client.

// DebridProvider returns the provider new debrid items use, or nil when none
// is set.
func (e *Engine) DebridProvider() debrid.Provider {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.debridProvider
}

// CachedTorrent returns h's .torrent bytes from the local metadata cache
// (~/.tork/metainfo). It reads one file and never contacts the swarm.
func (e *Engine) CachedTorrent(h metainfo.Hash) ([]byte, bool) {
	mi, ok := e.metainfo.Load(h)
	if !ok {
		return nil, false
	}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// FetchTorrent downloads a .torrent file over the same proxy-aware route as
// AddTorrentURL, keeps it in the metadata cache and returns its bytes. It
// never adds the torrent to the client.
func (e *Engine) FetchTorrent(ctx context.Context, url string) ([]byte, error) {
	mi, err := e.fetchMetaInfo(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := mi.UnmarshalInfo(); err != nil {
		return nil, err
	}
	_ = e.metainfo.Store(*mi)
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
