package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kaanemec/tork-w-debrid/internal/engine"
	"github.com/kaanemec/tork-w-debrid/internal/intake"
)

// AttachSession restores the terminal, retaining the model's search, selection,
// and file choices. An explicit CLI target still uses the normal preview flow.
func (a *App) AttachSession(target *intake.Target) (tea.Cmd, error) {
	a.background = true
	a.motion.detached = false // ensureFrame re-arms the clock on the next update
	if target != nil {
		if a.navigationLocked() {
			return nil, fmt.Errorf("finish or cancel the open dialog before opening another torrent")
		}
		if err := a.OpenTarget(*target); err != nil {
			return nil, err
		}
		cmd := a.startup
		a.startup = nil
		return cmd, nil
	}
	intro := a.maybeStartIntro() // once after each update (J8.8)
	if a.screen == screenSearch && !a.search.menuFocused {
		return tea.Batch(intro, a.search.input.Focus()), nil
	}
	if a.screen == screenSources {
		return tea.Batch(intro, a.checkEnabledSources(false)), nil
	}
	return intro, nil
}

func (a *App) DetachSession() tea.Cmd {
	// Nobody is watching: the frame clock stops after the frame in flight.
	a.motion.detached = true
	a.search.input.Blur()
	// A pending clipboard lookup belongs to the terminal that requested it.
	// Approved downloads may continue, but a late copy must not reach a new UI.
	if a.results.resolving && a.results.resolveYank {
		a.cancelResolve()
	}
	return nil
}

// CheckpointSession runs on the owning event loop, including when there is no
// terminal attached. Metadata-only previews are never inserted into the resume
// list: only downloads the user already approved are persisted.
func (a *App) CheckpointSession() error {
	if a.pendingAdds > 0 {
		return fmt.Errorf("a download is still being queued; retry torkwd stop in a moment")
	}
	a.downloads.snaps = a.eng.Snapshots()
	for _, snap := range a.downloads.snaps {
		if e := a.st.Find(snap.Magnet); e != nil {
			applySnapshotToEntry(e, snap)
			if snap.State == engine.StateDone || snap.State == engine.StateSeeding {
				e.Done = true
				if e.CompletedAt == nil {
					now := time.Now().UTC()
					e.CompletedAt = &now
				}
			}
		}
	}
	return a.st.Save(a.cfg.StatePath())
}

func (a *App) SessionStatus() string {
	active, seeding := 0, 0
	for _, s := range a.eng.Snapshots() {
		switch s.State {
		case engine.StateDownloading, engine.StateFetchingMeta, engine.StateVerifying:
			active++
		case engine.StateSeeding:
			seeding++
		}
	}
	return fmt.Sprintf("%d active · %d seeding", active, seeding)
}
