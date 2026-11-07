package engine

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/state"
)

// This file makes debrid downloads survive a restart (roadmap J3.4). The TUI
// copies Snapshot.Debrid into the item's state.json entry with
// ApplyDebridState as the item moves from the cloud to the local phase, and
// resumeAll (or the p key) hands the entry back to ResumeDebrid. Unlocked
// URLs and tokens are never saved: the next run unlocks the links again.

// Saved phases of a debrid entry (state.Entry.Phase).
const (
	DebridSavedCloud = "cloud"
	DebridSavedLocal = "local"
)

// DebridState is what state.json keeps of a debrid item. It never holds an
// unlocked URL or a token.
type DebridState struct {
	Via         string // provider ID, such as "realdebrid"
	RemoteID    string // "" when the service lost the torrent and a re-add is due
	RemoteFiles []int  // the chosen remote file IDs (nil = every file)
	Phase       string // DebridSavedCloud or DebridSavedLocal
	// TorrentData is the .torrent the item was added from, or nil for a magnet.
	TorrentData []byte
}

// savedState renders the item for state.json. Caller holds e.mu.
func (it *debridItem) savedState() *DebridState {
	phase := DebridSavedCloud
	if it.phase == DebridLocal || it.phase == DebridDone {
		phase = DebridSavedLocal
	}
	return &DebridState{
		Via: it.via, RemoteID: it.remoteID, RemoteFiles: slices.Clone(it.selected),
		Phase: phase, TorrentData: it.torrentData,
	}
}

// ApplyDebridState copies a debrid snapshot's saved fields onto its state
// entry and reports whether any changed (the caller then saves state.json).
// Snapshots of other downloads leave the entry alone.
func ApplyDebridState(e *state.Entry, s Snapshot) bool {
	d := s.Debrid
	if d == nil || e == nil {
		return false
	}
	changed := false
	set := func(dst *string, v string) {
		if *dst != v {
			*dst, changed = v, true
		}
	}
	set(&e.Via, d.Via)
	set(&e.RemoteID, d.RemoteID)
	set(&e.Phase, d.Phase)
	if !slices.Equal(e.RemoteFiles, d.RemoteFiles) {
		e.RemoteFiles, changed = slices.Clone(d.RemoteFiles), true
	}
	if !bytes.Equal(e.TorrentData, d.TorrentData) {
		e.TorrentData, changed = d.TorrentData, true
	}
	if e.Seed == nil || *e.Seed {
		e.Seed, changed = state.Bool(false), true // debrid downloads never seed (D6)
	}
	return changed
}

// DebridEntry builds the state.json entry of a started debrid item, for the
// caller to Upsert and save right after StartDebrid. Later changes (the
// local phase, a re-add) reach it through ApplyDebridState with the usual
// snapshot sync. It reports false for unknown hashes and previews.
func (e *Engine) DebridEntry(h metainfo.Hash) (state.Entry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.debrid[h]
	if !ok || it.phase == DebridAdding || it.phase == DebridPreview {
		return state.Entry{}, false
	}
	s := e.debridSnapshot(h, it, time.Now())
	entry := state.Entry{
		Magnet: it.magnet, Name: s.Name, AddedAt: time.Now().UTC(),
		DownloadDir: it.downloadDir, DataPath: it.dataPath,
		BytesCompleted: s.BytesCompleted, Length: s.Length,
	}
	ApplyDebridState(&entry, s)
	return entry, true
}

// ResumeDebrid brings back a debrid download from its state.json entry
// (entry.Via is set). The service is asked where the torrent is:
//
//   - ready: the links are unlocked again and the files resume from their
//     .part files;
//   - still in the cloud: polling goes on as before;
//   - gone (deleted on the service): the magnet (or .torrent) is added again
//     and the same files chosen by path and size;
//   - no login: the row says the account needs a login and the item waits
//     until SetDebridProvider gets an account or Resume is called.
//
// A finished entry comes back as done without any network call. An item
// already in the engine is resumed if it was paused. The caller decides
// whether a paused entry should resume at all (resumeAll skips them, as it
// does for torrents).
func (e *Engine) ResumeDebrid(entry state.Entry) (metainfo.Hash, error) {
	if entry.Via == "" {
		return metainfo.Hash{}, errors.New("not a debrid download")
	}
	spec, err := torrent.TorrentSpecFromMagnetUri(entry.Magnet)
	if err != nil {
		return metainfo.Hash{}, err
	}
	if spec.InfoHash.IsZero() {
		return metainfo.Hash{}, errors.New("magnet has no BitTorrent v1 infohash")
	}
	h := metainfo.Hash(spec.InfoHash)
	dir := e.normalizeOptions(AddOptions{DownloadDir: entry.DownloadDir}).DownloadDir

	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return h, errors.New("engine is closing")
	}
	if it, ok := e.debrid[h]; ok {
		e.mu.Unlock()
		return h, e.resumeDebrid(h, it, dir)
	}
	if _, ok := e.items[h]; ok {
		e.mu.Unlock()
		return h, ErrAlreadyTorrent
	}
	defer e.mu.Unlock()
	name := entry.Name
	if name == "?" {
		name = ""
	}
	it := &debridItem{
		hash: h, name: name, magnet: entry.Magnet, torrentData: entry.TorrentData,
		downloadDir: dir, via: entry.Via, short: viaShort(entry.Via),
		remoteID: entry.RemoteID, selected: slices.Clone(entry.RemoteFiles),
		dataPath: entry.DataPath, savedLength: entry.Length,
		wake: make(chan struct{}, 1), showRemote: true,
	}
	if it.dataPath != "" && !safePathWithin(dir, it.dataPath) {
		it.dataPath = ""
	}
	if p := e.debridProvider; p != nil && providerID(p.Name()) == entry.Via {
		it.provider, it.short = p, providerShort(p.Name())
	}
	switch {
	case entry.Done:
		it.phase, it.readyFired = DebridDone, true
		it.shownDone = entry.Length
		e.debrid[h] = it
		return h, nil
	case entry.Phase == DebridSavedLocal:
		// the files were ready before: unlock again once the service
		// confirms, without firing the ready hooks a second time
		it.phase, it.selectSent, it.readyFired = DebridLocal, true, true
	default:
		it.phase = DebridCloud
	}
	e.debrid[h] = it
	if it.provider == nil {
		it.paused, it.needsLogin, it.err = true, true, ErrNoDebrid
		it.note = it.holdNote(ErrNoDebrid, "")
		return h, nil
	}
	e.startDebridPollLocked(it)
	return h, nil
}

// resumeDebrid continues a paused or held item from its phase, unlocking
// the links again in the local phase. dir, when set, is where the files now
// live (after a move).
func (e *Engine) resumeDebrid(h metainfo.Hash, it *debridItem, dir string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.debrid[h] != it {
		return errors.New("unknown debrid download")
	}
	switch {
	case it.phase == DebridFailed:
		if it.err != nil {
			return fmt.Errorf("%s: %w", it.providerName(), it.err)
		}
		return errors.New(it.providerName() + " gave up on this torrent")
	case !it.paused:
		return nil
	}
	if it.provider == nil || it.needsLogin {
		p := e.debridProvider
		if p == nil {
			return ErrNoDebrid
		}
		it.provider, it.short = p, providerShort(p.Name())
	}
	if dir != "" && dir != it.downloadDir {
		it.downloadDir = dir
		it.dataPath = it.debridDataPath(it.info)
	}
	e.restartDebridLocked(it)
	return nil
}

// restartDebridLocked clears a pause or hold and starts the item's poller
// again, after the previous one has stopped (it may still be releasing its
// files). Caller holds e.mu.
func (e *Engine) restartDebridLocked(it *debridItem) {
	it.paused, it.needsLogin = false, false
	it.note, it.err = "", nil
	it.readds = 0
	it.showRemote = true
	if e.closing {
		return
	}
	prev := it.pollDone
	if prev != nil {
		select {
		case <-prev:
			prev = nil
		default:
		}
	}
	if prev == nil {
		if it.cancel == nil {
			e.startDebridPollLocked(it)
		}
		return
	}
	e.dwg.Add(1)
	go func() {
		defer e.dwg.Done()
		<-prev
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.debrid[it.hash] == it && !e.closing && !it.paused && it.cancel == nil {
			e.startDebridPollLocked(it)
		}
	}()
}

// viaShort is the short label for a saved provider ID ("realdebrid" → "RD").
func viaShort(via string) string {
	if via == config.DebridProviderRealDebrid {
		return "RD"
	}
	return via
}
