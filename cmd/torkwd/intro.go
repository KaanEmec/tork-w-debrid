package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"

	"github.com/kaanemec/tork-w-debrid/internal/tui"
)

// runIntro is `torkwd intro`: play the demoscene intro (J8.8) again, then
// exit. Any key stops it. With motion off (NO_COLOR, TERM=dumb, --no-motion)
// it shows the still poster frame until a key; piped output never shows it.
func runIntro(args []string, stdoutTTY, stdinTTY bool, stderr io.Writer) error {
	fs := flag.NewFlagSet("intro", flag.ContinueOnError)
	noMotion := fs.Bool("no-motion", false, "show the still poster frame instead of playing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: torkwd intro [--no-motion]")
	}
	if !stdoutTTY || !stdinTTY {
		fmt.Fprintln(stderr, "tork: the intro needs a terminal; nothing to show")
		return nil
	}
	motion := tui.MotionAllowed() && !*noMotion
	_, err := tea.NewProgram(tui.NewIntro(resolveVersion(), motion), tea.WithAltScreen()).Run()
	return err
}

func runIntroCommand(args []string) error {
	return runIntro(args, term.IsTerminal(os.Stdout.Fd()), term.IsTerminal(os.Stdin.Fd()), os.Stderr)
}
