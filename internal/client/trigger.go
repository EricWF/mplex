package client

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// TriggerKey identifies the optional in-band hotkey that opens the session switcher.
// It detects the key across legacy terminal encoding, Kitty keyboard protocol (CSI u),
// and xterm modifyOtherKeys (CSI 27;mods;code~) with zero input buffering lag.
type TriggerKey struct {
	Disabled  bool
	Label     string
	Legacy    []byte // exact legacy byte sequence (1 byte for Ctrl, 2 bytes for Alt)
	Codepoint int    // Unicode codepoint for CSI u / modifyOtherKeys (e.g. '\\' = 92)
	AltCode   int    // optional secondary codepoint (e.g. '@' = 64 for ctrl-space)
	ModMask   int    // xterm/Kitty modifier number: 5 = Ctrl, 3 = Alt
}

// ParseTriggerKey parses a human-readable key spec like "ctrl-\\", "ctrl-space",
// "ctrl-]", "ctrl-a", "alt-s", or "none".
func ParseTriggerKey(spec string) (TriggerKey, error) {
	s := strings.TrimSpace(strings.ToLower(spec))
	if s == "" {
		s = "ctrl-\\"
	}
	if s == "none" || s == "off" || s == "disabled" {
		return TriggerKey{Disabled: true, Label: "none"}, nil
	}

	// Normalize separators: accept ctrl+\, ctrl-\, C-\, ^\
	s = strings.ReplaceAll(s, "+", "-")
	if strings.HasPrefix(s, "^") && len(s) == 2 {
		s = "ctrl-" + s[1:]
	} else if strings.HasPrefix(s, "c-") && len(s) >= 3 {
		s = "ctrl-" + s[2:]
	} else if strings.HasPrefix(s, "m-") && len(s) >= 3 {
		s = "alt-" + s[2:]
	}

	if strings.HasPrefix(s, "ctrl-") {
		key := strings.TrimPrefix(s, "ctrl-")
		switch key {
		case "\\", "backslash":
			return TriggerKey{
				Label:     "Ctrl+\\",
				Legacy:    []byte{0x1c},
				Codepoint: '\\',
				ModMask:   5,
			}, nil
		case "space", "spc", "@":
			return TriggerKey{
				Label:     "Ctrl+Space",
				Legacy:    []byte{0x00},
				Codepoint: ' ',
				AltCode:   '@',
				ModMask:   5,
			}, nil
		case "]":
			return TriggerKey{
				Label:     "Ctrl+]",
				Legacy:    []byte{0x1d},
				Codepoint: ']',
				ModMask:   5,
			}, nil
		case "^", "6":
			return TriggerKey{
				Label:     "Ctrl+^",
				Legacy:    []byte{0x1e},
				Codepoint: '^',
				AltCode:   '6',
				ModMask:   5,
			}, nil
		case "_", "/":
			return TriggerKey{
				Label:     "Ctrl+_",
				Legacy:    []byte{0x1f},
				Codepoint: '_',
				AltCode:   '/',
				ModMask:   5,
			}, nil
		default:
			if len(key) == 1 && key[0] >= 'a' && key[0] <= 'z' {
				b := key[0] - 'a' + 1
				return TriggerKey{
					Label:     "Ctrl+" + strings.ToUpper(key),
					Legacy:    []byte{b},
					Codepoint: int(key[0]),
					ModMask:   5,
				}, nil
			}
		}
		return TriggerKey{}, fmt.Errorf("unsupported ctrl trigger key %q (try ctrl-\\, ctrl-space, ctrl-], ctrl-a..z)", spec)
	}

	if strings.HasPrefix(s, "alt-") {
		key := strings.TrimPrefix(s, "alt-")
		if len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e && key[0] != '[' && key[0] != 'O' {
			return TriggerKey{
				Label:     "Alt+" + key,
				Legacy:    []byte{0x1b, key[0]},
				Codepoint: int(key[0]),
				ModMask:   3,
			}, nil
		}
		return TriggerKey{}, fmt.Errorf("unsupported alt trigger key %q", spec)
	}

	return TriggerKey{}, fmt.Errorf("invalid trigger key %q (expected ctrl-<key>, alt-<key>, or none)", spec)
}

// Match scans a raw input chunk for the first occurrence of this trigger key.
// It checks legacy bytes, Kitty CSI u sequences, and xterm modifyOtherKeys sequences.
// Never buffers incomplete ESC bytes so bare Escape in vim/editors has zero lag.
func (tk TriggerKey) Match(chunk []byte) (before, matched, after []byte, ok bool) {
	if tk.Disabled || len(chunk) == 0 {
		return chunk, nil, nil, false
	}

	for i := 0; i < len(chunk); i++ {
		// 1. Check CSI sequences: Kitty keyboard protocol (\x1b[...u) and modifyOtherKeys (\x1b[27;...~)
		if chunk[i] == 0x1b && i+2 < len(chunk) && chunk[i+1] == '[' {
			if end, isTrig := tk.matchCSI(chunk[i:]); isTrig {
				return chunk[:i], chunk[i : i+end], chunk[i+end:], true
			}
			// Skip past complete CSI sequence so inner bytes aren't misread as legacy bytes.
			if end := scanCSIEnd(chunk[i:]); end > 0 {
				i += end - 1
				continue
			}
		}

		// 2. Check legacy encoding (1-byte Ctrl or 2-byte Alt)
		if len(tk.Legacy) == 1 {
			if chunk[i] == tk.Legacy[0] {
				return chunk[:i], chunk[i : i+1], chunk[i+1:], true
			}
		} else if len(tk.Legacy) == 2 {
			if i+1 < len(chunk) && bytes.Equal(chunk[i:i+2], tk.Legacy) {
				return chunk[:i], chunk[i : i+2], chunk[i+2:], true
			}
		}
	}

	return chunk, nil, nil, false
}

func (tk TriggerKey) matchesCodepoint(cp int) bool {
	if cp == tk.Codepoint {
		return true
	}
	if tk.AltCode != 0 && cp == tk.AltCode {
		return true
	}
	// Accept uppercase variant if shift wasn't requested or caps-lock normalized.
	if tk.Codepoint >= 'a' && tk.Codepoint <= 'z' && cp == tk.Codepoint-'a'+'A' {
		return true
	}
	return false
}

// matchCSI checks whether seq (starting with "\x1b[") is a Kitty CSI u or
// modifyOtherKeys sequence representing this TriggerKey (press or repeat, NOT release).
func (tk TriggerKey) matchCSI(seq []byte) (end int, ok bool) {
	end = scanCSIEnd(seq)
	if end < 4 {
		return 0, false
	}
	final := seq[end-1]
	body := string(seq[2 : end-1])

	switch final {
	case 'u':
		// Kitty keyboard protocol: <codepoint>(:<shifted>(:<base>))?;<mods>(:<event>)?u
		parts := strings.Split(body, ";")
		if len(parts) < 2 {
			return 0, false
		}
		cpParts := strings.Split(parts[0], ":")
		cp, err := strconv.Atoi(cpParts[0])
		if err != nil {
			return 0, false
		}
		// Check alternate base-layout codepoint if present (3rd subparam).
		cpMatch := tk.matchesCodepoint(cp)
		if !cpMatch && len(cpParts) >= 3 && cpParts[2] != "" {
			if baseCP, err := strconv.Atoi(cpParts[2]); err == nil {
				cpMatch = tk.matchesCodepoint(baseCP)
			}
		}
		if !cpMatch {
			return 0, false
		}

		modParts := strings.Split(parts[1], ":")
		modVal, err := strconv.Atoi(modParts[0])
		if err != nil {
			return 0, false
		}
		// Ignore key release events (:3).
		if len(modParts) >= 2 && modParts[1] == "3" {
			return 0, false
		}
		// Ignore CapsLock (64) / NumLock (128) bits in Kitty modifier mask.
		cleanMod := ((modVal - 1) &^ (64 | 128)) + 1
		if cleanMod == tk.ModMask {
			return end, true
		}

	case '~':
		// xterm modifyOtherKeys: 27;<mods>;<codepoint>~
		parts := strings.Split(body, ";")
		if len(parts) != 3 || parts[0] != "27" {
			return 0, false
		}
		modVal, err1 := strconv.Atoi(parts[1])
		cp, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return 0, false
		}
		if modVal == tk.ModMask && tk.matchesCodepoint(cp) {
			return end, true
		}
	}

	return 0, false
}

func scanCSIEnd(seq []byte) int {
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
