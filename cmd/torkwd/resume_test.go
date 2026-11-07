package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kaanemec/tork-w-debrid/internal/state"
)

// A saved debrid entry comes back as a debrid item, never as a torrent that
// would join the swarm (J3.4). Without a linked account it waits for a login.
func TestResumeAllKeepsDebridEntriesOffTheSwarm(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, eng := testEngine(t)
	attachDebrid(eng, cfg) // no debrid.json: calls fail with "not linked"
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Show"
	st := &state.State{Entries: []state.Entry{{
		Magnet: magnet, Name: "Show", DownloadDir: t.TempDir(),
		Via: "realdebrid", RemoteID: "RID1", Phase: "cloud",
	}}}
	resumeAll(eng, st, cfg, cfg.DownloadDir)
	snaps := eng.Snapshots()
	if len(snaps) != 1 || snaps[0].Via != "realdebrid" || snaps[0].PeersTotal != 0 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	deadline := 0
	for !strings.Contains(snaps[0].Note, "needs login") && deadline < 500 {
		snaps = eng.Snapshots()
		deadline++
		waitTick()
	}
	if !strings.Contains(snaps[0].Note, "needs login") {
		t.Fatalf("note = %q, want the login hint", snaps[0].Note)
	}
}

func waitTick() { time.Sleep(10 * time.Millisecond) }
