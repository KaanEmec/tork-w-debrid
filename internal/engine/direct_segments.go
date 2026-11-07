package engine

import (
	"context"
	"slices"
	"sync"
	"time"
)

// segmentRun schedules the byte ranges of one segmented HTTPS transfer
// (roadmap J7.3 and J7.4). Every worker writes straight into the shared
// .part file, so the scheduler only tracks which bytes are finished, which
// are waiting, and which each connection is writing now:
//
//   - A first claim is at most total/connections bytes, so the opening split
//     is even.
//   - When nothing is left unclaimed, a free worker takes the upper half of
//     the running range with the most bytes still to go (work stealing). The
//     running worker's end moves down under the lock, and it stops there.
//   - A failed worker hands the rest of its range back with a retry time, so
//     the bytes it already wrote are kept and the others carry on.
//   - Every few seconds a connection far slower than the average is told to
//     reconnect, as it may get a faster route.
//
// Work stealing and the slow-connection restart follow Surge
// (https://github.com/SurgeDM/Surge, MIT License, Copyright (c) SurgeDM
// contributors); the code here is tork's own.
type segmentRun struct {
	mu        sync.Mutex
	total     int64
	minChunk  int64
	maxClaim  int64
	completed []byteRange // finished bytes, normalized, active prefixes excluded
	pending   []pendingRange
	workers   []*segWorker
	running   int // workers started and not yet exited

	want      int // configured connections
	throttle  int // connections taken away after 429/503
	limit     int // connections allowed now: max(1, want-throttle)
	lastBusy  time.Time
	holdUntil time.Time // no new request before this (Retry-After)

	unsaved    int64 // bytes written since the last checkpoint
	failStreak int   // failed attempts in a row with no byte written anywhere
	noteDirty  bool  // the row shows a transient note to replace on progress
	restarts   int

	validator    string // validator observed on validatorURL; "" once the link changed
	validatorURL string

	budget *ConnectionBudget
	member *budgetMember

	err    error
	cancel context.CancelFunc
	kick   chan struct{} // asks the checkpointer to save the manifest
	events chan struct{} // wakes the supervisor
}

type pendingRange struct {
	byteRange
	tries     int
	notBefore time.Time
}

type segWorker struct {
	exited       bool
	active       bool // streaming a claimed range
	waiting      bool // backing off after an error
	start        int64
	pos          int64 // next byte to write
	end          int64 // inclusive; may move down while active
	reserved     int64 // bytes [pos, pos+reserved) are being written now
	tries        int
	attemptStart int64
	restart      bool
	lastRestart  time.Time
	slow         int
	bytes        int64 // bytes written by this worker in total
	streamSince  time.Time
	speed        ring
	hist         []sample
	buf          []byte
}

func newSegmentRun(total, minChunk int64, completed []byteRange, want int) *segmentRun {
	want = max(want, 1)
	s := &segmentRun{
		total: total, minChunk: max(minChunk, 1),
		completed: normalizeRanges(completed),
		want:      want, limit: want,
		kick:   make(chan struct{}, 1),
		events: make(chan struct{}, 1),
	}
	s.maxClaim = max(s.minChunk, (total+int64(want)-1)/int64(want))
	start := int64(0)
	for _, done := range s.completed {
		if start < done.Start {
			s.pending = append(s.pending, pendingRange{byteRange: byteRange{Start: start, End: done.Start - 1}})
		}
		start = max(start, done.End+1)
	}
	if start < total {
		s.pending = append(s.pending, pendingRange{byteRange: byteRange{Start: start, End: total - 1}})
	}
	return s
}

func rangeLen(r byteRange) int64 { return r.End - r.Start + 1 }

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// addWorker registers a new worker. The caller has taken its budget slot.
func (s *segmentRun) addWorker() *segWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running++
	for i, w := range s.workers {
		if w.exited {
			s.workers[i] = &segWorker{buf: w.buf}
			return s.workers[i]
		}
	}
	w := &segWorker{buf: make([]byte, 128<<10)}
	s.workers = append(s.workers, w)
	return w
}

// exitLocked retires w and gives back its budget slot. Caller holds s.mu.
func (s *segmentRun) exitLocked(w *segWorker) {
	if w.exited {
		return
	}
	w.exited, w.active, w.waiting = true, false, false
	s.running--
	if s.budget != nil {
		s.budget.release(s.member)
	}
	signal(s.events)
}

func (s *segmentRun) exit(w *segWorker) {
	s.mu.Lock()
	s.exitLocked(w)
	s.mu.Unlock()
}

// claim gives w its next range. With wait > 0 the worker should sleep and ask
// again; with ok false it has been retired (no work, over its connection
// limit, or the transfer failed).
func (s *segmentRun) claim(w *segWorker, now time.Time) (r byteRange, wait time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.running > s.limit || (s.budget != nil && s.budget.overShare(s.member)) {
		s.exitLocked(w)
		return byteRange{}, 0, false
	}
	if now.Before(s.holdUntil) {
		return byteRange{}, s.holdUntil.Sub(now), true
	}
	best := -1
	var earliest time.Time
	for i, p := range s.pending {
		if p.notBefore.After(now) {
			if earliest.IsZero() || p.notBefore.Before(earliest) {
				earliest = p.notBefore
			}
			continue
		}
		if best < 0 || rangeLen(p.byteRange) > rangeLen(s.pending[best].byteRange) {
			best = i
		}
	}
	if best >= 0 {
		p := s.pending[best]
		r = p.byteRange
		if rangeLen(r) > s.maxClaim && rangeLen(r)-s.maxClaim >= s.minChunk {
			r.End = r.Start + s.maxClaim - 1
			s.pending[best].Start = r.End + 1
		} else {
			s.pending = slices.Delete(s.pending, best, best+1)
		}
		s.activateLocked(w, r, p.tries, now)
		return r, 0, true
	}
	if victim, left := s.largestRunningLocked(); victim != nil && left >= 2*s.minChunk {
		mid := victim.pos + victim.reserved + left/2
		r = byteRange{Start: mid, End: victim.end}
		victim.end = mid - 1
		s.activateLocked(w, r, 0, now)
		return r, 0, true
	}
	if !earliest.IsZero() {
		return byteRange{}, earliest.Sub(now), true
	}
	s.exitLocked(w)
	return byteRange{}, 0, false
}

// largestRunningLocked finds the running range with the most bytes still to
// go. Only ranges whose current attempt has delivered data count: splitting
// a request that has not answered yet (or keeps failing) would only spread
// its failures over more segments, each starting again at zero tries.
func (s *segmentRun) largestRunningLocked() (*segWorker, int64) {
	var victim *segWorker
	var most int64
	for _, o := range s.workers {
		if o.exited || !o.active || o.pos == o.attemptStart {
			continue
		}
		if left := o.end - (o.pos + o.reserved) + 1; left > most {
			victim, most = o, left
		}
	}
	return victim, most
}

func (s *segmentRun) activateLocked(w *segWorker, r byteRange, tries int, now time.Time) {
	w.active, w.waiting, w.restart = true, false, false
	w.start, w.pos, w.end, w.reserved = r.Start, r.Start, r.End, 0
	w.tries, w.attemptStart = tries, r.Start
	w.streamSince = now
	w.hist = w.hist[:0]
}

// attempt reports where w's next request starts and ends.
func (s *segmentRun) attempt(w *segWorker) (start, end int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.attemptStart = w.pos
	return w.pos, w.end
}

// reserve claims up to n bytes at w's position for writing. It returns the
// file offset and how many bytes still belong to w (0 once its end moved
// down to where it is), or restart when the slow-connection check asked w to
// reconnect.
func (s *segmentRun) reserve(w *segWorker, n int64) (offset, k int64, restart bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.restart {
		return 0, 0, true
	}
	k = max(min(n, w.end-w.pos+1), 0)
	w.reserved = k
	return w.pos, k, false
}

func (s *segmentRun) unreserve(w *segWorker) {
	s.mu.Lock()
	w.reserved = 0
	s.mu.Unlock()
}

// commit records k reserved bytes as written. It reports whether w reached
// its end, whether a checkpoint is due, and the note to show if the row's
// transient note should be replaced (clear is true then).
func (s *segmentRun) commit(w *segWorker, k int64) (done, checkpoint bool, note string, clear bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.pos += k
	w.reserved = 0
	w.bytes += k
	s.unsaved += k
	if k > 0 {
		s.failStreak = 0
	}
	if s.noteDirty && k > 0 {
		s.noteDirty = false
		note, clear = s.steadyNoteLocked(), true
	}
	return w.pos > w.end, s.unsaved >= directTuning.checkpointBytes, note, clear
}

func (s *segmentRun) steadyNoteLocked() string {
	if s.throttle > 0 {
		return busyNote(s.limit)
	}
	return ""
}

// release ends w's attempt. Written bytes join the finished set, and what is
// left of its range goes back to the queue. countTry marks a failure: the
// range's tries go up (reset first when the attempt made real progress), and
// it waits max(minDelay, back-off) before anyone retries it. It returns the
// range's tries.
func (s *segmentRun) release(w *segWorker, countTry bool, minDelay time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.pos > w.start {
		s.completed = normalizeRanges(append(s.completed, byteRange{Start: w.start, End: w.pos - 1}))
	}
	w.active, w.reserved, w.restart = false, 0, false
	tries := w.tries
	if w.pos <= w.end {
		var notBefore time.Time
		if countTry {
			if w.pos-w.attemptStart >= directTuning.progressReset {
				tries = 0
			}
			tries++
			s.failStreak++
			notBefore = now.Add(max(minDelay, directBackoff(tries)))
			s.noteDirty = true
		}
		s.pending = append(s.pending, pendingRange{
			byteRange: byteRange{Start: w.pos, End: w.end}, tries: tries, notBefore: notBefore,
		})
		w.waiting = countTry
	}
	w.tries = tries
	w.start = w.pos
	signal(s.events)
	return tries
}

// busy lowers the connection count by one (down to 1) and holds new
// requests for retryAfter. It returns the new limit.
func (s *segmentRun) busy(retryAfter time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.want-s.throttle > 1 {
		s.throttle++
	}
	s.limit = max(1, s.want-s.throttle)
	s.lastBusy = now
	if until := now.Add(retryAfter); until.After(s.holdUntil) {
		s.holdUntil = until
	}
	return s.limit
}

// setWant applies the configured connection count and lets a lowered count
// climb back by one after directTuning.throttleRecover without errors.
func (s *segmentRun) setWant(want int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.want = max(want, 1)
	if s.throttle > 0 && now.Sub(s.lastBusy) >= directTuning.throttleRecover {
		s.throttle--
		s.lastBusy = now
		s.noteDirty = true
	}
	s.throttle = min(s.throttle, s.want-1)
	s.limit = max(1, s.want-s.throttle)
}

func (s *segmentRun) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		if s.cancel != nil {
			s.cancel()
		}
	}
	s.mu.Unlock()
	signal(s.events)
}

func (s *segmentRun) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *segmentRun) streak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failStreak
}

// spawnable reports whether another worker would find work within limits.
func (s *segmentRun) spawnable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.running >= s.limit {
		return false
	}
	if len(s.pending) > 0 {
		return true
	}
	_, left := s.largestRunningLocked()
	return left >= 2*s.minChunk
}

func (s *segmentRun) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running == 0
}

// doneRanges returns every written byte (finished ranges plus the written
// prefix of each running one) and resets the checkpoint counter. Bytes
// reserved but not yet committed are left out.
func (s *segmentRun) doneRanges() []byteRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]byteRange(nil), s.completed...)
	for _, w := range s.workers {
		if !w.exited && w.active && w.pos > w.start {
			out = append(out, byteRange{Start: w.start, End: w.pos - 1})
		}
	}
	s.unsaved = 0
	return normalizeRanges(out)
}

// validatorFor returns the validator to send with a request to link: only
// the URL it was observed on can be held to it.
func (s *segmentRun) validatorFor(link string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if link != s.validatorURL {
		return ""
	}
	return s.validator
}

// sample records each worker's progress for speeds; called every tick.
func (s *segmentRun) sample(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		if !w.exited {
			w.speed.push(sample{at: now, bytes: w.bytes})
		}
	}
}

// healthCheck restarts a connection that ran below slowRatio of the
// average speed for slowChecks checks in a row (after Surge's health check).
func (s *segmentRun) healthCheck(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := int(directTuning.healthWindow/directTuning.healthInterval) + 1
	type rated struct {
		w     *segWorker
		speed float64
	}
	var eligible []rated
	var sum float64
	for _, w := range s.workers {
		if w.exited || !w.active {
			continue
		}
		w.hist = append(w.hist, sample{at: now, bytes: w.bytes})
		if len(w.hist) > keep {
			w.hist = append(w.hist[:0], w.hist[len(w.hist)-keep:]...)
		}
		first := w.hist[0]
		dt := now.Sub(first.at)
		if len(w.hist) < keep || first.at.Before(w.streamSince) || dt <= 0 {
			continue
		}
		speed := float64(w.bytes-first.bytes) / dt.Seconds()
		eligible = append(eligible, rated{w, speed})
		sum += speed
	}
	if len(eligible) < 2 {
		return
	}
	avg := sum / float64(len(eligible))
	for _, r := range eligible {
		w := r.w
		if r.speed >= directTuning.slowRatio*avg {
			w.slow = 0
			continue
		}
		w.slow++
		left := w.end - (w.pos + w.reserved) + 1
		if w.slow >= directTuning.slowChecks && left >= s.minChunk &&
			(w.lastRestart.IsZero() || now.Sub(w.lastRestart) >= directTuning.restartCooldown) {
			w.restart = true
			w.lastRestart = now
			w.slow = 0
			s.restarts++
		}
	}
}

// ByteSpan is an inclusive byte range of a download.
type ByteSpan struct {
	Start, End int64
}

// SegmentInfo is one connection of a segmented download.
type SegmentInfo struct {
	Start, End int64 // range this connection works on (End inclusive)
	Done       int64 // bytes written from Start
	SpeedBps   float64
	Tries      int  // failed attempts on this range so far
	Retrying   bool // waiting to retry, or running again after a failure
	Restarted  bool // restarted by the slow-connection check recently
}

// SegmentMap is a live picture of a segmented download for a segment bar.
type SegmentMap struct {
	Total       int64
	Completed   []ByteSpan // finished bytes outside the running segments
	Segments    []SegmentInfo
	Connections int // connections streaming now
	Limit       int // connections allowed now (lowered while the server is busy)
	Restarts    int // slow connections restarted in this run
}

func (s *segmentRun) snapshot(now time.Time) SegmentMap {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := SegmentMap{Total: s.total, Limit: s.limit, Restarts: s.restarts}
	m.Completed = make([]ByteSpan, len(s.completed))
	for i, r := range s.completed {
		m.Completed[i] = ByteSpan{Start: r.Start, End: r.End}
	}
	for _, w := range s.workers {
		if w.exited || (!w.active && !w.waiting) {
			continue
		}
		if w.active {
			m.Connections++
		}
		m.Segments = append(m.Segments, SegmentInfo{
			Start: w.start, End: w.end, Done: w.pos - w.start,
			SpeedBps: w.speed.speedBps(), Tries: w.tries,
			Retrying:  w.waiting || w.tries > 0,
			Restarted: !w.lastRestart.IsZero() && now.Sub(w.lastRestart) < directTuning.restartCooldown,
		})
	}
	return m
}

// finished reports whether the run is over: no worker left and nothing to
// do, or the run failed.
func (s *segmentRun) finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running == 0 && (len(s.pending) == 0 || s.err != nil)
}
