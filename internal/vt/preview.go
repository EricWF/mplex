package vt

import "bytes"

// RenderPreview renders up to maxRows x maxCols of the active screen as
// ANSI-styled strings (each line self-contained with trailing SGR reset) for
// the switcher's live preview pane.
func (t *Terminal) RenderPreview(maxRows, maxCols int) []string {
	if maxRows <= 0 || maxCols <= 0 {
		return nil
	}
	s := t.Active()

	// Determine the interesting vertical slice:
	// If in alt-screen, show from top (row 0); if in main screen, show up to
	// the last non-empty row (or cursor row), taking the bottom maxRows if taller.
	startRow := 0
	endRow := s.Rows
	if !t.InAltScreen {
		lastContent := s.CursorRow
		for r := s.Rows - 1; r > lastContent; r-- {
			if s.Lines[r].trimmedLen() > 0 {
				lastContent = r
				break
			}
		}
		endRow = lastContent + 1
		if endRow > maxRows {
			startRow = endRow - maxRows
		}
	} else if endRow > maxRows {
		endRow = maxRows
	}

	out := make([]string, 0, endRow-startRow)
	var buf bytes.Buffer
	for r := startRow; r < endRow; r++ {
		buf.Reset()
		line := s.Lines[r]
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
			colUsed += w
		}
		if !curStyle.IsDefault() {
			buf.WriteString("\x1b[0m")
		}
		out = append(out, buf.String())
	}
	return out
}
