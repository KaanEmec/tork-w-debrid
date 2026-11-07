package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/kaanemec/tork-w-debrid/internal/debrid"
	"github.com/kaanemec/tork-w-debrid/internal/debrid/realdebrid"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/intake"
)

// The preview in debrid mode (roadmap J4.1). With a Real-Debrid account
// linked and "use for downloads" on, the preview never touches the swarm
// (README D3): the file list comes from the local metadata cache, the
// .torrent the user gave, or Real-Debrid itself, in that order. The torrent
// goes to Real-Debrid in the background as soon as the preview opens, so the
// service is usually done reading it by the time the user has chosen files.

// previewSourceKind is what the user asked to preview.
type previewSourceKind int

const (
	sourceMagnet previewSourceKind = iota
	sourceTorrentURL
	sourceTorrentFile
)

// previewSource is kept on the preview so `t` can reopen the same input as
// a normal torrent.
type previewSource struct {
	kind  previewSourceKind
	value string // magnet, URL or path
	name  string
}

// fileListSource is where a preview's file list came from.
type fileListSource int

const (
	listFromSwarm   fileListSource = iota // the torrent client (normal preview)
	listFromCache                         // ~/.tork/metainfo
	listFromTorrent                       // the .torrent file or URL the user gave
	listFromRemote                        // Real-Debrid's own file list
)

// provenance is the muted line above the footer in debrid mode.
func (s fileListSource) provenance() string {
	switch s {
	case listFromCache:
		return "file list from the metadata cache · no swarm contacted"
	case listFromTorrent:
		return "file list from the .torrent file · no swarm contacted"
	case listFromRemote:
		return "file list from Real-Debrid · no swarm contacted"
	}
	return ""
}

// debridPreviewOpenMsg opens a debrid-mode preview. files is set when the
// list came from the cache or a .torrent; otherwise the preview waits for
// Real-Debrid's list.
type debridPreviewOpenMsg struct {
	gen     uint64
	src     previewSource
	from    screen
	hash    metainfo.Hash
	name    string
	data    []byte // .torrent bytes to send, or nil to send the magnet
	files   []engine.FileInfo
	list    fileListSource
	problem string // the link can't be used; nothing was sent
	err     error
}

// debridPreviewAddedMsg reports the background add to Real-Debrid.
type debridPreviewAddedMsg struct {
	gen    uint64
	hash   metainfo.Hash
	magnet string
	owned  bool
	err    error
}

const debridAddTimeout = time.Minute

// debridPreviewRoute decides whether a new preview goes through Real-Debrid.
// problem is set when an account is linked but can't be used right now (it
// needs a new login): the preview then explains that and offers `t` instead
// of quietly falling back to the swarm.
func (a *App) debridPreviewRoute() (on bool, problem string) {
	if a.cfg == nil || a.eng == nil || a.cfg.Dir() == "" || !a.cfg.Debrid.Enabled {
		return false, ""
	}
	if debrid.Linked(a.cfg) {
		return true, ""
	}
	l, err := debrid.NewStore(a.cfg).Load()
	switch {
	case errors.Is(err, debrid.ErrNotLinked):
		return false, ""
	case err != nil:
		return true, "Real-Debrid: the saved link can't be used (" + err.Error() + ")"
	case !l.Usable():
		return true, "Real-Debrid: the account needs a new login · link it again in Settings"
	}
	return false, "" // linked to another provider
}

// ensureDebridProvider gives the engine a Real-Debrid client the first time
// a debrid preview needs one. A provider set elsewhere (tests, a resumed
// session) is kept.
func (a *App) ensureDebridProvider() {
	if a.eng.DebridProvider() != nil {
		return
	}
	client, _ := realdebrid.NewFromConfig(a.cfg, realdebrid.Endpoints{})
	a.eng.SetDebridProvider(client)
}

// launchDebridPreview opens src in debrid mode, or reports false when
// downloads don't go through Real-Debrid.
func (a *App) launchDebridPreview(src previewSource, from screen) (tea.Cmd, bool) {
	on, problem := a.debridPreviewRoute()
	if !on {
		return nil, false
	}
	a.previewSeq++
	gen := a.previewSeq
	if problem != "" {
		msg := debridPreviewOpenMsg{gen: gen, src: src, from: from, name: src.name, problem: problem}
		return func() tea.Msg { return msg }, true
	}
	a.ensureDebridProvider()
	eng := a.eng
	return func() (msg tea.Msg) {
		defer guard(&msg, func(r any) tea.Msg {
			return debridPreviewOpenMsg{gen: gen, src: src, from: from, err: fmt.Errorf("preview panicked: %v", r)}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m := readDebridPreviewSource(ctx, eng, src)
		m.gen, m.src, m.from = gen, src, from
		return m
	}, true
}

// readDebridPreviewSource finds the torrent's file list without the swarm:
// the metadata cache for a magnet, or the .torrent itself.
func readDebridPreviewSource(ctx context.Context, eng *engine.Engine, src previewSource) debridPreviewOpenMsg {
	m := debridPreviewOpenMsg{name: src.name}
	var data []byte
	switch src.kind {
	case sourceMagnet:
		mag, err := metainfo.ParseMagnetUri(src.value)
		if err != nil {
			m.err = err
			return m
		}
		m.hash = mag.InfoHash
		cached, ok := eng.CachedTorrent(m.hash)
		if !ok {
			return m // Real-Debrid will read it
		}
		data, m.list = cached, listFromCache
	case sourceTorrentURL:
		fetched, err := eng.FetchTorrent(ctx, src.value)
		if err != nil {
			m.err = fmt.Errorf("fetch torrent: %w", err)
			return m
		}
		data, m.list = fetched, listFromTorrent
	case sourceTorrentFile:
		read, err := readTorrentFile(src.value)
		if err != nil {
			m.err = err
			return m
		}
		data, m.list = read, listFromTorrent
	}
	h, name, files, err := torrentFileList(data)
	if err != nil {
		if src.kind == sourceMagnet {
			m.list = listFromSwarm // a bad cache entry: let Real-Debrid read the magnet
			return m
		}
		m.err = fmt.Errorf("read torrent: %w", err)
		return m
	}
	if src.kind == sourceMagnet && h != m.hash {
		m.list = listFromSwarm
		return m
	}
	m.hash, m.data, m.files = h, data, files
	if src.kind != sourceMagnet && name != "" {
		m.name = name
	}
	return m
}

// readTorrentFile reads a local .torrent with the engine's size bound.
func readTorrentFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open torrent: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > intake.MaxTorrentBytes {
		return nil, errors.New("torrent file must be a regular file no larger than 16 MiB")
	}
	return io.ReadAll(io.LimitReader(f, intake.MaxTorrentBytes))
}

// torrentFileList reads a .torrent's files the way the torrent client names
// them ("<torrent name>/<path>", or just the name for a single file), so the
// tree, junk rules and risk flags behave as in a normal preview. BEP 47
// padding files are left out: they hold no data and Real-Debrid never lists
// them.
func torrentFileList(data []byte) (h metainfo.Hash, name string, files []engine.FileInfo, err error) {
	if len(data) == 0 {
		return h, "", nil, errors.New("empty torrent")
	}
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return h, "", nil, err
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return h, "", nil, err
	}
	for i, fi := range info.UpvertedFiles() {
		if strings.Contains(fi.Attr, "p") {
			continue
		}
		path := strings.Join(append([]string{info.BestName()}, fi.BestPath()...), "/")
		files = append(files, engine.FileInfo{Index: i, Path: path, Length: fi.Length})
	}
	if len(files) == 0 {
		return h, "", nil, errors.New("torrent has no files")
	}
	return mi.HashInfoBytes(), info.BestName(), files, nil
}

// remoteFileList turns Real-Debrid's files into preview files. Real-Debrid
// leaves the torrent's folder out of its paths, so a multi-file list gets
// root put back in front, as the torrent client shows it. ids maps each
// preview index to its Real-Debrid file ID; it is never index+1.
func remoteFileList(root string, remote []debrid.RemoteFile) (files []engine.FileInfo, ids map[int]int) {
	ids = make(map[int]int, len(remote))
	for i, rf := range remote {
		path := strings.TrimLeft(rf.Path, "/")
		if len(remote) > 1 && root != "" {
			path = root + "/" + path
		}
		files = append(files, engine.FileInfo{Index: i, Path: path, Length: rf.Bytes})
		ids[i] = rf.ID
	}
	return files, ids
}

// onDebridPreviewOpen shows the preview and sends the torrent to
// Real-Debrid in the background.
func (a *App) onDebridPreviewOpen(msg debridPreviewOpenMsg) tea.Cmd {
	if msg.gen != a.previewSeq {
		return nil // a newer preview replaced this one
	}
	if msg.err != nil {
		return a.showError("preview failed: " + msg.err.Error())
	}
	magnet := ""
	if msg.src.kind == sourceMagnet {
		magnet = msg.src.value
	}
	p := newPreviewModel(msg.hash, magnet, msg.name, msg.from, false)
	p.debrid, p.src, p.gen = true, msg.src, msg.gen
	a.preview = p
	a.screen = screenPreview
	if msg.problem != "" {
		a.preview.debridErr = msg.problem
		return nil
	}
	if msg.files != nil {
		a.preview.setFiles(msg.files, msg.list)
	}
	return tea.Batch(a.debridAddCmd(msg.gen, msg.src, msg.data), a.ensureTick())
}

// debridAddCmd adds the torrent to Real-Debrid: the .torrent bytes when we
// have them (the service then knows the files at once), else the magnet.
func (a *App) debridAddCmd(gen uint64, src previewSource, data []byte) tea.Cmd {
	eng := a.eng
	return func() (msg tea.Msg) {
		defer guard(&msg, func(r any) tea.Msg {
			return debridPreviewAddedMsg{gen: gen, err: fmt.Errorf("add panicked: %v", r)}
		})
		ctx, cancel := context.WithTimeout(context.Background(), debridAddTimeout)
		defer cancel()
		if data != nil {
			h, _, magnet, owned, err := eng.AddDebridTorrentForPreview(ctx, data)
			return debridPreviewAddedMsg{gen: gen, hash: h, magnet: magnet, owned: owned, err: err}
		}
		h, owned, err := eng.AddDebridForPreview(ctx, src.value)
		return debridPreviewAddedMsg{gen: gen, hash: h, magnet: src.value, owned: owned, err: err}
	}
}

func (a *App) onDebridPreviewAdded(msg debridPreviewAddedMsg) tea.Cmd {
	p := &a.preview
	if a.screen != screenPreview || !p.debrid || p.gen != msg.gen {
		// The user left this preview while the add was in flight: leave
		// nothing behind on Real-Debrid.
		if msg.err == nil && msg.owned {
			a.eng.CancelDebridPreview(msg.hash)
		}
		return nil
	}
	if msg.err != nil {
		p.debridErr = debridProblem(msg.err)
		p.startPending = false
		return nil
	}
	p.hash, p.owned, p.added = msg.hash, msg.owned, true
	if msg.magnet != "" {
		p.magnet = msg.magnet
	}
	p.refresh(a.eng)
	if p.startPending {
		return a.startPreviewDownload()
	}
	return nil
}

// debridProblem is the preview's one-line reason for a refused torrent.
func debridProblem(err error) string {
	text := debrid.Message(err)
	if !strings.Contains(text, ": ") {
		text = "Real-Debrid: " + text
	}
	return text
}

// refreshDebrid follows the Real-Debrid item: its file list once the
// service has read the torrent, and a refusal while converting.
func (p *previewModel) refreshDebrid(eng *engine.Engine) {
	if !p.added || p.debridErr != "" {
		return
	}
	item, ok := eng.DebridItem(p.hash)
	if !ok {
		p.debridErr = "Real-Debrid: the torrent is no longer there"
		p.startPending = false
		return
	}
	if p.saveDir == "" {
		p.saveDir = item.DownloadDir
	}
	if item.Phase == engine.DebridFailed {
		err := item.Err
		if err == nil {
			err = errors.New("the service gave up on this torrent")
		}
		p.debridErr = debridProblem(err)
		p.startPending = false
		return
	}
	if p.ready {
		return
	}
	remote, ok := eng.DebridFiles(p.hash)
	if !ok {
		return
	}
	files, ids := remoteFileList(item.Remote.Name, remote)
	p.setFiles(files, listFromRemote)
	p.remoteIDs = ids
}

// cancelDebridPreview leaves nothing on Real-Debrid for a preview the user
// closed. An item that existed before this preview (owned=false) is kept.
func (a *App) cancelDebridPreview() {
	p := &a.preview
	var zero metainfo.Hash
	switch {
	case p.added && p.owned:
		a.eng.CancelDebridPreview(p.hash)
	case !p.added && p.hash != zero:
		// Still adding: the engine drops the placeholder and deletes the
		// remote copy when the add returns. A late add result is cancelled
		// by onDebridPreviewAdded.
		if item, ok := a.eng.DebridItem(p.hash); ok && item.Phase == engine.DebridAdding {
			a.eng.CancelDebridPreview(p.hash)
		}
	}
	p.startPending = false
}

// fallBackToTorrent is `t` on a refused debrid preview: the user chose to
// fetch it from the swarm instead, so open today's torrent preview.
func (a *App) fallBackToTorrent() tea.Cmd {
	p := &a.preview
	a.cancelDebridPreview()
	src, from := p.src, p.from
	a.screen = from
	return a.torrentPreviewCmd(src, from)
}

// startDebridPreviewDownload is enter in debrid mode. It never waits on the
// network: while the add or Real-Debrid's file list is still on its way the
// start is parked (startPending) and finished from the tick.
func (a *App) startDebridPreviewDownload() tea.Cmd {
	p := &a.preview
	if p.debridErr != "" {
		return nil
	}
	if !p.added {
		p.startPending = true
		return a.ensureTick()
	}
	ids, ok, cmd := a.debridSelection()
	if !ok {
		return cmd
	}
	p.startPending = false
	// StartDebrid opens the 5-second ready window in the engine (J4.3); the
	// tick shows its outcome. The state entry (Via, RemoteID, RemoteFiles,
	// Phase) comes from the engine and is kept current by
	// applySnapshotToEntry (J3.4).
	dir := ""
	if a.cfg != nil {
		dir = a.cfg.DownloadDir
	}
	if err := a.eng.StartDebrid(p.hash, ids, engine.DebridOptions{DownloadDir: dir}); err != nil {
		return a.showError(debridProblem(err))
	}
	p.started = true
	h, name := p.hash, p.name
	a.leavePreview()
	return a.recordDebridStart(h, name)
}

// debridSelection turns the ticks into Real-Debrid file IDs (J4.2). nil
// means every file, sent as files=all: shorter, and nothing to get wrong. A
// list from Real-Debrid carries its IDs; a list from the cache or a .torrent
// is matched with mapSelection once Real-Debrid's list is there. ok is false
// when the start has to wait for that list, or when a file could not be
// matched and the preview was rebuilt from Real-Debrid's list.
func (a *App) debridSelection() (ids []int, ok bool, cmd tea.Cmd) {
	p := &a.preview
	if !p.ready || len(p.excluded) == 0 {
		return nil, true, nil
	}
	if p.list == listFromRemote {
		for _, f := range p.files {
			if !p.excluded[f.Index] {
				ids = append(ids, p.remoteIDs[f.Index])
			}
		}
		sort.Ints(ids)
		return ids, true, nil
	}
	remote, ready := a.eng.DebridFiles(p.hash)
	if !ready {
		p.startPending = true
		return nil, false, a.ensureTick()
	}
	p.startPending = false
	ids, err := mapSelection(p.files, remote, p.excluded)
	if err != nil {
		return nil, false, a.adoptRemoteList(remote)
	}
	return ids, true, nil
}

// adoptRemoteList rebuilds the preview from Real-Debrid's list when the
// local list can't be matched to it, carrying the ticks over by path, and
// asks the user to look again rather than guess.
func (a *App) adoptRemoteList(remote []debrid.RemoteFile) tea.Cmd {
	p := &a.preview
	root := ""
	if item, ok := a.eng.DebridItem(p.hash); ok {
		root = item.Remote.Name
	}
	excludedPaths := map[string]bool{}
	keptPaths := map[string]bool{}
	for _, f := range p.files {
		key := remotePathKey(f.Path)
		if p.excluded[f.Index] {
			excludedPaths[key] = true
		} else {
			keptPaths[key] = true
		}
	}
	files, ids := remoteFileList(root, remote)
	p.setFiles(files, listFromRemote)
	p.remoteIDs = ids
	for _, f := range files {
		key := strings.TrimLeft(remote[f.Index].Path, "/")
		switch {
		case excludedPaths[key]:
			p.excluded[f.Index] = true
		case keptPaths[key]:
			delete(p.excluded, f.Index)
		}
	}
	p.tree = buildPreviewTree(p.files, p.excluded)
	p.rebuildRows()
	p.rebuildStats()
	p.win.clamp(len(p.rows), a.previewRows())
	p.notice = "file list updated from Real-Debrid; check your selection"
	return a.showToast(p.notice, toastWarn, toastLong)
}

// remotePathKey is a torrent-client path as Real-Debrid writes it: without
// the torrent's folder.
func remotePathKey(path string) string {
	path = strings.TrimLeft(strings.ReplaceAll(path, "\\", "/"), "/")
	if i := strings.Index(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// resumePendingDebridStart finishes a parked start once the add and, when
// needed, Real-Debrid's file list have arrived. Called from the tick.
func (a *App) resumePendingDebridStart() tea.Cmd {
	p := &a.preview
	if a.screen != screenPreview || !p.debrid || !p.startPending || !p.added || p.debridErr != "" {
		return nil
	}
	return a.startPreviewDownload()
}

// viewDebridWaiting is the preview while Real-Debrid reads the torrent.
func (a *App) viewDebridWaiting() string {
	p := &a.preview
	width := a.contentWidth()
	waited := max(time.Since(p.startedAt).Round(time.Second), 0)
	detail := styleDim.Render("no swarm contacted · cached torrents take about a second")
	if waited >= 10*time.Second {
		detail = styleDim.Render("still reading; press enter to download all files now")
	}
	status := "Real-Debrid is reading the torrent…"
	if p.startPending {
		status = "all files chosen · waiting for Real-Debrid…"
	}
	msg := lipgloss.JoinVertical(lipgloss.Center,
		styleFg.Bold(true).Render(truncate(p.name, width-4)),
		"",
		spinnerGlyphIn(a.vframe(), styleRoute)+styleDim.Render(" "+status),
		detail,
		styleDim.Render("save to "+truncatePathLeft(tildePath(a.previewDestination()), width-4-lipgloss.Width("save to   ·  via Real-Debrid"))+"  ·  ")+styleRoute.Render("via Real-Debrid"),
	)
	body := lipgloss.Place(width, a.bodyHeight(), lipgloss.Center, lipgloss.Center, msg)
	return a.chrome(a.previewContext(), body, hints(hint("enter", "download all now"), hint("esc", "cancel")))
}

// viewDebridProblem explains why Real-Debrid can't take this torrent and
// offers the swarm, which the user has to choose explicitly.
func (a *App) viewDebridProblem() string {
	p := &a.preview
	width := a.contentWidth()
	msg := lipgloss.JoinVertical(lipgloss.Center,
		styleFg.Bold(true).Render(truncate(p.name, width-4)),
		"",
		styleErr.Render(truncate(p.debridErr, width-4)),
		"",
		styleDim.Render(truncate("t downloads it as a normal torrent instead; peers will see your IP", width-4)),
	)
	body := lipgloss.Place(width, a.bodyHeight(), lipgloss.Center, lipgloss.Center, msg)
	return a.chrome(a.previewContext(), body, hints(hint("t", "normal torrent"), hint("esc", "back")))
}

// debridFootnote is the line above the footer in a ready debrid preview: a
// pending start, a rebuilt list, or where the list came from.
func (a *App) debridFootnote() string {
	p := &a.preview
	switch {
	case p.startPending && !p.added:
		return spinnerGlyphIn(a.vframe(), styleRoute) + styleDim.Render(" sending to Real-Debrid…")
	case p.startPending:
		return spinnerGlyphIn(a.vframe(), styleRoute) + styleDim.Render(" waiting for Real-Debrid's file list…")
	case p.notice != "":
		return styleWarn.Render(p.notice)
	}
	return styleDim.Render(p.list.provenance())
}
