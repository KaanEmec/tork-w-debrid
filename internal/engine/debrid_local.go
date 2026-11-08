package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/config"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
)

// This file is the local phase of a debrid item (roadmap J3.3): once the
// service has every selected file, each link is unlocked and its file fetched
// with tork's own HTTPS downloader (runDirect), which brings Range resume,
// segmented transfers (E7), per-segment retry and link refresh along. The
// files are children of the debrid item, not entries of Engine.direct, so the
// downloads list keeps one row per torrent with one combined progress.
//
// Files land where the torrent client would put them (README D5):
// <download dir>/<torrent name>/<path in torrent>, or straight in the
// download dir for a single-file torrent. Every path comes from the service
// and is checked with safeRelPath before anything touches the disk.

// debridLocalFiles is how many files of one debrid torrent download at once;
// the rest wait their turn.
const debridLocalFiles = 2

// debridUnlockTries bounds the attempts to unlock one link while the service
// is busy or unreachable.
const debridUnlockTries = 6

// debridChild is one file of a debrid item's local phase. Guarded by Engine.mu.
type debridChild struct {
	d        *directItem // nil until an archive's link is unlocked
	fileID   int         // remote file ID; 0 for an archive
	link     string      // hoster link, unlocked again on every run
	prefix   string      // "<torrent name>/" for a multi-file torrent, else ""
	index    int         // position among the item's links
	archive  bool        // the service packed several files into this one
	sizeHint int64       // shown until the size is known (archives)
	started  bool        // handed to the downloader in this run
}

// size is the child's length as far as it is known.
func (c *debridChild) size() int64 {
	if c.d != nil {
		if c.d.length > 0 {
			return c.d.length
		}
		if c.d.expectedSize > 0 {
			return c.d.expectedSize
		}
	}
	return c.sizeHint
}

func debridConnsOrDefault(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// SetDebridSettings applies the debrid block of the config to the engine:
// connections per file and per torrent (roadmap J7.5) and the clean-up after
// a finished download. Running transfers use the new connection counts the
// next time a connection claims a segment; the adaptive lowering after busy
// answers (J7.3) stays below them. Call it after saving a debrid setting.
func (e *Engine) SetDebridSettings(c config.DebridConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.debridConns = min(debridConnsOrDefault(c.Connections, DefaultDownloadConnections), MaxDownloadConnections)
	e.debridTotal = debridConnsOrDefault(c.MaxTotalConnections, DefaultConnectionBudget)
	e.debridDeleteAfter = c.DeleteAfterDownload
	for _, it := range e.debrid {
		if it.budget != nil {
			it.budget.SetTotal(e.debridTotal)
		}
	}
}

// debridTorrentName is the folder of a multi-file torrent: the name the
// service reports, else the magnet's name. It reports false when neither is
// a safe folder name.
func (it *debridItem) debridTorrentName(info debrid.RemoteInfo) (string, bool) {
	for _, name := range []string{info.Name, it.name} {
		if safeDirectFilename(name) {
			return name, true
		}
	}
	return "", false
}

// debridDataPath is the item's path for the downloads list (the torrent
// folder, or the single file), or "" while unknown or unsafe. Caller holds
// e.mu.
func (it *debridItem) debridDataPath(info debrid.RemoteInfo) string {
	if it.downloadDir == "" || len(info.Files) == 0 {
		return ""
	}
	if len(info.Files) == 1 {
		path, _ := debridFilePath(it.downloadDir, info.Files[0].Path)
		return path
	}
	name, ok := it.debridTorrentName(info)
	if !ok {
		return ""
	}
	path, _ := safeDataPath(it.downloadDir, name)
	return path
}

// debridFilePath joins a checked "/" path under dir.
func debridFilePath(dir, rel string) (string, bool) {
	if !safeDirectName(rel) {
		return "", false
	}
	if strings.Contains(rel, "/") {
		return safeRelDataPath(dir, rel)
	}
	return safeDataPath(dir, rel)
}

// planDebridLocal pairs the selected files with the service's links and
// builds one child per link. Caller holds e.mu.
func (e *Engine) planDebridLocal(it *debridItem, info debrid.RemoteInfo) (children []*debridChild, note string, err error) {
	files := info.SelectedFiles()
	links := info.Links
	switch {
	case len(links) == 0:
		return nil, "", errors.New("the service sent no download links")
	case len(links) > len(files):
		return nil, "", fmt.Errorf("the service sent %d links for %d files; not guessing which is which", len(links), len(files))
	}
	prefix := ""
	if len(info.Files) > 1 {
		name, ok := it.debridTorrentName(info)
		if !ok {
			return nil, "", errors.New("the torrent's name is not a safe folder name; refusing to write it")
		}
		prefix = name + "/"
	}
	hash := it.hash.HexString()
	if len(links) < len(files) {
		// Usually the service packed the files into one RAR: download what it
		// gives, named after the unlocked file.
		note = it.providerName() + " sent one archive"
		if len(links) > 1 {
			note = fmt.Sprintf("%s sent %d archives", it.providerName(), len(links))
		}
		for i, link := range links {
			children = append(children, &debridChild{
				link: link, prefix: prefix, index: i, archive: true,
				sizeHint: info.Bytes / int64(len(links)),
			})
		}
		return children, note, nil
	}
	for i, f := range files {
		rel := prefix + f.Path
		path, ok := debridFilePath(it.downloadDir, rel)
		if !ok {
			return nil, "", fmt.Errorf("unsafe path %q in the torrent; refusing to write it", f.Path)
		}
		c := &debridChild{fileID: f.ID, link: links[i], prefix: prefix, index: i}
		c.d = e.newDebridFile(it, rel, path, f.Bytes, fmt.Sprintf("%s/%d", hash, f.ID))
		children = append(children, c)
	}
	return children, "", nil
}

// newDebridFile builds a child download. Its URL is set when its link is
// unlocked. Caller holds e.mu.
func (e *Engine) newDebridFile(it *debridItem, rel, path string, size int64, key string) *directItem {
	return &directItem{
		name: rel, expectedSize: size, length: size,
		downloadDir: it.downloadDir, dataPath: path,
		state: StatePaused, resumeKey: key,
		debridConns: true, budget: it.budget,
	}
}

func (it *debridItem) providerName() string {
	if it.provider != nil {
		if name := it.provider.Name(); name != "" {
			return name
		}
	}
	if it.via == config.DebridProviderRealDebrid {
		return "Real-Debrid"
	}
	return "the debrid service"
}

// debridRefresher unlocks link again for a fresh URL (roadmap J7.2/J7.3).
func debridRefresher(p debrid.Provider, link string) URLRefresher {
	return func(ctx context.Context) (string, error) {
		u, err := p.Unlock(ctx, link)
		if err != nil {
			return "", err
		}
		return u.URL, nil
	}
}

// runDebridLocal downloads a ready item's files. It runs on the item's
// poller goroutine, so Pause, Remove and Close stop it through ctx; every
// file it started is stopped (keeping its .part) before it returns.
func (e *Engine) runDebridLocal(ctx context.Context, it *debridItem, info debrid.RemoteInfo) {
	defer e.stopDebridChildren(it)
	e.mu.Lock()
	if e.debrid[it.hash] != it || ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	it.phase = DebridLocal
	if it.budget == nil {
		it.budget = NewConnectionBudget(e.debridTotal)
	} else {
		it.budget.SetTotal(e.debridTotal)
	}
	if path := it.debridDataPath(info); path != "" {
		it.dataPath = path
	}
	children, note, err := e.planDebridLocal(it, info)
	if err != nil {
		e.holdDebridLocked(ctx, it, err, "")
		e.mu.Unlock()
		return
	}
	it.children, it.localNote = children, note
	it.localWake = make(chan struct{}, 1)
	p := it.provider
	e.mu.Unlock()

	for {
		e.mu.Lock()
		if e.debrid[it.hash] != it || ctx.Err() != nil || e.closing {
			e.mu.Unlock()
			return
		}
		next, running, failed, done := it.childProgress()
		switch {
		case done:
			cleanup := e.finishDebridLocked(it)
			e.mu.Unlock()
			if cleanup != "" {
				e.deleteRemoteLater(p, cleanup)
			}
			return
		case failed != nil && running == 0:
			e.holdDebridLocked(ctx, it, failed.d.err, failed.d.note)
			e.mu.Unlock()
			return
		case failed == nil && next != nil && running < debridLocalFiles:
			next.started = true
			e.mu.Unlock()
			if err := e.startDebridChild(ctx, it, p, next); err != nil {
				e.mu.Lock()
				e.holdDebridLocked(ctx, it, err, "")
				e.mu.Unlock()
				return
			}
			continue
		}
		wake := it.localWake
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

// childProgress sums up the children: the next one to start, how many run,
// the first that failed, and whether all are done. Caller holds e.mu.
func (it *debridItem) childProgress() (next *debridChild, running int, failed *debridChild, done bool) {
	done = true
	for _, c := range it.children {
		switch {
		case !c.started:
			done = false
			if next == nil {
				next = c
			}
		case c.d == nil: // being unlocked
			done = false
			running++
		case c.d.state == StateDone:
		case c.d.cancel != nil || c.d.state == StateDownloading:
			done = false
			running++
		default: // the downloader gave up on it
			done = false
			if failed == nil {
				failed = c
			}
		}
	}
	return next, running, failed, done
}

// startDebridChild unlocks a child's link and starts its download.
func (e *Engine) startDebridChild(ctx context.Context, it *debridItem, p debrid.Provider, c *debridChild) error {
	u, err := e.unlockDebridLink(ctx, it, p, c.link)
	if err != nil {
		return err
	}
	if err := checkRefreshedURL(u.URL, ""); err != nil {
		return fmt.Errorf("unlocked link: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.debrid[it.hash] != it || ctx.Err() != nil || e.closing {
		return context.Canceled
	}
	if c.archive {
		if !safeDirectFilename(u.Filename) {
			return fmt.Errorf("unsafe file name %q from the service; refusing to write it", u.Filename)
		}
		rel := c.prefix + u.Filename
		path, ok := debridFilePath(it.downloadDir, rel)
		if !ok {
			return fmt.Errorf("unsafe file name %q from the service; refusing to write it", u.Filename)
		}
		c.d = e.newDebridFile(it, rel, path, u.Bytes, fmt.Sprintf("%s/archive/%d", it.hash.HexString(), c.index))
	}
	d := c.d
	d.url, d.refreshURL = u.URL, debridRefresher(p, c.link)
	d.urlGen++
	d.budget = it.budget
	if done, err := debridFileOnDisk(d); err != nil {
		return err
	} else if done {
		d.done, d.length, d.state = d.expectedSize, d.expectedSize, StateDone
		signal(it.localWake)
		return nil
	}
	e.startDirectLocked(d)
	runDone, wake := d.runDone, it.localWake
	e.dwg.Add(1)
	go func() {
		defer e.dwg.Done()
		<-runDone
		signal(wake)
	}()
	return nil
}

// debridFileOnDisk checks a child's destination: true when the file is
// already there with the expected size. Any other file there is refused
// rather than overwritten, as the ISO shelf does.
func debridFileOnDisk(d *directItem) (bool, error) {
	fi, err := os.Lstat(d.dataPath)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, err
	case fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular():
		return false, fmt.Errorf("%s already exists and is not a regular file", d.name)
	case d.expectedSize > 0 && fi.Size() == d.expectedSize:
		return true, nil
	case d.expectedSize > 0:
		return false, fmt.Errorf("%s already exists with size %d, expected %d; refusing to overwrite", d.name, fi.Size(), d.expectedSize)
	}
	return false, fmt.Errorf("%s already exists and its size is unknown; refusing to overwrite", d.name)
}

// unlockDebridLink unlocks a hoster link, waiting through a busy or
// unreachable service.
func (e *Engine) unlockDebridLink(ctx context.Context, it *debridItem, p debrid.Provider, link string) (debrid.Unlocked, error) {
	for tries := 1; ; tries++ {
		u, err := p.Unlock(ctx, link)
		if err == nil {
			return u, nil
		}
		if ctx.Err() != nil {
			return debrid.Unlocked{}, ctx.Err()
		}
		if !transientDebridErr(err) || tries >= debridUnlockTries {
			return debrid.Unlocked{}, err
		}
		if !debridSleep(ctx, it, debridRetryDelay(tries)) {
			return debrid.Unlocked{}, ctx.Err()
		}
	}
}

// finishDebridLocked marks the item done. It returns the remote ID to delete
// when debrid.delete_after_download is on. Caller holds e.mu.
func (e *Engine) finishDebridLocked(it *debridItem) (deleteID string) {
	it.phase, it.cancel = DebridDone, nil
	it.note, it.err = "", nil
	it.autoRetries = 0
	if e.debridDeleteAfter && it.remoteID != "" && !e.closing {
		it.remoteDeleted = true
		return it.remoteID
	}
	return ""
}

// holdDebridLocked stops the item with a note after a failure that a retry
// (p) may fix: the files keep their .part data and the phase is kept. A
// cancelled ctx means pause or remove already spoke for the item. Caller
// holds e.mu.
func (e *Engine) holdDebridLocked(ctx context.Context, it *debridItem, err error, fallback string) {
	if ctx.Err() != nil || e.debrid[it.hash] != it {
		return
	}
	it.paused, it.cancel = true, nil
	it.needsLogin = needsDebridLogin(err) // SetDebridProvider continues it
	it.err = err
	it.note = it.holdNote(err, fallback)
	if lostNetwork(err) && it.autoRetries < debridTuning.autoRetries {
		e.autoRestartDebridLocked(it)
	}
}

// lostNetwork reports a hold a later retry should fix by itself: the files'
// connections or the service's API could not be reached.
func lostNetwork(err error) bool {
	var ne directNetError
	return errors.As(err, &ne) || transientDebridErr(err)
}

// autoRestartDebridLocked starts a held item again after a back-off, unless
// it was resumed, paused or removed in the meantime. Caller holds e.mu.
func (e *Engine) autoRestartDebridLocked(it *debridItem) {
	it.autoRetries++
	it.autoPending = true
	wait := min(debridTuning.autoRetryBase<<min(it.autoRetries-1, 6), max(debridTuning.retryMax, debridTuning.autoRetryBase))
	it.note = fmt.Sprintf("connection lost - retrying in %s · p retries now", wait.Round(time.Second))
	gen := it.holdGen
	time.AfterFunc(wait, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.debrid[it.hash] == it && it.paused && it.holdGen == gen && !e.closing {
			e.restartDebridLocked(it)
		}
	})
}

// holdNote words a local-phase failure for the row.
func (it *debridItem) holdNote(err error, fallback string) string {
	var de *debrid.Error
	switch {
	case needsDebridLogin(err):
		return "☁ " + it.providerName() + " account needs login"
	case errors.Is(err, debrid.ErrNotFound):
		return "☁ " + it.short + " link no longer exists - press p to retry"
	case errors.As(err, &de):
		text := debrid.Message(err)
		if i := strings.Index(text, ": "); i >= 0 {
			text = text[i+2:]
		}
		return "☁ " + it.short + " " + text + " - press p to retry"
	case fallback != "":
		return fallback
	case err != nil:
		return shortNetErr(err)
	}
	return "stopped - press p to retry"
}

// stopDebridChildren pauses every running file of the item and waits until
// each has released its files.
func (e *Engine) stopDebridChildren(it *debridItem) {
	e.mu.Lock()
	var waits []chan struct{}
	for _, c := range it.children {
		if c.d == nil || !c.started {
			continue
		}
		pauseDirectLocked(c.d)
		if c.d.runDone != nil {
			waits = append(waits, c.d.runDone)
		}
	}
	e.mu.Unlock()
	for _, ch := range waits {
		<-ch
	}
}

// debridLocalSnapshot fills the combined progress of the local phase: bytes
// and length summed over the files, speed and ETA from the sum. Caller holds
// e.mu.
func (e *Engine) debridLocalSnapshot(s *Snapshot, it *debridItem, now time.Time) {
	var done, length int64
	started := false
	note := ""
	for _, c := range it.children {
		size := c.size()
		length += size
		if c.d != nil {
			got := c.d.done
			if size > 0 {
				got = min(got, size)
			}
			done += got
			if c.started && c.d.state == StateDownloading && c.d.note != "" && note == "" {
				note = c.d.note
			}
			// unlocked and handed to the downloader (not just being unlocked)
			started = started || (c.started && (c.d.state == StateDownloading || c.d.state == StateDone))
		}
	}
	if len(it.children) == 0 {
		length = it.info.Bytes
		if length == 0 {
			length = it.savedLength
		}
	}
	// a fresh run starts each file at 0 until it has read its .part file:
	// never show less than was shown before
	done = max(done, it.shownDone)
	if length > 0 {
		done = min(done, length)
	}
	it.shownDone = done
	s.Length, s.BytesCompleted = length, done
	switch {
	case it.paused:
		s.State = StatePaused
		s.Retrying = it.autoPending
		s.Note = it.note
		if s.Note == "" {
			s.Note = "paused"
		}
		it.samples = ring{}
		return
	case it.phase == DebridDone:
		s.State = StateDone
		s.BytesCompleted = s.Length
		s.Note = it.localNote
		return
	case !started && !it.remoteVisible(now):
		s.State = StateFetchingMeta // still starting, like a torrent connecting
		s.Note = NoteDebridStarting
		return
	}
	s.State = StateDownloading
	it.samples.push(sample{at: now, bytes: done})
	s.SpeedBps = it.samples.speedBps()
	if s.SpeedBps > 0 && s.Length > s.BytesCompleted {
		s.ETA = time.Duration(float64(s.Length-s.BytesCompleted) / s.SpeedBps * float64(time.Second))
	}
	s.Note = it.localNote
	if note != "" {
		s.Note = note
	}
}

// debridSegments returns the segment map of the item's first running file,
// for the segment bar (J7.6).
func (e *Engine) debridSegments(h metainfo.Hash) (SegmentMap, bool) {
	e.mu.Lock()
	it, ok := e.debrid[h]
	var files []*directItem
	if ok {
		for _, c := range it.children {
			if c.d != nil && c.started && c.d.state == StateDownloading {
				files = append(files, c.d)
			}
		}
	}
	e.mu.Unlock()
	for _, d := range files {
		if m, ok := e.directSegments(d); ok {
			return m, true
		}
	}
	return SegmentMap{}, false
}
