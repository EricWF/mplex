package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/EricWF/mplex/internal/protocol"
)

func TestAutoUpdatePrecedence(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.toml")

	clearUpdateEnvs := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"MPLEX_CONFIG",
			"MPLEX_AUTO_UPDATE",
			"MPLEX_NO_UPDATE_CHECK",
			"MPLEX_FORCE_UPDATE_CHECK",
			"MPLEX_SKIP_UPDATE_ONCE",
		} {
			t.Setenv(k, "")
		}
		t.Setenv("MPLEX_CONFIG", cfgPath)
	}

	t.Run("default true when config missing", func(t *testing.T) {
		clearUpdateEnvs(t)
		if !autoUpdateEnabled() {
			t.Fatalf("expected autoUpdateEnabled() = true by default")
		}
	})

	t.Run("config auto_update=false disables startup prompt", func(t *testing.T) {
		clearUpdateEnvs(t)
		if err := os.WriteFile(cfgPath, []byte("auto_update = false\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if autoUpdateEnabled() {
			t.Fatalf("expected autoUpdateEnabled() = false when config sets auto_update = false")
		}
	})

	t.Run("MPLEX_AUTO_UPDATE=true overrides config false", func(t *testing.T) {
		clearUpdateEnvs(t)
		if err := os.WriteFile(cfgPath, []byte("auto_update = false\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Setenv("MPLEX_AUTO_UPDATE", "true")
		if !autoUpdateEnabled() {
			t.Fatalf("expected MPLEX_AUTO_UPDATE=true to override config false")
		}
	})

	t.Run("MPLEX_NO_UPDATE_CHECK=0 overrides config false", func(t *testing.T) {
		clearUpdateEnvs(t)
		if err := os.WriteFile(cfgPath, []byte("auto_update = false\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Setenv("MPLEX_NO_UPDATE_CHECK", "0")
		if !autoUpdateEnabled() {
			t.Fatalf("expected MPLEX_NO_UPDATE_CHECK=0 to override config false")
		}
	})

	t.Run("MPLEX_FORCE_UPDATE_CHECK=1 overrides config false", func(t *testing.T) {
		clearUpdateEnvs(t)
		if err := os.WriteFile(cfgPath, []byte("auto_update = false\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Setenv("MPLEX_FORCE_UPDATE_CHECK", "1")
		if !autoUpdateEnabled() {
			t.Fatalf("expected MPLEX_FORCE_UPDATE_CHECK=1 to override config false")
		}
	})

	t.Run("MPLEX_NO_UPDATE_CHECK=1 disables even when config true", func(t *testing.T) {
		clearUpdateEnvs(t)
		if err := os.WriteFile(cfgPath, []byte("auto_update = true\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Setenv("MPLEX_NO_UPDATE_CHECK", "1")
		if autoUpdateEnabled() {
			t.Fatalf("expected MPLEX_NO_UPDATE_CHECK=1 to disable auto update")
		}
	})

	t.Run("MPLEX_SKIP_UPDATE_ONCE=1 disables", func(t *testing.T) {
		clearUpdateEnvs(t)
		if err := os.WriteFile(cfgPath, []byte("auto_update = true\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Setenv("MPLEX_SKIP_UPDATE_ONCE", "1")
		if autoUpdateEnabled() {
			t.Fatalf("expected MPLEX_SKIP_UPDATE_ONCE=1 to disable auto update")
		}
	})
}

func TestFormatSessionCompletionDesc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := formatSessionCompletionDesc(protocol.SessionInfo{
		Name:      "api",
		FgCommand: "go",
		Cwd:       filepath.Join(home, "proj", "api"),
	})
	want := "go (~/proj/api)"
	if got != want {
		t.Fatalf("formatSessionCompletionDesc = %q, want %q", got, want)
	}

	gotSanitized := formatSessionCompletionDesc(protocol.SessionInfo{
		Name:      "api",
		FgCommand: "vim\tfoo\n",
		Cwd:       "/tmp/work",
	})
	if gotSanitized != "vim foo (/tmp/work)" {
		t.Fatalf("sanitized desc = %q, want %q", gotSanitized, "vim foo (/tmp/work)")
	}

	if d := formatSessionCompletionDesc(protocol.SessionInfo{Attached: true}); d != "attached" {
		t.Fatalf("empty attached desc = %q, want attached", d)
	}
	if d := formatSessionCompletionDesc(protocol.SessionInfo{Attached: false}); d != "detached" {
		t.Fatalf("empty detached desc = %q, want detached", d)
	}
}
