package vt

import (
	"bytes"
	"fmt"
)

// ResetOuterModes returns an escape sequence that safely resets interactive terminal
// modes (mouse tracking, bracketed paste, focus events, Kitty keyboard protocol,
// modifyOtherKeys, application cursor/keypad, SGR, hyperlinks, cursor style/visibility)
// WITHOUT wiping the screen or scrollback. Used when detaching cleanly or opening the switcher.
func ResetOuterModes(exitAltScreen bool) []byte {
	var buf bytes.Buffer
	if exitAltScreen {
		// 1047l cleanly exits alternate screen if active without running DECRC on main screen.
		buf.WriteString("\x1b[?1047l")
	}
	buf.WriteString("\x1b[?2026l")                         // end synchronized output if stuck
	buf.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l")   // disable mouse tracking
	buf.WriteString("\x1b[?1004l")                         // disable focus events
	buf.WriteString("\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l") // disable mouse encodings
	buf.WriteString("\x1b[?2004l")                         // disable bracketed paste
	buf.WriteString("\x1b[?1l\x1b>")                       // normal cursor & keypad
	buf.WriteString("\x1b[?6l\x1b[4l\x1b[20l")             // reset DECOM, IRM, LNM
	buf.WriteString("\x1b[?7h")                            // enable DECAWM
	buf.WriteString("\x1b[<32u")                           // clear Kitty keyboard stack
	buf.WriteString("\x1b[>4;0m")                          // reset modifyOtherKeys
	buf.WriteString("\x1b]8;;\x1b\\")                      // close any open OSC 8 hyperlink
	buf.WriteString("\x1b[0m")                             // reset SGR
	buf.WriteString("\x1b[0 q")                            // reset cursor shape
	buf.WriteString("\x1b[?25h")                           // ensure cursor is visible
	return buf.Bytes()
}

// BuildReplay produces an atomic, synchronized-output escape sequence that restores
// the outer terminal to match this shadow Terminal's exact state:
//   - Outer native scrollback populated with up to maxScrollbackLines of main scrollback
//   - Main screen grid + soft-wrap attributes + saved cursor + cursor position
//   - Alternate screen grid (if active) + cursor + saved cursor
//   - Active SGR style, OSC 8 hyperlink, scroll regions, charsets, titles, CWD
//   - Mouse tracking, bracketed paste, focus events, Kitty keyboard stack, modifyOtherKeys, cursor shape/visibility
func (t *Terminal) BuildReplay(maxScrollbackLines int) []byte {
	var buf bytes.Buffer
	buf.Grow(16 * 1024)

	// Begin synchronized output so modern terminals render the entire switch in one frame.
	buf.WriteString("\x1b[?2026h")

	// Step 1: Clean baseline on main screen buffer.
	buf.WriteString("\x1b[?1049l")                         // leave alt screen if active
	buf.WriteString("\x1b[?25l")                           // hide cursor during repaint
	buf.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l")   // mouse off
	buf.WriteString("\x1b[?1004l")                         // focus off
	buf.WriteString("\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l")
	buf.WriteString("\x1b[?2004l")                         // bracketed paste off
	buf.WriteString("\x1b[?1l\x1b>")                       // normal cursor & keypad
	buf.WriteString("\x1b[?6l\x1b[4l\x1b[20l")             // reset DECOM, IRM, LNM
	buf.WriteString("\x1b[?7h")                            // enable DECAWM for soft-wrap replay
	buf.WriteString("\x1b[<32u")                           // clear Kitty keyboard stack
	buf.WriteString("\x1b[>4;0m")                          // clear modifyOtherKeys
	buf.WriteString("\x1b[r")                              // reset scroll margins
	buf.WriteString("\x1b(B\x1b)B\x0f")                    // reset charsets to ASCII G0
	buf.WriteString("\x1b]8;;\x1b\\")                      // close OSC 8 hyperlink
	buf.WriteString("\x1b[0m")                             // reset SGR
	buf.WriteString("\x1b[0 q")                            // default cursor shape
	buf.WriteString("\x1b[3J\x1b[2J\x1b[H")                // clear outer scrollback + screen + home

	// Step 2: Replay main screen scrollback + visible main screen rows.
	sb := t.Main.Scrollback
	if maxScrollbackLines >= 0 && len(sb) > maxScrollbackLines {
		sb = sb[len(sb)-maxScrollbackLines:]
	}
	totalLines := len(sb) + len(t.Main.Lines)
	curStyle := Style{}

	lineAt := func(idx int) Line {
		if idx < len(sb) {
			return sb[idx]
		}
		return t.Main.Lines[idx-len(sb)]
	}

	for i := 0; i < totalLines; i++ {
		line := lineAt(i)
		softWrapNext := false
		if i < totalLines-1 && line.Wrapped {
			next := lineAt(i + 1)
			if next.trimmedLen() > 0 || next.Wrapped {
				softWrapNext = true
			}
		}

		var end int
		if softWrapNext {
			end = len(line.Cells)
			next := lineAt(i + 1)
			// If wrapped early because a wide char didn't fit in the final column,
			// stop before the trailing padding space so outer terminal early-wraps too.
			if end > 0 && line.Cells[end-1].IsBlankDefault() && len(next.Cells) > 0 && next.Cells[0].Width == 2 {
				end--
			}
		} else {
			end = line.trimmedLen()
		}

		writeCells(&buf, line.Cells[:end], &curStyle, &t.Links)

		if i < totalLines-1 && !softWrapNext {
			// Reset non-default background before \r\n to avoid BCE bleed.
			if curStyle.Bg.Kind != ColorDefault || curStyle.HyperlinkID != 0 {
				WriteStyleTransition(&buf, curStyle, Style{}, &t.Links)
				curStyle = Style{}
			}
			buf.WriteString("\r\n")
		}
	}

	// Reset style after painting main screen.
	if !curStyle.IsDefault() {
		WriteStyleTransition(&buf, curStyle, Style{}, &t.Links)
		curStyle = Style{}
	}

	// Restore main screen scroll margins, saved cursor, Kitty stack, and cursor position.
	writeScrollRegion(&buf, t.Main)
	writeSavedCursor(&buf, t.Main, &t.Links)
	writeKittyStack(&buf, t.Main.KittyStack)
	fmt.Fprintf(&buf, "\x1b[%d;%dH", t.Main.CursorRow+1, t.Main.CursorCol+1)

	// Step 3: If currently in alternate screen, enter alt screen and paint it on top.
	// Note: Main screen + scrollback + main saved cursor are already in place underneath,
	// so exiting vim/less/htop returns cleanly to the exact main screen!
	if t.InAltScreen {
		// Enter alt screen (1047 avoids overwriting the main saved cursor we just placed,
		// or 1049 saves current main cursor—using 1047 preserves DECSC if main had a separate saved cursor!).
		if t.Main.Saved.Valid {
			buf.WriteString("\x1b[?1047h")
		} else {
			buf.WriteString("\x1b[?1049h")
		}
		buf.WriteString("\x1b[r\x1b[2J\x1b[H")
		buf.WriteString("\x1b[<32u")

		altStyle := Style{}
		for r := 0; r < t.Alt.Rows; r++ {
			line := t.Alt.Lines[r]
			end := line.trimmedLen()
			if end == 0 {
				continue
			}
			fmt.Fprintf(&buf, "\x1b[%d;1H", r+1)
			writeCells(&buf, line.Cells[:end], &altStyle, &t.Links)
		}
		if !altStyle.IsDefault() {
			WriteStyleTransition(&buf, altStyle, Style{}, &t.Links)
		}

		writeScrollRegion(&buf, t.Alt)
		writeSavedCursor(&buf, t.Alt, &t.Links)
		writeKittyStack(&buf, t.Alt.KittyStack)
		fmt.Fprintf(&buf, "\x1b[%d;%dH", t.Alt.CursorRow+1, t.Alt.CursorCol+1)
	}

	// Step 4: Restore active modes, charsets, active SGR/hyperlink, titles, cursor shape & visibility.
	t.writeActiveModes(&buf)

	// Restore active SGR style & hyperlink for subsequent live output.
	if !t.Style.IsDefault() {
		WriteStyleTransition(&buf, Style{}, t.Style, &t.Links)
	}

	// End synchronized output (unless app itself has 2026 active? Never leave outer stuck).
	buf.WriteString("\x1b[?2026l")

	return buf.Bytes()
}

// BuildModeRestore re-applies only the active terminal modes, cursor visibility/shape,
// and titles (used for an instant zero-redraw cancel from the switcher when the
// main screen wasn't modified while paused).
func (t *Terminal) BuildModeRestore() []byte {
	var buf bytes.Buffer
	buf.WriteString("\x1b[?1049l") // exit switcher's alt screen
	buf.WriteString("\x1b[<32u")
	writeKittyStack(&buf, t.Main.KittyStack)
	t.writeActiveModes(&buf)
	if !t.Style.IsDefault() {
		WriteStyleTransition(&buf, Style{}, t.Style, &t.Links)
	}
	return buf.Bytes()
}

func (t *Terminal) writeActiveModes(buf *bytes.Buffer) {
	// Metadata (titles & CWD)
	if t.Title != "" {
		buf.WriteString("\x1b]2;")
		buf.WriteString(t.Title)
		buf.WriteString("\x1b\\")
	}
	if t.IconTitle != "" && t.IconTitle != t.Title {
		buf.WriteString("\x1b]1;")
		buf.WriteString(t.IconTitle)
		buf.WriteString("\x1b\\")
	}

	// Character sets
	if t.CharsetG0 != 0 && t.CharsetG0 != 'B' {
		buf.WriteString("\x1b(")
		buf.WriteByte(t.CharsetG0)
	}
	if t.CharsetG1 != 0 && t.CharsetG1 != 'B' {
		buf.WriteString("\x1b)")
		buf.WriteByte(t.CharsetG1)
	}
	if t.ActiveCharset == 1 {
		buf.WriteByte(0x0E)
	}

	// DEC / ANSI modes
	if t.AppCursorKeys {
		buf.WriteString("\x1b[?1h")
	}
	if t.AppKeypad {
		buf.WriteString("\x1b=")
	}
	if !t.AutoWrap {
		buf.WriteString("\x1b[?7l")
	}
	if t.OriginMode {
		buf.WriteString("\x1b[?6h")
		// Re-position cursor relative to scroll region when DECOM is enabled!
		s := t.Active()
		fmt.Fprintf(buf, "\x1b[%d;%dH", s.CursorRow-s.ScrollTop+1, s.CursorCol+1)
	}
	if t.InsertMode {
		buf.WriteString("\x1b[4h")
	}
	if t.AutoNewline {
		buf.WriteString("\x1b[20h")
	}
	if t.CursorBlink {
		buf.WriteString("\x1b[?12h")
	}
	if t.MouseMode != 0 {
		fmt.Fprintf(buf, "\x1b[?%dh", t.MouseMode)
	}
	if t.MouseEncoding != 0 {
		fmt.Fprintf(buf, "\x1b[?%dh", t.MouseEncoding)
	}
	if t.FocusEvents {
		buf.WriteString("\x1b[?1004h")
	}
	if t.BracketedPaste {
		buf.WriteString("\x1b[?2004h")
	}
	if t.ModifyOtherKeys != 0 {
		fmt.Fprintf(buf, "\x1b[>4;%dm", t.ModifyOtherKeys)
	}
	if t.CursorStyle != 0 {
		fmt.Fprintf(buf, "\x1b[%d q", t.CursorStyle)
	}
	if t.CursorVisible {
		buf.WriteString("\x1b[?25h")
	} else {
		buf.WriteString("\x1b[?25l")
	}
}

func writeCells(buf *bytes.Buffer, cells []Cell, curStyle *Style, links *HyperlinkTable) {
	for _, c := range cells {
		if c.Width == 0 {
			continue // wide-char continuation cell
		}
		if c.Style != *curStyle {
			WriteStyleTransition(buf, *curStyle, c.Style, links)
			*curStyle = c.Style
		}
		r := c.Rune
		if r == 0 {
			r = ' '
		}
		buf.WriteRune(r)
		if c.Extra != "" {
			buf.WriteString(c.Extra)
		}
	}
}

func writeScrollRegion(buf *bytes.Buffer, s *Screen) {
	if s.ScrollTop != 0 || s.ScrollBottom != s.Rows-1 {
		fmt.Fprintf(buf, "\x1b[%d;%dr", s.ScrollTop+1, s.ScrollBottom+1)
	}
}

func writeSavedCursor(buf *bytes.Buffer, s *Screen, links *HyperlinkTable) {
	if !s.Saved.Valid {
		return
	}
	fmt.Fprintf(buf, "\x1b[%d;%dH", s.Saved.Row+1, s.Saved.Col+1)
	if !s.Saved.Style.IsDefault() {
		WriteStyleTransition(buf, Style{}, s.Saved.Style, links)
	}
	buf.WriteString("\x1b7") // DECSC
	if !s.Saved.Style.IsDefault() {
		WriteStyleTransition(buf, s.Saved.Style, Style{}, links)
	}
}

func writeKittyStack(buf *bytes.Buffer, stack []uint32) {
	for _, flags := range stack {
		fmt.Fprintf(buf, "\x1b[>%du", flags)
	}
}
