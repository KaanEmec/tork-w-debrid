package engine

import (
	"context"
	"slices"
	"sync"
)

// Connection limits for segmented HTTPS downloads (roadmap D10 and J7.5).
const (
	// DefaultDownloadConnections is the connection count of one debrid file.
	DefaultDownloadConnections = 4
	// MaxDownloadConnections is the most connections one download may use.
	MaxDownloadConnections = 16
	// DefaultConnectionBudget caps the connections of all files of one torrent.
	DefaultConnectionBudget = 8
)

// ConnectionBudget caps the connections that several downloads open together,
// for example the files of one debrid torrent. Each download that runs gets a
// fair share (the total split evenly, the first ones taking the remainder),
// and the total is never exceeded, even while downloads join or leave.
type ConnectionBudget struct {
	mu      sync.Mutex
	total   int
	inUse   int
	members []*budgetMember
	changed chan struct{} // closed and replaced whenever a slot frees up
}

type budgetMember struct{ inUse int }

// NewConnectionBudget returns a budget of total connections (at least 1).
func NewConnectionBudget(total int) *ConnectionBudget {
	return &ConnectionBudget{total: max(total, 1), changed: make(chan struct{})}
}

// SetTotal changes the cap. Running downloads above their new share give up
// connections as their segments end.
func (b *ConnectionBudget) SetTotal(total int) {
	b.mu.Lock()
	b.total = max(total, 1)
	b.notifyLocked()
	b.mu.Unlock()
}

// Total is the cap; InUse the connections open now.
func (b *ConnectionBudget) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func (b *ConnectionBudget) InUse() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

func (b *ConnectionBudget) join() *budgetMember {
	m := &budgetMember{}
	b.mu.Lock()
	b.members = append(b.members, m)
	b.notifyLocked()
	b.mu.Unlock()
	return m
}

func (b *ConnectionBudget) leave(m *budgetMember) {
	b.mu.Lock()
	if i := slices.Index(b.members, m); i >= 0 {
		b.members = slices.Delete(b.members, i, i+1)
	}
	b.inUse -= m.inUse
	m.inUse = 0
	b.notifyLocked()
	b.mu.Unlock()
}

// share is m's fair part of the total. Caller holds b.mu.
func (b *ConnectionBudget) shareLocked(m *budgetMember) int {
	i := slices.Index(b.members, m)
	if i < 0 {
		return 0
	}
	n := len(b.members)
	share := b.total / n
	if i < b.total%n {
		share++
	}
	return share
}

func (b *ConnectionBudget) share(m *budgetMember) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shareLocked(m)
}

// overShare reports whether m holds more connections than its share.
func (b *ConnectionBudget) overShare(m *budgetMember) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return m.inUse > b.shareLocked(m)
}

// tryAcquire takes one connection for m when the total and m's share allow.
func (b *ConnectionBudget) tryAcquire(m *budgetMember) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse >= b.total || m.inUse >= b.shareLocked(m) {
		return false
	}
	b.inUse++
	m.inUse++
	return true
}

// acquire waits until tryAcquire succeeds or ctx ends.
func (b *ConnectionBudget) acquire(ctx context.Context, m *budgetMember) error {
	for {
		b.mu.Lock()
		if b.inUse < b.total && m.inUse < b.shareLocked(m) {
			b.inUse++
			m.inUse++
			b.mu.Unlock()
			return nil
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *ConnectionBudget) release(m *budgetMember) {
	b.mu.Lock()
	if m.inUse > 0 {
		m.inUse--
		b.inUse--
	}
	b.notifyLocked()
	b.mu.Unlock()
}

func (b *ConnectionBudget) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}
