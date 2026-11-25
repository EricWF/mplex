package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultPathAndResolvePath(t *testing.T) {
	t.Run("XDG_STATE_HOME", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_STATE_HOME", xdg)
		got, err := DefaultPath("")
		if err != nil {
			t.Fatalf("DefaultPath: %v", err)
		}
		want := filepath.Join(xdg, "mplex", "workspaces", "default.json")
		if got != want {
			t.Fatalf("DefaultPath() = %q, want %q", got, want)
		}

		gotNamed, err := DefaultPath("work")
		if err != nil {
			t.Fatalf("DefaultPath(work): %v", err)
		}
		wantNamed := filepath.Join(xdg, "mplex", "workspaces", "work.json")
		if gotNamed != wantNamed {
			t.Fatalf("DefaultPath(work) = %q, want %q", gotNamed, wantNamed)
		}
	})

	t.Run("HOME fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", home)
		got, err := DefaultPath("dev")
		if err != nil {
			t.Fatalf("DefaultPath(dev): %v", err)
		}
		want := filepath.Join(home, ".local", "state", "mplex", "workspaces", "dev.json")
		if got != want {
			t.Fatalf("DefaultPath(dev) = %q, want %q", got, want)
		}
	})

	t.Run("explicit file", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "my-snap.json")
		p, name, err := ResolvePath("", custom)
		if err != nil {
			t.Fatalf("ResolvePath: %v", err)
		}
		if p != custom || name != "my-snap" {
			t.Fatalf("ResolvePath = (%q, %q), want (%q, %q)", p, name, custom, "my-snap")
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		for _, bad := range []string{"../evil", "a/b", `a\b`, "..", ".", "foo..bar"} {
			if _, err := DefaultPath(bad); err == nil {
				t.Errorf("DefaultPath(%q) expected error, got nil", bad)
			}
		}
	})
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "work.json")
	savedAt := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	snap := Snapshot{
		Name:    "work",
		SavedAt: savedAt,
		Sessions: []SavedSession{
			{
				Name:      "api",
				Cwd:       "/srv/api",
				Title:     "api server",
				FgCommand: "go",
			},
			{
				Name:           "logs",
				Cwd:            "/var/log",
				FgCommand:      "tail",
				ScrollbackTail: []string{"line 1", "line 2"},
			},
		},
	}

	if err := Save(path, snap); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify exact JSON schema fields on disk (scrollback_tail omitted when empty).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("json.Unmarshal raw: %v", err)
	}
	if int(rawMap["version"].(float64)) != CurrentVersion {
		t.Errorf("version = %v, want %d", rawMap["version"], CurrentVersion)
	}
	if rawMap["name"] != "work" {
		t.Errorf("name = %v, want work", rawMap["name"])
	}
	if _, ok := rawMap["saved_at"]; !ok {
		t.Errorf("missing saved_at field")
	}
	rawSessions := rawMap["sessions"].([]any)
	firstSess := rawSessions[0].(map[string]any)
	if _, exists := firstSess["scrollback_tail"]; exists {
		t.Errorf("expected scrollback_tail to be omitted when empty")
	}
	secondSess := rawSessions[1].(map[string]any)
	if _, exists := secondSess["scrollback_tail"]; !exists {
		t.Errorf("expected scrollback_tail to be present on second session")
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, CurrentVersion)
	}
	if !loaded.SavedAt.Equal(savedAt) {
		t.Errorf("SavedAt = %v, want %v", loaded.SavedAt, savedAt)
	}
	if loaded.Name != "work" {
		t.Errorf("Name = %q, want work", loaded.Name)
	}
	if len(loaded.Sessions) != 2 {
		t.Fatalf("len(Sessions) = %d, want 2", len(loaded.Sessions))
	}
	if loaded.Sessions[0].Name != "api" || loaded.Sessions[0].Cwd != "/srv/api" ||
		loaded.Sessions[0].Title != "api server" || loaded.Sessions[0].FgCommand != "go" {
		t.Errorf("unexpected session[0]: %+v", loaded.Sessions[0])
	}
	if len(loaded.Sessions[1].ScrollbackTail) != 2 || loaded.Sessions[1].ScrollbackTail[1] != "line 2" {
		t.Errorf("unexpected scrollback_tail: %+v", loaded.Sessions[1].ScrollbackTail)
	}

	// Ensure no leftover temp files remain in directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "work.json" {
		t.Errorf("unexpected leftover files: %v", entries)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
			t.Fatalf("expected error for missing file")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("expected error for invalid JSON")
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "v99.json")
		if err := os.WriteFile(p, []byte(`{"version": 99, "name": "future", "sessions": []}`), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), "unsupported workspace snapshot version") {
			t.Fatalf("expected unsupported version error, got: %v", err)
		}
	})
}

func TestResolveRestoreDir(t *testing.T) {
	existing := t.TempDir()
	if got := ResolveRestoreDir(existing); got != existing {
		t.Errorf("ResolveRestoreDir(existing) = %q, want %q", got, existing)
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	missing := filepath.Join(existing, "no-such-dir")
	if got := ResolveRestoreDir(missing); got != fakeHome {
		t.Errorf("ResolveRestoreDir(missing) = %q, want %q", got, fakeHome)
	}

	// Regular file (not a directory) should also fall back to HOME.
	filePath := filepath.Join(existing, "a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := ResolveRestoreDir(filePath); got != fakeHome {
		t.Errorf("ResolveRestoreDir(file) = %q, want %q", got, fakeHome)
	}
}
