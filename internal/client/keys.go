package client

import (
	"fmt"
	"strings"
)

// ParseSendKey parses a human-friendly key name (e.g. "enter", "ctrl-c", "up",
// "alt-x", "^d", "c-c") into the raw terminal byte sequence to write to a PTY.
func ParseSendKey(spec string) ([]byte, error) {
	s := strings.TrimSpace(strings.ToLower(spec))
	s = strings.ReplaceAll(s, "+", "-")
	switch s {
	case "enter", "return", "cr":
		return []byte{'\r'}, nil
	case "lf", "newline":
		return []byte{'\n'}, nil
	case "tab":
		return []byte{'\t'}, nil
	case "esc", "escape":
		return []byte{0x1b}, nil
	case "space", "spc":
		return []byte{' '}, nil
	case "bs", "backspace", "bspace":
		return []byte{0x7f}, nil
	case "up":
		return []byte("\x1b[A"), nil
	case "down":
		return []byte("\x1b[B"), nil
	case "right":
		return []byte("\x1b[C"), nil
	case "left":
		return []byte("\x1b[D"), nil
	case "home":
		return []byte("\x1b[H"), nil
	case "end":
		return []byte("\x1b[F"), nil
	case "pgup", "pageup":
		return []byte("\x1b[5~"), nil
	case "pgdn", "pagedown":
		return []byte("\x1b[6~"), nil
	case "del", "delete":
		return []byte("\x1b[3~"), nil
	}

	if strings.HasPrefix(s, "^") && len(s) == 2 {
		s = "ctrl-" + s[1:]
	} else if strings.HasPrefix(s, "c-") && len(s) >= 3 {
		s = "ctrl-" + s[2:]
	} else if strings.HasPrefix(s, "m-") && len(s) >= 3 {
		s = "alt-" + s[2:]
	}

	if strings.HasPrefix(s, "ctrl-") {
		k := strings.TrimPrefix(s, "ctrl-")
		switch k {
		case "space", "spc", "@":
			return []byte{0x00}, nil
		case "[":
			return []byte{0x1b}, nil
		case "\\", "backslash":
			return []byte{0x1c}, nil
		case "]":
			return []byte{0x1d}, nil
		case "^", "6":
			return []byte{0x1e}, nil
		case "_", "/":
			return []byte{0x1f}, nil
		default:
			if len(k) == 1 && k[0] >= 'a' && k[0] <= 'z' {
				return []byte{k[0] - 'a' + 1}, nil
			}
		}
		return nil, fmt.Errorf("unsupported key %q", spec)
	}

	if strings.HasPrefix(s, "alt-") {
		k := strings.TrimPrefix(s, "alt-")
		if len(k) == 1 && k[0] >= 0x20 && k[0] <= 0x7e {
			return []byte{0x1b, k[0]}, nil
		}
		return nil, fmt.Errorf("unsupported key %q", spec)
	}

	return nil, fmt.Errorf("unknown key %q (try enter, tab, esc, up, down, ctrl-c, ctrl-d, ...)", spec)
}
