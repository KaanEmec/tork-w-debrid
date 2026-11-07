package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/intake"
)

// This file adds debrid items (roadmap J3.2): a torrent that a debrid service
// (Real-Debrid) downloads in its cloud before tork fetches the files over
// HTTPS. A debridItem owns the torrent's whole life on the service. It is
// created for the preview (AddDebridForPreview), started with a file
// selection (StartDebrid), and polled until the service has the files ready,
// when the ready hook fires and the local phase (J3.3) takes over.
//
// The engine owns the item, not the TUI, so on macOS and Linux the poller
// keeps running in the background session after the terminal closes. Every
// provider call goes through the provider's own rate limiter (Real-Debrid's
// is shared by the whole process), so many items cannot exceed the API limit.

// DebridPhase is where a debrid item is in its life. The string values are
// stable, so callers may persist them (J3.4 saves "cloud" and "local").
type DebridPhase string

const (
	// DebridAdding: the torrent is being added to the service.
	DebridAdding DebridPhase = "adding"
	// DebridPreview: the torrent is on the service and waits for StartDebrid.
	DebridPreview DebridPhase = "preview"
	// DebridCloud: files are chosen and the service is downloading them.
	DebridCloud DebridPhase = "cloud"
	// DebridReady: the service has every selected file; the local phase
	// (J3.3) starts from the ready hook.
	DebridReady DebridPhase = "ready"
	// DebridLocal: the files are being fetched over HTTPS (J3.3).
	DebridLocal DebridPhase = "local"
	// DebridDone: every file is on disk (J3.3).
	DebridDone DebridPhase = "done"
	// DebridFailed: the service or the account refused the torrent; the
	// item's note says why. Polling has stopped.
	DebridFailed DebridPhase = "failed"
)

var (
	// ErrNoDebrid: no debrid provider is set (no linked account).
	ErrNoDebrid = errors.New("no debrid account is linked")
	// ErrAlreadyTorrent: the infohash is already a normal torrent download.
	ErrAlreadyTorrent = errors.New("this torrent is already in the list as a normal torrent download")
	// ErrAlreadyDebrid: the infohash is already a debrid download.
	ErrAlreadyDebrid = errors.New("this torrent is already in the list as a debrid download")
	// ErrDebridNotStarted: the item is not waiting for StartDebrid.
	ErrDebridNotStarted = errors.New("the debrid download cannot start from its current state")
)

// debridReadyWindow is how long after the file selection a debrid item keeps
// looking like a starting download before it shows the cloud's progress
// (roadmap J4.3). Tests shorten it.
var debridReadyWindow = 5 * time.Second

// debridTuning holds the poll intervals. Tests shrink them.
var debridTuning = struct {
	fastPoll     time.Duration // while converting and inside the ready window
	downloadPoll time.Duration // while the service downloads
	queuedPoll   time.Duration // while the service has not started yet
	retryBase    time.Duration // first back-off after a transient error
	retryMax     time.Duration // longest back-off
	cleanupWait  time.Duration // bound on a background Delete
}{
	fastPoll:     time.Second,
	downloadPoll: 5 * time.Second,
	queuedPoll:   15 * time.Second,
	retryBase:    2 * time.Second,
	retryMax:     time.Minute,
	cleanupWait:  15 * time.Second,
}

// DebridOptions configures StartDebrid.
type DebridOptions struct {
	// DownloadDir is where the files land ("" = the default download dir).
	DownloadDir string
}

// RemoteProgress is a provider-neutral view of a cloud download, carried on
// Snapshot.Remote for debrid items.
type RemoteProgress struct {
	Status debrid.RemoteStatus
	// Progress is 0–100.
	Progress float64
	SpeedBps int64
	Seeders  int
	// Reason explains a failed status in a few words.
	Reason string
}

// DebridReadyEvent is passed to the ready hooks.
type DebridReadyEvent struct {
	Hash metainfo.Hash
	Info debrid.RemoteInfo
}

// DebridItemInfo describes a debrid item for callers that save it (J3.4) or
// show it (J4.x).
type DebridItemInfo struct {
	Hash     metainfo.Hash
	Name     string
	Magnet   string
	Via      string // provider ID, such as "realdebrid"
	RemoteID string
	Phase    DebridPhase
	// Selected is the selection given to StartDebrid (nil = every file).
	Selected []int
	// TorrentData is the .torrent the item was added from, or nil.
	TorrentData []byte
	DownloadDir string
	// Remote is the last RemoteInfo seen (zero before the first poll).
	Remote debrid.RemoteInfo
	// Err is why the item failed or was held, or nil.
	Err error
	// Paused: stopped by Pause or held after a failure (Note says which);
	// Resume continues from Phase.
	Paused bool
	Note   string
	// DataPath is the torrent folder (or the single file) under DownloadDir.
	DataPath string
	// RemoteDeleted: the cloud copy was deleted after the download.
	RemoteDeleted bool
}

// debridItem is one torrent on a debrid service. All fields are guarded by
// Engine.mu.
type debridItem struct {
	hash        metainfo.Hash
	name        string
	magnet      string
	torrentData []byte // the .torrent bytes, when added from a file or URL
	downloadDir string
	provider    debrid.Provider
	via         string // provider ID, such as "realdebrid"
	short       string // short provider label for notes, such as "RD"

	remoteID   string
	phase      DebridPhase
	info       debrid.RemoteInfo // last Info answer
	infoAt     time.Time         // zero until the first Info answer
	selected   []int             // remote file IDs given to StartDebrid (nil = all)
	selectSent bool              // SelectFiles succeeded (or was not needed)
	selectAt   time.Time         // when the selection reached the service
	readyAt    time.Time         // when Ready was first seen
	readyFired bool              // the ready hook ran
	err        error             // why the item failed
	note       string

	cancel   context.CancelFunc // stops the poller; nil while none runs
	pollDone chan struct{}      // closed when the current poller returns
	wake     chan struct{}      // nudges a sleeping poller (StartDebrid)

	// children are the per-file HTTPS downloads of the local phase (J3.3).
	// They are not in Engine.direct, so the list shows one row per torrent.
	children  []*debridChild
	budget    *ConnectionBudget // debrid.max_total_connections for all files
	localWake chan struct{}     // a file of the local phase stopped
	localNote string            // shown on the row while the files download
	dataPath  string            // torrent folder or single file (D5), "" while unknown
	shownDone int64             // combined bytes last shown; never goes back
	samples   ring              // combined speed of the local phase

	// paused: stopped by Pause or held after a failure a retry may fix (the
	// note says which). The phase is kept, so Resume goes back to it.
	paused        bool
	remoteDeleted bool // delete_after_download removed the cloud copy
}

// SetDebridProvider sets the debrid service new debrid items use (nil turns
// debrid off for new items). Items already added keep their provider.
func (e *Engine) SetDebridProvider(p debrid.Provider) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.debridProvider = p
}

// OnDebridReady registers fn to run once per item when the service has every
// selected file. It runs on the poller's goroutine without the engine lock.
func (e *Engine) OnDebridReady(fn func(DebridReadyEvent)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.debridHooks = append(e.debridHooks, fn)
}

// AddDebridForPreview adds a magnet to the debrid service and returns its
// infohash as soon as the service accepted it; the file list follows through
// DebridFiles. It returns owned=false when the item already existed, so a
// cancelled preview must not delete it.
func (e *Engine) AddDebridForPreview(ctx context.Context, magnet string) (metainfo.Hash, bool, error) {
	spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return metainfo.Hash{}, false, err
	}
	if spec.InfoHash.IsZero() {
		return metainfo.Hash{}, false, errors.New("magnet has no BitTorrent v1 infohash")
	}
	h := metainfo.Hash(spec.InfoHash)
	return e.addDebridPreview(ctx, h, spec.DisplayName, magnet, nil)
}

// AddDebridTorrentForPreview is AddDebridForPreview for a .torrent file's
// bytes. It also returns the torrent's name and an infohash magnet for the
// saved state.
func (e *Engine) AddDebridTorrentForPreview(ctx context.Context, data []byte) (h metainfo.Hash, name, magnet string, owned bool, err error) {
	if len(data) == 0 || int64(len(data)) > intake.MaxTorrentBytes {
		return h, "", "", false, errors.New("torrent file must be non-empty and no larger than 16 MiB")
	}
	mi, err := metainfo.Load(io.LimitReader(bytes.NewReader(data), intake.MaxTorrentBytes+1))
	if err != nil {
		return h, "", "", false, fmt.Errorf("read torrent: %w", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return h, "", "", false, fmt.Errorf("read torrent: %w", err)
	}
	h = mi.HashInfoBytes()
	name = info.Name
	magnet = mi.Magnet(&h, &info).String()
	_ = e.metainfo.Store(*mi)
	h, owned, err = e.addDebridPreview(ctx, h, name, magnet, append([]byte(nil), data...))
	return h, name, magnet, owned, err
}

func (e *Engine) addDebridPreview(ctx context.Context, h metainfo.Hash, name, magnet string, data []byte) (metainfo.Hash, bool, error) {
	e.mu.Lock()
	p := e.debridProvider
	switch {
	case e.closing:
		e.mu.Unlock()
		return h, false, errors.New("engine is closing")
	case p == nil:
		e.mu.Unlock()
		return h, false, ErrNoDebrid
	}
	if _, ok := e.debrid[h]; ok {
		e.mu.Unlock()
		return h, false, nil
	}
	if _, ok := e.items[h]; ok {
		e.mu.Unlock()
		return h, false, ErrAlreadyTorrent
	}
	it := &debridItem{
		hash: h, name: name, magnet: magnet, torrentData: data,
		downloadDir: e.defaultDownloadDir,
		provider:    p, via: providerID(p.Name()), short: providerShort(p.Name()),
		phase: DebridAdding, wake: make(chan struct{}, 1),
	}
	e.debrid[h] = it // a placeholder, so a second add of the same hash waits its turn
	e.mu.Unlock()

	var id string
	var err error
	if data != nil {
		id, err = p.AddTorrentFile(ctx, data)
	} else {
		id, err = p.AddMagnet(ctx, magnet)
	}

	e.mu.Lock()
	if e.debrid[h] != it { // cancelled while adding
		e.mu.Unlock()
		if err == nil {
			e.deleteRemoteLater(p, id)
		}
		return h, false, errors.New("debrid preview was cancelled")
	}
	if err != nil {
		delete(e.debrid, h)
		e.mu.Unlock()
		return h, false, err
	}
	it.remoteID = id
	it.phase = DebridPreview
	if !e.closing {
		e.startDebridPollLocked(it)
	}
	e.mu.Unlock()
	return h, true, nil
}

// DebridFiles returns the remote torrent's files. ready is false while the
// service is still reading the torrent (or the item is unknown or failed).
func (e *Engine) DebridFiles(h metainfo.Hash) ([]debrid.RemoteFile, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.debrid[h]
	if !ok || it.infoAt.IsZero() || it.info.Status == debrid.Converting || len(it.info.Files) == 0 {
		return nil, false
	}
	return append([]debrid.RemoteFile(nil), it.info.Files...), true
}

// DebridItem describes a debrid item, or reports false for any other hash.
func (e *Engine) DebridItem(h metainfo.Hash) (DebridItemInfo, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.debrid[h]
	if !ok {
		return DebridItemInfo{}, false
	}
	info := it.info
	info.Files = append([]debrid.RemoteFile(nil), info.Files...)
	info.Links = append([]string(nil), info.Links...)
	return DebridItemInfo{
		Hash: h, Name: it.displayName(), Magnet: it.magnet, Via: it.via,
		RemoteID: it.remoteID, Phase: it.phase,
		Selected:    append([]int(nil), it.selected...),
		TorrentData: it.torrentData, DownloadDir: it.downloadDir,
		Remote: info, Err: it.err,
		Paused: it.paused, Note: it.note, DataPath: it.dataPath, RemoteDeleted: it.remoteDeleted,
	}, true
}

// StartDebrid chooses the files to download (remote file IDs; nil or empty =
// every file) and begins the cloud phase. It never blocks on the network:
// the poller sends the selection, and when the service is still reading the
// torrent it waits for the file list first. Starting an item that already
// runs is a no-op.
func (e *Engine) StartDebrid(h metainfo.Hash, fileIDs []int, opts DebridOptions) error {
	dir := e.normalizeOptions(AddOptions{DownloadDir: opts.DownloadDir}).DownloadDir
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.debrid[h]
	if !ok {
		return errors.New("unknown debrid download")
	}
	if _, ok := e.items[h]; ok {
		return ErrAlreadyTorrent
	}
	switch it.phase {
	case DebridPreview:
	case DebridCloud, DebridReady, DebridLocal, DebridDone:
		return nil
	default:
		return ErrDebridNotStarted
	}
	if len(fileIDs) > 0 && !it.infoAt.IsZero() && len(it.info.Files) > 0 {
		known := make(map[int]bool, len(it.info.Files))
		for _, f := range it.info.Files {
			known[f.ID] = true
		}
		for _, id := range fileIDs {
			if !known[id] {
				return fmt.Errorf("the torrent has no file with ID %d", id)
			}
		}
	}
	if len(fileIDs) > 0 {
		it.selected = append([]int(nil), fileIDs...)
	}
	it.downloadDir = dir
	it.phase = DebridCloud
	if e.closing {
		return nil
	}
	if it.cancel == nil {
		e.startDebridPollLocked(it)
	} else {
		select {
		case it.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// CancelDebridPreview forgets an item the user left in the preview without
// downloading, and deletes the remote torrent in the background (bounded,
// and stopped by Close). Items that already started are left alone.
func (e *Engine) CancelDebridPreview(h metainfo.Hash) {
	e.mu.Lock()
	it, ok := e.debrid[h]
	if !ok || (it.phase != DebridPreview && it.phase != DebridAdding && it.phase != DebridFailed) {
		e.mu.Unlock()
		return
	}
	delete(e.debrid, h)
	e.stopDebridPollLocked(it)
	p, id := it.provider, it.remoteID
	e.mu.Unlock()
	if id != "" { // "" while adding: the adder deletes it when the add returns
		e.deleteRemoteLater(p, id)
	}
}

// removeDebrid stops a debrid item's poller and forgets it. The remote copy
// and local data are J3.5's business (RemoveOptions).
func (e *Engine) removeDebrid(h metainfo.Hash, it *debridItem) error {
	e.mu.Lock()
	if e.debrid[h] == it {
		delete(e.debrid, h)
	}
	e.stopDebridPollLocked(it)
	done := it.pollDone
	e.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

// deleteRemoteLater deletes a remote torrent on a tracked goroutine, bounded
// by debridTuning.cleanupWait and cancelled by Close.
func (e *Engine) deleteRemoteLater(p debrid.Provider, id string) {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return
	}
	e.dwg.Add(1)
	base := e.debridCtx
	e.mu.Unlock()
	go func() {
		defer e.dwg.Done()
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(base, debridTuning.cleanupWait)
		defer cancel()
		_ = p.Delete(ctx, id)
	}()
}

// startDebridPollLocked launches the item's poller. Caller holds e.mu and
// has checked e.closing.
func (e *Engine) startDebridPollLocked(it *debridItem) {
	ctx, cancel := context.WithCancel(e.debridCtx)
	done := make(chan struct{})
	it.cancel = cancel
	it.pollDone = done
	e.dwg.Add(1)
	go func() {
		defer e.dwg.Done()
		defer close(done)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				e.mu.Lock()
				e.failDebridLocked(it, fmt.Errorf("internal debrid failure: %v", recovered))
				e.mu.Unlock()
			}
		}()
		e.runDebrid(ctx, it)
	}()
}

// stopDebridPollLocked cancels the item's poller. Caller holds e.mu.
func (e *Engine) stopDebridPollLocked(it *debridItem) {
	if it.cancel != nil {
		it.cancel()
		it.cancel = nil
	}
}

// runDebrid polls the service for one item until the file list is known (in
// the preview), the files are ready, the torrent fails, or ctx ends.
func (e *Engine) runDebrid(ctx context.Context, it *debridItem) {
	failures := 0
	for {
		e.mu.Lock()
		if e.debrid[it.hash] != it || ctx.Err() != nil {
			e.mu.Unlock()
			return
		}
		p, id := it.provider, it.remoteID
		needSelect := it.phase == DebridCloud && !it.selectSent && it.info.Status == debrid.WaitingSelection
		selected := append([]int(nil), it.selected...)
		e.mu.Unlock()

		if needSelect {
			err := p.SelectFiles(ctx, id, selected)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				if transientDebridErr(err) {
					failures++
					if !debridSleep(ctx, it, debridRetryDelay(failures)) {
						return
					}
					continue
				}
				e.mu.Lock()
				e.failDebridLocked(it, err)
				e.mu.Unlock()
				return
			}
			e.mu.Lock()
			it.selectSent, it.selectAt = true, time.Now()
			e.mu.Unlock()
		}

		info, err := p.Info(ctx, id)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if transientDebridErr(err) {
				failures++
				if !debridSleep(ctx, it, debridRetryDelay(failures)) {
					return
				}
				continue
			}
			// ErrNotFound (deleted on the service) fails for now; J3.4 re-adds.
			e.mu.Lock()
			e.failDebridLocked(it, err)
			e.mu.Unlock()
			return
		}
		failures = 0

		now := time.Now()
		e.mu.Lock()
		if e.debrid[it.hash] != it {
			e.mu.Unlock()
			return
		}
		it.info, it.infoAt = info, now
		if it.name == "" && info.Name != "" {
			it.name = info.Name
		}
		if it.phase != DebridPreview {
			if path := it.debridDataPath(info); path != "" {
				it.dataPath = path
			}
		}
		if it.phase == DebridCloud && !it.selectSent && info.Status != debrid.Converting && info.Status != debrid.WaitingSelection {
			// the service skipped the selection (already chosen): the window
			// starts now
			it.selectSent, it.selectAt = true, now
		}
		switch {
		case info.Status == debrid.Failed:
			e.failDebridLocked(it, nil)
			e.mu.Unlock()
			return
		case it.phase == DebridPreview && info.Status != debrid.Converting:
			// the file list is known: nothing changes until StartDebrid
			it.cancel = nil
			e.mu.Unlock()
			return
		case it.phase == DebridCloud && it.selectSent && info.Status == debrid.Ready:
			it.phase, it.readyAt = DebridReady, now
			fire := !it.readyFired
			it.readyFired = true
			hooks := slices.Clone(e.debridHooks)
			e.mu.Unlock()
			if fire {
				e.debridBecameReady(it, info, hooks)
			}
			e.runDebridLocal(ctx, it, info)
			return
		}
		wait := it.pollInterval(now)
		e.mu.Unlock()
		if !debridSleep(ctx, it, wait) {
			return
		}
	}
}

// debridBecameReady runs the ready hooks once per item when the service has
// every selected file, just before the local phase (J3.3) starts.
func (e *Engine) debridBecameReady(it *debridItem, info debrid.RemoteInfo, hooks []func(DebridReadyEvent)) {
	ev := DebridReadyEvent{Hash: it.hash, Info: info}
	for _, fn := range hooks {
		func() {
			defer func() { _ = recover() }() // a hook must never crash the engine
			fn(ev)
		}()
	}
}

// pollInterval is the wait before the next Info call. Caller holds e.mu.
func (it *debridItem) pollInterval(now time.Time) time.Duration {
	switch {
	case it.info.Status == debrid.Converting:
		return debridTuning.fastPoll
	case it.phase == DebridCloud && (!it.selectSent || now.Sub(it.selectAt) < debridReadyWindow):
		return debridTuning.fastPoll
	case it.info.Status == debrid.Queued:
		return debridTuning.queuedPoll
	default:
		return debridTuning.downloadPoll
	}
}

// failDebridLocked ends the item with a short note: err for a refused call,
// or the remote status's reason when err is nil. Caller holds e.mu.
func (e *Engine) failDebridLocked(it *debridItem, err error) {
	it.phase, it.cancel = DebridFailed, nil
	if err == nil {
		reason := it.info.Reason
		if reason == "" {
			reason = "the service gave up on this torrent"
		}
		err = errors.New(reason)
		it.note = "☁ " + it.short + " " + reason
	} else {
		text := debrid.Message(err)
		if i := strings.Index(text, ": "); i >= 0 {
			text = text[i+2:]
		}
		it.note = "☁ " + it.short + " " + text
	}
	it.err = err
}

// transientDebridErr reports errors worth polling through with back-off.
func transientDebridErr(err error) bool {
	return errors.Is(err, debrid.ErrUnavailable) || errors.Is(err, debrid.ErrRateLimited)
}

// debridRetryDelay is the back-off before retry number n (1-based).
func debridRetryDelay(n int) time.Duration {
	d := debridTuning.retryBase << min(max(n-1, 0), 6)
	return min(d, debridTuning.retryMax)
}

// debridSleep waits d, returning early (true) when StartDebrid wakes the
// poller, and false when ctx ends.
func debridSleep(ctx context.Context, it *debridItem, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-it.wake:
		return true
	}
}

func (it *debridItem) displayName() string {
	if it.name != "" {
		return it.name
	}
	if it.info.Name != "" {
		return it.info.Name
	}
	return it.hash.HexString()
}

// remoteVisible reports whether the row shows the cloud's progress: the ready
// window after the selection has passed without the files being ready.
func (it *debridItem) remoteVisible(now time.Time) bool {
	if !it.selectSent || it.selectAt.IsZero() {
		return false
	}
	end := now
	if !it.readyAt.IsZero() {
		end = it.readyAt
	}
	return end.Sub(it.selectAt) >= debridReadyWindow
}

// debridSnapshot renders a debrid item in the shared Snapshot shape. Caller
// holds e.mu.
func (e *Engine) debridSnapshot(h metainfo.Hash, it *debridItem, now time.Time) Snapshot {
	s := Snapshot{
		Hash: h, Name: it.displayName(), Magnet: it.magnet,
		DownloadDir: it.downloadDir, DataPath: it.dataPath, Via: it.via,
	}
	if !it.infoAt.IsZero() {
		s.Remote = &RemoteProgress{
			Status: it.info.Status, Progress: it.info.Progress,
			SpeedBps: it.info.SpeedBps, Seeders: it.info.Seeders, Reason: it.info.Reason,
		}
	}
	switch it.phase {
	case DebridAdding:
		s.State = StateFetchingMeta
		s.Note = "☁ adding to " + it.short
	case DebridPreview:
		if it.infoAt.IsZero() || it.info.Status == debrid.Converting {
			s.State = StateFetchingMeta
			s.Note = "☁ " + it.short + " reading the torrent"
		} else {
			s.State = StatePreviewing
		}
	case DebridCloud, DebridReady:
		if !it.remoteVisible(now) {
			s.State = StateFetchingMeta // "starting", like a torrent connecting
			return s
		}
		s.State = StateRemote
		progress := it.info.Progress
		if it.phase == DebridReady {
			progress = 100
		}
		s.Length = it.info.Bytes
		s.BytesCompleted = int64(float64(s.Length) * min(max(progress, 0), 100) / 100)
		s.Seeders = it.info.Seeders
		s.SpeedBps = float64(it.info.SpeedBps)
		if s.SpeedBps > 0 && s.Length > s.BytesCompleted {
			s.ETA = time.Duration(float64(s.Length-s.BytesCompleted) / s.SpeedBps * float64(time.Second))
		}
		switch {
		case it.phase == DebridReady:
			s.Note = "☁ " + it.short + " ready"
		case it.info.Status == debrid.Queued || it.info.Status == debrid.WaitingSelection:
			s.Note = "☁ " + it.short + " queued"
		default:
			s.Note = fmt.Sprintf("☁ %s %d %%", it.short, int(progress))
		}
	case DebridFailed:
		s.State = StatePaused
		s.Note = it.note
	default: // local and done: the sum over the files
		e.debridLocalSnapshot(&s, it, now)
	}
	return s
}

// refuseDebridDuplicateLocked is the torrent side of the one-hash-one-kind rule.
// Caller holds e.mu.
func (e *Engine) refuseDebridDuplicateLocked(h metainfo.Hash) error {
	if _, ok := e.debrid[h]; ok {
		return ErrAlreadyDebrid
	}
	return nil
}

func (e *Engine) refuseDebridDuplicate(h metainfo.Hash) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refuseDebridDuplicateLocked(h)
}

// providerID turns a display name ("Real-Debrid") into a stable ID
// ("realdebrid") for Snapshot.Via and the saved state.
func providerID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// providerShort abbreviates a display name for notes: "Real-Debrid" → "RD".
func providerShort(name string) string {
	var b strings.Builder
	for _, word := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == ' ' || r == '_' }) {
		for _, r := range word {
			b.WriteRune(unicode.ToUpper(r))
			break
		}
	}
	if b.Len() < 2 {
		return name
	}
	return b.String()
}
