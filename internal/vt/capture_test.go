package vt

import (
	"strings"
	"testing"
)

func TestCaptureScrollbackJoinWrappedANSIAndAltScreen(t *testing.T) {
	term := New(5, 10, 100)

	// Write enough lines to push into scrollback, including a soft-wrapped 16-char line
	// and an ANSI-colored line.
	term.Write([]byte("ROW1\r\nROW2\r\nROW3\r\nROW4\r\n"))
	term.Write([]byte("0123456789ABCDEF\r\n"))
	term.Write([]byte("\x1b[31mRED_TEXT\x1b[0m\r\n"))

	// 1. Plain capture with scrollback + JoinWrapped=true joins the 16-char line back!
	joined := term.Capture(CaptureOptions{
		IncludeScrollback: true,
		JoinWrapped:       true,
	})
	joinedText := strings.Join(joined, "\n")
	if !strings.Contains(joinedText, "ROW1") || !strings.Contains(joinedText, "0123456789ABCDEF") || !strings.Contains(joinedText, "RED_TEXT") {
		t.Fatalf("expected joined scrollback output, got:\n%s", joinedText)
	}
	if strings.Contains(joinedText, "\x1b[") {
		t.Fatalf("plain capture should not contain ANSI escapes: %q", joinedText)
	}

	// 2. Raw wrap (JoinWrapped=false) splits the 16-char line at col 10.
	raw := term.Capture(CaptureOptions{
		IncludeScrollback: true,
		JoinWrapped:       false,
	})
	if strings.Contains(strings.Join(raw, "\n"), "0123456789ABCDEF") {
		t.Fatalf("expected raw wrap to split 16-char line at width 10, got: %v", raw)
	}

	// 3. ANSI=true preserves color codes and MaxLines limits tail.
	ansiTail := term.Capture(CaptureOptions{
		IncludeScrollback: true,
		ANSI:              true,
		JoinWrapped:       true,
		MaxLines:          2,
	})
	if len(ansiTail) != 2 {
		t.Fatalf("expected 2 lines with MaxLines=2, got %d: %v", len(ansiTail), ansiTail)
	}
	if !strings.Contains(strings.Join(ansiTail, "\n"), "\x1b[31m") {
		t.Fatalf("expected ANSI red SGR code in capture, got: %q", ansiTail)
	}

	// 4. Alt screen captures alt buffer content cleanly.
	term.Write([]byte("\x1b[?1049h\x1b[1;1HALT_HEADER\x1b[2;1HALT_BODY"))
	altLines := term.Capture(CaptureOptions{JoinWrapped: true})
	altText := strings.Join(altLines, "\n")
	if !strings.Contains(altText, "ALT_HEADER") || !strings.Contains(altText, "ALT_BODY") {
		t.Fatalf("expected alt screen text, got: %q", altText)
	}
	if strings.Contains(altText, "ROW1") {
		t.Fatalf("alt screen capture should not leak main scrollback: %q", altText)
	}
}
