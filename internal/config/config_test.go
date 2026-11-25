package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPathResolution(t *testing.T) {
	t.Run("MPLEX_CONFIG wins", func(t *testing.T) {
		t.Setenv("MPLEX_CONFIG", "/custom/mplex.toml")
		t.Setenv("XDG_CONFIG_HOME", "/xdg/cfg")
		if got := DefaultPath(); got != "/custom/mplex.toml" {
			t.Fatalf("DefaultPath() = %q, want %q", got, "/custom/mplex.toml")
		}
	})

	t.Run("XDG_CONFIG_HOME fallback", func(t *testing.T) {
		t.Setenv("MPLEX_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg/cfg")
		want := filepath.Join("/xdg/cfg", "mplex", "config.toml")
		if got := DefaultPath(); got != want {
			t.Fatalf("DefaultPath() = %q, want %q", got, want)
		}
	})

	t.Run("HOME fallback", func(t *testing.T) {
		t.Setenv("MPLEX_CONFIG", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		want := filepath.Join(tmpHome, ".config", "mplex", "config.toml")
		if got := DefaultPath(); got != want {
			t.Fatalf("DefaultPath() = %q, want %q", got, want)
		}
	})
}

func TestLoadMissingFileIsNotError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "config.toml")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(missing) returned unexpected error: %v", err)
	}
	if cfg.Key != "" || cfg.Socket != "" || cfg.Scrollback != 0 || cfg.ReplayLines != 0 ||
		cfg.AutoUpdate != nil || cfg.WindowTitle != nil || cfg.Naming != "" {
		t.Fatalf("expected zero Config for missing file, got %+v", cfg)
	}
	if cfg.IsSet("key") || cfg.IsSet("auto_update") {
		t.Fatalf("expected no fields marked Set for missing file")
	}
}

func TestParseFullConfig(t *testing.T) {
	raw := "\xef\xbb\xbf" + `
# Top comment
   # Indented comment

[general] # section header ignored
key = "ctrl-\\" # inline comment with # hash
socket = '/tmp/my#sock/mplex.sock' # literal single-quoted string with # inside
scrollback = 25_000 # underscores in integer
replay_lines = 5000
auto_update = false # disable startup checks
window_title = true
naming = "cwd"

[future_section]
unknown_key = ["ignored", "array"]
another_unknown = { a = 1 }
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Key != `ctrl-\` {
		t.Errorf("Key = %q, want %q", cfg.Key, `ctrl-\`)
	}
	if cfg.Socket != "/tmp/my#sock/mplex.sock" {
		t.Errorf("Socket = %q, want %q", cfg.Socket, "/tmp/my#sock/mplex.sock")
	}
	if cfg.Scrollback != 25000 {
		t.Errorf("Scrollback = %d, want 25000", cfg.Scrollback)
	}
	if cfg.ReplayLines != 5000 {
		t.Errorf("ReplayLines = %d, want 5000", cfg.ReplayLines)
	}
	if cfg.AutoUpdate == nil || *cfg.AutoUpdate != false {
		t.Errorf("AutoUpdate = %v, want false", cfg.AutoUpdate)
	}
	if cfg.WindowTitle == nil || *cfg.WindowTitle != true {
		t.Errorf("WindowTitle = %v, want true", cfg.WindowTitle)
	}
	if cfg.Naming != "cwd" {
		t.Errorf("Naming = %q, want %q", cfg.Naming, "cwd")
	}
	for _, k := range []string{"key", "socket", "scrollback", "replay_lines", "auto_update", "window_title", "naming"} {
		if !cfg.IsSet(k) {
			t.Errorf("expected IsSet(%q) = true", k)
		}
	}
}

func TestParseStringEscapesAndSingleQuotes(t *testing.T) {
	raw := "key = \"line1\\nline2\\t\\\"quoted\\\"\\\\end\" # comment\r\n" +
		"socket = 'literal \\n \\t \"no-escape\" # keep'\r\n" +
		"naming = \"\\u0063wd\"\r\n"

	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	wantKey := "line1\nline2\t\"quoted\"\\end"
	if cfg.Key != wantKey {
		t.Errorf("Key = %q, want %q", cfg.Key, wantKey)
	}
	wantSocket := `literal \n \t "no-escape" # keep`
	if cfg.Socket != wantSocket {
		t.Errorf("Socket = %q, want %q", cfg.Socket, wantSocket)
	}
	if cfg.Naming != "cwd" {
		t.Errorf("Naming = %q, want %q", cfg.Naming, "cwd")
	}
	if cfg.IsSet("scrollback") || cfg.AutoUpdate != nil || cfg.WindowTitle != nil {
		t.Errorf("unset fields should stay unset: %+v", cfg)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{"missing equals", "key ctrl-a", "line 1"},
		{"malformed section", "[unclosed", "line 1"},
		{"missing value", "key = ", "line 1"},
		{"unquoted string", "key = ctrl-a", "quoted string"},
		{"unterminated double quote", `key = "ctrl-a`, "unterminated"},
		{"unterminated single quote", `key = 'ctrl-a`, "unterminated"},
		{"trailing garbage after double quote", `key = "ctrl-a" extra`, "trailing"},
		{"trailing garbage after single quote", `key = 'ctrl-a' extra`, "trailing"},
		{"bad escape", `key = "ctrl-\q"`, "escape"},
		{"bad int", `scrollback = abc`, "integer"},
		{"negative int", `scrollback = -10`, "non-negative"},
		{"bad underscore int", `scrollback = 10__0`, "integer"},
		{"bad bool", `auto_update = maybe`, "boolean"},
		{"bad bool window_title", `window_title = 1`, "boolean"},
		{"second line error", "key = \"alt-s\"\nreplay_lines = nope\n", "line 2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.input)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestFormatAndWriteRoundTrip(t *testing.T) {
	orig := Config{
		Key:         `ctrl-\`,
		Socket:      "/tmp/custom/mplex.sock",
		Scrollback:  15000,
		ReplayLines: 4000,
		AutoUpdate:  BoolPtr(false),
		WindowTitle: BoolPtr(true),
		Naming:      "cwd",
	}

	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	if err := Write(path, orig); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got.Key != orig.Key ||
		got.Socket != orig.Socket ||
		got.Scrollback != orig.Scrollback ||
		got.ReplayLines != orig.ReplayLines ||
		got.AutoUpdate == nil || *got.AutoUpdate != *orig.AutoUpdate ||
		got.WindowTitle == nil || *got.WindowTitle != *orig.WindowTitle ||
		got.Naming != orig.Naming {
		t.Fatalf("roundtrip mismatch:\n got: %+v\nwant: %+v", got, orig)
	}
}

func TestStarterTOMLParsesCleanlyAndMatchesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(StarterTOML()))
	if err != nil {
		t.Fatalf("StarterTOML failed to parse: %v", err)
	}
	def := Default()
	if cfg.Key != def.Key {
		t.Errorf("Key = %q, want %q", cfg.Key, def.Key)
	}
	if cfg.Socket != def.Socket {
		t.Errorf("Socket = %q, want %q", cfg.Socket, def.Socket)
	}
	if cfg.Scrollback != def.Scrollback {
		t.Errorf("Scrollback = %d, want %d", cfg.Scrollback, def.Scrollback)
	}
	if cfg.ReplayLines != def.ReplayLines {
		t.Errorf("ReplayLines = %d, want %d", cfg.ReplayLines, def.ReplayLines)
	}
	if cfg.AutoUpdate == nil || *cfg.AutoUpdate != DefaultAutoUpdate {
		t.Errorf("AutoUpdate = %v, want %v", cfg.AutoUpdate, DefaultAutoUpdate)
	}
	if cfg.WindowTitle == nil || *cfg.WindowTitle != DefaultWindowTitle {
		t.Errorf("WindowTitle = %v, want %v", cfg.WindowTitle, DefaultWindowTitle)
	}
	if cfg.Naming != def.Naming {
		t.Errorf("Naming = %q, want %q", cfg.Naming, def.Naming)
	}
}

func TestWriteStarterNoOverwriteAndForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mplex", "config.toml")
	if err := WriteStarter(path, false); err != nil {
		t.Fatalf("initial WriteStarter failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("key = \"alt-s\"\n"), 0o644); err != nil {
		t.Fatalf("overwrite fixture: %v", err)
	}
	if err := WriteStarter(path, false); err == nil {
		t.Fatalf("expected WriteStarter(force=false) to refuse overwrite")
	}
	// Verify original custom content wasn't clobbered.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "key = \"alt-s\"\n" {
		t.Fatalf("file was modified despite force=false: %q", string(data))
	}
	// With force=true it must overwrite cleanly.
	if err := WriteStarter(path, true); err != nil {
		t.Fatalf("WriteStarter(force=true) failed: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after force init failed: %v", err)
	}
	if cfg.Key != DefaultKey {
		t.Fatalf("Key after force init = %q, want %q", cfg.Key, DefaultKey)
	}
}
