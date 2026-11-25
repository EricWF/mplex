package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Built-in default configuration values.
const (
	DefaultKey         = `ctrl-\`
	DefaultSocket      = ""
	DefaultScrollback  = 10000
	DefaultReplayLines = 10000
	DefaultAutoUpdate  = true
	DefaultWindowTitle = true
	DefaultNaming      = "cwd"
)

// Config holds user-configured defaults loaded from config.toml.
// Zero values / nil pointers represent unset fields unless tracked in Set.
type Config struct {
	Key         string          `json:"key"`
	Socket      string          `json:"socket"`
	Scrollback  int             `json:"scrollback"`
	ReplayLines int             `json:"replay_lines"`
	AutoUpdate  *bool           `json:"auto_update"`
	WindowTitle *bool           `json:"window_title"`
	Naming      string          `json:"naming"`
	Set         map[string]bool `json:"-"`
}

// BoolPtr returns a pointer to v.
func BoolPtr(v bool) *bool {
	return &v
}

// IsSet reports whether the named TOML key was explicitly present when parsed.
func (c Config) IsSet(key string) bool {
	return c.Set != nil && c.Set[key]
}

// Default returns a Config populated with all built-in defaults.
func Default() Config {
	return Config{
		Key:         DefaultKey,
		Socket:      DefaultSocket,
		Scrollback:  DefaultScrollback,
		ReplayLines: DefaultReplayLines,
		AutoUpdate:  BoolPtr(DefaultAutoUpdate),
		WindowTitle: BoolPtr(DefaultWindowTitle),
		Naming:      DefaultNaming,
		Set: map[string]bool{
			"key":          true,
			"socket":       true,
			"scrollback":   true,
			"replay_lines": true,
			"auto_update":  true,
			"window_title": true,
			"naming":       true,
		},
	}
}

// DefaultPath returns the resolved config file path:
// $MPLEX_CONFIG, else $XDG_CONFIG_HOME/mplex/config.toml,
// else ~/.config/mplex/config.toml.
func DefaultPath() string {
	if p := os.Getenv("MPLEX_CONFIG"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mplex", "config.toml")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "mplex", "config.toml")
	}
	return filepath.Join(".config", "mplex", "config.toml")
}

// Load reads and parses the TOML config file at path.
// A missing file is not an error and returns an empty Config.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse parses flat TOML configuration data into a Config.
// Comments (# ... outside quotes), blank lines, unknown [section] headers,
// and unknown keys are ignored gracefully.
func Parse(data []byte) (Config, error) {
	s := string(data)
	s = strings.TrimPrefix(s, "\xef\xbb\xbf") // strip optional UTF-8 BOM
	s = strings.ReplaceAll(s, "\r\n", "\n")

	cfg := Config{
		Set: make(map[string]bool),
	}

	lines := strings.Split(s, "\n")
	for idx, rawLine := range lines {
		lineNum := idx + 1
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Handle [section] / [[array_of_tables]] headers gracefully.
		if strings.HasPrefix(line, "[") {
			clean := stripCommentOutsideQuotes(line)
			if !strings.HasSuffix(clean, "]") {
				return Config{}, fmt.Errorf("line %d: malformed section header", lineNum)
			}
			continue
		}

		keyPart, valPart, ok := splitKeyValue(line)
		if !ok {
			return Config{}, fmt.Errorf("line %d: expected 'key = value'", lineNum)
		}

		key, err := parseKey(keyPart)
		if err != nil {
			return Config{}, fmt.Errorf("line %d: %w", lineNum, err)
		}

		valStr := strings.TrimSpace(valPart)
		if valStr == "" {
			return Config{}, fmt.Errorf("line %d: missing value for %q", lineNum, key)
		}

		switch key {
		case "key":
			v, err := parseStringValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.Key = v
			cfg.Set["key"] = true

		case "socket":
			v, err := parseStringValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.Socket = v
			cfg.Set["socket"] = true

		case "scrollback":
			n, err := parseIntValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.Scrollback = n
			cfg.Set["scrollback"] = true

		case "replay_lines":
			n, err := parseIntValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.ReplayLines = n
			cfg.Set["replay_lines"] = true

		case "auto_update":
			b, err := parseBoolValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.AutoUpdate = BoolPtr(b)
			cfg.Set["auto_update"] = true

		case "window_title":
			b, err := parseBoolValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.WindowTitle = BoolPtr(b)
			cfg.Set["window_title"] = true

		case "naming":
			v, err := parseStringValue(valStr)
			if err != nil {
				return Config{}, fmt.Errorf("line %d: key %q: %w", lineNum, key, err)
			}
			cfg.Naming = v
			cfg.Set["naming"] = true

		default:
			// Ignore unknown keys for forward compatibility.
		}
	}

	return cfg, nil
}

// Format serializes cfg into flat TOML syntax.
func Format(cfg Config) string {
	var b strings.Builder
	if cfg.Key != "" || cfg.IsSet("key") {
		fmt.Fprintf(&b, "key = %s\n", quoteTOMLString(cfg.Key))
	}
	if cfg.Socket != "" || cfg.IsSet("socket") {
		fmt.Fprintf(&b, "socket = %s\n", quoteTOMLString(cfg.Socket))
	}
	if cfg.Scrollback > 0 || cfg.IsSet("scrollback") {
		fmt.Fprintf(&b, "scrollback = %d\n", cfg.Scrollback)
	}
	if cfg.ReplayLines > 0 || cfg.IsSet("replay_lines") {
		fmt.Fprintf(&b, "replay_lines = %d\n", cfg.ReplayLines)
	}
	if cfg.AutoUpdate != nil {
		fmt.Fprintf(&b, "auto_update = %t\n", *cfg.AutoUpdate)
	}
	if cfg.WindowTitle != nil {
		fmt.Fprintf(&b, "window_title = %t\n", *cfg.WindowTitle)
	}
	if cfg.Naming != "" || cfg.IsSet("naming") {
		fmt.Fprintf(&b, "naming = %s\n", quoteTOMLString(cfg.Naming))
	}
	return b.String()
}

// Write atomically writes cfg in TOML format to path, creating parent directories as needed.
func Write(path string, cfg Config) error {
	return writeFileAtomic(path, []byte(Format(cfg)), 0o644)
}

// StarterTOML returns a well-commented default config.toml template.
func StarterTOML() string {
	return `# mplex configuration file
# Precedence: CLI flags > MPLEX_* environment variables > this file > built-in defaults.

# Switcher trigger key (e.g. "ctrl-\\", "ctrl-space", "ctrl-]", "alt-s", "none").
# Use "none" with a terminal hotkey bound to "mplex switch" for zero-byte interception.
key = "ctrl-\\"

# Daemon Unix socket path. Leave empty to use $XDG_RUNTIME_DIR/mplex/mplex.sock
# or /tmp/mplex-<uid>/mplex.sock.
socket = ""

# Maximum scrollback lines retained per session in the shadow VT.
scrollback = 10000

# Maximum scrollback lines replayed into your outer terminal on session switch.
replay_lines = 10000

# Check GitHub Releases for newer versions on interactive startup (at most once every 24h).
auto_update = true

# Update the outer terminal window title when switching sessions.
window_title = true

# Default session naming strategy (e.g. "cwd").
naming = "cwd"
`
}

// WriteStarter writes the starter config.toml to path.
// Unless force is true, it refuses to overwrite an existing file.
func WriteStarter(path string, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data := []byte(StarterTOML())
	if !force {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("config file already exists at %s (pass --force to overwrite)", path)
			}
			return fmt.Errorf("create config %s: %w", path, err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write config %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close config %s: %w", path, err)
		}
		return nil
	}
	return writeFileAtomic(path, data, 0o644)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".mplex-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp config file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp config file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename config file: %w", err)
	}
	return nil
}

func quoteTOMLString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func stripCommentOutsideQuotes(s string) string {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

func splitKeyValue(line string) (string, string, bool) {
	inDouble := false
	inSingle := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inDouble {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '=':
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
		case '#':
			return "", "", false
		}
	}
	return "", "", false
}

func parseKey(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty key")
	}
	if strings.HasPrefix(raw, `"`) {
		return parseDoubleQuoted(raw)
	}
	if strings.HasPrefix(raw, `'`) {
		return parseSingleQuoted(raw)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isAlpha && !isDigit && c != '_' && c != '-' {
			return "", fmt.Errorf("invalid key %q", raw)
		}
	}
	return raw, nil
}

func parseStringValue(raw string) (string, error) {
	if strings.HasPrefix(raw, `"`) {
		return parseDoubleQuoted(raw)
	}
	if strings.HasPrefix(raw, `'`) {
		return parseSingleQuoted(raw)
	}
	return "", fmt.Errorf("expected quoted string, got %q", stripInlineComment(raw))
}

func parseDoubleQuoted(s string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			i++
			if i >= len(s) {
				return "", errors.New("unterminated escape sequence in string")
			}
			switch s[i] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if i+4 >= len(s) {
					return "", errors.New("incomplete \\uXXXX escape")
				}
				v, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
				if err != nil {
					return "", fmt.Errorf("invalid \\uXXXX escape: %w", err)
				}
				b.WriteRune(rune(v))
				i += 4
			case 'U':
				if i+8 >= len(s) {
					return "", errors.New("incomplete \\UXXXXXXXX escape")
				}
				v, err := strconv.ParseUint(s[i+1:i+9], 16, 32)
				if err != nil {
					return "", fmt.Errorf("invalid \\UXXXXXXXX escape: %w", err)
				}
				b.WriteRune(rune(v))
				i += 8
			default:
				return "", fmt.Errorf("unknown escape sequence \\%c", s[i])
			}
			continue
		}
		if c == '"' {
			rest := strings.TrimSpace(s[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", fmt.Errorf("unexpected trailing characters %q after string", rest)
			}
			return b.String(), nil
		}
		b.WriteByte(c)
	}
	return "", errors.New("unterminated double-quoted string")
}

func parseSingleQuoted(s string) (string, error) {
	end := strings.IndexByte(s[1:], '\'')
	if end < 0 {
		return "", errors.New("unterminated single-quoted string")
	}
	val := s[1 : 1+end]
	rest := strings.TrimSpace(s[1+end+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected trailing characters %q after string", rest)
	}
	return val, nil
}

func stripInlineComment(s string) string {
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func parseIntValue(raw string) (int, error) {
	clean := stripInlineComment(raw)
	if clean == "" {
		return 0, errors.New("expected integer")
	}
	if strings.HasPrefix(clean, "_") || strings.HasSuffix(clean, "_") || strings.Contains(clean, "__") {
		return 0, fmt.Errorf("invalid integer %q", clean)
	}
	numStr := strings.ReplaceAll(clean, "_", "")
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", clean)
	}
	if n < 0 {
		return 0, fmt.Errorf("expected non-negative integer, got %d", n)
	}
	return n, nil
}

func parseBoolValue(raw string) (bool, error) {
	clean := stripInlineComment(raw)
	switch clean {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("expected boolean (true or false), got %q", clean)
	}
}
