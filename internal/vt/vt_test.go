package vt

import (
	"fmt"
	"strings"
	"testing"
)

func lineText(l Line) string {
	var b strings.Builder
	end := l.trimmedLen()
	for i := 0; i < end; i++ {
		if l.Cells[i].Width == 0 {
			continue
		}
		r := l.Cells[i].Rune
		if r == 0 {
			r = ' '
		}
		b.WriteRune(r)
		b.WriteString(l.Cells[i].Extra)
	}
	return b.String()
}

func assertTerminalScreenEq(t *testing.T, label string, a, b *Screen) {
	t.Helper()
	if a.Rows != b.Rows || a.Cols != b.Cols {
		t.Fatalf("%s: dims mismatch: %dx%d vs %dx%d", label, a.Rows, a.Cols, b.Rows, b.Cols)
	}
	if len(a.Scrollback) != len(b.Scrollback) {
		t.Fatalf("%s: scrollback len mismatch: got %d, want %d", label, len(b.Scrollback), len(a.Scrollback))
	}
	for i := range a.Scrollback {
		if lineText(a.Scrollback[i]) != lineText(b.Scrollback[i]) {
			t.Fatalf("%s: scrollback[%d] text mismatch:\n got: %q\nwant: %q", label, i, lineText(b.Scrollback[i]), lineText(a.Scrollback[i]))
		}
		if a.Scrollback[i].Wrapped != b.Scrollback[i].Wrapped {
			t.Fatalf("%s: scrollback[%d] wrapped mismatch: got %v, want %v (%q)", label, i, b.Scrollback[i].Wrapped, a.Scrollback[i].Wrapped, lineText(a.Scrollback[i]))
		}
	}
	for r := 0; r < a.Rows; r++ {
		if lineText(a.Lines[r]) != lineText(b.Lines[r]) {
			t.Fatalf("%s: row %d text mismatch:\n got: %q\nwant: %q", label, r, lineText(b.Lines[r]), lineText(a.Lines[r]))
		}
		if a.Lines[r].Wrapped != b.Lines[r].Wrapped {
			t.Fatalf("%s: row %d wrapped mismatch: got %v, want %v", label, r, b.Lines[r].Wrapped, a.Lines[r].Wrapped)
		}
		for c := 0; c < a.Cols; c++ {
			ca, cb := a.Lines[r].Cells[c], b.Lines[r].Cells[c]
			if ca.Style.Fg != cb.Style.Fg || ca.Style.Bg != cb.Style.Bg ||
				ca.Style.Attrs != cb.Style.Attrs || ca.Style.Underline != cb.Style.Underline ||
				ca.Style.UnderlineColor != cb.Style.UnderlineColor {
				t.Fatalf("%s: cell (%d,%d) style mismatch:\n got: %+v\nwant: %+v", label, r, c, cb.Style, ca.Style)
			}
		}
	}
	if a.CursorRow != b.CursorRow || a.CursorCol != b.CursorCol {
		t.Fatalf("%s: cursor mismatch: got (%d,%d), want (%d,%d)", label, b.CursorRow, b.CursorCol, a.CursorRow, a.CursorCol)
	}
}

func TestReplayRoundTripScrollbackAndSoftWrap(t *testing.T) {
	termA := New(10, 20, 1000)

	// Generate 45 lines including long lines that soft-wrap across 3 rows.
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		if i%7 == 0 {
			fmt.Fprintf(&sb, "wrap-%02d-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789\r\n", i)
		} else {
			fmt.Fprintf(&sb, "line-%02d normal text\r\n", i)
		}
	}
	sb.WriteString("prompt$ ")
	termA.Write([]byte(sb.String()))

	if len(termA.Main.Scrollback) < 40 {
		t.Fatalf("expected >40 scrollback lines, got %d", len(termA.Main.Scrollback))
	}

	// Simulate dirty outer terminal (different content, alt-screen, mouse on) before replay.
	termB := New(10, 20, 1000)
	termB.Write([]byte("OLD DIRTY CONTENT\r\nOLD LINE 2\r\n\x1b[?1049h\x1b[?1000h\x1b[>5uDIRTY ALT"))

	replay := termA.BuildReplay(1000)
	termB.Write(replay)

	assertTerminalScreenEq(t, "main", termA.Main, termB.Main)
	if termB.InAltScreen {
		t.Fatalf("expected termB to exit alt screen")
	}
	if termB.MouseMode != 0 {
		t.Fatalf("expected mouse mode reset, got %d", termB.MouseMode)
	}
	if len(termB.Main.KittyStack) != 0 {
		t.Fatalf("expected kitty stack reset, got %v", termB.Main.KittyStack)
	}
}

func TestReplayRoundTripAltScreenPreservesMainAndScrollback(t *testing.T) {
	termA := New(8, 30, 500)

	// 1. Build scrollback + main screen prompt.
	for i := 0; i < 25; i++ {
		termA.Write([]byte(fmt.Sprintf("history line %02d\r\n", i)))
	}
	termA.Write([]byte("user@host:~$ vim file.go"))

	// 2. Enter alt screen (like vim/neovim) and enable modern modes.
	termA.Write([]byte(
		"\x1b[?1049h" + // alt screen
			"\x1b[?1002h\x1b[?1006h" + // button-event + SGR mouse
			"\x1b[?1004h" + // focus events
			"\x1b[?2004h" + // bracketed paste
			"\x1b[>5u" + // Kitty keyboard protocol
			"\x1b[6 q" + // steady bar cursor
			"\x1b]2;file.go - NVIM\x1b\\" +
			"\x1b[1;1H\x1b[1;38;2;255;120;60;4:3;58:2::255;0;0mfunc main() {" +
			"\x1b[0m\x1b[2;3H\x1b[38:2::100:220:150mprintln(\"hi\")\x1b[0m" +
			"\x1b[3;1H}" +
			"\x1b[2;5H",
	))

	// 3. Replay into fresh outer terminal B.
	termB := New(8, 30, 500)
	termB.Write(termA.BuildReplay(500))

	if !termB.InAltScreen {
		t.Fatalf("expected termB in alt screen")
	}
	if termB.MouseMode != 1002 || termB.MouseEncoding != 1006 {
		t.Fatalf("mouse mismatch: mode=%d enc=%d", termB.MouseMode, termB.MouseEncoding)
	}
	if !termB.FocusEvents || !termB.BracketedPaste {
		t.Fatalf("focus=%v paste=%v", termB.FocusEvents, termB.BracketedPaste)
	}
	if len(termB.Alt.KittyStack) != 1 || termB.Alt.KittyStack[0] != 5 {
		t.Fatalf("kitty stack mismatch: %v", termB.Alt.KittyStack)
	}
	if termB.CursorStyle != 6 {
		t.Fatalf("cursor style mismatch: got %d, want 6", termB.CursorStyle)
	}
	if termB.Title != "file.go - NVIM" {
		t.Fatalf("title mismatch: %q", termB.Title)
	}
	assertTerminalScreenEq(t, "alt", termA.Alt, termB.Alt)

	// 4. Now simulate quitting vim (\x1b[?1049l) on BOTH termA and termB!
	// Both must reveal the EXACT same main screen + 25 lines of native scrollback + cursor!
	termA.Write([]byte("\x1b[?1049l"))
	termB.Write([]byte("\x1b[?1049l"))

	if termB.InAltScreen {
		t.Fatalf("expected termB back on main screen")
	}
	assertTerminalScreenEq(t, "main-after-alt-exit", termA.Main, termB.Main)
}

func TestWideCharsCombiningMarksAndHyperlinks(t *testing.T) {
	termA := New(6, 15, 100)
	// Wide CJK + combining accent + OSC 8 hyperlink + early wrap before wide char at right edge.
	seq := "A" + "e\u0301" + " \x1b]8;id=1;https://example.com\x1b\\link\x1b]8;;\x1b\\ 日本語テストEND"
	// Feed byte-by-byte to stress streaming UTF-8 and escape parsing!
	for i := 0; i < len(seq); i++ {
		termA.Write([]byte{seq[i]})
	}

	termB := New(6, 15, 100)
	termB.Write(termA.BuildReplay(100))

	assertTerminalScreenEq(t, "wide-utf8", termA.Main, termB.Main)

	// Verify hyperlink survived replay onto "link" cells!
	hlID := termB.Main.Lines[0].Cells[3].Style.HyperlinkID
	hl, ok := termB.Links.Get(hlID)
	if !ok || hl.URI != "https://example.com" {
		t.Fatalf("hyperlink lost on replay: id=%d hl=%+v ok=%v", hlID, hl, ok)
	}
}

func TestResizeReflowSoftWrappedLines(t *testing.T) {
	term := New(6, 10, 100)
	term.Write([]byte("0123456789abcdefghij\r\nshort\r\n$ "))

	// At width 10: "0123456789" (wrapped=true), "abcdefghij" (wrapped=false), "short", "$ "
	if !term.Main.Lines[0].Wrapped {
		t.Fatalf("expected row 0 wrapped at width 10")
	}

	// Widen to 25 cols: wrapped logical line should rejoin onto a single line!
	term.Resize(6, 25)
	if got := lineText(term.Main.Lines[0]); got != "0123456789abcdefghij" {
		t.Fatalf("reflow widen failed: row 0 = %q", got)
	}
	if term.Main.Lines[0].Wrapped {
		t.Fatalf("row 0 should no longer be wrapped at width 25")
	}
	if got := lineText(term.Main.Lines[1]); got != "short" {
		t.Fatalf("row 1 = %q, want short", got)
	}
	if got := lineText(term.Main.Lines[2]); got != "$ " && got != "$" {
		t.Fatalf("row 2 = %q", got)
	}
	if term.Main.CursorRow != 2 || term.Main.CursorCol != 2 {
		t.Fatalf("cursor after widen: (%d,%d), want (2,2)", term.Main.CursorRow, term.Main.CursorCol)
	}

	// Narrow back to 8 cols: should cleanly re-wrap into 3 segments!
	term.Resize(6, 8)
	if got := lineText(term.Main.Lines[0]); got != "01234567" || !term.Main.Lines[0].Wrapped {
		t.Fatalf("narrow row 0 = %q wrapped=%v", got, term.Main.Lines[0].Wrapped)
	}
	if got := lineText(term.Main.Lines[1]); got != "89abcdef" || !term.Main.Lines[1].Wrapped {
		t.Fatalf("narrow row 1 = %q wrapped=%v", got, term.Main.Lines[1].Wrapped)
	}
	if got := lineText(term.Main.Lines[2]); got != "ghij" || term.Main.Lines[2].Wrapped {
		t.Fatalf("narrow row 2 = %q wrapped=%v", got, term.Main.Lines[2].Wrapped)
	}
}

func TestDetachedQueryResponder(t *testing.T) {
	term := New(24, 80, 100)
	var replies []string
	term.OnQueryReply = func(reply []byte) {
		replies = append(replies, string(reply))
	}

	// Snoop real terminal background color first.
	term.SnoopClientInput([]byte("\x1b]11;rgb:1a1a/1b1b/2626\x1b\\"))

	term.Write([]byte("\x1b[5;12H"))            // move cursor to row 5 col 12
	term.Write([]byte("\x1b[?2004h\x1b[>3u"))   // bracketed paste + kitty flags 3
	term.Write([]byte("\x1b[6n"))               // DSR cursor pos
	term.Write([]byte("\x1b[?2004$p"))          // DECRQM bracketed paste
	term.Write([]byte("\x1b[?u"))               // Kitty keyboard query
	term.Write([]byte("\x1b]11;?\x07"))         // OSC 11 background query
	term.Write([]byte("\x1b[c"))                // DA1

	want := []string{
		"\x1b[5;12R",
		"\x1b[?2004;1$y",
		"\x1b[?3u",
		"\x1b]11;rgb:1a1a/1b1b/2626\x1b\\",
		"\x1b[?62;22c",
	}
	if len(replies) != len(want) {
		t.Fatalf("got %d replies %q, want %d %q", len(replies), replies, len(want), want)
	}
	for i := range want {
		if replies[i] != want[i] {
			t.Fatalf("reply[%d] = %q, want %q", i, replies[i], want[i])
		}
	}
}

func TestOSC7CwdAndAPCGraphicsIgnoredCleanly(t *testing.T) {
	term := New(10, 40, 100)
	// Include a Kitty graphics APC payload + OSC 7 CWD + normal text.
	term.Write([]byte("\x1b]7;file://host/home/eric/my%20project\x07\x1b_Ga=T,f=100;AAAA====\x1b\\hello"))
	if term.Cwd != "/home/eric/my project" {
		t.Fatalf("cwd = %q, want /home/eric/my project", term.Cwd)
	}
	if got := lineText(term.Main.Lines[0]); got != "hello" {
		t.Fatalf("APC leaked onto screen: %q", got)
	}
}
