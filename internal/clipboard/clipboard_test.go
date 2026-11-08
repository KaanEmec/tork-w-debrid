package clipboard

import (
	"bytes"
	"errors"
	"github.com/charmbracelet/x/term"
	"io"
	"os"
	"strings"
	"testing"
)

type brokenWriter struct{ short bool }

func (w brokenWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestCopyFallbackWritesToTerminalAndReportsFailures(t *testing.T) {
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	unavailable := func(string) error { return errors.New("no desktop clipboard") }
	var terminal bytes.Buffer
	text := "magnet:?xt=urn:btih:1111111111111111111111111111111111111111"
	if err := write(text, NewOutput(&terminal), unavailable); err != nil {
		t.Fatal(err)
	}
	if terminal.String() != Sequence(text).String() {
		t.Fatal("terminal did not receive the complete clipboard sequence")
	}
	if err := write(text, brokenWriter{}, unavailable); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("failed terminal write reported success")
	}
	if err := write(text, brokenWriter{short: true}, unavailable); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("truncated copy reported success")
	}
	if err := write(text, nil, unavailable); err == nil {
		t.Fatal("copy without a terminal reported success")
	}
	terminal.Reset()
	if err := write(text, &terminal, func(string) error { return nil }); err != nil || terminal.Len() != 0 {
		t.Fatal("successful desktop copy also wrote terminal escapes")
	}
}

func TestSSHCopyUsesConnectingTerminalAndItsMultiplexer(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "remote connection")
	t.Setenv("TMUX", "/tmp/tmux/current-client")
	var terminal bytes.Buffer
	if err := write("test", &terminal, func(string) error { t.Fatal("remote copy used the host desktop clipboard"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(terminal.String(), "\x1bPtmux;") || !strings.Contains(terminal.String(), "dGVzdA==") {
		t.Fatal("copy ignored the attached terminal's tmux environment")
	}
}

// Each renderer write is one frame: the terminal must get it inside one
// synchronized-output block, so it never paints half a frame.
func TestSyncOutputWrapsEachWriteInOneBlock(t *testing.T) {
	var terminal bytes.Buffer
	out := NewSyncOutput(&terminal)
	for _, frame := range []string{"frame one", "frame two"} {
		terminal.Reset()
		if n, err := out.Write([]byte(frame)); err != nil || n != len(frame) {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if want := "\x1b[?2026h" + frame + "\x1b[?2026l"; terminal.String() != want {
			t.Fatalf("terminal got %q, want %q", terminal.String(), want)
		}
	}
	if n, err := NewSyncOutput(brokenWriter{}).Write([]byte("frame")); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed write = %d, %v", n, err)
	}
	if _, err := NewSyncOutput(brokenWriter{short: true}).Write([]byte("frame")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("a write that lost the closing marker reported %v", err)
	}
}

// Bubble Tea checks the complete term.File interface, not just io.Writer or Fd.
func TestOutputPreservesTerminalFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "terminal")
	if err != nil {
		t.Fatal(err)
	}
	out := NewOutput(f)
	var terminal term.File = out
	if terminal.Fd() != f.Fd() {
		t.Fatal("terminal descriptor hidden by clipboard wrapper")
	}
	if _, err := out.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := terminal.Read(buf); err != nil || string(buf) != "frame" {
		t.Fatal("file reads not forwarded")
	}
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(nil); err == nil {
		t.Fatal("file close not forwarded")
	}
	if term.IsTerminal(NewOutput(&bytes.Buffer{}).Fd()) {
		t.Fatal("buffer identified as a terminal")
	}
}
