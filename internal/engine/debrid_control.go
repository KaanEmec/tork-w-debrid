package engine

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// This file gives debrid items the row actions of other downloads (roadmap
// J3.5): pause, resume, remove (with or without the cloud copy) and a size
// check in place of verify. Move and relink need nothing here: the TUI
// removes the item, moves <download dir>/<torrent name>, and resumes the
// entry from its new folder with ResumeDebrid.

// RemoveOptions says what Engine.RemoveWithOptions deletes besides the row.
type RemoveOptions struct {
	// DeleteData deletes the downloaded files (and partial files).
	DeleteData bool
	// DeleteRemote deletes a debrid item's copy on the service. It is
	// ignored for torrents and plain HTTPS downloads.
	DeleteRemote bool
}

// RemoveResult reports what happened to the cloud copy. A failed remote
// delete never fails the local remove.
type RemoveResult struct {
	RemoteDeleted bool
	// RemoteErr is why the cloud copy could not be deleted, and Note a short
	// sentence for a toast ("cloud copy not deleted: Real-Debrid: ...").
	RemoteErr error
	Note      string
}

// RemoveWithOptions drops a download and forgets it. For a debrid item it
// stops the poller and every file, deletes the local data when asked, and
// deletes the cloud copy when asked: that call is bounded by a timeout, so
// run it off the UI thread. A failing remote delete still removes the row
// and the data and is reported in RemoveResult.
func (e *Engine) RemoveWithOptions(h metainfo.Hash, opts RemoveOptions) (RemoveResult, error) {
	e.mu.Lock()
	di, ok := e.debrid[h]
	e.mu.Unlock()
	if ok {
		return e.removeDebrid(h, di, opts)
	}
	return RemoveResult{}, e.Remove(h, opts.DeleteData)
}

// DeleteDebridRemote deletes a remote torrent that is not loaded in the
// engine (a paused entry of state.json), with the current provider.
func (e *Engine) DeleteDebridRemote(ctx context.Context, remoteID string) error {
	e.mu.Lock()
	p := e.debridProvider
	e.mu.Unlock()
	if p == nil {
		return ErrNoDebrid
	}
	if remoteID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, debridTuning.cleanupWait)
	defer cancel()
	if err := p.Delete(ctx, remoteID); err != nil && !errors.Is(err, debrid.ErrNotFound) {
		return err
	}
	return nil
}

// removeDebrid stops a debrid item and forgets it, then deletes its local
// data and cloud copy as asked.
func (e *Engine) removeDebrid(h metainfo.Hash, it *debridItem, opts RemoveOptions) (RemoveResult, error) {
	e.mu.Lock()
	if e.debrid[h] == it {
		delete(e.debrid, h)
	}
	e.stopDebridPollLocked(it)
	done := it.pollDone
	p, id, deleted := it.provider, it.remoteID, it.remoteDeleted
	e.mu.Unlock()
	if done != nil {
		<-done
	}
	e.stopDebridChildren(it) // a held item's files are stopped already; this is cheap

	var res RemoveResult
	var err error
	if opts.DeleteData {
		err = e.deleteDebridData(it)
	}
	if opts.DeleteRemote && id != "" && !deleted {
		if p == nil {
			res.RemoteErr = ErrNoDebrid
		} else {
			ctx, cancel := context.WithTimeout(e.debridCtx, debridTuning.cleanupWait)
			rerr := p.Delete(ctx, id)
			cancel()
			if rerr != nil && !errors.Is(rerr, debrid.ErrNotFound) {
				res.RemoteErr = rerr
			}
		}
		if res.RemoteErr != nil {
			res.Note = "cloud copy not deleted: " + debrid.Message(res.RemoteErr)
		} else {
			res.RemoteDeleted = true
		}
	}
	return res, err
}

// deleteDebridData deletes the torrent folder (or the single file) with
// its partial files, as delete-data does for a torrent. Without a known path
// it deletes the files the local phase knows of.
func (e *Engine) deleteDebridData(it *debridItem) error {
	e.mu.Lock()
	dir, dataPath := it.downloadDir, it.dataPath
	var files []string
	for _, c := range it.children {
		if c.d != nil && c.d.dataPath != "" {
			files = append(files, c.d.dataPath)
		}
	}
	e.mu.Unlock()
	if dataPath != "" {
		if !safePathWithin(dir, dataPath) {
			return errors.New("delete data refused: unknown or unsafe path")
		}
		files = []string{dataPath}
	}
	var first error
	for _, f := range files {
		if !safePathWithin(dir, f) {
			continue
		}
		for _, p := range []string{f + ".part.meta", f + ".part"} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) && first == nil {
				first = err
			}
		}
		if err := os.RemoveAll(f); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// pauseDebrid stops a debrid item. In the cloud phase only the polling
// stops (the service keeps downloading); in the local phase every file stops
// and keeps its .part data. Resume goes back to the phase, unlocking the
// links again first.
func (e *Engine) pauseDebrid(h metainfo.Hash, it *debridItem) error {
	e.mu.Lock()
	if e.debrid[h] != it || it.paused {
		e.mu.Unlock()
		return nil
	}
	switch it.phase {
	case DebridCloud, DebridReady, DebridLocal:
	default: // nothing runs in the preview, and a done or failed item is idle
		e.mu.Unlock()
		return nil
	}
	it.paused, it.needsLogin = true, false
	it.note, it.err = "", nil
	it.holdGen++ // a pending automatic restart must not undo the pause
	it.samples = ring{}
	e.stopDebridPollLocked(it)
	done := it.pollDone
	e.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

// verifyDebrid compares each finished file's size with the service's
// (debrid downloads have no piece hashes to check). A file that is missing
// or the wrong size is moved aside (.corrupt) and the item downloads it
// again; the result then has NeedsRepair set.
func (e *Engine) verifyDebrid(ctx context.Context, h metainfo.Hash, it *debridItem) (VerifyResult, error) {
	var result VerifyResult
	e.mu.Lock()
	if it.phase != DebridDone {
		e.mu.Unlock()
		return result, ErrVerificationIncomplete
	}
	type want struct {
		path string
		size int64
	}
	var files []want
	for _, c := range it.children {
		if c.d != nil {
			files = append(files, want{c.d.dataPath, c.d.expectedSize})
		}
	}
	p, id, remoteDeleted := it.provider, it.remoteID, it.remoteDeleted
	e.mu.Unlock()

	if len(files) == 0 { // finished in an earlier run: ask the service for the sizes
		if p == nil {
			return result, ErrNoDebrid
		}
		if id == "" || remoteDeleted {
			return result, errors.New("the cloud copy was deleted, so the file sizes are unknown")
		}
		info, err := p.Info(ctx, id)
		if err != nil {
			if errors.Is(err, debrid.ErrNotFound) {
				return result, errors.New("the cloud copy is gone, so the file sizes are unknown")
			}
			return result, err
		}
		e.mu.Lock()
		children, note, err := e.planDebridLocal(it, info)
		e.mu.Unlock()
		if err != nil {
			return result, err
		}
		if note != "" {
			return result, errors.New("the service packed these files into an archive; there are no sizes to compare")
		}
		for _, c := range children {
			files = append(files, want{c.d.dataPath, c.d.expectedSize})
		}
	}

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.CheckedFiles++
		fi, err := os.Lstat(f.path)
		switch {
		case os.IsNotExist(err):
			result.NeedsRepair = true
			continue
		case err != nil:
			return result, err
		case f.size <= 0 || (fi.Mode().IsRegular() && fi.Size() == f.size):
			continue
		}
		quarantine, err := nextQuarantinePath(f.path)
		if err != nil {
			return result, err
		}
		if err := os.Rename(f.path, quarantine); err != nil {
			return result, fmt.Errorf("move the wrong-size file aside: %w", err)
		}
		result.WrongSize++
		result.NeedsRepair = true
		if result.QuarantinePath == "" {
			result.QuarantinePath = quarantine
		}
	}
	if !result.NeedsRepair {
		return result, nil
	}
	// download what is missing again (the files that match are kept)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.debrid[h] == it && it.phase == DebridDone {
		it.phase, it.selectSent, it.readyFired = DebridLocal, true, true
		it.shownDone = 0
		it.remoteDeleted = false
		e.restartDebridLocked(it)
	}
	return result, nil
}
