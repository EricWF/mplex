package vt

import (
	"bytes"
	"strings"
)

// CaptureOptions configures how terminal screen and scrollback text is extracted.
type CaptureOptions struct {
	IncludeScrollback bool // include Main.Scrollback before visible lines (when on main screen)
	ANSI              bool // preserve SGR ANSI escape sequences instead of plain text
	JoinWrapped       bool // join soft-wrapped rows back into logical lines
	MaxLines          int  // if > 0, return only the last MaxLines lines
}

// Capture extracts the session's visible screen (and optionally scrollback) as
// plain UTF-8 text or ANSI-styled lines.
func (t *Terminal) Capture(opts CaptureOptions) []string {
	var rawLines []Line
	if t.InAltScreen {
		endRow := t.Alt.Rows
		for endRow > 0 && t.Alt.Lines[endRow-1].trimmedLen() == 0 {
			endRow--
		}
		rawLines = make([]Line, endRow)
		copy(rawLines, t.Alt.Lines[:endRow])
	} else {
		lastContent := t.Main.CursorRow
		for r := t.Main.Rows - 1; r > lastContent; r-- {
			if t.Main.Lines[r].trimmedLen() > 0 || t.Main.Lines[r].Wrapped {
				lastContent = r
				break
			}
		}
		if opts.IncludeScrollback {
			rawLines = make([]Line, 0, len(t.Main.Scrollback)+lastContent+1)
			rawLines = append(rawLines, t.Main.Scrollback...)
			rawLines = append(rawLines, t.Main.Lines[:lastContent+1]...)
		} else {
			rawLines = make([]Line, lastContent+1)
			copy(rawLines, t.Main.Lines[:lastContent+1])
		}
	}

	var out []string
	if opts.JoinWrapped {
		for i := 0; i < len(rawLines); {
			j := i
			for j < len(rawLines)-1 && rawLines[j].Wrapped {
				j++
			}
			out = append(out, t.renderCapturedRun(rawLines[i:j+1], opts.ANSI))
			i = j + 1
		}
	} else {
		out = make([]string, 0, len(rawLines))
		for i := range rawLines {
			out = append(out, t.renderCapturedRun(rawLines[i:i+1], opts.ANSI))
		}
	}

	if opts.MaxLines > 0 && len(out) > opts.MaxLines {
		out = out[len(out)-opts.MaxLines:]
	}
	return out
}

func (t *Terminal) renderCapturedRun(run []Line, ansi bool) string {
	if !ansi {
		var b strings.Builder
		for idx, line := range run {
			var end int
			if idx < len(run)-1 {
				end = len(line.Cells)
				if end > 0 && line.Cells[end-1].IsBlankDefault() && len(run[idx+1].Cells) > 0 && run[idx+1].Cells[0].Width == 2 {
					end--
				}
			} else {
				end = line.trimmedLen()
			}
			for c := 0; c < end; c++ {
				cell := line.Cells[c]
				if cell.Width == 0 {
					continue
				}
				ch := cell.Rune
				if ch == 0 {
					ch = ' '
				}
				b.WriteRune(ch)
				if cell.Extra != "" {
					b.WriteString(cell.Extra)
				}
			}
		}
		return strings.TrimRight(b.String(), " ")
	}

	var buf bytes.Buffer
	curStyle := Style{}
	for idx, line := range run {
		var end int
		if idx < len(run)-1 {
			end = len(line.Cells)
			if end > 0 && line.Cells[end-1].IsBlankDefault() && len(run[idx+1].Cells) > 0 && run[idx+1].Cells[0].Width == 2 {
				end--
			}
		} else {
			end = line.trimmedLen()
		}
		for c := 0; c < end; c++ {
			cell := line.Cells[c]
			if cell.Width == 0 {
				continue
			}
			st := cell.Style
			st.HyperlinkID = 0
			if st != curStyle {
				WriteStyleTransition(&buf, curStyle, st, &t.Links)
				curStyle = st
			}
			ch := cell.Rune
			if ch == 0 {
				ch = ' '
			}
			buf.WriteRune(ch)
			if cell.Extra != "" {
				buf.WriteString(cell.Extra)
			}
		}
	}
	if !curStyle.IsDefault() {
		buf.WriteString("\x1b[0m")
	}
	return buf.String()
}
