package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/completion"
	"github.com/EricWF/mplex/internal/config"
	"github.com/EricWF/mplex/internal/daemon"
	"github.com/EricWF/mplex/internal/protocol"
	"github.com/EricWF/mplex/internal/updater"
	"github.com/EricWF/mplex/internal/workspace"
)

type effectiveConfig struct {
	ConfigPath   string `json:"config_path"`
	ConfigExists bool   `json:"config_exists"`
	Key          string `json:"key"`
	Socket       string `json:"socket"`
	Scrollback   int    `json:"scrollback"`
	ReplayLines  int    `json:"replay_lines"`
	AutoUpdate   bool   `json:"auto_update"`
	WindowTitle  bool   `json:"window_title"`
	Naming       string `json:"naming"`
}

// loadEffectiveConfig resolves configuration with precedence:
// MPLEX_* env vars > config file > built-in defaults.
// CLI flags parsed by the caller override the returned values.
func loadEffectiveConfig() (effectiveConfig, error) {
	cfgPath := config.DefaultPath()
	_, statErr := os.Stat(cfgPath)
	exists := statErr == nil

	cfg, cfgErr := config.Load(cfgPath)

	socketPath := daemon.DefaultSocketPath()
	if os.Getenv("MPLEX_SOCKET") == "" && cfg.Socket != "" {
		socketPath = cfg.Socket
	}

	keySpec := config.DefaultKey
	if cfg.Key != "" {
		keySpec = cfg.Key
	}
	if v := os.Getenv("MPLEX_KEY"); v != "" {
		keySpec = v
	}

	scrollback := config.DefaultScrollback
	if cfg.Scrollback > 0 {
		scrollback = cfg.Scrollback
	}
	scrollback = envInt("MPLEX_SCROLLBACK", scrollback)

	replayLines := config.DefaultReplayLines
	if cfg.ReplayLines > 0 {
		replayLines = cfg.ReplayLines
	}
	replayLines = envInt("MPLEX_REPLAY_LINES", replayLines)

	autoUpdate := config.DefaultAutoUpdate
	if cfg.AutoUpdate != nil {
		autoUpdate = *cfg.AutoUpdate
	}
	if v, ok := envBool("MPLEX_AUTO_UPDATE"); ok {
		autoUpdate = v
	}
	if v, ok := envBool("MPLEX_NO_UPDATE_CHECK"); ok {
		autoUpdate = !v
	}
	if updater.IsForcedByEnv() {
		autoUpdate = true
	}
	if updater.IsDisabledByEnv() {
		autoUpdate = false
	}

	windowTitle := config.DefaultWindowTitle
	if cfg.WindowTitle != nil {
		windowTitle = *cfg.WindowTitle
	}
	if v, ok := envBool("MPLEX_WINDOW_TITLE"); ok {
		windowTitle = v
	}

	naming := config.DefaultNaming
	if cfg.Naming != "" {
		naming = cfg.Naming
	}
	if v := os.Getenv("MPLEX_NAMING"); v != "" {
		naming = v
	}

	return effectiveConfig{
		ConfigPath:   cfgPath,
		ConfigExists: exists,
		Key:          keySpec,
		Socket:       socketPath,
		Scrollback:   scrollback,
		ReplayLines:  replayLines,
		AutoUpdate:   autoUpdate,
		WindowTitle:  windowTitle,
		Naming:       naming,
	}, cfgErr
}

func autoUpdateEnabled() bool {
	eff, _ := loadEffectiveConfig()
	return eff.AutoUpdate
}

func envBool(name string) (bool, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return false, false
	}
	switch s {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func runConfigCmd(eff effectiveConfig, cfgErr error, args []string) error {
	fs := flag.NewFlagSet("mplex config", flag.ContinueOnError)
	fs.StringVar(&eff.Socket, "socket", eff.Socket, "daemon Unix socket path")
	showPath := fs.Bool("path", false, "print the resolved config file path")
	initCfg := fs.Bool("init", false, "write a starter config.toml if it does not exist")
	force := fs.Bool("force", false, "overwrite existing config file when used with --init")
	jsonOut := fs.Bool("json", false, "output effective configuration as JSON")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	if *showPath {
		if *jsonOut {
			return writeJSON(map[string]any{"path": eff.ConfigPath})
		}
		fmt.Println(eff.ConfigPath)
		return nil
	}

	if *initCfg {
		if err := config.WriteStarter(eff.ConfigPath, *force); err != nil {
			return err
		}
		if *jsonOut {
			return writeJSON(map[string]any{
				"path":        eff.ConfigPath,
				"initialized": true,
			})
		}
		fmt.Printf("Wrote starter config to %s\n", eff.ConfigPath)
		return nil
	}

	if cfgErr != nil {
		return cfgErr
	}

	if *jsonOut {
		return writeJSON(eff)
	}

	status := "not found; using defaults"
	if eff.ConfigExists {
		status = "loaded"
	}
	fmt.Printf("# Config file: %s (%s)\n", eff.ConfigPath, status)
	formatted := config.Format(config.Config{
		Key:         eff.Key,
		Socket:      eff.Socket,
		Scrollback:  eff.Scrollback,
		ReplayLines: eff.ReplayLines,
		AutoUpdate:  config.BoolPtr(eff.AutoUpdate),
		WindowTitle: config.BoolPtr(eff.WindowTitle),
		Naming:      eff.Naming,
		Set: map[string]bool{
			"key":          true,
			"socket":       true,
			"scrollback":   true,
			"replay_lines": true,
			"auto_update":  true,
			"window_title": true,
			"naming":       true,
		},
	})
	fmt.Print(formatted)
	return nil
}

func runCompletionCmd(args []string) error {
	fs := flag.NewFlagSet("mplex completion", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: mplex completion <%s>", strings.Join(completion.SupportedShells, "|"))
	}
	script, err := completion.Generate(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Print(script)
	return nil
}

// runCompleteSessionsCmd is a fast hidden subcommand invoked by shell completion
// scripts. It queries the daemon with a short timeout and exits 0 silently with
// empty output if the daemon is not running or unresponsive.
func runCompleteSessionsCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex __complete-sessions", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	describe := fs.Bool("describe", false, "include tab-separated description (cmd + cwd)")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return nil
	}

	if _, err := os.Stat(socketPath); err != nil {
		return nil
	}

	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err != nil {
		return nil
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if err := protocol.WriteJSON(conn, protocol.MsgListReq, protocol.ListReq{}); err != nil {
		return nil
	}
	frame, err := protocol.ReadFrame(conn)
	if err != nil || frame.Type != protocol.MsgListResp {
		return nil
	}
	var resp protocol.ListResp
	if err := protocol.DecodeJSON(frame.Payload, &resp); err != nil || resp.Error != "" {
		return nil
	}

	for _, s := range resp.Sessions {
		name := sanitizeCompletionText(s.Name)
		if name == "" {
			continue
		}
		if *describe {
			fmt.Printf("%s\t%s\n", name, formatSessionCompletionDesc(s))
		} else {
			fmt.Println(name)
		}
	}
	return nil
}

func formatSessionCompletionDesc(s protocol.SessionInfo) string {
	cmd := sanitizeCompletionText(s.FgCommand)
	cwd := sanitizeCompletionText(shortenHomePath(s.Cwd))
	switch {
	case cmd != "" && cwd != "":
		return fmt.Sprintf("%s (%s)", cmd, cwd)
	case cmd != "":
		return cmd
	case cwd != "":
		return cwd
	case s.Attached:
		return "attached"
	default:
		return "detached"
	}
}

func sanitizeCompletionText(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func shortenHomePath(p string) string {
	if p == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	home = filepath.Clean(home)
	clean := filepath.Clean(p)
	if clean == home {
		return "~"
	}
	prefix := home + string(filepath.Separator)
	if strings.HasPrefix(clean, prefix) {
		return "~" + string(filepath.Separator) + strings.TrimPrefix(clean, prefix)
	}
	return clean
}

func runSaveCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex save", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	filePath := fs.String("file", "", "explicit snapshot file path")
	withScrollback := fs.Bool("with-scrollback", false, "include scrollback tail lines for each session")
	tailLines := fs.Int("lines", 100, "max scrollback tail lines to save per session when --with-scrollback is set")
	jsonOut := fs.Bool("json", false, "output saved workspace snapshot as JSON")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"file":   true,
		"lines":  true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: mplex save [workspace-name] [--file path] [--with-scrollback] [--json]")
	}

	wsArg := ""
	if fs.NArg() == 1 {
		wsArg = fs.Arg(0)
	}
	path, wsName, err := workspace.ResolvePath(wsArg, *filePath)
	if err != nil {
		return err
	}

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	sessions, err := rc.List()
	if err != nil {
		return err
	}

	// Preserve session creation order for deterministic snapshots.
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
		}
		return sessions[i].Name < sessions[j].Name
	})

	saved := make([]workspace.SavedSession, 0, len(sessions))
	for _, s := range sessions {
		entry := workspace.SavedSession{
			Name:      s.Name,
			Cwd:       s.Cwd,
			Title:     s.Title,
			FgCommand: s.FgCommand,
		}
		if *withScrollback {
			maxLines := *tailLines
			if maxLines <= 0 {
				maxLines = 100
			}
			if capResp, capErr := rc.Capture(protocol.CaptureReq{
				Target:            s.ID,
				Lines:             maxLines,
				IncludeScrollback: true,
			}); capErr == nil && len(capResp.Lines) > 0 {
				entry.ScrollbackTail = capResp.Lines
			}
		}
		saved = append(saved, entry)
	}

	snap := workspace.Snapshot{
		Version:  workspace.CurrentVersion,
		SavedAt:  time.Now().UTC(),
		Name:     wsName,
		Sessions: saved,
	}
	if err := workspace.Save(path, snap); err != nil {
		return err
	}

	if *jsonOut {
		return writeJSON(struct {
			Path string `json:"path"`
			workspace.Snapshot
		}{
			Path:     path,
			Snapshot: snap,
		})
	}

	fmt.Printf("Saved %d session(s) to %s\n", len(snap.Sessions), path)
	return nil
}

func runRestoreCmd(socketPath string, scrollback int, args []string) error {
	fs := flag.NewFlagSet("mplex restore", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	filePath := fs.String("file", "", "explicit snapshot file path")
	force := fs.Bool("force", false, "create deduplicated sessions even if a session with the same name is running")
	jsonOut := fs.Bool("json", false, "output restore summary as JSON")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"file":   true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: mplex restore [workspace-name] [--file path] [--force] [--json]")
	}

	wsArg := ""
	if fs.NArg() == 1 {
		wsArg = fs.Arg(0)
	}
	path, _, err := workspace.ResolvePath(wsArg, *filePath)
	if err != nil {
		return err
	}

	snap, err := workspace.Load(path)
	if err != nil {
		return err
	}

	rc, err := client.DialOrStart(socketPath, nil)
	if err != nil {
		return err
	}
	existing, err := rc.List()
	if err != nil {
		_ = rc.Close()
		// Retry once in case an empty daemon exited right as we dialed.
		rc, err = client.DialOrStart(socketPath, nil)
		if err != nil {
			return err
		}
		existing, err = rc.List()
		if err != nil {
			_ = rc.Close()
			return err
		}
	}
	defer rc.Close()

	running := make(map[string]bool, len(existing))
	for _, s := range existing {
		running[s.Name] = true
	}

	created := make([]string, 0)
	skipped := make([]string, 0)
	createdSessions := make([]protocol.SessionInfo, 0)

	for _, saved := range snap.Sessions {
		if !*force && running[saved.Name] {
			skipped = append(skipped, saved.Name)
			continue
		}
		dir := workspace.ResolveRestoreDir(saved.Cwd)
		sess, err := rc.Create(protocol.CreateReq{
			Name:       saved.Name,
			Cwd:        dir,
			Shell:      os.Getenv("SHELL"),
			Env:        os.Environ(),
			Size:       protocol.WinSize{Rows: 24, Cols: 80},
			Scrollback: scrollback,
		})
		if err != nil {
			return fmt.Errorf("restore session %q: %w", saved.Name, err)
		}
		running[sess.Name] = true
		created = append(created, sess.Name)
		createdSessions = append(createdSessions, sess)
	}

	if *jsonOut {
		return writeJSON(map[string]any{
			"path":     path,
			"name":     snap.Name,
			"created":  created,
			"skipped":  skipped,
			"sessions": createdSessions,
		})
	}

	if len(created) == 0 && len(skipped) == 0 {
		fmt.Printf("Workspace %q (%s) has no sessions to restore.\n", snap.Name, path)
		return nil
	}
	if len(created) > 0 {
		fmt.Printf("Restored %d session(s): %s\n", len(created), strings.Join(created, ", "))
	}
	if len(skipped) > 0 {
		fmt.Printf("Skipped %d existing session(s): %s\n", len(skipped), strings.Join(skipped, ", "))
	}
	return nil
}
