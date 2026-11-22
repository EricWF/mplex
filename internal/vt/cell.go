package vt

import (
	"bytes"
	"strconv"
)

// ColorKind distinguishes default, 256-indexed, and 24-bit RGB colors.
type ColorKind uint8

const (
	ColorDefault ColorKind = iota
	ColorIndexed
	ColorRGB
)

// Color represents a foreground, background, or underline color.
type Color struct {
	Kind    ColorKind
	Index   uint8
	R, G, B uint8
}

// Style attribute bitmask.
const (
	AttrBold uint16 = 1 << iota
	AttrFaint
	AttrItalic
	AttrBlink
	AttrReverse
	AttrHidden
	AttrStrike
	AttrOverline
)

// Underline style variants (CSI 4:x m).
const (
	UnderlineNone uint8 = iota
	UnderlineSingle
	UnderlineDouble
	UnderlineCurly
	UnderlineDotted
	UnderlineDashed
)

// Style stores all visual attributes for a single terminal cell.
type Style struct {
	Fg             Color
	Bg             Color
	UnderlineColor Color
	Attrs          uint16
	Underline      uint8
	HyperlinkID    uint32
}

// IsDefault returns true when no SGR attributes or hyperlinks are active.
func (s Style) IsDefault() bool {
	return s == (Style{})
}

// IsVisualDefault returns true when no visual SGR attributes are active (ignoring hyperlink).
func (s Style) IsVisualDefault() bool {
	return s.Fg.Kind == ColorDefault &&
		s.Bg.Kind == ColorDefault &&
		s.UnderlineColor.Kind == ColorDefault &&
		s.Attrs == 0 &&
		s.Underline == UnderlineNone
}

// Cell represents one character cell in the terminal grid.
// Continuation cells of wide (width=2) characters have Width=0 and Rune=0.
type Cell struct {
	Rune  rune
	Extra string // combining marks / variation selectors / ZWJ sequences
	Width uint8  // 0 = wide-char continuation, 1 = standard, 2 = wide
	Style Style
}

// BlankCell creates an empty space cell with the given background style (BCE).
func BlankCell(bg Color, hyperlinkID uint32) Cell {
	var st Style
	if bg.Kind != ColorDefault {
		st.Bg = bg
	}
	st.HyperlinkID = hyperlinkID
	return Cell{
		Rune:  ' ',
		Width: 1,
		Style: st,
	}
}

// IsBlankDefault returns true if the cell is an unstyled space with no combining marks or hyperlink.
func (c Cell) IsBlankDefault() bool {
	return (c.Rune == 0 || c.Rune == ' ') && c.Extra == "" && c.Width == 1 && c.Style.IsDefault()
}

// Hyperlink represents an OSC 8 hyperlink target.
type Hyperlink struct {
	Params string
	URI    string
}

// HyperlinkTable interns OSC 8 hyperlinks by small uint32 ID (0 = none).
type HyperlinkTable struct {
	links []Hyperlink // 1-indexed; ID i maps to links[i-1]
	index map[Hyperlink]uint32
}

func (ht *HyperlinkTable) Intern(params, uri string) uint32 {
	if uri == "" {
		return 0
	}
	if ht.index == nil {
		ht.index = make(map[Hyperlink]uint32)
	}
	key := Hyperlink{Params: params, URI: uri}
	if id, ok := ht.index[key]; ok {
		return id
	}
	// Cap intern table size to keep memory bounded in very long-running sessions.
	if len(ht.links) >= 4096 {
		return 0
	}
	ht.links = append(ht.links, key)
	id := uint32(len(ht.links))
	ht.index[key] = id
	return id
}

func (ht *HyperlinkTable) Get(id uint32) (Hyperlink, bool) {
	if id == 0 || int(id) > len(ht.links) {
		return Hyperlink{}, false
	}
	return ht.links[id-1], true
}

// WriteStyleTransition writes the minimal SGR + OSC 8 escape sequence to transition
// from prev to next.
func WriteStyleTransition(buf *bytes.Buffer, prev, next Style, links *HyperlinkTable) {
	// Handle OSC 8 hyperlink change first.
	if prev.HyperlinkID != next.HyperlinkID {
		if next.HyperlinkID == 0 {
			buf.WriteString("\x1b]8;;\x1b\\")
		} else if hl, ok := links.Get(next.HyperlinkID); ok {
			buf.WriteString("\x1b]8;")
			buf.WriteString(hl.Params)
			buf.WriteByte(';')
			buf.WriteString(hl.URI)
			buf.WriteString("\x1b\\")
		} else {
			buf.WriteString("\x1b]8;;\x1b\\")
		}
	}

	// Check if visual SGR attributes changed at all.
	if prev.Fg == next.Fg &&
		prev.Bg == next.Bg &&
		prev.UnderlineColor == next.UnderlineColor &&
		prev.Attrs == next.Attrs &&
		prev.Underline == next.Underline {
		return
	}

	if next.IsVisualDefault() {
		buf.WriteString("\x1b[0m")
		return
	}

	// Determine if emitting a full reset is simpler/cleaner (e.g., if multiple attrs cleared).
	needReset := false
	removed := prev.Attrs &^ next.Attrs
	if removed != 0 {
		needReset = true
	}
	if prev.Underline != UnderlineNone && next.Underline == UnderlineNone {
		needReset = true
	}
	if prev.UnderlineColor.Kind != ColorDefault && next.UnderlineColor.Kind == ColorDefault {
		needReset = true
	}

	buf.WriteString("\x1b[")
	first := true
	add := func(s string) {
		if !first {
			buf.WriteByte(';')
		}
		first = false
		buf.WriteString(s)
	}
	addInt := func(n int) {
		if !first {
			buf.WriteByte(';')
		}
		first = false
		buf.WriteString(strconv.Itoa(n))
	}

	cur := prev
	if needReset {
		add("0")
		cur = Style{}
	}

	// Attributes
	if next.Attrs&AttrBold != 0 && cur.Attrs&AttrBold == 0 {
		add("1")
	}
	if next.Attrs&AttrFaint != 0 && cur.Attrs&AttrFaint == 0 {
		add("2")
	}
	if next.Attrs&AttrItalic != 0 && cur.Attrs&AttrItalic == 0 {
		add("3")
	}
	if next.Underline != cur.Underline {
		switch next.Underline {
		case UnderlineNone:
			add("24")
		case UnderlineSingle:
			add("4")
		case UnderlineDouble:
			add("4:2")
		case UnderlineCurly:
			add("4:3")
		case UnderlineDotted:
			add("4:4")
		case UnderlineDashed:
			add("4:5")
		}
	}
	if next.Attrs&AttrBlink != 0 && cur.Attrs&AttrBlink == 0 {
		add("5")
	}
	if next.Attrs&AttrReverse != 0 && cur.Attrs&AttrReverse == 0 {
		add("7")
	}
	if next.Attrs&AttrHidden != 0 && cur.Attrs&AttrHidden == 0 {
		add("8")
	}
	if next.Attrs&AttrStrike != 0 && cur.Attrs&AttrStrike == 0 {
		add("9")
	}
	if next.Attrs&AttrOverline != 0 && cur.Attrs&AttrOverline == 0 {
		add("53")
	}

	// Foreground color
	if next.Fg != cur.Fg {
		writeColorParam(buf, &first, next.Fg, 30, 90, 38, 39, false)
	}
	// Background color
	if next.Bg != cur.Bg {
		writeColorParam(buf, &first, next.Bg, 40, 100, 48, 49, false)
	}
	// Underline color
	if next.UnderlineColor != cur.UnderlineColor {
		if next.UnderlineColor.Kind == ColorDefault {
			addInt(59)
		} else {
			writeColorParam(buf, &first, next.UnderlineColor, 0, 0, 58, 59, true)
		}
	}

	buf.WriteByte('m')
}

func writeColorParam(buf *bytes.Buffer, first *bool, c Color, base8, bright8, extCode, defCode int, colonRGB bool) {
	sep := func() {
		if !*first {
			buf.WriteByte(';')
		}
		*first = false
	}
	switch c.Kind {
	case ColorDefault:
		sep()
		buf.WriteString(strconv.Itoa(defCode))
	case ColorIndexed:
		sep()
		if base8 != 0 && c.Index < 8 {
			buf.WriteString(strconv.Itoa(base8 + int(c.Index)))
		} else if bright8 != 0 && c.Index >= 8 && c.Index < 16 {
			buf.WriteString(strconv.Itoa(bright8 + int(c.Index-8)))
		} else {
			buf.WriteString(strconv.Itoa(extCode))
			if colonRGB {
				buf.WriteString(":5:")
			} else {
				buf.WriteString(";5;")
			}
			buf.WriteString(strconv.Itoa(int(c.Index)))
		}
	case ColorRGB:
		sep()
		buf.WriteString(strconv.Itoa(extCode))
		if colonRGB {
			buf.WriteString(":2::")
			buf.WriteString(strconv.Itoa(int(c.R)))
			buf.WriteByte(':')
			buf.WriteString(strconv.Itoa(int(c.G)))
			buf.WriteByte(':')
			buf.WriteString(strconv.Itoa(int(c.B)))
		} else {
			buf.WriteString(";2;")
			buf.WriteString(strconv.Itoa(int(c.R)))
			buf.WriteByte(';')
			buf.WriteString(strconv.Itoa(int(c.G)))
			buf.WriteByte(';')
			buf.WriteString(strconv.Itoa(int(c.B)))
		}
	}
}
