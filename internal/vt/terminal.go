package vt

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// QueryResponder is called when a terminal query arrives from the PTY
// while the terminal is in detached mode (or whenever query replies are enabled).
type QueryResponder func(reply []byte)

// Terminal is a full shadow VT100/xterm/Kitty terminal state machine.
// It passively mirrors PTY output so mplex can replay scrollback, screens,
// and active terminal modes atomically when switching or reattaching sessions.
type Terminal struct {
	parser parser

	Main *Screen
	Alt  *Screen

	InAltScreen bool
	Style       Style
	Links       HyperlinkTable

	// Standard / DEC modes
	AutoWrap        bool // ?7 DECAWM (default true)
	OriginMode      bool // ?6 DECOM
	CursorVisible   bool // ?25 DECTCEM (default true)
	CursorBlink     bool // ?12
	AppCursorKeys   bool // ?1 DECCKM
	AppKeypad       bool // ESC = / ESC >
	InsertMode      bool // ANSI 4 IRM
	AutoNewline     bool // ANSI 20 LNM
	BracketedPaste  bool // ?2004
	FocusEvents     bool // ?1004
	SyncOutput      bool // ?2026
	MouseMode       int  // 0, 1000, 1002, 1003
	MouseEncoding   int  // 0 (X10), 1005 (UTF8), 1006 (SGR), 1015 (URXVT), 1016 (SGR-Pixels)
	CursorStyle     int  // DECSCUSR 0..6
	ModifyOtherKeys int  // CSI > 4 ; m

	// Character sets (G0/G1: 'B' = ASCII, '0' = DEC Special Graphics)
	CharsetG0     byte
	CharsetG1     byte
	ActiveCharset uint8 // 0 = G0, 1 = G1

	// Last printed rune for CSI Ps b (REP)
	lastRune rune

	// Metadata from OSC sequences
	Title     string
	IconTitle string
	Cwd       string

	// Cached responses snooped from real outer terminal for detached queries
	CachedOSC10 string
	CachedOSC11 string
	CachedDA1   string
	CachedDA2   string

	// PendingDA1 tracks whether a DA1 query (CSI 0 c / CSI c) was emitted while
	// OnQueryReply was nil (i.e. forwarded to an attached outer terminal) and has
	// not yet been answered via SnoopClientInput.
	PendingDA1 bool

	// Generation counter incremented on every Write (lets client skip replay if unchanged)
	Generation uint64
	// BellCount is incremented whenever a BEL (0x07) control character is executed.
	BellCount uint64

	// Optional callback for replying to queries when detached (nil when client is attached)
	OnQueryReply QueryResponder
}

// New creates a new shadow terminal of the given dimensions and scrollback capacity.
func New(rows, cols, maxScrollback int) *Terminal {
	if rows < 1 {
		rows = 24
	}
	if cols < 1 {
		cols = 80
	}
	if maxScrollback < 0 {
		maxScrollback = 0
	}
	t := &Terminal{
		Main:          newScreen(rows, cols, maxScrollback),
		Alt:           newScreen(rows, cols, 0),
		AutoWrap:      true,
		CursorVisible: true,
		CharsetG0:     'B',
		CharsetG1:     'B',
	}
	return t
}

// Active returns the currently active screen (Alt or Main).
func (t *Terminal) Active() *Screen {
	if t.InAltScreen {
		return t.Alt
	}
	return t.Main
}

// Rows returns the terminal row count.
func (t *Terminal) Rows() int { return t.Main.Rows }

// Cols returns the terminal column count.
func (t *Terminal) Cols() int { return t.Main.Cols }

// Write feeds raw PTY output bytes into the shadow VT parser.
func (t *Terminal) Write(data []byte) {
	if len(data) == 0 {
		return
	}
	t.Generation++
	t.parser.feed(data, t)
}

// Resize resizes both main (with soft-wrap reflow) and alt screens.
func (t *Terminal) Resize(rows, cols int) {
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	if rows == t.Main.Rows && cols == t.Main.Cols {
		return
	}
	t.Main.ResizeReflow(rows, cols)
	t.Alt.ResizeNoReflow(rows, cols)
	t.Generation++
}

// SnoopClientInput passively inspects responses sent from the real outer terminal
// to the PTY (while attached) to cache color/DA replies for future detached queries.
// Never modifies or blocks the byte stream.
func (t *Terminal) SnoopClientInput(data []byte) {
	if len(data) < 5 {
		return
	}
	s := string(data)
	// Snoop DA1 replies: \x1b[?<digits/semicolons>c
	if idx := strings.Index(s, "\x1b[?"); idx >= 0 {
		if da, ok := parseDA1Reply(s[idx:]); ok {
			t.CachedDA1 = da
			t.PendingDA1 = false
		}
	}
	// Quick scan for OSC 10/11 color responses: \x1b]10;rgb:...\x07 or \x1b\\
	if len(data) < 8 {
		return
	}
	if idx := strings.Index(s, "\x1b]10;"); idx >= 0 {
		if end := findOscEnd(s[idx+5:]); end > 0 {
			val := s[idx+5 : idx+5+end]
			if val != "?" {
				t.CachedOSC10 = val
			}
		}
	}
	if idx := strings.Index(s, "\x1b]11;"); idx >= 0 {
		if end := findOscEnd(s[idx+5:]); end > 0 {
			val := s[idx+5 : idx+5+end]
			if val != "?" {
				t.CachedOSC11 = val
			}
		}
	}
}

func parseDA1Reply(s string) (string, bool) {
	if !strings.HasPrefix(s, "\x1b[?") || len(s) < 5 {
		return "", false
	}
	hasDigit := false
	for i := 3; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			hasDigit = true
			continue
		}
		if c == ';' {
			continue
		}
		if c == 'c' && hasDigit {
			return s[:i+1], true
		}
		return "", false
	}
	return "", false
}

// DefaultDA1Reply returns the cached DA1 reply from the real outer terminal,
// or a safe VT220 fallback if none has been observed yet.
func (t *Terminal) DefaultDA1Reply() string {
	if t.CachedDA1 != "" {
		return t.CachedDA1
	}
	return "\x1b[?62;22c"
}

func findOscEnd(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x07 {
			return i
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i
		}
	}
	return -1
}

func (t *Terminal) resetAll() {
	rows, cols := t.Main.Rows, t.Main.Cols
	t.Main.reset(rows, cols)
	t.Alt.reset(rows, cols)
	t.InAltScreen = false
	t.Style = Style{}
	t.AutoWrap = true
	t.OriginMode = false
	t.CursorVisible = true
	t.CursorBlink = false
	t.AppCursorKeys = false
	t.AppKeypad = false
	t.InsertMode = false
	t.AutoNewline = false
	t.BracketedPaste = false
	t.FocusEvents = false
	t.SyncOutput = false
	t.MouseMode = 0
	t.MouseEncoding = 0
	t.CursorStyle = 0
	t.ModifyOtherKeys = 0
	t.CharsetG0 = 'B'
	t.CharsetG1 = 'B'
	t.ActiveCharset = 0
	t.lastRune = 0
}

func (t *Terminal) softReset() {
	s := t.Active()
	t.CursorVisible = true
	t.OriginMode = false
	t.AutoWrap = true
	t.InsertMode = false
	t.AppCursorKeys = false
	t.AppKeypad = false
	t.Style = Style{}
	s.ScrollTop = 0
	s.ScrollBottom = s.Rows - 1
	s.Saved = SavedCursor{}
	s.PendingWrap = false
}

func (t *Terminal) reply(s string) {
	if t.OnQueryReply != nil {
		t.OnQueryReply([]byte(s))
	}
}

func (t *Terminal) currentCharset() byte {
	if t.ActiveCharset == 1 {
		return t.CharsetG1
	}
	return t.CharsetG0
}

// DEC Special Graphics character set (box drawing, used by ncurses/dialog/etc.)
var decSpecialGraphics = map[rune]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°',
	'g': '±', 'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐', 'l': '┌', 'm': '└',
	'n': '┼', 'o': '⎺', 'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽', 't': '├',
	'u': '┤', 'v': '┴', 'w': '┬', 'x': '│', 'y': '≤', 'z': '≥', '{': 'π',
	'|': '≠', '}': '£', '~': '·',
}

func (t *Terminal) printRune(r rune) {
	if t.currentCharset() == '0' {
		if mapped, ok := decSpecialGraphics[r]; ok {
			r = mapped
		}
	}

	s := t.Active()
	w := runewidth.RuneWidth(r)

	// Zero-width combining characters / variation selectors / ZWJ attach to previous cell.
	if w == 0 {
		// Skip non-combining format/control junk like NUL/BOM.
		if r == 0 || r == '\uFEFF' {
			return
		}
		targetRow := s.CursorRow
		targetCol := s.CursorCol
		if !s.PendingWrap {
			if targetCol == 0 {
				return
			}
			targetCol--
		}
		if targetRow >= 0 && targetRow < s.Rows && targetCol >= 0 && targetCol < s.Cols {
			if s.Lines[targetRow].Cells[targetCol].Width == 0 && targetCol > 0 {
				targetCol--
			}
			cell := &s.Lines[targetRow].Cells[targetCol]
			if len(cell.Extra) < 32 {
				var buf [4]byte
				n := utf8.EncodeRune(buf[:], r)
				cell.Extra += string(buf[:n])
			}
		}
		return
	}

	t.lastRune = r
	if w > 2 {
		w = 2
	}
	if w == 2 && s.Cols < 2 {
		w = 1
	}

	// Execute pending wrap before placing the new glyph.
	if s.PendingWrap {
		if t.AutoWrap {
			s.Lines[s.CursorRow].Wrapped = true
			s.CursorCol = 0
			t.index()
		}
		s.PendingWrap = false
	}

	// If a wide char doesn't fit in the final column, wrap early.
	if w == 2 && s.CursorCol == s.Cols-1 {
		if t.AutoWrap {
			s.clearWideOverlap(s.CursorRow, s.CursorCol, t.Style.Bg)
			s.Lines[s.CursorRow].Cells[s.CursorCol] = BlankCell(t.Style.Bg, 0)
			s.Lines[s.CursorRow].Wrapped = true
			s.CursorCol = 0
			t.index()
		} else {
			return
		}
	}

	// Handle Insert Mode (IRM): shift existing cells right.
	if t.InsertMode {
		row := &s.Lines[s.CursorRow]
		if s.CursorCol+w < s.Cols {
			copy(row.Cells[s.CursorCol+w:], row.Cells[s.CursorCol:s.Cols-w])
		}
		if row.Cells[s.Cols-1].Width == 2 {
			row.Cells[s.Cols-1] = BlankCell(t.Style.Bg, 0)
		}
	}

	// Clear any overwritten wide-character halves.
	s.clearWideOverlap(s.CursorRow, s.CursorCol, t.Style.Bg)
	if w == 2 && s.CursorCol+1 < s.Cols {
		s.clearWideOverlap(s.CursorRow, s.CursorCol+1, t.Style.Bg)
	}

	row := &s.Lines[s.CursorRow]
	row.Cells[s.CursorCol] = Cell{
		Rune:  r,
		Width: uint8(w),
		Style: t.Style,
	}
	if w == 2 && s.CursorCol+1 < s.Cols {
		row.Cells[s.CursorCol+1] = Cell{
			Rune:  0,
			Width: 0,
			Style: t.Style,
		}
	}

	if s.CursorCol+w >= s.Cols {
		s.CursorCol = s.Cols - 1
		if t.AutoWrap {
			s.PendingWrap = true
		}
	} else {
		s.CursorCol += w
	}
}

func (t *Terminal) execute(b byte) {
	s := t.Active()
	switch b {
	case 0x07: // BEL
		t.BellCount++
	case 0x08: // BS
		s.PendingWrap = false
		if s.CursorCol > 0 {
			s.CursorCol--
		}
	case 0x09: // HT (Tab)
		s.PendingWrap = false
		col := s.CursorCol + 1
		for col < s.Cols-1 && !s.TabStops[col] {
			col++
		}
		if col >= s.Cols {
			col = s.Cols - 1
		}
		s.CursorCol = col
	case 0x0A, 0x0B, 0x0C: // LF, VT, FF
		s.PendingWrap = false
		s.Lines[s.CursorRow].Wrapped = false
		t.index()
		if t.AutoNewline {
			s.CursorCol = 0
		}
	case 0x0D: // CR
		s.PendingWrap = false
		s.CursorCol = 0
	case 0x0E: // SO (Shift Out -> G1)
		t.ActiveCharset = 1
	case 0x0F: // SI (Shift In -> G0)
		t.ActiveCharset = 0
	}
}

// index moves cursor down one line, scrolling within scroll region if at ScrollBottom.
func (t *Terminal) index() {
	s := t.Active()
	if s.CursorRow == s.ScrollBottom {
		s.scrollUp(s.ScrollTop, s.ScrollBottom, 1, t.Style.Bg)
	} else if s.CursorRow < s.Rows-1 {
		s.CursorRow++
	}
}

// reverseIndex moves cursor up one line, scrolling down if at ScrollTop.
func (t *Terminal) reverseIndex() {
	s := t.Active()
	if s.CursorRow == s.ScrollTop {
		s.scrollDown(s.ScrollTop, s.ScrollBottom, 1, t.Style.Bg)
	} else if s.CursorRow > 0 {
		s.CursorRow--
	}
}

func (t *Terminal) saveCursor() {
	s := t.Active()
	s.Saved = SavedCursor{
		Valid:         true,
		Row:           s.CursorRow,
		Col:           s.CursorCol,
		Style:         t.Style,
		OriginMode:    t.OriginMode,
		AutoWrap:      t.AutoWrap,
		CharsetG0:     t.CharsetG0,
		CharsetG1:     t.CharsetG1,
		ActiveCharset: t.ActiveCharset,
	}
}

func (t *Terminal) restoreCursor() {
	s := t.Active()
	if !s.Saved.Valid {
		s.CursorRow = 0
		s.CursorCol = 0
		s.PendingWrap = false
		return
	}
	s.CursorRow = clamp(s.Saved.Row, 0, s.Rows-1)
	s.CursorCol = clamp(s.Saved.Col, 0, s.Cols-1)
	s.PendingWrap = false
	t.Style = s.Saved.Style
	t.OriginMode = s.Saved.OriginMode
	t.AutoWrap = s.Saved.AutoWrap
	t.CharsetG0 = s.Saved.CharsetG0
	t.CharsetG1 = s.Saved.CharsetG1
	t.ActiveCharset = s.Saved.ActiveCharset
}

func (t *Terminal) escDispatch(intermediates []byte, final byte) {
	s := t.Active()
	if len(intermediates) == 0 {
		switch final {
		case '7': // DECSC
			t.saveCursor()
		case '8': // DECRC
			t.restoreCursor()
		case 'D': // IND
			s.PendingWrap = false
			t.index()
		case 'E': // NEL
			s.PendingWrap = false
			s.Lines[s.CursorRow].Wrapped = false
			s.CursorCol = 0
			t.index()
		case 'H': // HTS
			if s.CursorCol >= 0 && s.CursorCol < len(s.TabStops) {
				s.TabStops[s.CursorCol] = true
			}
		case 'M': // RI
			s.PendingWrap = false
			t.reverseIndex()
		case 'c': // RIS (Full Reset)
			t.resetAll()
		case '=': // DECKPAM
			t.AppKeypad = true
		case '>': // DECKPNM
			t.AppKeypad = false
		}
		return
	}

	switch intermediates[0] {
	case '(':
		t.CharsetG0 = final
	case ')':
		t.CharsetG1 = final
	case '#':
		if final == '8' { // DECALN (fill screen with 'E')
			for r := 0; r < s.Rows; r++ {
				s.Lines[r].Wrapped = false
				for c := 0; c < s.Cols; c++ {
					s.Lines[r].Cells[c] = Cell{Rune: 'E', Width: 1}
				}
			}
			s.CursorRow = 0
			s.CursorCol = 0
			s.PendingWrap = false
		}
	}
}

func (t *Terminal) csiDispatch(private byte, intermediates []byte, params *CSIParams, final byte) {
	s := t.Active()

	// Kitty keyboard protocol: CSI > flags u, CSI < count u, CSI = flags ; mode u, CSI ? u
	if final == 'u' && len(intermediates) == 0 && private != 0 {
		t.handleKittyKeyboard(private, params)
		return
	}

	// Private DEC sequences (CSI ? ...)
	if private == '?' {
		if len(intermediates) == 1 && intermediates[0] == '$' && final == 'p' {
			// DECRQM: query DEC private mode
			mode := params.Get(0, 0)
			status := t.queryDECMode(mode)
			t.reply(fmt.Sprintf("\x1b[?%d;%d$y", mode, status))
			return
		}
		switch final {
		case 'h':
			for i := 0; i < params.Len(); i++ {
				t.setDECMode(params.Get(i, 0), true)
			}
		case 'l':
			for i := 0; i < params.Len(); i++ {
				t.setDECMode(params.Get(i, 0), false)
			}
		case 'J': // DECSED
			t.eraseInDisplay(params.Get(0, 0))
		case 'K': // DECSEL
			t.eraseInLine(params.Get(0, 0))
		case 'n': // DECDSR
			if params.Get(0, 0) == 6 {
				row := s.CursorRow + 1
				if t.OriginMode {
					row = s.CursorRow - s.ScrollTop + 1
				}
				t.reply(fmt.Sprintf("\x1b[?%d;%dR", row, s.CursorCol+1))
			}
		}
		return
	}

	// Private '>' sequences (Secondary DA, modifyOtherKeys, XTVERSION)
	if private == '>' {
		switch final {
		case 'c': // DA2
			if t.CachedDA2 != "" {
				t.reply(t.CachedDA2)
			} else {
				t.reply("\x1b[>1;10;0c")
			}
		case 'm': // XTMODKEYS (modifyOtherKeys)
			if params.Get(0, 0) == 4 {
				t.ModifyOtherKeys = params.Get(1, 0)
			}
		case 'q': // XTVERSION
			t.reply("\x1bP>|mplex(0.1)\x1b\\")
		}
		return
	}

	if private != 0 {
		return
	}

	// Sequences with intermediates
	if len(intermediates) > 0 {
		switch {
		case intermediates[0] == ' ' && final == 'q': // DECSCUSR (Cursor style)
			style := params.Get(0, 0)
			if style >= 0 && style <= 6 {
				t.CursorStyle = style
			}
		case intermediates[0] == '!' && final == 'p': // DECSTR (Soft reset)
			t.softReset()
		}
		return
	}

	// Standard ECMA-48 / VT100 CSI sequences
	switch final {
	case '@': // ICH - Insert Blank Characters
		n := params.GetPos(0, 1)
		t.insertChars(n)
	case 'A': // CUU - Cursor Up
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		top := 0
		if s.CursorRow >= s.ScrollTop {
			top = s.ScrollTop
		}
		s.CursorRow = clamp(s.CursorRow-n, top, s.Rows-1)
	case 'B', 'e': // CUD / VPR - Cursor Down
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		bot := s.Rows - 1
		if s.CursorRow <= s.ScrollBottom {
			bot = s.ScrollBottom
		}
		s.CursorRow = clamp(s.CursorRow+n, 0, bot)
	case 'C', 'a': // CUF / HPR - Cursor Forward
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		s.CursorCol = clamp(s.CursorCol+n, 0, s.Cols-1)
	case 'D': // CUB - Cursor Backward
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		s.CursorCol = clamp(s.CursorCol-n, 0, s.Cols-1)
	case 'E': // CNL - Cursor Next Line
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		s.CursorCol = 0
		s.CursorRow = clamp(s.CursorRow+n, 0, s.Rows-1)
	case 'F': // CPL - Cursor Previous Line
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		s.CursorCol = 0
		s.CursorRow = clamp(s.CursorRow-n, 0, s.Rows-1)
	case 'G', '`': // CHA / HPA - Cursor Horizontal Absolute
		col := params.GetPos(0, 1) - 1
		s.PendingWrap = false
		s.CursorCol = clamp(col, 0, s.Cols-1)
	case 'H', 'f': // CUP / HVP - Cursor Position
		row := params.GetPos(0, 1) - 1
		col := params.GetPos(1, 1) - 1
		t.moveCursorAbs(row, col)
	case 'I': // CHT - Cursor Forward Tabulation
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		for i := 0; i < n; i++ {
			col := s.CursorCol + 1
			for col < s.Cols-1 && !s.TabStops[col] {
				col++
			}
			s.CursorCol = clamp(col, 0, s.Cols-1)
		}
	case 'J': // ED - Erase in Display
		t.eraseInDisplay(params.Get(0, 0))
	case 'K': // EL - Erase in Line
		t.eraseInLine(params.Get(0, 0))
	case 'L': // IL - Insert Lines
		n := params.GetPos(0, 1)
		if s.CursorRow >= s.ScrollTop && s.CursorRow <= s.ScrollBottom {
			s.PendingWrap = false
			s.CursorCol = 0
			s.scrollDown(s.CursorRow, s.ScrollBottom, n, t.Style.Bg)
		}
	case 'M': // DL - Delete Lines
		n := params.GetPos(0, 1)
		if s.CursorRow >= s.ScrollTop && s.CursorRow <= s.ScrollBottom {
			s.PendingWrap = false
			s.CursorCol = 0
			s.scrollUp(s.CursorRow, s.ScrollBottom, n, t.Style.Bg)
		}
	case 'P': // DCH - Delete Characters
		n := params.GetPos(0, 1)
		t.deleteChars(n)
	case 'S': // SU - Scroll Up
		n := params.GetPos(0, 1)
		s.scrollUp(s.ScrollTop, s.ScrollBottom, n, t.Style.Bg)
	case 'T': // SD - Scroll Down
		if params.Len() <= 1 {
			n := params.GetPos(0, 1)
			s.scrollDown(s.ScrollTop, s.ScrollBottom, n, t.Style.Bg)
		}
	case 'X': // ECH - Erase Characters
		n := params.GetPos(0, 1)
		t.eraseChars(n)
	case 'Z': // CBT - Cursor Backward Tabulation
		n := params.GetPos(0, 1)
		s.PendingWrap = false
		for i := 0; i < n; i++ {
			col := s.CursorCol - 1
			for col > 0 && !s.TabStops[col] {
				col--
			}
			s.CursorCol = clamp(col, 0, s.Cols-1)
		}
	case 'b': // REP - Repeat Preceding Graphic Character
		if t.lastRune != 0 {
			n := params.GetPos(0, 1)
			if n > s.Cols*s.Rows {
				n = s.Cols * s.Rows
			}
			r := t.lastRune
			for i := 0; i < n; i++ {
				t.printRune(r)
			}
		}
	case 'c': // DA1 - Primary Device Attributes
		if params.Get(0, 0) == 0 {
			if t.OnQueryReply != nil {
				t.PendingDA1 = false
				t.reply(t.DefaultDA1Reply())
			} else {
				t.PendingDA1 = true
			}
		}
	case 'd': // VPA - Vertical Line Position Absolute
		row := params.GetPos(0, 1) - 1
		s.PendingWrap = false
		if t.OriginMode {
			s.CursorRow = clamp(s.ScrollTop+row, s.ScrollTop, s.ScrollBottom)
		} else {
			s.CursorRow = clamp(row, 0, s.Rows-1)
		}
	case 'g': // TBC - Tab Clear
		switch params.Get(0, 0) {
		case 0:
			if s.CursorCol >= 0 && s.CursorCol < len(s.TabStops) {
				s.TabStops[s.CursorCol] = false
			}
		case 3:
			for i := range s.TabStops {
				s.TabStops[i] = false
			}
		}
	case 'h': // SM - Set ANSI Mode
		for i := 0; i < params.Len(); i++ {
			t.setANSIMode(params.Get(i, 0), true)
		}
	case 'l': // RM - Reset ANSI Mode
		for i := 0; i < params.Len(); i++ {
			t.setANSIMode(params.Get(i, 0), false)
		}
	case 'm': // SGR - Select Graphic Rendition
		t.applySGR(params)
	case 'n': // DSR - Device Status Report
		switch params.Get(0, 0) {
		case 5:
			t.reply("\x1b[0n")
		case 6:
			row := s.CursorRow + 1
			if t.OriginMode {
				row = s.CursorRow - s.ScrollTop + 1
			}
			t.reply(fmt.Sprintf("\x1b[%d;%dR", row, s.CursorCol+1))
		}
	case 'r': // DECSTBM - Set Top and Bottom Margins
		top := params.GetPos(0, 1) - 1
		bot := params.GetPos(1, s.Rows) - 1
		if top < 0 {
			top = 0
		}
		if bot >= s.Rows {
			bot = s.Rows - 1
		}
		if top < bot {
			s.ScrollTop = top
			s.ScrollBottom = bot
			t.moveCursorAbs(0, 0)
		}
	case 's': // SCOSC - Save Cursor Position
		if params.Len() == 0 {
			t.saveCursor()
		}
	case 'u': // SCORC - Restore Cursor Position (no private prefix)
		t.restoreCursor()
	}
}

func (t *Terminal) moveCursorAbs(row, col int) {
	s := t.Active()
	s.PendingWrap = false
	if t.OriginMode {
		s.CursorRow = clamp(s.ScrollTop+row, s.ScrollTop, s.ScrollBottom)
	} else {
		s.CursorRow = clamp(row, 0, s.Rows-1)
	}
	s.CursorCol = clamp(col, 0, s.Cols-1)
}

func (t *Terminal) insertChars(n int) {
	s := t.Active()
	s.PendingWrap = false
	row := &s.Lines[s.CursorRow]
	row.Wrapped = false
	if n > s.Cols-s.CursorCol {
		n = s.Cols - s.CursorCol
	}
	s.clearWideOverlap(s.CursorRow, s.CursorCol, t.Style.Bg)
	copy(row.Cells[s.CursorCol+n:], row.Cells[s.CursorCol:s.Cols-n])
	blank := BlankCell(t.Style.Bg, 0)
	for c := s.CursorCol; c < s.CursorCol+n; c++ {
		row.Cells[c] = blank
	}
	if row.Cells[s.Cols-1].Width == 2 {
		row.Cells[s.Cols-1] = blank
	}
}

func (t *Terminal) deleteChars(n int) {
	s := t.Active()
	s.PendingWrap = false
	row := &s.Lines[s.CursorRow]
	row.Wrapped = false
	if n > s.Cols-s.CursorCol {
		n = s.Cols - s.CursorCol
	}
	s.clearWideOverlap(s.CursorRow, s.CursorCol, t.Style.Bg)
	if s.CursorCol+n < s.Cols {
		s.clearWideOverlap(s.CursorRow, s.CursorCol+n, t.Style.Bg)
	}
	copy(row.Cells[s.CursorCol:], row.Cells[s.CursorCol+n:])
	blank := BlankCell(t.Style.Bg, 0)
	for c := s.Cols - n; c < s.Cols; c++ {
		row.Cells[c] = blank
	}
}

func (t *Terminal) eraseChars(n int) {
	s := t.Active()
	s.PendingWrap = false
	row := &s.Lines[s.CursorRow]
	end := s.CursorCol + n
	if end > s.Cols {
		end = s.Cols
	}
	blank := BlankCell(t.Style.Bg, 0)
	for c := s.CursorCol; c < end; c++ {
		s.clearWideOverlap(s.CursorRow, c, t.Style.Bg)
		row.Cells[c] = blank
	}
}

func (t *Terminal) eraseInLine(mode int) {
	s := t.Active()
	s.PendingWrap = false
	row := &s.Lines[s.CursorRow]
	blank := BlankCell(t.Style.Bg, 0)
	switch mode {
	case 0: // Cursor to end of line
		row.Wrapped = false
		s.clearWideOverlap(s.CursorRow, s.CursorCol, t.Style.Bg)
		for c := s.CursorCol; c < s.Cols; c++ {
			row.Cells[c] = blank
		}
	case 1: // Start of line to cursor (inclusive)
		s.clearWideOverlap(s.CursorRow, s.CursorCol, t.Style.Bg)
		for c := 0; c <= s.CursorCol && c < s.Cols; c++ {
			row.Cells[c] = blank
		}
	case 2: // Entire line
		row.Wrapped = false
		for c := 0; c < s.Cols; c++ {
			row.Cells[c] = blank
		}
	}
}

func (t *Terminal) eraseInDisplay(mode int) {
	s := t.Active()
	s.PendingWrap = false
	switch mode {
	case 0: // Cursor to end of display
		t.eraseInLine(0)
		for r := s.CursorRow + 1; r < s.Rows; r++ {
			s.Lines[r] = makeBlankLine(s.Cols, t.Style.Bg, 0)
		}
	case 1: // Start of display to cursor
		for r := 0; r < s.CursorRow; r++ {
			s.Lines[r] = makeBlankLine(s.Cols, t.Style.Bg, 0)
		}
		t.eraseInLine(1)
	case 2: // Entire visible display
		for r := 0; r < s.Rows; r++ {
			s.Lines[r] = makeBlankLine(s.Cols, t.Style.Bg, 0)
		}
	case 3: // Clear scrollback buffer (xterm CSI 3 J)
		s.Scrollback = s.Scrollback[:0]
	}
}

func (t *Terminal) setANSIMode(mode int, enabled bool) {
	switch mode {
	case 4:
		t.InsertMode = enabled
	case 20:
		t.AutoNewline = enabled
	}
}

func (t *Terminal) setDECMode(mode int, enabled bool) {
	switch mode {
	case 1:
		t.AppCursorKeys = enabled
	case 6:
		t.OriginMode = enabled
		t.moveCursorAbs(0, 0)
	case 7:
		t.AutoWrap = enabled
	case 12:
		t.CursorBlink = enabled
	case 25:
		t.CursorVisible = enabled
	case 47, 1047:
		if enabled && !t.InAltScreen {
			t.InAltScreen = true
			t.Alt.reset(t.Main.Rows, t.Main.Cols)
		} else if !enabled && t.InAltScreen {
			t.InAltScreen = false
		}
	case 1048:
		if enabled {
			t.saveCursor()
		} else {
			t.restoreCursor()
		}
	case 1049:
		if enabled {
			if !t.InAltScreen {
				t.saveCursor()
				t.InAltScreen = true
				t.Alt.reset(t.Main.Rows, t.Main.Cols)
			}
		} else {
			if t.InAltScreen {
				t.InAltScreen = false
				t.restoreCursor()
			}
		}
	case 1000, 1002, 1003:
		if enabled {
			t.MouseMode = mode
		} else if t.MouseMode == mode {
			t.MouseMode = 0
		}
	case 1004:
		t.FocusEvents = enabled
	case 1005, 1006, 1015, 1016:
		if enabled {
			t.MouseEncoding = mode
		} else if t.MouseEncoding == mode {
			t.MouseEncoding = 0
		}
	case 2004:
		t.BracketedPaste = enabled
	case 2026:
		t.SyncOutput = enabled
	}
}

func (t *Terminal) queryDECMode(mode int) int {
	// DECRPM status: 0 = not recognized, 1 = set, 2 = reset
	boolStatus := func(b bool) int {
		if b {
			return 1
		}
		return 2
	}
	switch mode {
	case 1:
		return boolStatus(t.AppCursorKeys)
	case 6:
		return boolStatus(t.OriginMode)
	case 7:
		return boolStatus(t.AutoWrap)
	case 12:
		return boolStatus(t.CursorBlink)
	case 25:
		return boolStatus(t.CursorVisible)
	case 47, 1047, 1049:
		return boolStatus(t.InAltScreen)
	case 1000, 1002, 1003:
		return boolStatus(t.MouseMode == mode)
	case 1004:
		return boolStatus(t.FocusEvents)
	case 1005, 1006, 1015, 1016:
		return boolStatus(t.MouseEncoding == mode)
	case 2004:
		return boolStatus(t.BracketedPaste)
	case 2026:
		return boolStatus(t.SyncOutput)
	default:
		return 0
	}
}

func (t *Terminal) handleKittyKeyboard(private byte, params *CSIParams) {
	s := t.Active()
	switch private {
	case '?': // Query current flags
		var cur uint32
		if len(s.KittyStack) > 0 {
			cur = s.KittyStack[len(s.KittyStack)-1]
		}
		t.reply(fmt.Sprintf("\x1b[?%du", cur))
	case '>': // Push flags onto stack
		flags := uint32(params.Get(0, 0))
		if len(s.KittyStack) < 64 {
			s.KittyStack = append(s.KittyStack, flags)
		} else {
			// Per Kitty spec, if stack is full, evict oldest entry.
			copy(s.KittyStack, s.KittyStack[1:])
			s.KittyStack[len(s.KittyStack)-1] = flags
		}
	case '<': // Pop n entries from stack
		n := params.GetPos(0, 1)
		if n >= len(s.KittyStack) {
			s.KittyStack = s.KittyStack[:0]
		} else {
			s.KittyStack = s.KittyStack[:len(s.KittyStack)-n]
		}
	case '=': // Set/modify current flags: CSI = flags ; mode u
		flags := uint32(params.Get(0, 0))
		mode := params.Get(1, 1) // 1=replace, 2=OR, 3=AND-NOT
		if len(s.KittyStack) == 0 {
			s.KittyStack = append(s.KittyStack, 0)
		}
		top := &s.KittyStack[len(s.KittyStack)-1]
		switch mode {
		case 1:
			*top = flags
		case 2:
			*top |= flags
		case 3:
			*top &^= flags
		}
	}
}

func (t *Terminal) applySGR(params *CSIParams) {
	if params.Len() == 0 {
		// Preserve active OSC 8 hyperlink across SGR 0!
		hl := t.Style.HyperlinkID
		t.Style = Style{HyperlinkID: hl}
		return
	}

	for i := 0; i < params.Len(); i++ {
		code := params.Get(i, 0)
		switch code {
		case 0:
			hl := t.Style.HyperlinkID
			t.Style = Style{HyperlinkID: hl}
		case 1:
			t.Style.Attrs |= AttrBold
		case 2:
			t.Style.Attrs |= AttrFaint
		case 3:
			t.Style.Attrs |= AttrItalic
		case 4:
			if params.SubLen(i) >= 2 {
				sub := params.Sub(i, 1, 1)
				if sub >= 0 && sub <= 5 {
					t.Style.Underline = uint8(sub)
				}
			} else {
				t.Style.Underline = UnderlineSingle
			}
		case 5, 6:
			t.Style.Attrs |= AttrBlink
		case 7:
			t.Style.Attrs |= AttrReverse
		case 8:
			t.Style.Attrs |= AttrHidden
		case 9:
			t.Style.Attrs |= AttrStrike
		case 21:
			t.Style.Underline = UnderlineDouble
		case 22:
			t.Style.Attrs &^= AttrBold | AttrFaint
		case 23:
			t.Style.Attrs &^= AttrItalic
		case 24:
			t.Style.Underline = UnderlineNone
		case 25:
			t.Style.Attrs &^= AttrBlink
		case 27:
			t.Style.Attrs &^= AttrReverse
		case 28:
			t.Style.Attrs &^= AttrHidden
		case 29:
			t.Style.Attrs &^= AttrStrike
		case 30, 31, 32, 33, 34, 35, 36, 37:
			t.Style.Fg = Color{Kind: ColorIndexed, Index: uint8(code - 30)}
		case 38:
			if c, consumed, ok := parseExtendedColor(params, i); ok {
				t.Style.Fg = c
				i += consumed
			}
		case 39:
			t.Style.Fg = Color{}
		case 40, 41, 42, 43, 44, 45, 46, 47:
			t.Style.Bg = Color{Kind: ColorIndexed, Index: uint8(code - 40)}
		case 48:
			if c, consumed, ok := parseExtendedColor(params, i); ok {
				t.Style.Bg = c
				i += consumed
			}
		case 49:
			t.Style.Bg = Color{}
		case 53:
			t.Style.Attrs |= AttrOverline
		case 55:
			t.Style.Attrs &^= AttrOverline
		case 58:
			if c, consumed, ok := parseExtendedColor(params, i); ok {
				t.Style.UnderlineColor = c
				i += consumed
			}
		case 59:
			t.Style.UnderlineColor = Color{}
		case 90, 91, 92, 93, 94, 95, 96, 97:
			t.Style.Fg = Color{Kind: ColorIndexed, Index: uint8(code - 90 + 8)}
		case 100, 101, 102, 103, 104, 105, 106, 107:
			t.Style.Bg = Color{Kind: ColorIndexed, Index: uint8(code - 100 + 8)}
		}
	}
}

func parseExtendedColor(params *CSIParams, i int) (Color, int, bool) {
	// Colon-separated subparams: 38:5:idx or 38:2:cs:r:g:b or 38:2:r:g:b
	if params.SubLen(i) >= 2 {
		mode := params.Sub(i, 1, -1)
		switch mode {
		case 5:
			if params.SubLen(i) >= 3 {
				idx := clamp(params.Sub(i, 2, 0), 0, 255)
				return Color{Kind: ColorIndexed, Index: uint8(idx)}, 0, true
			}
		case 2:
			if params.SubLen(i) >= 6 {
				r := clamp(params.Sub(i, 3, 0), 0, 255)
				g := clamp(params.Sub(i, 4, 0), 0, 255)
				b := clamp(params.Sub(i, 5, 0), 0, 255)
				return Color{Kind: ColorRGB, R: uint8(r), G: uint8(g), B: uint8(b)}, 0, true
			}
			if params.SubLen(i) == 5 {
				r := clamp(params.Sub(i, 2, 0), 0, 255)
				g := clamp(params.Sub(i, 3, 0), 0, 255)
				b := clamp(params.Sub(i, 4, 0), 0, 255)
				return Color{Kind: ColorRGB, R: uint8(r), G: uint8(g), B: uint8(b)}, 0, true
			}
		}
		return Color{}, 0, false
	}

	// Semicolon-separated: 38;5;idx or 38;2;r;g;b
	if i+1 >= params.Len() {
		return Color{}, 0, false
	}
	mode := params.Get(i+1, -1)
	switch mode {
	case 5:
		if i+2 < params.Len() {
			idx := clamp(params.Get(i+2, 0), 0, 255)
			return Color{Kind: ColorIndexed, Index: uint8(idx)}, 2, true
		}
	case 2:
		if i+4 < params.Len() {
			r := clamp(params.Get(i+2, 0), 0, 255)
			g := clamp(params.Get(i+3, 0), 0, 255)
			b := clamp(params.Get(i+4, 0), 0, 255)
			return Color{Kind: ColorRGB, R: uint8(r), G: uint8(g), B: uint8(b)}, 4, true
		}
	}
	return Color{}, 0, false
}

func (t *Terminal) oscDispatch(data []byte) {
	s := string(data)
	sep := strings.IndexByte(s, ';')
	if sep < 0 {
		return
	}
	code, err := strconv.Atoi(s[:sep])
	if err != nil {
		return
	}
	rest := s[sep+1:]

	switch code {
	case 0: // Window + Icon title
		t.Title = rest
		t.IconTitle = rest
	case 1: // Icon title
		t.IconTitle = rest
	case 2: // Window title
		t.Title = rest
	case 7: // Current working directory (file://host/path)
		if u, err := url.Parse(rest); err == nil && u.Scheme == "file" && u.Path != "" {
			if decoded, err := url.PathUnescape(u.Path); err == nil {
				t.Cwd = decoded
			} else {
				t.Cwd = u.Path
			}
		}
	case 8: // Hyperlink: OSC 8 ; params ; uri ST
		if sep2 := strings.IndexByte(rest, ';'); sep2 >= 0 {
			params := rest[:sep2]
			uri := rest[sep2+1:]
			t.Style.HyperlinkID = t.Links.Intern(params, uri)
		}
	case 10: // Foreground color query
		if rest == "?" {
			val := t.CachedOSC10
			if val == "" {
				val = "rgb:ffff/ffff/ffff"
			}
			t.reply("\x1b]10;" + val + "\x1b\\")
		}
	case 11: // Background color query
		if rest == "?" {
			val := t.CachedOSC11
			if val == "" {
				val = "rgb:0000/0000/0000"
			}
			t.reply("\x1b]11;" + val + "\x1b\\")
		}
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
