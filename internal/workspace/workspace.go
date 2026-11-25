package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CurrentVersion is the workspace snapshot schema version.
const CurrentVersion = 1

// DefaultName is the default workspace name when none is specified.
const DefaultName = "default"

// Snapshot represents a persisted workspace of mplex sessions.
type Snapshot struct {
	Version  int            `json:"version"`
	SavedAt  time.Time      `json:"saved_at"`
	Name     string         `json:"name"`
	Sessions []SavedSession `json:"sessions"`
}

// SavedSession stores the restorable state of a single mplex session.
type SavedSession struct {
	Name           string   `json:"name"`
	Cwd            string   `json:"cwd"`
	Title          string   `json:"title,omitempty"`
	FgCommand      string   `json:"fg_command,omitempty"`
	ScrollbackTail []string `json:"scrollback_tail,omitempty"`
}

// DefaultDir returns the directory for saved workspace snapshots:
// $XDG_STATE_HOME/mplex/workspaces, falling back to ~/.local/state/mplex/workspaces.
func DefaultDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "mplex", "workspaces")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "mplex", "workspaces")
	}
	return filepath.Join(os.TempDir(), "mplex", "workspaces")
}

// DefaultPath returns the snapshot file path for the named workspace under DefaultDir.
// An empty name defaults to "default".
func DefaultPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultName
	}
	if err := validateName(name); err != nil {
		return "", err
	}
	return filepath.Join(DefaultDir(), name+".json"), nil
}

// ResolvePath resolves the snapshot path and effective workspace name from an optional
// workspace name and explicit --file path.
func ResolvePath(name, explicitFile string) (path string, wsName string, err error) {
	name = strings.TrimSpace(name)
	explicitFile = strings.TrimSpace(explicitFile)
	if explicitFile != "" {
		if name == "" {
			base := filepath.Base(explicitFile)
			name = strings.TrimSuffix(base, filepath.Ext(base))
			if name == "" || name == "." || name == string(filepath.Separator) {
				name = DefaultName
			}
		}
		return explicitFile, name, nil
	}
	if name == "" {
		name = DefaultName
	}
	p, err := DefaultPath(name)
	if err != nil {
		return "", "", err
	}
	return p, name, nil
}

func validateName(name string) error {
	if name == "." || name == ".." ||
		strings.Contains(name, "..") ||
		strings.ContainsAny(name, `/\:`+"\x00") {
		return fmt.Errorf("invalid workspace name %q", name)
	}
	return nil
}

// Save atomically writes snap as indented JSON to path via a temp file and rename.
func Save(path string, snap Snapshot) error {
	if snap.Version == 0 {
		snap.Version = CurrentVersion
	}
	if snap.SavedAt.IsZero() {
		snap.SavedAt = time.Now().UTC()
	}
	if snap.Name == "" {
		snap.Name = DefaultName
	}
	if snap.Sessions == nil {
		snap.Sessions = []SavedSession{}
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal workspace snapshot: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create workspace directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".workspace-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp workspace file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp workspace file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp workspace file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp workspace file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp workspace file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename workspace file: %w", err)
	}
	return nil
}

// Load reads and validates a workspace snapshot from path.
func Load(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read workspace snapshot %s: %w", path, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("parse workspace snapshot %s: %w", path, err)
	}
	if snap.Version != 0 && snap.Version != CurrentVersion {
		return Snapshot{}, fmt.Errorf("unsupported workspace snapshot version %d in %s", snap.Version, path)
	}
	if snap.Version == 0 {
		snap.Version = CurrentVersion
	}
	if snap.Sessions == nil {
		snap.Sessions = []SavedSession{}
	}
	return snap, nil
}

// ResolveRestoreDir returns savedCwd if it exists and is a directory;
// otherwise it falls back to the user's home directory (or current directory).
func ResolveRestoreDir(savedCwd string) string {
	if savedCwd != "" {
		if st, err := os.Stat(savedCwd); err == nil && st.IsDir() {
			return savedCwd
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	wd, _ := os.Getwd()
	return wd
}
