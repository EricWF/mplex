package vt

// Line holds a single terminal row and whether it soft-wrapped onto the next row.
type Line struct {
	Cells   []Cell
	Wrapped bool
}

func makeBlankLine(cols int, bg Color, hyperlinkID uint32) Line {
	cells := make([]Cell, cols)
	blank := BlankCell(bg, hyperlinkID)
	for i := range cells {
		cells[i] = blank
	}
	return Line{Cells: cells}
}

func (l *Line) clone() Line {
	cp := make([]Cell, len(l.Cells))
	copy(cp, l.Cells)
	return Line{Cells: cp, Wrapped: l.Wrapped}
}

// trimmedLen returns the index past the last non-blank-default cell (for normal unwrapped lines).
func (l *Line) trimmedLen() int {
	n := len(l.Cells)
	for n > 0 && l.Cells[n-1].IsBlankDefault() {
		n--
	}
	return n
}

// SavedCursor stores DECSC / DECRC cursor & attribute state.
type SavedCursor struct {
	Valid         bool
	Row           int
	Col           int
	Style         Style
	OriginMode    bool
	AutoWrap      bool
	CharsetG0     byte
	CharsetG1     byte
	ActiveCharset uint8
}

// Screen represents either the main screen (with scrollback) or the alternate screen.
type Screen struct {
	Rows          int
	Cols          int
	Lines         []Line
	Scrollback    []Line
	MaxScrollback int

	CursorRow   int
	CursorCol   int
	PendingWrap bool

	ScrollTop    int // 0-indexed inclusive
	ScrollBottom int // 0-indexed inclusive

	TabStops []bool
	Saved    SavedCursor

	// Kitty keyboard protocol stack (maintained independently for main & alt screens).
	KittyStack []uint32
}

func newScreen(rows, cols, maxScrollback int) *Screen {
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	s := &Screen{
		Rows:          rows,
		Cols:          cols,
		Lines:         make([]Line, rows),
		MaxScrollback: maxScrollback,
		ScrollTop:     0,
		ScrollBottom:  rows - 1,
		TabStops:      makeDefaultTabStops(cols),
	}
	for r := 0; r < rows; r++ {
		s.Lines[r] = makeBlankLine(cols, Color{}, 0)
	}
	return s
}

func makeDefaultTabStops(cols int) []bool {
	stops := make([]bool, cols)
	for c := 8; c < cols; c += 8 {
		stops[c] = true
	}
	return stops
}

func (s *Screen) reset(rows, cols int) {
	s.Rows = rows
	s.Cols = cols
	s.Lines = make([]Line, rows)
	for r := 0; r < rows; r++ {
		s.Lines[r] = makeBlankLine(cols, Color{}, 0)
	}
	s.CursorRow = 0
	s.CursorCol = 0
	s.PendingWrap = false
	s.ScrollTop = 0
	s.ScrollBottom = rows - 1
	s.TabStops = makeDefaultTabStops(cols)
	s.Saved = SavedCursor{}
	s.KittyStack = s.KittyStack[:0]
}

func (s *Screen) pushScrollback(line Line) {
	if s.MaxScrollback <= 0 {
		return
	}
	if len(s.Scrollback) >= s.MaxScrollback {
		// Drop oldest batch to amortize slice shifting to O(1).
		drop := s.MaxScrollback / 8
		if drop < 1 {
			drop = 1
		}
		copy(s.Scrollback, s.Scrollback[drop:])
		// Zero tail references for GC.
		for i := len(s.Scrollback) - drop; i < len(s.Scrollback); i++ {
			s.Scrollback[i] = Line{}
		}
		s.Scrollback = s.Scrollback[:len(s.Scrollback)-drop]
	}
	s.Scrollback = append(s.Scrollback, line)
}

// scrollUp scrolls lines within [top, bottom] up by n rows.
// When top == 0 && bottom == Rows-1, scrolled-off lines go into Scrollback (if MaxScrollback > 0).
func (s *Screen) scrollUp(top, bottom, n int, bg Color) {
	if top < 0 {
		top = 0
	}
	if bottom >= s.Rows {
		bottom = s.Rows - 1
	}
	if top > bottom || n <= 0 {
		return
	}
	regionH := bottom - top + 1
	if n > regionH {
		n = regionH
	}
	if top == 0 && bottom == s.Rows-1 && s.MaxScrollback > 0 {
		for i := 0; i < n; i++ {
			s.pushScrollback(s.Lines[i])
		}
	}
	copy(s.Lines[top:bottom-n+1], s.Lines[top+n:bottom+1])
	for r := bottom - n + 1; r <= bottom; r++ {
		s.Lines[r] = makeBlankLine(s.Cols, bg, 0)
	}
}

// scrollDown scrolls lines within [top, bottom] down by n rows.
func (s *Screen) scrollDown(top, bottom, n int, bg Color) {
	if top < 0 {
		top = 0
	}
	if bottom >= s.Rows {
		bottom = s.Rows - 1
	}
	if top > bottom || n <= 0 {
		return
	}
	regionH := bottom - top + 1
	if n > regionH {
		n = regionH
	}
	copy(s.Lines[top+n:bottom+1], s.Lines[top:bottom-n+1])
	for r := top; r < top+n; r++ {
		s.Lines[r] = makeBlankLine(s.Cols, bg, 0)
	}
}

func (s *Screen) clearWideOverlap(row, col int, bg Color) {
	if row < 0 || row >= s.Rows || col < 0 || col >= s.Cols {
		return
	}
	cells := s.Lines[row].Cells
	if cells[col].Width == 0 && col > 0 && cells[col-1].Width == 2 {
		cells[col-1] = BlankCell(bg, 0)
	} else if cells[col].Width == 2 && col+1 < s.Cols && cells[col+1].Width == 0 {
		cells[col+1] = BlankCell(bg, 0)
	}
}

// ResizeNoReflow resizes the alternate screen without touching scrollback.
func (s *Screen) ResizeNoReflow(newRows, newCols int) {
	if newRows < 1 {
		newRows = 1
	}
	if newCols < 1 {
		newCols = 1
	}
	if newRows == s.Rows && newCols == s.Cols {
		return
	}

	newLines := make([]Line, newRows)
	for r := 0; r < newRows; r++ {
		if r < s.Rows {
			newLines[r] = resizeLineCells(s.Lines[r], newCols)
		} else {
			newLines[r] = makeBlankLine(newCols, Color{}, 0)
		}
	}
	s.Lines = newLines
	s.Rows = newRows
	s.Cols = newCols
	s.ScrollTop = 0
	s.ScrollBottom = newRows - 1
	s.TabStops = makeDefaultTabStops(newCols)
	if s.CursorRow >= newRows {
		s.CursorRow = newRows - 1
	}
	if s.CursorCol >= newCols {
		s.CursorCol = newCols - 1
	}
	s.PendingWrap = false
}

func resizeLineCells(line Line, newCols int) Line {
	out := make([]Cell, newCols)
	blank := BlankCell(Color{}, 0)
	copyN := len(line.Cells)
	if copyN > newCols {
		copyN = newCols
	}
	copy(out[:copyN], line.Cells[:copyN])
	for i := copyN; i < newCols; i++ {
		out[i] = blank
	}
	// Avoid orphaned wide-character lead cell at right edge.
	if newCols > 0 && out[newCols-1].Width == 2 {
		out[newCols-1] = blank
	}
	return Line{Cells: out, Wrapped: false}
}

// ResizeReflow resizes the main screen while reflowing soft-wrapped lines across
// scrollback and visible rows, preserving cursor location accurately.
func (s *Screen) ResizeReflow(newRows, newCols int) {
	if newRows < 1 {
		newRows = 1
	}
	if newCols < 1 {
		newCols = 1
	}
	if newRows == s.Rows && newCols == s.Cols {
		return
	}

	// Fast path when only height changed: move lines between scrollback and visible screen.
	if newCols == s.Cols {
		s.resizeHeightOnly(newRows)
		return
	}

	// Combine scrollback + visible rows up to the last non-empty row (or cursor row).
	lastContentRow := s.CursorRow
	for r := s.Rows - 1; r > lastContentRow; r-- {
		if s.Lines[r].trimmedLen() > 0 || s.Lines[r].Wrapped {
			lastContentRow = r
			break
		}
	}

	totalSrc := len(s.Scrollback) + lastContentRow + 1
	srcLines := make([]Line, 0, totalSrc)
	srcLines = append(srcLines, s.Scrollback...)
	srcLines = append(srcLines, s.Lines[:lastContentRow+1]...)

	cursorSrcLine := len(s.Scrollback) + s.CursorRow
	cursorSrcCol := s.CursorCol
	if s.PendingWrap {
		cursorSrcCol = s.Cols
	}

	var reflowed []Line
	newCursorGlobalRow := 0
	newCursorCol := 0
	cursorFound := false

	// Process logical lines (runs of consecutive lines joined by Wrapped == true).
	for i := 0; i < len(srcLines); {
		j := i
		for j < len(srcLines)-1 && srcLines[j].Wrapped {
			j++
		}

		// Collect cells for logical line [i..j] and track cursor cell offset if inside.
		var logical []Cell
		cursorOffset := -1
		for k := i; k <= j; k++ {
			line := srcLines[k]
			var end int
			if k < j {
				// Wrapped segment: keep full width, except skip trailing padding blank before wide char.
				end = len(line.Cells)
				if end > 0 && line.Cells[end-1].IsBlankDefault() && k+1 <= j && len(srcLines[k+1].Cells) > 0 && srcLines[k+1].Cells[0].Width == 2 {
					end--
				}
			} else {
				end = line.trimmedLen()
			}
			if k == cursorSrcLine {
				if cursorSrcCol <= end {
					cursorOffset = len(logical) + cursorSrcCol
				} else {
					// Pad up to cursor column so cursor stays on its column.
					padTo := cursorSrcCol
					if padTo > len(line.Cells) {
						padTo = len(line.Cells)
					}
					if padTo > end {
						end = padTo
					}
					cursorOffset = len(logical) + cursorSrcCol
				}
			}
			logical = append(logical, line.Cells[:end]...)
		}

		// Wrap logical cells into rows of width newCols.
		if len(logical) == 0 {
			if cursorOffset >= 0 {
				newCursorGlobalRow = len(reflowed)
				newCursorCol = 0
				cursorFound = true
			}
			reflowed = append(reflowed, makeBlankLine(newCols, Color{}, 0))
		} else {
			curLine := makeBlankLine(newCols, Color{}, 0)
			col := 0
			for idx := 0; idx < len(logical); idx++ {
				cell := logical[idx]
				if cell.Width == 0 {
					// Continuation cell already emitted with its lead cell.
					continue
				}
				w := int(cell.Width)
				if w < 1 {
					w = 1
				}
				if w == 2 && newCols < 2 {
					w = 1
					cell.Width = 1
				}
				if col+w > newCols {
					curLine.Wrapped = true
					reflowed = append(reflowed, curLine)
					curLine = makeBlankLine(newCols, Color{}, 0)
					col = 0
				}
				if cursorOffset == idx {
					newCursorGlobalRow = len(reflowed)
					newCursorCol = col
					cursorFound = true
				}
				curLine.Cells[col] = cell
				if w == 2 && col+1 < newCols {
					curLine.Cells[col+1] = Cell{Rune: 0, Width: 0, Style: cell.Style}
				}
				col += w
			}
			if cursorOffset >= len(logical) {
				if col >= newCols {
					// Cursor sits at right margin (pending wrap or next line).
					newCursorGlobalRow = len(reflowed)
					newCursorCol = newCols - 1
				} else {
					newCursorGlobalRow = len(reflowed)
					newCursorCol = col
				}
				cursorFound = true
			}
			reflowed = append(reflowed, curLine)
		}

		i = j + 1
	}

	// Split reflowed lines into new Scrollback and visible Lines (len == newRows).
	if len(reflowed) <= newRows {
		s.Scrollback = s.Scrollback[:0]
		s.Lines = make([]Line, newRows)
		copy(s.Lines, reflowed)
		for r := len(reflowed); r < newRows; r++ {
			s.Lines[r] = makeBlankLine(newCols, Color{}, 0)
		}
		if cursorFound {
			s.CursorRow = newCursorGlobalRow
			s.CursorCol = newCursorCol
		}
	} else {
		sbCount := len(reflowed) - newRows
		if sbCount > s.MaxScrollback {
			trim := sbCount - s.MaxScrollback
			reflowed = reflowed[trim:]
			sbCount = s.MaxScrollback
			newCursorGlobalRow -= trim
		}
		s.Scrollback = make([]Line, sbCount)
		copy(s.Scrollback, reflowed[:sbCount])
		s.Lines = make([]Line, newRows)
		copy(s.Lines, reflowed[sbCount:sbCount+newRows])
		if cursorFound {
			s.CursorRow = newCursorGlobalRow - sbCount
			s.CursorCol = newCursorCol
		}
	}

	s.Rows = newRows
	s.Cols = newCols
	s.ScrollTop = 0
	s.ScrollBottom = newRows - 1
	s.TabStops = makeDefaultTabStops(newCols)
	if s.CursorRow < 0 {
		s.CursorRow = 0
	}
	if s.CursorRow >= newRows {
		s.CursorRow = newRows - 1
	}
	if s.CursorCol < 0 {
		s.CursorCol = 0
	}
	if s.CursorCol >= newCols {
		s.CursorCol = newCols - 1
	}
	s.PendingWrap = false
}

func (s *Screen) resizeHeightOnly(newRows int) {
	if newRows > s.Rows {
		grow := newRows - s.Rows
		// Pull lines back from scrollback first (matches Ghostty/Kitty/Alacritty/xterm behavior).
		pull := grow
		if pull > len(s.Scrollback) {
			pull = len(s.Scrollback)
		}
		newLines := make([]Line, 0, newRows)
		if pull > 0 {
			sbStart := len(s.Scrollback) - pull
			newLines = append(newLines, s.Scrollback[sbStart:]...)
			s.Scrollback = s.Scrollback[:sbStart]
			s.CursorRow += pull
		}
		newLines = append(newLines, s.Lines...)
		for len(newLines) < newRows {
			newLines = append(newLines, makeBlankLine(s.Cols, Color{}, 0))
		}
		s.Lines = newLines
	} else {
		shrink := s.Rows - newRows
		// Trim blank lines below cursor first, then push top rows to scrollback.
		trailingBlank := 0
		for r := s.Rows - 1; r > s.CursorRow && trailingBlank < shrink; r-- {
			if s.Lines[r].trimmedLen() == 0 && !s.Lines[r].Wrapped {
				trailingBlank++
			} else {
				break
			}
		}
		pushTop := shrink - trailingBlank
		if pushTop > 0 {
			for i := 0; i < pushTop; i++ {
				s.pushScrollback(s.Lines[i])
			}
			s.CursorRow -= pushTop
			if s.CursorRow < 0 {
				s.CursorRow = 0
			}
		}
		newLines := make([]Line, newRows)
		copy(newLines, s.Lines[pushTop:pushTop+newRows])
		s.Lines = newLines
	}
	s.Rows = newRows
	s.ScrollTop = 0
	s.ScrollBottom = newRows - 1
	if s.CursorRow >= newRows {
		s.CursorRow = newRows - 1
	}
	s.PendingWrap = false
}
