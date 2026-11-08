package engine

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// directManifestVersion 2 keys the manifest by resume key (roadmap J7.2), so
// a fresh link for the same file keeps every byte already downloaded.
// Version 1 manifests, keyed by URL, are still read.
const directManifestVersion = 2

type byteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // inclusive
}

type directManifest struct {
	Version   int         `json:"version"`
	URL       string      `json:"url"`
	ResumeKey string      `json:"resume_key,omitempty"`
	Total     int64       `json:"total"`
	Validator string      `json:"validator,omitempty"`
	Completed []byteRange `json:"completed,omitempty"`
}

// directIdentity is what a saved manifest must match to be resumed.
type directIdentity struct {
	key       string // the resume key, or the URL for ordinary downloads
	url       string // the URL in use now
	keyed     bool   // key is a caller-supplied ResumeKey
	total     int64
	validator string
}

type directProtocolError struct{ error }

// directProbe is what the 0-0 range probe learned about the remote file.
type directProbe struct {
	total     int64
	validator string
	url       string
}

// runDirectParallel downloads over several connections when the server
// supports ranges and the file can be identified (checksum, validator, or a
// known size that the server confirms). It reports false when the caller
// should fall back to a single connection.
func (e *Engine) runDirectParallel(ctx context.Context, it *directItem, dest string, member *budgetMember) (bool, error) {
	probe, ok, err := e.discoverDirectRanges(ctx, it, member)
	if err != nil {
		return true, err
	}
	if !ok {
		return false, nil
	}
	total, validator := probe.total, probe.validator
	if it.expectedSize > 0 && total != it.expectedSize {
		return true, fmt.Errorf("server reported %d bytes, expected %d", total, it.expectedSize)
	}
	knownSize := it.expectedSize > 0 && total == it.expectedSize
	if total < 2*e.directMinChunkSize || (it.checksum.Empty() && validator == "" && !knownSize) {
		return false, nil
	}
	part, meta := dest+".part", dest+".part.meta"
	if fi, err := os.Lstat(part); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular()) {
		return true, errors.New("partial download path is not a regular file; refusing to write")
	}
	if fi, err := os.Lstat(meta); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular()) {
		return true, errors.New("direct-download metadata path is not a regular file; refusing to read it")
	} else if err != nil && !os.IsNotExist(err) {
		return true, fmt.Errorf("inspect direct-download metadata: %w", err)
	}
	id := directIdentity{key: it.key(), url: probe.url, keyed: it.resumeKey != "", total: total, validator: validator}
	manifest, err := loadDirectManifest(meta, id)
	if err != nil {
		_ = os.Remove(meta)
		_ = os.Remove(part)
		manifest = directManifest{}
	}
	if manifest.Version == 0 {
		manifest = directManifest{}
		if fi, statErr := os.Stat(part); statErr == nil && fi.Size() > 0 && fi.Size() < total {
			manifest.Completed = []byteRange{{Start: 0, End: fi.Size() - 1}}
		}
	}
	manifest.Version, manifest.URL, manifest.ResumeKey = directManifestVersion, probe.url, it.resumeKey
	manifest.Total, manifest.Validator = total, validator
	if err := saveDirectManifest(meta, manifest); err != nil {
		return true, err
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return true, err
	}
	if err := f.Truncate(total); err != nil {
		f.Close()
		return true, err
	}

	run := newSegmentRun(total, e.directMinChunkSize, manifest.Completed, e.directWant(it))
	run.validator, run.validatorURL = validator, probe.url
	if it.budget != nil && member != nil {
		run.budget, run.member = it.budget, member
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	run.cancel = cancel
	e.mu.Lock()
	it.done, it.length, it.seg = rangeBytes(manifest.Completed), total, run
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		if it.seg == run {
			it.seg = nil
		}
		e.mu.Unlock()
	}()

	// The checkpointer saves partly done ranges whenever directTuning.
	// checkpointBytes of new data arrived or a worker stopped. It snapshots
	// the ranges before syncing, so the manifest never lists unsynced bytes.
	var saveMu sync.Mutex
	checkpoint := func() error {
		ranges := run.doneRanges()
		if err := f.Sync(); err != nil {
			return err
		}
		saveMu.Lock()
		defer saveMu.Unlock()
		manifest.Completed = ranges
		return saveDirectManifest(meta, manifest)
	}
	checkpointDone := make(chan struct{})
	go func() {
		defer close(checkpointDone)
		for {
			select {
			case <-run.kick:
				if err := checkpoint(); err != nil {
					run.fail(directFatalError{err})
				}
			case <-workCtx.Done():
				return
			}
		}
	}()

	var workers sync.WaitGroup
	e.superviseSegments(workCtx, it, run, f, &workers)
	workers.Wait()
	cancel()
	<-checkpointDone

	runErr := run.failure()
	var protocol directProtocolError
	if errors.As(runErr, &protocol) {
		f.Close()
		_ = os.Remove(part)
		_ = os.Remove(meta)
		return false, nil
	}
	saveErr := checkpoint()
	closeErr := f.Close()
	switch {
	case runErr != nil:
		return true, runErr
	case ctx.Err() != nil:
		return true, ctx.Err()
	case saveErr != nil:
		return true, saveErr
	case closeErr != nil:
		return true, closeErr
	case rangeBytes(manifest.Completed) != total:
		return true, errors.New("segmented download ended with missing bytes")
	}
	if err := finalizeParallelDirect(it, part, dest); err != nil {
		_ = os.Remove(meta)
		e.mu.Lock()
		it.done = 0
		e.mu.Unlock()
		return true, err
	}
	_ = os.Remove(meta)
	e.mu.Lock()
	it.done, it.length, it.state, it.cancel = total, total, StateDone, nil
	if it.state == StateDone {
		it.note = ""
	}
	e.mu.Unlock()
	return true, nil
}

// superviseSegments starts workers while there is work and the connection
// limits allow, samples speeds, runs the slow-connection check, and returns
// once every worker has stopped and nothing is left (or the run ended).
func (e *Engine) superviseSegments(ctx context.Context, it *directItem, run *segmentRun, f *os.File, workers *sync.WaitGroup) {
	ticker := time.NewTicker(directTuning.tick)
	defer ticker.Stop()
	lastHealth := time.Now()
	for {
		run.setWant(e.directWant(it), time.Now())
		for ctx.Err() == nil && run.spawnable() {
			if run.budget != nil && !run.budget.tryAcquire(run.member) {
				break
			}
			w := run.addWorker()
			workers.Add(1)
			go e.segmentWorker(ctx, it, run, w, f, workers)
		}
		if ctx.Err() != nil || run.finished() {
			return
		}
		select {
		case now := <-ticker.C:
			run.sample(now)
			if now.Sub(lastHealth) >= directTuning.healthInterval {
				run.healthCheck(now)
				lastHealth = now
			}
		case <-run.events:
		case <-ctx.Done():
		}
	}
}

func (e *Engine) segmentWorker(ctx context.Context, it *directItem, run *segmentRun, w *segWorker, f *os.File, workers *sync.WaitGroup) {
	defer workers.Done()
	for {
		_, wait, ok := run.claim(w, time.Now())
		if !ok {
			return
		}
		if wait > 0 {
			if !sleepCtx(ctx, min(wait, directTuning.tick)) {
				run.exit(w)
				return
			}
			continue
		}
		gen, err := e.fetchSegment(ctx, it, run, w, f)
		if err == nil {
			run.release(w, false, 0, time.Now())
			continue
		}
		if ctx.Err() != nil {
			run.release(w, false, 0, time.Now())
			run.exit(w)
			return
		}
		if !e.recoverSegment(ctx, it, run, w, gen, err) {
			run.exit(w)
			return
		}
		signal(run.kick)
	}
}

// recoverSegment handles one failed attempt (roadmap J7.3). It reports false
// when the whole transfer has failed.
func (e *Engine) recoverSegment(ctx context.Context, it *directItem, run *segmentRun, w *segWorker, gen int, err error) bool {
	now := time.Now()
	kind := classifyDirectErr(err)
	switch kind {
	case errKindRestart:
		run.release(w, false, 0, now)
		return true
	case errKindProtocol, errKindFatal:
		run.release(w, false, 0, now)
		run.fail(err)
		return false
	case errKindExpired:
		if e.directCanRefresh(it) {
			run.release(w, false, 0, now)
			if rerr := e.refreshDirectURL(ctx, it, gen); rerr != nil {
				if ctx.Err() == nil {
					run.fail(directFatalError{rerr})
				}
				return false
			}
			return true
		}
	}
	var minDelay time.Duration
	var status directStatusError
	if kind == errKindBusy && errors.As(err, &status) {
		minDelay = status.retryAfter
		e.setDirectNote(it, busyNote(run.busy(status.retryAfter, now)))
	}
	tries := run.release(w, true, minDelay, now)
	if tries >= directTuning.maxTries {
		run.fail(giveUpErr(err, "a segment failed %d times - %s", tries, lastErrPart(err)))
		return false
	}
	if run.streak() >= 2*directTuning.maxTries {
		run.fail(giveUpErr(err, "every connection keeps failing - %s", lastErrPart(err)))
		return false
	}
	if kind != errKindBusy {
		e.setDirectNote(it, fmt.Sprintf("reconnecting (%d/%d)", tries+1, directTuning.maxTries))
	}
	return true
}

func busyNote(connections int) string {
	if connections == 1 {
		return "server busy, using 1 connection"
	}
	return fmt.Sprintf("server busy, using %d connections", connections)
}

// lastErrPart is the innermost part of an error message, without the
// "op url: " prefixes net/http adds.
func lastErrPart(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	return s
}

// discoverDirectRanges probes for range support. A link that has expired is
// refreshed first, and a keyed (debrid) download retries a busy or failing
// probe rather than dropping to one connection.
func (e *Engine) discoverDirectRanges(ctx context.Context, it *directItem, member *budgetMember) (directProbe, bool, error) {
	if it.budget != nil && member != nil {
		if err := it.budget.acquire(ctx, member); err != nil {
			return directProbe{}, false, err
		}
		defer it.budget.release(member)
	}
	for tries := 1; ; tries++ {
		link, gen, err := e.directURL(ctx, it)
		if err != nil {
			return directProbe{}, false, err
		}
		probe, err := e.probeDirectRanges(ctx, it, link)
		if err == nil {
			return probe, true, nil
		}
		if ctx.Err() != nil {
			return directProbe{}, false, ctx.Err()
		}
		var status directStatusError
		switch classifyDirectErr(err) {
		case errKindExpired:
			if e.directCanRefresh(it) {
				if rerr := e.refreshDirectURL(ctx, it, gen); rerr != nil {
					return directProbe{}, false, rerr
				}
				continue
			}
		case errKindBusy, errKindRetry:
			if it.resumeKey != "" && tries < directTuning.maxTries {
				delay := directBackoff(tries)
				if errors.As(err, &status) {
					delay = max(delay, status.retryAfter)
				}
				e.setDirectNote(it, fmt.Sprintf("reconnecting (%d/%d)", tries+1, directTuning.maxTries))
				if !sleepCtx(ctx, delay) {
					return directProbe{}, false, ctx.Err()
				}
				continue
			}
		}
		return directProbe{}, false, nil
	}
}

func (e *Engine) probeDirectRanges(ctx context.Context, it *directItem, link string) (directProbe, error) {
	validator := ""
	head, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err == nil {
		head.Header.Set("User-Agent", directUserAgent)
		head.Header.Set("Accept-Encoding", "identity")
		if resp, doErr := e.doDirectRequest(head, it); doErr == nil {
			validator = responseValidator(resp)
			resp.Body.Close()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return directProbe{}, directFatalError{err}
	}
	req.Header.Set("User-Agent", directUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := e.doDirectRequest(req, it)
	if err != nil {
		return directProbe{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		return directProbe{}, directProtocolError{errors.New("server ignores range requests")}
	default:
		return directProbe{}, newDirectStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != 0 || end != 0 || total <= 0 {
		return directProbe{}, directProtocolError{errors.New("unusable Content-Range in probe")}
	}
	if got := responseValidator(resp); got != "" {
		if validator != "" && got != validator {
			return directProbe{}, directProtocolError{errors.New("validators disagree")}
		}
		validator = got
	}
	return directProbe{total: total, validator: validator, url: link}, nil
}

// fetchSegment streams w's range into the file until the range is done, its
// end moves below the data, or the attempt fails. It returns the URL
// generation it used, for refreshDirectURL.
func (e *Engine) fetchSegment(ctx context.Context, it *directItem, run *segmentRun, w *segWorker, f *os.File) (int, error) {
	link, gen, err := e.directURL(ctx, it)
	if err != nil {
		return gen, err
	}
	start, end := run.attempt(w)
	if start > end {
		return gen, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return gen, directFatalError{err}
	}
	req.Header.Set("User-Agent", directUserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	validator := run.validatorFor(link)
	if validator != "" {
		req.Header.Set("If-Range", validator)
	}
	resp, err := e.doDirectRequest(req, it)
	if err != nil {
		return gen, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		return gen, directProtocolError{fmt.Errorf("range request returned %d", resp.StatusCode)}
	default:
		return gen, newDirectStatusError(resp)
	}
	gotStart, gotEnd, gotTotal, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || gotStart != start || gotEnd != end || gotTotal != run.total {
		return gen, directProtocolError{errors.New("range response did not match request")}
	}
	if got := responseValidator(resp); validator != "" && got != "" && got != validator {
		return gen, directProtocolError{errors.New("remote file changed during download")}
	}
	for {
		n, readErr := resp.Body.Read(w.buf)
		if n > 0 {
			off, k, restart := run.reserve(w, int64(n))
			if restart {
				return gen, errRestartSegment
			}
			if k == 0 {
				return gen, nil // the end moved below this data: another worker owns it
			}
			if _, err := f.WriteAt(w.buf[:k], off); err != nil {
				run.unreserve(w)
				return gen, directFatalError{err}
			}
			done, checkpoint, note, clear := run.commit(w, k)
			e.mu.Lock()
			it.done += k
			if clear && it.state == StateDownloading {
				it.note = note
			}
			e.mu.Unlock()
			if checkpoint {
				signal(run.kick)
			}
			if done {
				if off+k == end+1 && k == int64(n) {
					var extra [1]byte
					if m, err := resp.Body.Read(extra[:]); m != 0 || (err != nil && err != io.EOF) {
						return gen, directProtocolError{errors.New("range response exceeded requested length")}
					}
				}
				return gen, nil
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return gen, io.ErrUnexpectedEOF
			}
			return gen, readErr
		}
	}
}

func finalizeParallelDirect(it *directItem, part, dest string) error {
	if it.checksum.Empty() {
		// Nothing to compare against: reading the whole file back only kept
		// the row at 100% for seconds (about 10 s for a 4 GB debrid file).
		return os.Rename(part, dest)
	}
	f, err := os.Open(part)
	if err != nil {
		return err
	}
	h, err := it.checksum.newHash()
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !it.checksum.Empty() && hex.EncodeToString(h.Sum(nil)) != it.checksum.Hex {
		_ = os.Remove(part)
		return fmt.Errorf("%s checksum mismatch - data discarded, press p to retry", it.checksum.Label())
	}
	return os.Rename(part, dest)
}

func parseContentRange(raw string) (start, end, total int64, ok bool) {
	if !strings.HasPrefix(raw, "bytes ") {
		return
	}
	parts := strings.Split(strings.TrimPrefix(raw, "bytes "), "/")
	if len(parts) != 2 {
		return
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return
	}
	start, err1 := strconv.ParseInt(bounds[0], 10, 64)
	end, err2 := strconv.ParseInt(bounds[1], 10, 64)
	total, err3 := strconv.ParseInt(parts[1], 10, 64)
	ok = err1 == nil && err2 == nil && err3 == nil && start >= 0 && end >= start && end < total
	return
}

func responseValidator(resp *http.Response) string {
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}
	return strings.TrimSpace(resp.Header.Get("Last-Modified"))
}

func loadDirectManifest(path string, id directIdentity) (directManifest, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return directManifest{}, nil
	}
	if err != nil {
		return directManifest{}, err
	}
	var m directManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return directManifest{}, err
	}
	if !m.matches(id) {
		return directManifest{}, errors.New("stale direct manifest")
	}
	normal := normalizeRanges(m.Completed)
	if len(normal) != len(m.Completed) {
		return directManifest{}, errors.New("invalid direct manifest ranges")
	}
	for i, r := range normal {
		if r.Start < 0 || r.End < r.Start || r.End >= id.total || r != m.Completed[i] {
			return directManifest{}, errors.New("invalid direct manifest range")
		}
	}
	return m, nil
}

// matches reports whether a saved manifest describes the same remote file.
// A changed URL alone never disqualifies a keyed download: only the total
// size, or validators observed on the same URL, can.
func (m directManifest) matches(id directIdentity) bool {
	if m.Total != id.total {
		return false
	}
	switch m.Version {
	case 1:
		return m.URL == id.url && m.Validator == id.validator
	case directManifestVersion:
		key := m.ResumeKey
		if key == "" {
			key = m.URL
		}
		if key != id.key {
			return false
		}
		if id.keyed && m.URL != id.url {
			return true // validators from different links cannot be compared
		}
		return m.Validator == id.validator
	}
	return false
}

// readDirectManifestPrefix returns how many leading bytes of the .part file a
// saved manifest for this item vouches for, and whether the manifest belongs
// to it. The single-connection path uses it to resume after a segmented run.
func readDirectManifestPrefix(path string, it *directItem) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var m directManifest
	if json.Unmarshal(data, &m) != nil {
		return 0, false
	}
	key := m.ResumeKey
	if key == "" {
		key = m.URL
	}
	if (m.Version == 1 && m.URL != it.url) || (m.Version == directManifestVersion && key != it.key()) ||
		(m.Version != 1 && m.Version != directManifestVersion) {
		return 0, false
	}
	completed := normalizeRanges(m.Completed)
	if len(completed) == 0 || completed[0].Start != 0 || completed[0].End >= m.Total {
		return 0, true
	}
	return completed[0].End + 1, true
}

func saveDirectManifest(path string, m directManifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".direct-meta-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func normalizeRanges(in []byteRange) []byteRange {
	out := append([]byteRange(nil), in...)
	slices.SortFunc(out, func(a, b byteRange) int { return cmp.Compare(a.Start, b.Start) })
	merged := out[:0]
	for _, r := range out {
		if len(merged) == 0 || r.Start > merged[len(merged)-1].End+1 {
			merged = append(merged, r)
			continue
		}
		if r.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = r.End
		}
	}
	return merged
}

func missingDirectRanges(total int64, complete []byteRange, chunk int64) []byteRange {
	complete = normalizeRanges(complete)
	var missing []byteRange
	start := int64(0)
	for _, done := range complete {
		for start < done.Start {
			end := min(done.Start-1, start+chunk-1)
			missing = append(missing, byteRange{start, end})
			start = end + 1
		}
		start = max(start, done.End+1)
	}
	for start < total {
		end := min(total-1, start+chunk-1)
		missing = append(missing, byteRange{start, end})
		start = end + 1
	}
	return missing
}

func rangeBytes(ranges []byteRange) int64 {
	var n int64
	for _, r := range normalizeRanges(ranges) {
		n += r.End - r.Start + 1
	}
	return n
}
