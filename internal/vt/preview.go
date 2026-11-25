package vt

import "bytes"

// RenderPreview renders up to maxRows x maxCols of the active screen as
// ANSI-styled strings (each line self-contained with trailing SGR reset) for
// the switcher's live preview pane.
func (t *Terminal) RenderPreview(maxRows, maxCols int) []string {
	lines, _, _ := t.RenderPreviewScrolled(maxRows, maxCols, 0)
	return lines
}

// RenderPreviewScrolled renders up to maxRows x maxCols of the terminal as
// ANSI-styled strings, optionally scrolled up by scrollOffset lines into main
// scrollback when not in the alternate screen buffer. It returns the rendered
// lines, total available lines, and the clamped actual scroll offset applied.
func (t *Terminal) RenderPreviewScrolled(maxRows, maxCols, scrollOffset int) (lines []string, totalLines int, actualOffset int) {
	if maxRows <= 0 || maxCols <= 0 {
		return nil, 0, 0
	}

	var buf bytes.Buffer
	if t.InAltScreen {
		s := t.Alt
		totalLines = s.Rows
		endRow := s.Rows
		if endRow > maxRows {
			endRow = maxRows
		}
		out := make([]string, 0, endRow)
		for r := 0; r < endRow; r++ {
			out = append(out, t.renderPreviewLine(&buf, s.Lines[r], maxCols))
		}
		return out, totalLines, 0
	}

	s := t.Main
	lastContent := s.CursorRow
	for r := s.Rows - 1; r > lastContent; r-- {
		if s.Lines[r].trimmedLen() > 0 {
			lastContent = r
			break
		}
	}
	activeCount := lastContent + 1
	sbLen := len(s.Scrollback)
	totalLines = sbLen + activeCount

	maxOffset := totalLines - maxRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if scrollOffset < 0 {
		scrollOffset = 0
	}
	if scrollOffset > maxOffset {
		scrollOffset = maxOffset
	}
	actualOffset = scrollOffset

	endIdx := totalLines - actualOffset
	startIdx := endIdx - maxRows
	if startIdx < 0 {
		startIdx = 0
	}

	out := make([]string, 0, endIdx-startIdx)
	for idx := startIdx; idx < endIdx; idx++ {
		var line Line
		if idx < sbLen {
			line = s.Scrollback[idx]
		} else {
			line = s.Lines[idx-sbLen]
		}
		out = append(out, t.renderPreviewLine(&buf, line, maxCols))
	}
	return out, totalLines, actualOffset
}

func (t *Terminal) renderPreviewLine(buf *bytes.Buffer, line Line, maxCols int) string {
	buf.Reset()
	curStyle := Style{}
	colUsed := 0
	end := line.trimmedLen()
	for c := 0; c < end; c++ {
		cell := line.Cells[c]
		if cell.Width == 0 {
			continue
		}
		w := int(cell.Width)
		if w < 1 {
			w = 1
		}
		if colUsed+w > maxCols {
			break
		}
		// Strip OSC 8 links in preview to avoid accidental link bleed across panes.
		st := cell.Style
		st.HyperlinkID = 0
		if st != curStyle {
			WriteStyleTransition(buf, curStyle, st, &t.Links)
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
		colUsed += w
	}
	if !curStyle.IsDefault() {
		buf.WriteString("\x1b[0m")
	}
	return buf.String()
}
