package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestIntroCommandNeedsATerminal(t *testing.T) {
	var stderr bytes.Buffer
	if err := runIntro(nil, false, true, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "needs a terminal") {
		t.Fatalf("piped output must skip the intro, got %q", stderr.String())
	}
	if err := runIntro([]string{"extra"}, true, true, &stderr); err == nil {
		t.Fatal("stray arguments must be rejected")
	}
}
