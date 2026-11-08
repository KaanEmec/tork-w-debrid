package tui

import (
	"testing"
	"time"
)

// The intro follows the wall clock: a frame is due every 1/30 s from the
// start, a late tick skips ahead instead of slowing the intro down, and an
// early or repeated tick never moves it back.
func TestIntroFramesFollowTheClock(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	steps := []struct {
		at   time.Duration
		want int
	}{
		{introPeriod, 1},
		{2*introPeriod + time.Millisecond, 2}, // a little late: still frame 2
		{5 * introPeriod, 5},                  // very late: skips 3 and 4
		{4 * introPeriod, 5},                  // never backwards
		{5*introPeriod + time.Millisecond, 5}, // a second tick in one frame
	}
	frame := 0
	for _, s := range steps {
		frame = nextIntroFrame(frame, start, at(s.at))
		if frame != s.want {
			t.Fatalf("tick at %v: frame %d, want %d", s.at, frame, s.want)
		}
	}
	if got := nextIntroFrame(7, start, time.Time{}); got != 8 {
		t.Fatalf("a tick without a time moves one frame, got %d", got)
	}
}

func TestIntroEndsOnTheClockAfterEightSeconds(t *testing.T) {
	a := introApp(t, "v0.2.0", "")
	a.maybeStartIntro()
	start := a.intro.start
	a.Update(frameMsg{at: start.Add(introSeconds*time.Second - introPeriod)})
	if !a.IntroPlaying() || a.intro.frame != introFrames-1 {
		t.Fatalf("frame %d: the intro ended early", a.intro.frame)
	}
	a.Update(frameMsg{at: start.Add(introSeconds * time.Second)})
	if a.IntroPlaying() {
		t.Fatal("the intro must end after 8 s of wall clock")
	}
}

func TestIntroTickLandsOnTheFrameGrid(t *testing.T) {
	start := time.Now().Add(-introPeriod / 2) // halfway through frame 0
	msg, ok := introTick(start)().(frameMsg)
	if !ok {
		t.Fatal("the intro tick did not deliver a frameMsg")
	}
	if got := introFrameAt(start, msg.at); got != 1 {
		t.Fatalf("the tick fired in frame %d, want 1", got)
	}
	if late := msg.at.Sub(start.Add(introPeriod)); late > 20*time.Millisecond {
		t.Fatalf("the tick fired %v after the frame was due", late)
	}
}
