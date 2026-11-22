package switcher

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"

	"github.com/EricWF/mplex/internal/protocol"
)

// ActionKind describes what the user chose when closing the switcher.
type ActionKind int

const (
	ActionCancel ActionKind = iota
	ActionSelect
	ActionCreate
	ActionDetach
	ActionSendLiteral
)

// Result is returned when the interactive switcher closes.
type Result struct {
	Action  ActionKind
	Target  string // Session ID (ActionSelect) or desired name (ActionCreate)
	Literal []byte // Raw bytes to forward on ActionSendLiteral
}

// Callbacks connects the switcher UI to live daemon RPCs.
type Callbacks struct {
	ListSessions func() ([]protocol.SessionInfo, error)
	GetPreview   func(target string, rows, cols int) ([]string, bool, error)
	KillSession  func(target string) error
	Rename       func(target, newName string) (protocol.SessionInfo, error)
	MatchTrigger func(chunk []byte) (matched []byte, ok bool)
}

// Config configures one invocation of the switcher overlay.
type Config struct {
	CurrentSessionID string
	LastSessionID    string
	TriggerLabel     string
	Notice           string
	DefaultCwd       string
	Rows             int
	Cols             int
	AllowCancel      bool // false when current session exited and user must pick/create/detach
	InputCh          <-chan []byte
	ResizeCh         <-chan protocol.WinSize
	RefreshCh        <-chan string
	Out              io.Writer
	Callbacks        Callbacks
}

type uiMode int

const (
	modeBrowse uiMode = iota
	modeConfirmKill
	modeRename
)

type listEntry struct {
	isCreate  bool
	createLbl string
	createVal string
	result    FuzzyResult
}

type previewCacheEntry struct {
	lines       []string
	inAltScreen bool
	rows, cols  int
}

// Run displays the fzf-style interactive session switcher on the outer terminal's
// alternate screen buffer and blocks until the user selects an action.
func Run(cfg Config) Result {
	if cfg.Rows < 10 {
		cfg.Rows = 24
	}
	if cfg.Cols < 40 {
		cfg.Cols = 80
	}

	// Enter clean alt screen with SGR mouse enabled just for the switcher.
	_, _ = cfg.Out.Write([]byte(
		"\x1b[?1049h" + // enter alt screen
			"\x1b[?2026l" + // ensure sync off
			"\x1b[?1000h\x1b[?1006h" + // SGR mouse click + scroll inside switcher
			"\x1b[?2004l" + // disable bracketed paste inside switcher
			"\x1b[<32u" + // clear Kitty keyboard stack inside switcher
			"\x1b[>4;0m" +
			"\x1b[0 q" + // default cursor
			"\x1b[0m\x1b[2J\x1b[H",
	))
	defer func() {
		// Disable switcher mouse mode cleanly before caller restores session or exits.
		_, _ = cfg.Out.Write([]byte("\x1b[?1000l\x1b[?1006l\x1b[0m"))
	}()

	homeDir, _ := os.UserHomeDir()

	st := &state{
		cfg:          cfg,
		homeDir:      homeDir,
		rows:         cfg.Rows,
		cols:         cfg.Cols,
		notice:       cfg.Notice,
		previewCache: make(map[string]previewCacheEntry),
	}

	st.reloadSessions()
	st.rebuildEntries()
	// Pre-select the last-used session if there is another one (makes quick switch fast!),
	// or current session if only one exists.
	st.selectInitial()
	st.ensurePreview()
	st.render()

	for {
		select {
		case sz, ok := <-cfg.ResizeCh:
			if !ok {
				return Result{Action: ActionCancel}
			}
			if sz.Rows > 0 && sz.Cols > 0 {
				st.rows = int(sz.Rows)
				st.cols = int(sz.Cols)
				st.previewCache = make(map[string]previewCacheEntry)
				st.ensurePreview()
				st.render()
			}

		case msg, ok := <-cfg.RefreshCh:
			if !ok {
				return Result{Action: ActionCancel}
			}
			if msg != "" {
				st.notice = msg
			}
			st.previewCache = make(map[string]previewCacheEntry)
			st.reloadSessions()
			st.rebuildEntries()
			st.ensurePreview()
			st.render()

		case chunk, ok := <-cfg.InputCh:
			if !ok {
				return Result{Action: ActionCancel}
			}
			if res, done := st.handleInput(chunk); done {
				return res
			}
			st.ensurePreview()
			st.render()
		}
	}
}

type state struct {
	cfg     Config
	homeDir string
	rows    int
	cols    int

	sessions []protocol.SessionInfo
	entries  []listEntry
	selected int
	scroll   int

	query     []rune
	cursorPos int

	mode         uiMode
	renameTarget protocol.SessionInfo
	renameBuf    []rune
	renameCursor int
	killTarget   protocol.SessionInfo
	notice       string

	previewCache map[string]previewCacheEntry
}

func (st *state) reloadSessions() {
	if st.cfg.Callbacks.ListSessions == nil {
		return
	}
	list, err := st.cfg.Callbacks.ListSessions()
	if err != nil {
		st.notice = "error: " + err.Error()
		return
	}
	st.sessions = list
}

func (st *state) selectInitial() {
	if len(st.entries) == 0 {
		st.selected = 0
		return
	}
	// Put cursor on currently attached session by default (so Up/Down or Tab jumps naturally).
	if st.cfg.CurrentSessionID != "" {
		for i, e := range st.entries {
			if !e.isCreate && e.result.Session.ID == st.cfg.CurrentSessionID {
				st.selected = i
				return
			}
		}
	}
	st.selected = 0
}

func (st *state) rebuildEntries() {
	q := strings.TrimSpace(string(st.query))
	filtered := FilterSessions(st.sessions, q)

	entries := make([]listEntry, 0, len(filtered)+1)
	exactMatch := false
	for _, r := range filtered {
		if q != "" && strings.EqualFold(r.Session.Name, q) {
			exactMatch = true
		}
		entries = append(entries, listEntry{result: r})
	}

	if q == "" {
		entries = append(entries, listEntry{
			isCreate:  true,
			createLbl: "+ New session",
			createVal: "",
		})
	} else if !exactMatch {
		entries = append(entries, listEntry{
			isCreate:  true,
			createLbl: fmt.Sprintf("+ Create new session %q", q),
			createVal: q,
		})
	}

	st.entries = entries
	if st.selected >= len(st.entries) {
		st.selected = len(st.entries) - 1
	}
	if st.selected < 0 {
		st.selected = 0
	}
	st.adjustScroll()
}

func (st *state) listViewportHeight() int {
	// Header (2) + Search (2) + Footer (2) = 6 lines of chrome.
	h := st.rows - 6
	if h < 1 {
		h = 1
	}
	return h
}

func (st *state) adjustScroll() {
	vh := st.listViewportHeight()
	if st.selected < st.scroll {
		st.scroll = st.selected
	}
	if st.selected >= st.scroll+vh {
		st.scroll = st.selected - vh + 1
	}
	if st.scroll < 0 {
		st.scroll = 0
	}
}

func (st *state) hasPreviewPane() bool {
	return st.cols >= 76
}

func (st *state) splitWidths() (leftW, rightW int) {
	if !st.hasPreviewPane() {
		return st.cols, 0
	}
	leftW = st.cols * 44 / 100
	if leftW < 34 {
		leftW = 34
	}
	if leftW > 56 {
		leftW = 56
	}
	rightW = st.cols - leftW - 1 // 1 col for vertical divider
	if rightW < 20 {
		return st.cols, 0
	}
	return leftW, rightW
}

func (st *state) ensurePreview() {
	if !st.hasPreviewPane() || st.cfg.Callbacks.GetPreview == nil {
		return
	}
	if st.selected < 0 || st.selected >= len(st.entries) {
		return
	}
	e := st.entries[st.selected]
	if e.isCreate {
		return
	}
	_, rightW := st.splitWidths()
	pRows := st.rows - 5
	pCols := rightW - 2
	if pRows < 1 || pCols < 1 {
		return
	}
	id := e.result.Session.ID
	if cached, ok := st.previewCache[id]; ok && cached.rows == pRows && cached.cols == pCols {
		return
	}
	lines, inAlt, err := st.cfg.Callbacks.GetPreview(id, pRows, pCols)
	if err == nil {
		st.previewCache[id] = previewCacheEntry{
			lines:       lines,
			inAltScreen: inAlt,
			rows:        pRows,
			cols:        pCols,
		}
	}
}

func (st *state) handleInput(chunk []byte) (Result, bool) {
	// If in browse mode and user hits the trigger key again, forward literal key!
	if st.mode == modeBrowse && st.cfg.AllowCancel && st.cfg.Callbacks.MatchTrigger != nil {
		if matched, ok := st.cfg.Callbacks.MatchTrigger(chunk); ok && len(matched) == len(chunk) {
			return Result{Action: ActionSendLiteral, Literal: matched}, true
		}
	}

	for len(chunk) > 0 {
		// SGR Mouse events: \x1b[<btn;x;yM or m
		if bytes.HasPrefix(chunk, []byte("\x1b[<")) {
			consumed, res, done := st.handleSGRMouse(chunk)
			if done {
				return res, true
			}
			if consumed > 0 {
				chunk = chunk[consumed:]
				continue
			}
		}

		// Escape sequences
		if chunk[0] == 0x1b {
			if len(chunk) == 1 {
				// Bare Escape
				if res, done := st.handleEsc(); done {
					return res, true
				}
				return Result{}, false
			}
			// Alt+Enter (\x1b\r or \x1b\n)
			if chunk[1] == '\r' || chunk[1] == '\n' {
				chunk = chunk[2:]
				if st.mode == modeBrowse {
					return Result{Action: ActionCreate, Target: strings.TrimSpace(string(st.query))}, true
				}
				continue
			}
			// Alt+d -> detach
			if chunk[1] == 'd' || chunk[1] == 'D' {
				return Result{Action: ActionDetach}, true
			}
			// CSI arrows / home / end / delete
			if chunk[1] == '[' {
				end := scanCSIEndLocal(chunk)
				if end > 0 {
					seq := string(chunk[2:end])
					chunk = chunk[end:]
					st.handleCSIKey(seq)
					continue
				}
			}
			// SS3 arrows (\x1bOA, \x1bOB, \x1bOC, \x1bOD)
			if chunk[1] == 'O' && len(chunk) >= 3 {
				switch chunk[2] {
				case 'A':
					st.moveSelection(-1)
				case 'B':
					st.moveSelection(1)
				case 'C':
					st.moveInputCursor(1)
				case 'D':
					st.moveInputCursor(-1)
				}
				chunk = chunk[3:]
				continue
			}
			// Unknown ESC sequence: consume ESC and continue
			chunk = chunk[1:]
			continue
		}

		// Control characters
		b := chunk[0]
		if b < 0x20 || b == 0x7f {
			chunk = chunk[1:]
			if res, done := st.handleControlByte(b); done {
				return res, true
			}
			continue
		}

		// UTF-8 rune
		r, sz := utf8.DecodeRune(chunk)
		if r == utf8.RuneError && sz == 1 {
			chunk = chunk[1:]
			continue
		}
		chunk = chunk[sz:]
		if res, done := st.handleRune(r); done {
			return res, true
		}
	}

	return Result{}, false
}

func (st *state) handleEsc() (Result, bool) {
	switch st.mode {
	case modeConfirmKill, modeRename:
		st.mode = modeBrowse
		st.notice = ""
		return Result{}, false
	default:
		if st.cfg.AllowCancel {
			return Result{Action: ActionCancel}, true
		}
		// If current session exited, Esc clears query instead of closing into nowhere.
		if len(st.query) > 0 {
			st.query = st.query[:0]
			st.cursorPos = 0
			st.rebuildEntries()
		}
		return Result{}, false
	}
}

func (st *state) handleControlByte(b byte) (Result, bool) {
	if st.mode == modeConfirmKill {
		switch b {
		case 0x03: // Ctrl+C
			st.mode = modeBrowse
		case '\r', '\n':
			st.mode = modeBrowse // default No
		}
		return Result{}, false
	}

	if st.mode == modeRename {
		switch b {
		case 0x03: // Ctrl+C
			st.mode = modeBrowse
		case '\r', '\n':
			st.commitRename()
		case 0x7f, 0x08: // Backspace
			if st.renameCursor > 0 {
				st.renameBuf = append(st.renameBuf[:st.renameCursor-1], st.renameBuf[st.renameCursor:]...)
				st.renameCursor--
			}
		case 0x01: // Ctrl+A
			st.renameCursor = 0
		case 0x05: // Ctrl+E
			st.renameCursor = len(st.renameBuf)
		case 0x15: // Ctrl+U
			st.renameBuf = st.renameBuf[:0]
			st.renameCursor = 0
		}
		return Result{}, false
	}

	// Browse mode
	switch b {
	case 0x03: // Ctrl+C
		if st.cfg.AllowCancel {
			return Result{Action: ActionCancel}, true
		}
		return Result{Action: ActionDetach}, true

	case '\r': // Enter
		return st.confirmSelection()

	case '\t': // Tab -> quick toggle to previous session!
		if st.cfg.LastSessionID != "" && st.cfg.LastSessionID != st.cfg.CurrentSessionID {
			for _, s := range st.sessions {
				if s.ID == st.cfg.LastSessionID {
					return Result{Action: ActionSelect, Target: s.ID}, true
				}
			}
		}
		// Fallback: pick first session other than current
		for _, s := range st.sessions {
			if s.ID != st.cfg.CurrentSessionID {
				return Result{Action: ActionSelect, Target: s.ID}, true
			}
		}

	case 0x10, 0x0b: // Ctrl+P, Ctrl+K -> up
		st.moveSelection(-1)

	case 0x0e, 0x0a: // Ctrl+N, Ctrl+J -> down
		st.moveSelection(1)

	case 0x0f: // Ctrl+O -> force create new session
		return Result{Action: ActionCreate, Target: strings.TrimSpace(string(st.query))}, true

	case 0x11: // Ctrl+Q -> detach client
		return Result{Action: ActionDetach}, true

	case 0x18, 0x04: // Ctrl+X / Ctrl+D -> kill selected session
		if st.selected >= 0 && st.selected < len(st.entries) && !st.entries[st.selected].isCreate {
			st.killTarget = st.entries[st.selected].result.Session
			st.mode = modeConfirmKill
			st.notice = ""
		}

	case 0x12: // Ctrl+R -> rename selected session
		if st.selected >= 0 && st.selected < len(st.entries) && !st.entries[st.selected].isCreate {
			st.renameTarget = st.entries[st.selected].result.Session
			st.renameBuf = []rune(st.renameTarget.Name)
			st.renameCursor = len(st.renameBuf)
			st.mode = modeRename
			st.notice = ""
		}

	case 0x7f, 0x08: // Backspace
		if st.cursorPos > 0 {
			st.query = append(st.query[:st.cursorPos-1], st.query[st.cursorPos:]...)
			st.cursorPos--
			st.rebuildEntries()
		}

	case 0x01: // Ctrl+A
		st.cursorPos = 0

	case 0x05: // Ctrl+E
		st.cursorPos = len(st.query)

	case 0x15: // Ctrl+U -> clear query
		if len(st.query) > 0 {
			st.query = st.query[:0]
			st.cursorPos = 0
			st.rebuildEntries()
		}

	case 0x17: // Ctrl+W -> delete word backward
		if st.cursorPos > 0 {
			i := st.cursorPos
			for i > 0 && st.query[i-1] == ' ' {
				i--
			}
			for i > 0 && st.query[i-1] != ' ' {
				i--
			}
			st.query = append(st.query[:i], st.query[st.cursorPos:]...)
			st.cursorPos = i
			st.rebuildEntries()
		}
	}

	return Result{}, false
}

func (st *state) handleRune(r rune) (Result, bool) {
	if st.mode == modeConfirmKill {
		switch r {
		case 'y', 'Y':
			st.mode = modeBrowse
			if st.cfg.Callbacks.KillSession != nil {
				if err := st.cfg.Callbacks.KillSession(st.killTarget.ID); err != nil {
					st.notice = "kill failed: " + err.Error()
				} else {
					st.notice = fmt.Sprintf("Killed session %q", st.killTarget.Name)
					delete(st.previewCache, st.killTarget.ID)
					if st.killTarget.ID == st.cfg.CurrentSessionID {
						st.cfg.AllowCancel = false
						st.cfg.CurrentSessionID = ""
					}
					st.reloadSessions()
					st.rebuildEntries()
				}
			}
		default:
			st.mode = modeBrowse
		}
		return Result{}, false
	}

	if st.mode == modeRename {
		st.renameBuf = append(st.renameBuf[:st.renameCursor], append([]rune{r}, st.renameBuf[st.renameCursor:]...)...)
		st.renameCursor++
		return Result{}, false
	}

	// Browse mode: insert into search query
	st.notice = ""
	st.query = append(st.query[:st.cursorPos], append([]rune{r}, st.query[st.cursorPos:]...)...)
	st.cursorPos++
	st.selected = 0
	st.rebuildEntries()
	return Result{}, false
}

func (st *state) handleCSIKey(seq string) {
	switch seq {
	case "A": // Up
		if st.mode == modeBrowse {
			st.moveSelection(-1)
		}
	case "B": // Down
		if st.mode == modeBrowse {
			st.moveSelection(1)
		}
	case "C": // Right
		st.moveInputCursor(1)
	case "D": // Left
		st.moveInputCursor(-1)
	case "H", "1~": // Home
		if st.mode == modeRename {
			st.renameCursor = 0
		} else {
			st.cursorPos = 0
		}
	case "F", "4~": // End
		if st.mode == modeRename {
			st.renameCursor = len(st.renameBuf)
		} else {
			st.cursorPos = len(st.query)
		}
	case "3~": // Delete
		if st.mode == modeRename {
			if st.renameCursor < len(st.renameBuf) {
				st.renameBuf = append(st.renameBuf[:st.renameCursor], st.renameBuf[st.renameCursor+1:]...)
			}
		} else if st.mode == modeBrowse {
			if st.cursorPos < len(st.query) {
				st.query = append(st.query[:st.cursorPos], st.query[st.cursorPos+1:]...)
				st.rebuildEntries()
			}
		}
	case "5~": // PageUp
		if st.mode == modeBrowse {
			st.moveSelection(-5)
		}
	case "6~": // PageDown
		if st.mode == modeBrowse {
			st.moveSelection(5)
		}
	}
}

func (st *state) handleSGRMouse(chunk []byte) (consumed int, res Result, done bool) {
	// Format: \x1b[<btn;x;yM or \x1b[<btn;x;ym
	end := -1
	for i := 3; i < len(chunk); i++ {
		if chunk[i] == 'M' || chunk[i] == 'm' {
			end = i + 1
			break
		}
	}
	if end < 0 {
		return 0, Result{}, false
	}
	if st.mode != modeBrowse || chunk[end-1] != 'M' {
		return end, Result{}, false
	}
	parts := strings.Split(string(chunk[3:end-1]), ";")
	if len(parts) != 3 {
		return end, Result{}, false
	}
	btn, err1 := strconv.Atoi(parts[0])
	x, err2 := strconv.Atoi(parts[1])
	y, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return end, Result{}, false
	}

	switch btn {
	case 64: // wheel up
		st.moveSelection(-1)
	case 65: // wheel down
		st.moveSelection(1)
	case 0: // left click
		leftW, _ := st.splitWidths()
		// List starts at 1-indexed terminal row 5 (header=1..2, search=3..4, list=5..)
		if x <= leftW && y >= 5 && y < 5+st.listViewportHeight() {
			idx := st.scroll + (y - 5)
			if idx >= 0 && idx < len(st.entries) {
				if idx == st.selected {
					r, d := st.confirmSelection()
					return end, r, d
				}
				st.selected = idx
			}
		}
	}
	return end, Result{}, false
}

func (st *state) moveSelection(delta int) {
	if len(st.entries) == 0 {
		return
	}
	st.selected += delta
	if st.selected < 0 {
		st.selected = len(st.entries) - 1
	} else if st.selected >= len(st.entries) {
		st.selected = 0
	}
	st.adjustScroll()
}

func (st *state) moveInputCursor(delta int) {
	if st.mode == modeRename {
		st.renameCursor += delta
		if st.renameCursor < 0 {
			st.renameCursor = 0
		}
		if st.renameCursor > len(st.renameBuf) {
			st.renameCursor = len(st.renameBuf)
		}
		return
	}
	st.cursorPos += delta
	if st.cursorPos < 0 {
		st.cursorPos = 0
	}
	if st.cursorPos > len(st.query) {
		st.cursorPos = len(st.query)
	}
}

func (st *state) confirmSelection() (Result, bool) {
	if len(st.entries) == 0 {
		return Result{Action: ActionCreate, Target: strings.TrimSpace(string(st.query))}, true
	}
	e := st.entries[st.selected]
	if e.isCreate {
		return Result{Action: ActionCreate, Target: e.createVal}, true
	}
	if e.result.Session.ID == st.cfg.CurrentSessionID && st.cfg.AllowCancel {
		return Result{Action: ActionCancel}, true
	}
	return Result{Action: ActionSelect, Target: e.result.Session.ID}, true
}

func (st *state) commitRename() {
	newName := strings.TrimSpace(string(st.renameBuf))
	st.mode = modeBrowse
	if newName == "" || newName == st.renameTarget.Name {
		return
	}
	if st.cfg.Callbacks.Rename != nil {
		if _, err := st.cfg.Callbacks.Rename(st.renameTarget.ID, newName); err != nil {
			st.notice = "rename failed: " + err.Error()
		} else {
			st.notice = fmt.Sprintf("Renamed to %q", newName)
			st.reloadSessions()
			st.rebuildEntries()
		}
	}
}

func (st *state) render() {
	var buf bytes.Buffer
	buf.Grow(8192)

	buf.WriteString("\x1b[?2026h") // begin synchronized frame
	buf.WriteString("\x1b[?25l")   // hide cursor while drawing
	buf.WriteString("\x1b[0m\x1b[2J\x1b[H")

	leftW, rightW := st.splitWidths()

	// --- Row 1: Header ---
	buf.WriteString("\x1b[1;1H")
	header := fmt.Sprintf(" \x1b[1;36mmplex\x1b[0m \x1b[2m· %d %s\x1b[0m", len(st.sessions), plural(len(st.sessions), "session", "sessions"))
	buf.WriteString(header)
	if st.notice != "" {
		buf.WriteString("  \x1b[33m")
		buf.WriteString(truncateVisual(st.notice, max(10, st.cols-28)))
		buf.WriteString("\x1b[0m")
	}

	// --- Row 2: Thin horizontal rule ---
	buf.WriteString("\x1b[2;1H\x1b[2m")
	if rightW > 0 {
		buf.WriteString(strings.Repeat("─", leftW))
		buf.WriteString("┬")
		buf.WriteString(strings.Repeat("─", rightW))
	} else {
		buf.WriteString(strings.Repeat("─", st.cols))
	}
	buf.WriteString("\x1b[0m")

	// --- Row 3: Search / Rename input bar (left pane) ---
	buf.WriteString("\x1b[3;1H")
	var cursorRow, cursorCol int
	if st.mode == modeRename {
		prefix := " \x1b[1;33mrename ❯\x1b[0m "
		prefixVis := 10
		buf.WriteString(prefix)
		text := string(st.renameBuf)
		maxInputW := max(4, leftW-prefixVis-1)
		buf.WriteString(truncateVisual(text, maxInputW))
		cursorRow = 3
		cursorCol = prefixVis + 1 + runewidth.StringWidth(string(st.renameBuf[:st.renameCursor]))
		if cursorCol > leftW {
			cursorCol = leftW
		}
	} else {
		prefix := " \x1b[1;35m❯\x1b[0m "
		prefixVis := 3
		buf.WriteString(prefix)
		text := string(st.query)
		if text == "" {
			buf.WriteString("\x1b[2mfilter or type name to create...\x1b[0m")
		} else {
			maxInputW := max(4, leftW-prefixVis-1)
			buf.WriteString(truncateVisual(text, maxInputW))
		}
		cursorRow = 3
		cursorCol = prefixVis + 1 + runewidth.StringWidth(string(st.query[:st.cursorPos]))
		if cursorCol > leftW {
			cursorCol = leftW
		}
	}

	// --- Row 4: Search divider ---
	buf.WriteString("\x1b[4;1H\x1b[2m")
	buf.WriteString(strings.Repeat("─", leftW))
	buf.WriteString("\x1b[0m")

	// --- Rows 5 .. rows-2: Session List (left pane) ---
	vh := st.listViewportHeight()
	for i := 0; i < vh; i++ {
		row := 5 + i
		idx := st.scroll + i
		if idx >= len(st.entries) {
			break
		}
		fmt.Fprintf(&buf, "\x1b[%d;1H", row)
		st.renderListRow(&buf, st.entries[idx], idx == st.selected, leftW)
	}

	// --- Right pane: Live Preview ---
	if rightW > 0 {
		divCol := leftW + 1
		for r := 3; r <= st.rows-2; r++ {
			fmt.Fprintf(&buf, "\x1b[%d;%dH\x1b[2m│\x1b[0m", r, divCol)
		}
		st.renderPreviewPane(&buf, divCol+1, rightW)
	}

	// --- Row rows-1: Bottom rule ---
	fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[2m", st.rows-1)
	if rightW > 0 {
		buf.WriteString(strings.Repeat("─", leftW))
		buf.WriteString("┴")
		buf.WriteString(strings.Repeat("─", rightW))
	} else {
		buf.WriteString(strings.Repeat("─", st.cols))
	}
	buf.WriteString("\x1b[0m")

	// --- Row rows: Footer / Confirm prompt ---
	fmt.Fprintf(&buf, "\x1b[%d;1H", st.rows)
	switch st.mode {
	case modeConfirmKill:
		msg := fmt.Sprintf(" \x1b[1;33mKill session %q?\x1b[0m [y/N]", st.killTarget.Name)
		buf.WriteString(msg)
	case modeRename:
		buf.WriteString(" \x1b[2mEnter\x1b[0m save · \x1b[2mEsc\x1b[0m cancel")
	default:
		var hints []string
		hints = append(hints, "Enter:switch")
		if len(st.sessions) > 1 {
			hints = append(hints, "Tab:last")
		}
		hints = append(hints, "Alt+Enter:new", "Ctrl+R:rename", "Ctrl+X:kill", "Ctrl+Q:detach")
		if st.cfg.AllowCancel {
			hints = append(hints, "Esc:back")
			if st.cfg.TriggerLabel != "" && st.cfg.TriggerLabel != "none" {
				hints = append(hints, st.cfg.TriggerLabel+":literal")
			}
		}
		footer := " " + strings.Join(hints, " · ")
		buf.WriteString("\x1b[2m")
		buf.WriteString(truncateVisual(footer, st.cols-1))
		buf.WriteString("\x1b[0m")
	}

	// Position cursor cleanly inside input prompt (unless confirming kill).
	if st.mode != modeConfirmKill {
		fmt.Fprintf(&buf, "\x1b[%d;%dH\x1b[?25h", cursorRow, cursorCol)
	}
	buf.WriteString("\x1b[?2026l") // end synchronized frame

	_, _ = cfgWrite(st.cfg.Out, buf.Bytes())
}

func (st *state) renderListRow(buf *bytes.Buffer, e listEntry, selected bool, width int) {
	if selected {
		buf.WriteString("\x1b[48;5;237m")
	}

	if e.isCreate {
		pointer := "  "
		if selected {
			pointer = "\x1b[1;32m▸ \x1b[0;48;5;237m"
		}
		buf.WriteString(pointer)
		buf.WriteString("\x1b[32m")
		lbl := truncateVisual(e.createLbl, max(4, width-3))
		buf.WriteString(lbl)
		pad := width - 2 - runewidth.StringWidth(lbl)
		if pad > 0 && selected {
			buf.WriteString(strings.Repeat(" ", pad))
		}
		buf.WriteString("\x1b[0m")
		return
	}

	s := e.result.Session
	used := 0
	if selected {
		buf.WriteString("\x1b[1;36m▸ \x1b[0;48;5;237m")
	} else {
		buf.WriteString("  ")
	}
	used += 2

	// Status indicator dot
	if s.ID == st.cfg.CurrentSessionID {
		buf.WriteString("\x1b[32m●\x1b[0m")
	} else if s.Attached {
		buf.WriteString("\x1b[33m●\x1b[0m")
	} else {
		buf.WriteString("\x1b[2m○\x1b[0m")
	}
	if selected {
		buf.WriteString("\x1b[48;5;237m")
	}
	buf.WriteByte(' ')
	used += 2

	// Session name with fuzzy match highlighting
	matchSet := make(map[int]bool, len(e.result.NameMatch))
	for _, idx := range e.result.NameMatch {
		matchSet[idx] = true
	}

	maxNameW := max(8, width/2)
	nameW := 0
	for i, r := range []rune(s.Name) {
		rw := runewidth.RuneWidth(r)
		if rw < 1 {
			rw = 1
		}
		if nameW+rw > maxNameW {
			break
		}
		if matchSet[i] {
			buf.WriteString("\x1b[1;35m")
			buf.WriteRune(r)
			buf.WriteString("\x1b[0m")
			if selected {
				buf.WriteString("\x1b[48;5;237m")
			}
		} else {
			if selected {
				buf.WriteString("\x1b[1m")
				buf.WriteRune(r)
				buf.WriteString("\x1b[22m")
			} else {
				buf.WriteRune(r)
			}
		}
		nameW += rw
	}
	used += nameW

	// Metadata suffix: fg command + short cwd + age
	var metaParts []string
	if s.FgCommand != "" {
		metaParts = append(metaParts, s.FgCommand)
	}
	if s.Cwd != "" {
		metaParts = append(metaParts, shortenPath(s.Cwd, st.homeDir))
	}
	age := shortAge(s.LastActiveAt)

	avail := width - used - 1
	if avail > 4 {
		meta := ""
		if len(metaParts) > 0 {
			meta = " " + strings.Join(metaParts, " · ")
		}
		ageSuffix := ""
		if age != "" && avail > len(age)+3 {
			ageSuffix = " " + age
		}
		metaLimit := avail - runewidth.StringWidth(ageSuffix)
		if metaLimit > 2 && meta != "" {
			meta = truncateVisual(meta, metaLimit)
			buf.WriteString("\x1b[2m")
			buf.WriteString(meta)
			buf.WriteString("\x1b[0m")
			if selected {
				buf.WriteString("\x1b[48;5;237m")
			}
			used += runewidth.StringWidth(meta)
		}
		if ageSuffix != "" {
			pad := width - used - runewidth.StringWidth(ageSuffix) - 1
			if pad > 0 {
				buf.WriteString(strings.Repeat(" ", pad))
				used += pad
			}
			buf.WriteString("\x1b[2m")
			buf.WriteString(ageSuffix)
			used += runewidth.StringWidth(ageSuffix)
		}
	}

	if selected && used < width {
		buf.WriteString(strings.Repeat(" ", width-used))
	}
	buf.WriteString("\x1b[0m")
}

func (st *state) renderPreviewPane(buf *bytes.Buffer, startCol, width int) {
	if st.selected < 0 || st.selected >= len(st.entries) {
		return
	}
	e := st.entries[st.selected]

	// Row 3: preview header
	fmt.Fprintf(buf, "\x1b[3;%dH", startCol)
	if e.isCreate {
		buf.WriteString(" \x1b[1;32mNew Session\x1b[0m")
		fmt.Fprintf(buf, "\x1b[4;%dH\x1b[2m%s\x1b[0m", startCol, strings.Repeat("─", width))
		cwd := shortenPath(st.cfg.DefaultCwd, st.homeDir)
		if cwd == "" {
			cwd = "~"
		}
		fmt.Fprintf(buf, "\x1b[6;%dH \x1b[2mDirectory:\x1b[0m %s", startCol, truncateVisual(cwd, max(4, width-13)))
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		fmt.Fprintf(buf, "\x1b[7;%dH \x1b[2mShell:\x1b[0m     %s", startCol, truncateVisual(shell, max(4, width-13)))
		fmt.Fprintf(buf, "\x1b[9;%dH \x1b[2mPress Enter to launch a native terminal.\x1b[0m", startCol)
		return
	}

	s := e.result.Session
	header := fmt.Sprintf(" %s", s.Name)
	if s.FgCommand != "" {
		header += " [" + s.FgCommand + "]"
	}
	if s.InAltScreen {
		header += " · fullscreen"
	} else if s.Scrollback > 0 {
		header += fmt.Sprintf(" · %d scrollback", s.Scrollback)
	}
	buf.WriteString("\x1b[1m")
	buf.WriteString(truncateVisual(header, max(4, width-1)))
	buf.WriteString("\x1b[0m")

	// Row 4: preview divider
	fmt.Fprintf(buf, "\x1b[4;%dH\x1b[2m%s\x1b[0m", startCol, strings.Repeat("─", width))

	// Rows 5..rows-2: live screen preview lines
	cached, ok := st.previewCache[s.ID]
	if !ok || len(cached.lines) == 0 {
		fmt.Fprintf(buf, "\x1b[6;%dH \x1b[2m(empty screen)\x1b[0m", startCol)
		return
	}

	maxLines := st.rows - 6
	for i := 0; i < len(cached.lines) && i < maxLines; i++ {
		fmt.Fprintf(buf, "\x1b[%d;%dH %s\x1b[0m", 5+i, startCol, cached.lines[i])
	}
}

func shortenPath(p, home string) string {
	if p == "" {
		return ""
	}
	if home != "" && strings.HasPrefix(p, home) {
		p = "~" + strings.TrimPrefix(p, home)
	}
	parts := strings.Split(p, "/")
	if len(parts) > 3 {
		return parts[0] + "/…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	return p
}

func shortAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 10*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncateVisual(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxW {
		return s
	}
	if maxW == 1 {
		return "…"
	}
	var out strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if rw < 1 {
			rw = 1
		}
		if w+rw > maxW-1 {
			break
		}
		out.WriteRune(r)
		w += rw
	}
	out.WriteRune('…')
	return out.String()
}

func scanCSIEndLocal(seq []byte) int {
	if len(seq) < 3 || seq[0] != 0x1b || seq[1] != '[' {
		return 0
	}
	for i := 2; i < len(seq); i++ {
		b := seq[i]
		if b >= 0x40 && b <= 0x7E {
			return i + 1
		}
		if b < 0x20 || b > 0x3F {
			return 0
		}
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func cfgWrite(w io.Writer, b []byte) (int, error) {
	if w == nil {
		return len(b), nil
	}
	return w.Write(b)
}
