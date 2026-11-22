package vt

import "unicode/utf8"

// Parser implements Paul Williams' VT500 / ECMA-48 state machine with
// streaming UTF-8 decoding and CSI colon subparameter support (e.g. 4:3, 38:2::R:G:B).
type parserState uint8

const (
	stateGround parserState = iota
	stateEscape
	stateEscapeIntermediate
	stateCsiEntry
	stateCsiParam
	stateCsiIntermediate
	stateCsiIgnore
	stateDcsEntry
	stateDcsParam
	stateDcsIntermediate
	stateDcsPassthrough
	stateDcsIgnore
	stateOscString
	stateSosPmApcString
)

const (
	maxCSIParams    = 32
	maxCSISubParams = 8
	maxOscBytes     = 512 * 1024
)

// CSIParams holds parsed semicolon-separated parameters and optional colon-separated subparameters.
type CSIParams struct {
	// values[i] holds the subparameters for parameter i; values[i][0] is the main value.
	// -1 indicates an omitted/default parameter.
	values [maxCSIParams][maxCSISubParams]int
	subLen [maxCSIParams]int
	count  int
}

func (p *CSIParams) reset() {
	p.count = 0
}

func (p *CSIParams) Len() int {
	return p.count
}

// Get returns parameter idx (0-based), or def if omitted or <= 0 when zeroIsDef is true.
func (p *CSIParams) Get(idx, def int) int {
	if idx < 0 || idx >= p.count {
		return def
	}
	v := p.values[idx][0]
	if v < 0 {
		return def
	}
	return v
}

// GetPos returns parameter idx, treating both omitted (-1) and 0 as def.
func (p *CSIParams) GetPos(idx, def int) int {
	v := p.Get(idx, def)
	if v <= 0 {
		return def
	}
	return v
}

// SubLen returns the number of subparameters for parameter idx (1 if no colons were used).
func (p *CSIParams) SubLen(idx int) int {
	if idx < 0 || idx >= p.count {
		return 0
	}
	return p.subLen[idx]
}

// Sub returns subparameter subIdx of parameter idx, or def if omitted.
func (p *CSIParams) Sub(idx, subIdx, def int) int {
	if idx < 0 || idx >= p.count || subIdx < 0 || subIdx >= p.subLen[idx] {
		return def
	}
	v := p.values[idx][subIdx]
	if v < 0 {
		return def
	}
	return v
}

func (p *CSIParams) startIfEmpty() {
	if p.count == 0 {
		p.count = 1
		p.values[0][0] = -1
		p.subLen[0] = 1
	}
}

func (p *CSIParams) pushDigit(b byte) {
	p.startIfEmpty()
	idx := p.count - 1
	sub := p.subLen[idx] - 1
	cur := p.values[idx][sub]
	if cur < 0 {
		cur = 0
	}
	// Saturate to avoid integer overflow on hostile inputs.
	if cur < 1000000 {
		cur = cur*10 + int(b-'0')
	}
	p.values[idx][sub] = cur
}

func (p *CSIParams) nextParam() {
	p.startIfEmpty()
	if p.count < maxCSIParams {
		p.values[p.count][0] = -1
		p.subLen[p.count] = 1
		p.count++
	}
}

func (p *CSIParams) nextSubParam() {
	p.startIfEmpty()
	idx := p.count - 1
	if p.subLen[idx] < maxCSISubParams {
		p.values[idx][p.subLen[idx]] = -1
		p.subLen[idx]++
	}
}

type parserHandler interface {
	printRune(r rune)
	execute(b byte)
	escDispatch(intermediates []byte, final byte)
	csiDispatch(private byte, intermediates []byte, params *CSIParams, final byte)
	oscDispatch(data []byte)
}

type parser struct {
	state parserState

	private       byte
	intermediates [4]byte
	intermLen     int
	params        CSIParams

	oscBuf []byte
	oscEsc bool // saw ESC inside OSC/DCS/APC string (waiting for '\' ST)

	// Streaming UTF-8 accumulator for Ground state.
	utf8Buf [4]byte
	utf8Len int
	utf8Exp int
}

func (p *parser) reset() {
	p.state = stateGround
	p.private = 0
	p.intermLen = 0
	p.params.reset()
	p.oscBuf = p.oscBuf[:0]
	p.oscEsc = false
	p.utf8Len = 0
	p.utf8Exp = 0
}

func (p *parser) clearSeq() {
	p.private = 0
	p.intermLen = 0
	p.params.reset()
}

func (p *parser) collectInterm(b byte) {
	if p.intermLen < len(p.intermediates) {
		p.intermediates[p.intermLen] = b
		p.intermLen++
	}
}

func (p *parser) feed(data []byte, h parserHandler) {
	for i := 0; i < len(data); i++ {
		b := data[i]

		// Fast path: handle pending UTF-8 continuation bytes in Ground state.
		if p.state == stateGround && p.utf8Len > 0 {
			if b&0xC0 == 0x80 {
				p.utf8Buf[p.utf8Len] = b
				p.utf8Len++
				if p.utf8Len == p.utf8Exp {
					r, _ := utf8.DecodeRune(p.utf8Buf[:p.utf8Len])
					p.utf8Len = 0
					p.utf8Exp = 0
					h.printRune(r)
				}
				continue
			}
			// Invalid/interrupted UTF-8 sequence: flush replacement and re-process b.
			p.utf8Len = 0
			p.utf8Exp = 0
			h.printRune(utf8.RuneError)
		}

		// Handle ESC inside string states (OSC / DCS / APC) looking for ST (ESC \).
		if p.oscEsc {
			p.oscEsc = false
			if b == '\\' {
				if p.state == stateOscString {
					h.oscDispatch(p.oscBuf)
					p.oscBuf = p.oscBuf[:0]
				}
				p.state = stateGround
				continue
			}
			// Not ST: terminate string silently and process b in Escape state!
			if p.state == stateOscString {
				p.oscBuf = p.oscBuf[:0]
			}
			p.clearSeq()
			p.state = stateEscape
			// fall through to stateEscape processing for b
		}

		// Universal anywhere transitions (CAN / SUB / ESC).
		switch b {
		case 0x18, 0x1A: // CAN, SUB
			p.oscBuf = p.oscBuf[:0]
			p.state = stateGround
			h.execute(b)
			continue
		case 0x1B: // ESC
			if p.state == stateOscString || p.state == stateDcsPassthrough || p.state == stateSosPmApcString {
				p.oscEsc = true
				continue
			}
			p.clearSeq()
			p.state = stateEscape
			continue
		}

		switch p.state {
		case stateGround:
			if b < 0x20 {
				h.execute(b)
			} else if b < 0x7F {
				h.printRune(rune(b))
			} else if b == 0x7F {
				// DEL ignored in Ground
			} else if b >= 0xC0 {
				p.utf8Buf[0] = b
				p.utf8Len = 1
				switch {
				case b&0xE0 == 0xC0:
					p.utf8Exp = 2
				case b&0xF0 == 0xE0:
					p.utf8Exp = 3
				case b&0xF8 == 0xF0:
					p.utf8Exp = 4
				default:
					p.utf8Len = 0
					h.printRune(utf8.RuneError)
				}
			} else {
				// Stray continuation byte (0x80..0xBF)
				// Note: ignore C1 controls (0x80..0x9F) rather than mis-parsing UTF-8
				if b >= 0xA0 {
					h.printRune(utf8.RuneError)
				}
			}

		case stateEscape:
			switch {
			case b < 0x20:
				h.execute(b)
			case b >= 0x20 && b <= 0x2F:
				p.collectInterm(b)
				p.state = stateEscapeIntermediate
			case b == '[':
				p.clearSeq()
				p.state = stateCsiEntry
			case b == ']':
				p.oscBuf = p.oscBuf[:0]
				p.state = stateOscString
			case b == 'P':
				p.clearSeq()
				p.state = stateDcsEntry
			case b == 'X', b == '^', b == '_': // SOS, PM, APC (e.g. Kitty graphics)
				p.state = stateSosPmApcString
			case b >= 0x30 && b <= 0x7E:
				h.escDispatch(p.intermediates[:p.intermLen], b)
				p.state = stateGround
			case b == 0x7F:
				// ignore
			default:
				p.state = stateGround
			}

		case stateEscapeIntermediate:
			switch {
			case b < 0x20:
				h.execute(b)
			case b >= 0x20 && b <= 0x2F:
				p.collectInterm(b)
			case b >= 0x30 && b <= 0x7E:
				h.escDispatch(p.intermediates[:p.intermLen], b)
				p.state = stateGround
			case b == 0x7F:
				// ignore
			default:
				p.state = stateGround
			}

		case stateCsiEntry:
			switch {
			case b < 0x20:
				h.execute(b)
			case b >= 0x20 && b <= 0x2F:
				p.collectInterm(b)
				p.state = stateCsiIntermediate
			case b >= '0' && b <= '9':
				p.params.pushDigit(b)
				p.state = stateCsiParam
			case b == ';':
				p.params.nextParam()
				p.state = stateCsiParam
			case b == ':':
				p.params.nextSubParam()
				p.state = stateCsiParam
			case b >= 0x3C && b <= 0x3F: // '<', '=', '>', '?'
				p.private = b
				p.state = stateCsiParam
			case b >= 0x40 && b <= 0x7E:
				h.csiDispatch(p.private, p.intermediates[:p.intermLen], &p.params, b)
				p.state = stateGround
			case b == 0x7F:
				// ignore
			default:
				p.state = stateGround
			}

		case stateCsiParam:
			switch {
			case b < 0x20:
				h.execute(b)
			case b >= '0' && b <= '9':
				p.params.pushDigit(b)
			case b == ';':
				p.params.nextParam()
			case b == ':':
				p.params.nextSubParam()
			case b >= 0x20 && b <= 0x2F:
				p.collectInterm(b)
				p.state = stateCsiIntermediate
			case b >= 0x3C && b <= 0x3F:
				p.state = stateCsiIgnore
			case b >= 0x40 && b <= 0x7E:
				h.csiDispatch(p.private, p.intermediates[:p.intermLen], &p.params, b)
				p.state = stateGround
			case b == 0x7F:
				// ignore
			default:
				p.state = stateGround
			}

		case stateCsiIntermediate:
			switch {
			case b < 0x20:
				h.execute(b)
			case b >= 0x20 && b <= 0x2F:
				p.collectInterm(b)
			case b >= 0x30 && b <= 0x3F:
				p.state = stateCsiIgnore
			case b >= 0x40 && b <= 0x7E:
				h.csiDispatch(p.private, p.intermediates[:p.intermLen], &p.params, b)
				p.state = stateGround
			case b == 0x7F:
				// ignore
			default:
				p.state = stateGround
			}

		case stateCsiIgnore:
			if b < 0x20 {
				h.execute(b)
			} else if b >= 0x40 && b <= 0x7E {
				p.state = stateGround
			}

		case stateOscString:
			if b == 0x07 { // BEL terminates OSC
				h.oscDispatch(p.oscBuf)
				p.oscBuf = p.oscBuf[:0]
				p.state = stateGround
			} else if b == 0x9C { // 8-bit ST
				h.oscDispatch(p.oscBuf)
				p.oscBuf = p.oscBuf[:0]
				p.state = stateGround
			} else if b >= 0x20 || b == '\t' {
				if len(p.oscBuf) < maxOscBytes {
					p.oscBuf = append(p.oscBuf, b)
				}
			}

		case stateDcsEntry:
			switch {
			case b >= 0x20 && b <= 0x2F:
				p.state = stateDcsIntermediate
			case b >= 0x30 && b <= 0x3F:
				p.state = stateDcsParam
			case b >= 0x40 && b <= 0x7E:
				p.state = stateDcsPassthrough
			}

		case stateDcsParam:
			switch {
			case b >= 0x20 && b <= 0x2F:
				p.state = stateDcsIntermediate
			case b >= 0x40 && b <= 0x7E:
				p.state = stateDcsPassthrough
			}

		case stateDcsIntermediate:
			if b >= 0x40 && b <= 0x7E {
				p.state = stateDcsPassthrough
			}

		case stateDcsPassthrough, stateDcsIgnore, stateSosPmApcString:
			// Ignore bytes until ST (ESC \ handled above) or 8-bit ST (0x9C).
			if b == 0x9C {
				p.state = stateGround
			}
		}
	}
}
