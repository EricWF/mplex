package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/daemon"
	"github.com/EricWF/mplex/internal/mcp"
	"github.com/EricWF/mplex/internal/protocol"
	"github.com/EricWF/mplex/internal/updater"
)

var version = "dev"

func currentVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mplex: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	eff, cfgErr := loadEffectiveConfig()
	socketPath := eff.Socket
	keySpec := eff.Key
	scrollback := eff.Scrollback
	replayLines := eff.ReplayLines
	windowTitle := eff.WindowTitle
	namingMode := eff.Naming

	// Parse leading global flags before subcommand.
	fs := flag.NewFlagSet("mplex", flag.ContinueOnError)
	fs.Usage = printUsage
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.StringVar(&keySpec, "key", keySpec, "switcher trigger key (e.g. ctrl-\\, ctrl-space, alt-s, none)")
	fs.IntVar(&scrollback, "scrollback", scrollback, "max scrollback lines kept per session")
	fs.IntVar(&replayLines, "replay-lines", replayLines, "max scrollback lines replayed on switch")
	fs.BoolVar(&windowTitle, "window-title", windowTitle, "tag outer terminal window title with [session] name")
	fs.StringVar(&namingMode, "naming", namingMode, "default session naming mode (cwd or numeric)")
	openPicker := fs.Bool("pick", false, "open session switcher immediately")
	nested := fs.Bool("nested", false, "allow nesting mplex inside an existing mplex session")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	eff.Socket = socketPath
	eff.Key = keySpec
	eff.Scrollback = scrollback
	eff.ReplayLines = replayLines
	eff.WindowTitle = windowTitle
	eff.Naming = namingMode

	if *showVersion {
		fmt.Printf("mplex %s\n", currentVersion())
		return nil
	}

	rest := fs.Args()
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
		rest = rest[1:]
	}

	if cfgErr != nil {
		switch sub {
		case "__complete-sessions", "config", "completion", "version", "help", "-h", "--help":
		default:
			return cfgErr
		}
	}

	switch sub {
	case "__complete-sessions", "config", "completion", "version", "help", "-h", "--help":
	default:
		var err error
		namingMode, err = normalizeNamingMode(namingMode)
		if err != nil {
			return err
		}
	}

	switch sub {
	case "version":
		fmt.Printf("mplex %s\n", currentVersion())
		return nil

	case "help", "-h", "--help":
		printUsage()
		return nil

	case "config":
		return runConfigCmd(eff, cfgErr, rest)

	case "completion":
		return runCompletionCmd(rest)

	case "__complete-sessions":
		return runCompleteSessionsCmd(socketPath, rest)

	case "save":
		return runSaveCmd(socketPath, rest)

	case "restore":
		return runRestoreCmd(socketPath, scrollback, rest)

	case "update", "upgrade":
		return runUpdateCmd(socketPath, rest)

	case "daemon":
		return runDaemonCmd(socketPath, rest)

	case "ls", "list":
		return runListCmd(socketPath, rest)

	case "new", "n":
		return runNewCmd(socketPath, keySpec, namingMode, scrollback, replayLines, windowTitle, rest)

	case "attach", "a":
		return runAttachCmd(socketPath, keySpec, namingMode, scrollback, replayLines, windowTitle, *nested, rest)

	case "switch", "pick", "s":
		return runSwitchCmd(socketPath, keySpec, namingMode, scrollback, replayLines, windowTitle, rest)

	case "capture", "read", "logs":
		return runCaptureCmd(socketPath, rest)

	case "follow":
		return runFollowCmd(socketPath, rest)

	case "wait":
		return runWaitCmd(socketPath, rest)

	case "mcp":
		return runMCPCmd(socketPath, rest)

	case "send", "write":
		return runSendCmd(socketPath, rest)

	case "kill", "rm":
		return runKillCmd(socketPath, rest)

	case "rename":
		return runRenameCmd(socketPath, rest)

	case "detach", "d":
		return runDetachCmd(socketPath, rest)

	case "":
		// If already running inside an mplex session on the same socket, trigger
		// the switcher on the outer client instead of accidentally nesting!
		if !*nested && os.Getenv("MPLEX_SESSION") != "" && os.Getenv("MPLEX_SOCKET") == socketPath {
			return runSwitchCmd(socketPath, keySpec, namingMode, scrollback, replayLines, windowTitle, nil)
		}
		maybePromptUpdateAndReexec(socketPath)
		trig, err := client.ParseTriggerKey(keySpec)
		if err != nil {
			return err
		}
		return client.RunInteractive(client.Options{
			SocketPath:  socketPath,
			Trigger:     trig,
			OpenPicker:  *openPicker,
			Scrollback:  scrollback,
			ReplayLines: replayLines,
			WindowTitle: windowTitle,
			NamingMode:  namingMode,
		})

	default:
		// Shorthand: `mplex <name>` attaches to (or creates) session <name>.
		// Inside an existing mplex session, switch the outer window directly instead of nesting.
		if !*nested && os.Getenv("MPLEX_SESSION") != "" && os.Getenv("MPLEX_SOCKET") == socketPath {
			return switchInsideMplex(socketPath, sub, true, scrollback, namingMode)
		}
		maybePromptUpdateAndReexec(socketPath)
		trig, err := client.ParseTriggerKey(keySpec)
		if err != nil {
			return err
		}
		return client.RunInteractive(client.Options{
			SocketPath:      socketPath,
			Trigger:         trig,
			InitialTarget:   sub,
			CreateIfMissing: true,
			Scrollback:      scrollback,
			ReplayLines:     replayLines,
			WindowTitle:     windowTitle,
			NamingMode:      namingMode,
		})
	}
}

func runDaemonCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex daemon", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := daemon.New(socketPath)
	d.AutoExitOnEmpty = true
	// Catch SIGHUP instead of inheriting SIG_IGN so spawned PTY shells reset
	// SIGHUP to SIG_DFL across execve and terminate cleanly on `mplex kill`.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = d.Close()
	}()
	return d.Start()
}

func runListCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex list", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	jsonOut := fs.Bool("json", false, "output sessions as JSON")
	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		if *jsonOut {
			return writeJSON([]protocol.SessionInfo{})
		}
		fmt.Println("No running mplex sessions.")
		return nil
	}
	defer rc.Close()

	sessions, err := rc.List()
	if err != nil {
		return err
	}
	if *jsonOut {
		if sessions == nil {
			sessions = []protocol.SessionInfo{}
		}
		return writeJSON(sessions)
	}
	if len(sessions) == 0 {
		fmt.Println("No active mplex sessions.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tNAME\tCMD\tCWD\tSCROLLBACK\tACTIVE")
	for _, s := range sessions {
		st := "detached"
		if s.Attached {
			st = "attached"
		}
		cmd := s.FgCommand
		if cmd == "" {
			cmd = "-"
		}
		cwd := s.Cwd
		if cwd == "" {
			cwd = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			st, s.Name, cmd, cwd, s.Scrollback, formatSince(s.LastActiveAt))
	}
	return tw.Flush()
}

func runNewCmd(socketPath, keySpec, namingMode string, scrollback, replayLines int, windowTitle bool, args []string) error {
	fs := flag.NewFlagSet("mplex new", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.StringVar(&keySpec, "key", keySpec, "switcher trigger key")
	fs.StringVar(&namingMode, "naming", namingMode, "default session naming mode (cwd or numeric)")
	fs.BoolVar(&windowTitle, "window-title", windowTitle, "tag outer terminal window title with [session] name")
	detached := fs.Bool("d", false, "create session detached without attaching")
	cwd := fs.String("c", "", "starting directory for the new session")
	jsonOut := fs.Bool("json", false, "create detached and output session metadata as JSON")

	// Allow flags both before and after the session name (up to "--"), e.g.:
	//   mplex new build -d -c ~/proj -- make watch
	parseArgs, afterDash := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"key":    true,
		"naming": true,
		"c":      true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	var err error
	namingMode, err = normalizeNamingMode(namingMode)
	if err != nil {
		return err
	}

	var name string
	var cmdArgs []string
	rem := fs.Args()
	if len(rem) > 0 {
		name = rem[0]
		if len(rem) > 1 && len(afterDash) == 0 {
			cmdArgs = rem[1:]
		}
	}
	if len(afterDash) > 0 {
		cmdArgs = afterDash
	}

	startDir := *cwd
	if startDir == "" {
		startDir, _ = os.Getwd()
	}

	if *detached || *jsonOut {
		rc, err := client.DialOrStart(socketPath, nil)
		if err != nil {
			return err
		}
		defer rc.Close()

		sess, err := rc.Create(protocol.CreateReq{
			Name:       name,
			Cwd:        startDir,
			Shell:      os.Getenv("SHELL"),
			Cmd:        cmdArgs,
			Env:        os.Environ(),
			Size:       protocol.WinSize{Rows: 24, Cols: 80},
			Scrollback: scrollback,
			NamingMode: namingMode,
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeJSON(sess)
		}
		fmt.Printf("Created session %q (id=%s)\n", sess.Name, sess.ID)
		return nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("mplex new without -d requires a TTY (pass -d to create a detached session)")
	}
	maybePromptUpdateAndReexec(socketPath)

	trig, err := client.ParseTriggerKey(keySpec)
	if err != nil {
		return err
	}
	return client.RunInteractive(client.Options{
		SocketPath:  socketPath,
		Trigger:     trig,
		CreateName:  name,
		CreateCwd:   startDir,
		CreateCmd:   cmdArgs,
		Scrollback:  scrollback,
		ReplayLines: replayLines,
		WindowTitle: windowTitle,
		NamingMode:  namingMode,
	})
}

func runAttachCmd(socketPath, keySpec, namingMode string, scrollback, replayLines int, windowTitle, nested bool, args []string) error {
	fs := flag.NewFlagSet("mplex attach", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.StringVar(&keySpec, "key", keySpec, "switcher trigger key")
	fs.BoolVar(&windowTitle, "window-title", windowTitle, "tag outer terminal window title with [session] name")
	create := fs.Bool("create", false, "create the session if it does not exist")
	pick := fs.Bool("pick", false, "open session switcher")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"key":    true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}

	// Inside an existing mplex session on the same socket, switch the outer client
	// directly instead of nesting mplex inside mplex.
	if !nested && os.Getenv("MPLEX_SESSION") != "" && os.Getenv("MPLEX_SOCKET") == socketPath {
		if *pick || target == "" {
			return runSwitchCmd(socketPath, keySpec, namingMode, scrollback, replayLines, windowTitle, nil)
		}
		return switchInsideMplex(socketPath, target, *create, scrollback, namingMode)
	}

	maybePromptUpdateAndReexec(socketPath)

	trig, err := client.ParseTriggerKey(keySpec)
	if err != nil {
		return err
	}

	return client.RunInteractive(client.Options{
		SocketPath:      socketPath,
		Trigger:         trig,
		InitialTarget:   target,
		CreateIfMissing: *create,
		OpenPicker:      *pick,
		Scrollback:      scrollback,
		ReplayLines:     replayLines,
		WindowTitle:     windowTitle,
		NamingMode:      namingMode,
	})
}

func switchInsideMplex(socketPath, target string, createIfMissing bool, scrollback int, namingMode string) error {
	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	_, err = rc.SwitchSession("", os.Getenv("MPLEX_SESSION_ID"), target)
	if err == nil {
		return nil
	}
	if createIfMissing && strings.Contains(err.Error(), "not found") {
		wd, _ := os.Getwd()
		sess, createErr := rc.Create(protocol.CreateReq{
			Name:       target,
			Cwd:        wd,
			Shell:      os.Getenv("SHELL"),
			Env:        os.Environ(),
			Size:       protocol.WinSize{Rows: 24, Cols: 80},
			Scrollback: scrollback,
			NamingMode: namingMode,
		})
		if createErr != nil {
			return createErr
		}
		_, err = rc.SwitchSession("", os.Getenv("MPLEX_SESSION_ID"), sess.ID)
		return err
	}
	return err
}

func runSwitchCmd(socketPath, keySpec, namingMode string, scrollback, replayLines int, windowTitle bool, args []string) error {
	fs := flag.NewFlagSet("mplex switch", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.BoolVar(&windowTitle, "window-title", windowTitle, "tag outer terminal window title with [session] name")
	clientID := fs.String("client", "", "specific client ID to target")
	jsonOut := fs.Bool("json", false, "output result as JSON")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"client": true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}

	// First try out-of-band switch / switcher trigger on an already attached interactive client!
	if rc, err := client.Dial(socketPath, nil); err == nil {
		if target != "" {
			resp, swErr := rc.SwitchSession(*clientID, os.Getenv("MPLEX_SESSION_ID"), target)
			_ = rc.Close()
			if swErr == nil {
				if *jsonOut {
					return writeJSON(map[string]any{
						"switched": true,
						"client":   resp.Client,
						"session":  resp.Session,
					})
				}
				if resp.Session != nil {
					fmt.Printf("Switched to session %q\n", resp.Session.Name)
				}
				return nil
			}
			// If the requested session doesn't exist, surface that error directly instead of falling back.
			if !strings.Contains(swErr.Error(), "no active interactive client") {
				return swErr
			}
			if *jsonOut || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return swErr
			}
		} else {
			trErr := rc.TriggerSwitcher(*clientID, os.Getenv("MPLEX_SESSION_ID"))
			_ = rc.Close()
			if trErr == nil {
				if *jsonOut {
					return writeJSON(map[string]any{"triggered": true})
				}
				return nil
			}
			if *jsonOut || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return trErr
			}
		}
	} else if *jsonOut || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("daemon not running: %w", err)
	}

	// If no interactive client is attached yet (and we have a TTY), start one!
	maybePromptUpdateAndReexec(socketPath)
	trig, err := client.ParseTriggerKey(keySpec)
	if err != nil {
		return err
	}
	if target != "" {
		return client.RunInteractive(client.Options{
			SocketPath:    socketPath,
			Trigger:       trig,
			InitialTarget: target,
			Scrollback:    scrollback,
			ReplayLines:   replayLines,
			WindowTitle:   windowTitle,
			NamingMode:    namingMode,
		})
	}
	return client.RunInteractive(client.Options{
		SocketPath:  socketPath,
		Trigger:     trig,
		OpenPicker:  true,
		Scrollback:  scrollback,
		ReplayLines: replayLines,
		WindowTitle: windowTitle,
		NamingMode:  namingMode,
	})
}

func runCaptureCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex capture", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	lines := fs.Int("lines", 0, "limit output to the last N lines (0 = all)")
	scrollback := fs.Bool("scrollback", false, "include scrollback history before the visible screen")
	history := fs.Bool("history", false, "include scrollback/screen history before streaming live output (with -f)")
	ansi := fs.Bool("ansi", false, "preserve ANSI color/style escape sequences")
	rawWrap := fs.Bool("raw-wrap", false, "keep raw terminal row breaks instead of joining soft-wrapped lines")
	jsonOut := fs.Bool("json", false, "output captured screen and session metadata as JSON")
	followShort := fs.Bool("f", false, "stream live session output continuously (read-only)")
	followLong := fs.Bool("follow", false, "stream live session output continuously (read-only)")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"lines":  true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	} else if envID := os.Getenv("MPLEX_SESSION_ID"); envID != "" {
		target = envID
	}
	if target == "" {
		return fmt.Errorf("usage: mplex capture <session> [--lines N] [--scrollback] [--ansi] [--follow|-f] [--json]")
	}

	if *followShort || *followLong {
		if *jsonOut {
			return fmt.Errorf("--json cannot be combined with --follow")
		}
		return followSession(socketPath, target, *scrollback || *history || *lines > 0, *lines)
	}

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	resp, err := rc.Capture(protocol.CaptureReq{
		Target:            target,
		Lines:             *lines,
		IncludeScrollback: *scrollback || *history,
		ANSI:              *ansi,
		RawWrap:           *rawWrap,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		if resp.Lines == nil {
			resp.Lines = []string{}
		}
		return writeJSON(resp)
	}
	if resp.Text != "" {
		fmt.Println(resp.Text)
	}
	return nil
}

func runFollowCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex follow", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	history := fs.Bool("history", false, "print existing screen/scrollback history before streaming live bytes")
	lines := fs.Int("lines", 0, "limit initial history snapshot to the last N lines (implies --history when > 0)")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"lines":  true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	} else if envID := os.Getenv("MPLEX_SESSION_ID"); envID != "" {
		target = envID
	}
	if target == "" {
		return fmt.Errorf("usage: mplex follow <session> [--history] [--lines N]")
	}

	return followSession(socketPath, target, *history || *lines > 0, *lines)
}

func followSession(socketPath, target string, includeHistory bool, lines int) error {
	rc, err := client.Dial(socketPath, os.Stdout)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	if _, err := rc.Follow(protocol.FollowReq{
		Target:         target,
		IncludeHistory: includeHistory,
		Lines:          lines,
	}); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		return nil
	case <-rc.SessionExitedCh:
		return nil
	case <-rc.Done():
		return nil
	}
}

func runWaitCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex wait", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	match := fs.String("match", "", "wait until captured text matches this regular expression")
	idle := fs.Duration("idle", 0, "wait until session has no new activity for this duration (e.g. 2s)")
	exit := fs.Bool("exit", false, "wait until session exits")
	scrollback := fs.Bool("scrollback", false, "include scrollback history when checking --match")
	timeout := fs.Duration("timeout", client.DefaultWaitTimeout, "maximum duration to wait")
	jsonOut := fs.Bool("json", false, "output structured wait result as JSON")

	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket":  true,
		"match":   true,
		"idle":    true,
		"timeout": true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	} else if envID := os.Getenv("MPLEX_SESSION_ID"); envID != "" {
		target = envID
	}
	if target == "" {
		return fmt.Errorf("usage: mplex wait <session> [--match <regex>] [--idle <duration>] [--exit] [--scrollback] [--timeout <duration>] [--json]")
	}
	if *idle < 0 {
		return fmt.Errorf("--idle must be a positive duration")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be a positive duration")
	}
	if *match == "" && *idle == 0 && !*exit {
		return fmt.Errorf("usage: mplex wait <session> requires at least one condition (--match, --idle, or --exit)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		if *exit {
			res := client.WaitResult{
				Matched: true,
				Reason:  "exit",
			}
			if *jsonOut {
				return writeJSON(res)
			}
			fmt.Printf("Session %q exited\n", target)
			return nil
		}
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	res, err := client.Wait(ctx, rc, client.WaitOptions{
		Target:            target,
		Match:             *match,
		Idle:              *idle,
		Exit:              *exit,
		IncludeScrollback: *scrollback,
		Timeout:           *timeout,
	})
	if err != nil {
		if *jsonOut && errors.Is(err, client.ErrWaitTimeout) {
			_ = writeJSON(res)
		}
		return err
	}

	if *jsonOut {
		return writeJSON(res)
	}
	switch res.Reason {
	case "match":
		if res.MatchLine != "" {
			fmt.Println(res.MatchLine)
		}
	case "idle":
		name := target
		if res.Session != nil && res.Session.Name != "" {
			name = res.Session.Name
		}
		fmt.Printf("Session %q is idle\n", name)
	case "exit":
		name := target
		if res.Session != nil && res.Session.Name != "" {
			name = res.Session.Name
		}
		fmt.Printf("Session %q exited\n", name)
	}
	return nil
}

func runMCPCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := mcp.NewServer(mcp.Options{
		SocketPath: socketPath,
		Version:    currentVersion(),
		ErrOut:     os.Stderr,
		Scrollback: envInt("MPLEX_SCROLLBACK", 10000),
	})
	return srv.ServeContext(ctx, os.Stdin, os.Stdout)
}

type multiStringFlag []string

func (m *multiStringFlag) String() string { return strings.Join(*m, ",") }
func (m *multiStringFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func runSendCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex send", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	enter := fs.Bool("enter", false, "append Enter (carriage return) after the input")
	readStdin := fs.Bool("stdin", false, "read raw input bytes from stdin")
	jsonOut := fs.Bool("json", false, "output send result as JSON")
	var keys multiStringFlag
	fs.Var(&keys, "key", "special key to send (repeatable: enter, ctrl-c, ctrl-d, esc, tab, up, down, ...)")

	parseArgs, afterDash := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"key":    true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	rem := fs.Args()
	if len(rem) == 0 {
		return fmt.Errorf("usage: mplex send <session> [text...] [--enter] [--key <key>] [--stdin] [--json]")
	}
	target := rem[0]
	textParts := append([]string{}, rem[1:]...)
	if len(afterDash) > 0 {
		textParts = append(textParts, afterDash...)
	}

	var payload bytes.Buffer
	if *readStdin {
		stdinData, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		payload.Write(stdinData)
	}
	if len(textParts) > 0 {
		payload.WriteString(strings.Join(textParts, " "))
	}
	for _, ks := range keys {
		b, err := client.ParseSendKey(ks)
		if err != nil {
			return err
		}
		payload.Write(b)
	}
	if *enter {
		payload.WriteByte('\r')
	}
	if payload.Len() == 0 {
		return fmt.Errorf("nothing to send (provide text, --enter, --key, or --stdin)")
	}

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	resp, err := rc.SendToSession(target, payload.Bytes())
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(resp)
	}
	fmt.Printf("Sent %d bytes to session %q\n", resp.Bytes, resp.Session.Name)
	return nil
}

func runUpdateCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex update", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	checkOnly := fs.Bool("check", false, "check if a newer release is available without installing")
	yes := fs.Bool("y", false, "upgrade without prompting for confirmation")
	yesLong := fs.Bool("yes", false, "upgrade without prompting for confirmation")
	force := fs.Bool("force", false, "install latest release even if already up to date")
	jsonOut := fs.Bool("json", false, "output update check result as JSON (with --check)")
	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}

	cur := currentVersion()
	cfg := updater.Config{
		CurrentVersion: cur,
		CheckTimeout:   10 * time.Second,
	}

	res, _, err := updater.Check(context.Background(), cfg, true)
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}

	isRelease := updater.IsReleaseVersion(cur)
	hasUpdate := !isRelease || updater.IsNewer(cur, res.LatestVersion)

	if *checkOnly {
		if *jsonOut {
			return writeJSON(map[string]any{
				"current_version":  cur,
				"latest_version":   res.LatestVersion,
				"update_available": hasUpdate,
				"release_url":      res.Release.ReleaseURL,
			})
		}
		if hasUpdate {
			fmt.Printf("Update available: %s -> %s\n", cur, res.LatestVersion)
			if res.Release.ReleaseURL != "" {
				fmt.Printf("Release: %s\n", res.Release.ReleaseURL)
			}
		} else {
			fmt.Printf("mplex is up to date (%s).\n", cur)
		}
		return nil
	}

	if !hasUpdate && !*force {
		if *jsonOut {
			return writeJSON(map[string]any{
				"current_version":  cur,
				"latest_version":   res.LatestVersion,
				"update_available": false,
				"upgraded":         false,
			})
		}
		fmt.Printf("mplex is already up to date (%s).\n", cur)
		return nil
	}

	if !*yes && !*yesLong {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("mplex %s is available (current: %s); pass --yes to upgrade non-interactively", res.LatestVersion, cur)
		}
		fmt.Printf("mplex %s is available (current: %s).\n", res.LatestVersion, cur)
		if res.Release.ReleaseURL != "" {
			fmt.Printf("Release notes: %s\n", res.Release.ReleaseURL)
		}
		fmt.Print("Upgrade now? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		ans := strings.TrimSpace(strings.ToLower(line))
		if ans != "y" && ans != "yes" {
			fmt.Println("Upgrade canceled.")
			return nil
		}
	}

	if !*jsonOut {
		fmt.Printf("Downloading mplex %s...\n", res.LatestVersion)
	}
	target, err := updater.Upgrade(context.Background(), cfg, res.Release)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(map[string]any{
			"current_version": cur,
			"latest_version":  res.LatestVersion,
			"upgraded":        true,
			"target_path":     target,
		})
	}
	fmt.Printf("Upgraded mplex to %s (%s)\n", res.LatestVersion, target)
	if hint := updater.MacOSQuarantineHint(runtime.GOOS, target); hint != "" {
		fmt.Println(hint)
	}
	notifyIfDaemonRunning(socketPath, os.Stdout)
	return nil
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func maybePromptUpdateAndReexec(socketPath string) {
	if !term.IsTerminal(int(os.Stdin.Fd())) ||
		!term.IsTerminal(int(os.Stdout.Fd())) ||
		!term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	if !autoUpdateEnabled() {
		return
	}
	cfg := updater.Config{
		CurrentVersion: currentVersion(),
	}
	pr, err := updater.PromptIfAvailable(context.Background(), cfg, os.Stdin, os.Stderr)
	if err != nil || !pr.Upgraded || pr.TargetPath == "" {
		return
	}
	notifyIfDaemonRunning(socketPath, os.Stderr)
	env := append(os.Environ(), "MPLEX_SKIP_UPDATE_ONCE=1")
	if execErr := syscall.Exec(pr.TargetPath, os.Args, env); execErr != nil {
		fmt.Fprintf(os.Stderr, "mplex: could not relaunch upgraded binary (%v); continuing with current version.\n", execErr)
		if hint := updater.MacOSQuarantineHint(runtime.GOOS, pr.TargetPath); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
	}
}

func notifyIfDaemonRunning(socketPath string, out *os.File) {
	if rc, err := client.Dial(socketPath, nil); err == nil {
		_ = rc.Close()
		fmt.Fprintln(out, "Note: existing sessions continue running on the previous daemon until restarted.")
	}
}

func reorderFlagsBeforeDash(args []string, valueFlags map[string]bool) (reordered []string, afterDash []string) {
	before := args
	for i, a := range args {
		if a == "--" {
			before = args[:i]
			afterDash = args[i+1:]
			break
		}
	}
	var flags, pos []string
	for i := 0; i < len(before); i++ {
		a := before[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				continue
			}
			if valueFlags[name] && i+1 < len(before) {
				i++
				flags = append(flags, before[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...), afterDash
}

func runKillCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex kill", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	all := fs.Bool("all", false, "kill all sessions")
	jsonOut := fs.Bool("json", false, "output result as JSON")
	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if !*all && fs.NArg() == 0 {
		return fmt.Errorf("usage: mplex kill <session> | mplex kill --all")
	}

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	killed, err := rc.Kill(target, *all)
	if err != nil {
		return err
	}
	if *jsonOut {
		if killed == nil {
			killed = []string{}
		}
		return writeJSON(map[string]any{"killed": killed})
	}
	if len(killed) > 0 {
		fmt.Printf("Killed: %s\n", strings.Join(killed, ", "))
	}
	return nil
}

func runRenameCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex rename", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	jsonOut := fs.Bool("json", false, "output renamed session metadata as JSON")
	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{"socket": true})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: mplex rename <old-name> <new-name>")
	}
	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()

	sess, err := rc.Rename(fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(sess)
	}
	fmt.Printf("Renamed session to %q\n", sess.Name)
	return nil
}

func runDetachCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex detach", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	clientID := fs.String("client", "", "client ID to detach")
	jsonOut := fs.Bool("json", false, "output result as JSON")
	parseArgs, _ := reorderFlagsBeforeDash(args, map[string]bool{
		"socket": true,
		"client": true,
	})
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()
	if err := rc.Detach(*clientID, os.Getenv("MPLEX_SESSION_ID")); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(map[string]any{"detached": true})
	}
	return nil
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func normalizeNamingMode(v string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(v))
	if m == "" {
		return "cwd", nil
	}
	switch m {
	case "cwd", "numeric":
		return m, nil
	default:
		return "", fmt.Errorf("invalid --naming mode %q (expected \"cwd\" or \"numeric\")", v)
	}
}

func formatSince(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func printUsage() {
	fmt.Print(`mplex — native-behavior terminal multiplexer with fzf-style session switcher

Usage:
  mplex [flags]                             Start/resume session (opens creation screen if another window is attached)
  mplex <name>                              Attach to session <name>, creating it if needed
  mplex new [name] [-d] [-c dir] [--json]   Create a new session (optionally: -- <cmd...>)
  mplex attach [name] [--create] [--pick]   Attach to an existing session (switches outer window if inside mplex)
  mplex switch [name] [--client id] [--json] Open switcher or switch an active window directly to <name>
  mplex list [--json]                       List active sessions
  mplex capture <name> [flags]              Read a session's screen/scrollback (--lines, --scrollback, --ansi, -f/--follow, --json)
  mplex follow <name> [flags]               Stream live read-only output from a session (--history, --lines)
  mplex wait <name> [flags]                 Wait for a session condition (--match, --idle, --exit, --scrollback, --timeout, --json)
  mplex send <name> [text...] [flags]       Send input/keys to a session without attaching (--enter, --key, --stdin, --json)
  mplex mcp                                 Run built-in stdio MCP server (JSON-RPC 2.0) for LLM/agent integrations
  mplex rename <old> <new> [--json]         Rename a session
  mplex kill <name> | --all [--json]        Kill one or all sessions
  mplex detach [--client id] [--json]       Detach an active client
  mplex save [name] [flags]                 Save sessions to a workspace snapshot (--file, --with-scrollback, --json)
  mplex restore [name] [flags]              Restore sessions from a workspace snapshot (--file, --force, --json)
  mplex config [--path|--init] [--json]     Inspect or initialize config.toml
  mplex completion <fish|zsh|bash>          Print shell completion script
  mplex update [--check] [-y] [--json]      Check GitHub for a newer release and upgrade in place

Flags & Environment:
  --key <spec>            Switcher trigger key (default: ctrl-\, env: MPLEX_KEY)
                          Examples: ctrl-\, ctrl-space, ctrl-], alt-s, none
                          Use --key=none with a terminal hotkey bound to "mplex switch"
                          for 100% zero-byte keyboard interception.
  --pick                  Open the session switcher immediately on attach
  --socket <path>         Daemon Unix socket path (env: MPLEX_SOCKET)
  --scrollback <lines>    Max scrollback lines tracked per session (default: 10000)
  --replay-lines <lines>  Max scrollback lines replayed on switch (default: 10000)
  --window-title          Tag outer terminal window title with [session] (default: true, env: MPLEX_WINDOW_TITLE)
  --naming <mode>         Default session naming mode: cwd or numeric (default: cwd, env: MPLEX_NAMING)
  MPLEX_CONFIG=<path>     Config file path (default: ~/.config/mplex/config.toml)
  MPLEX_NO_UPDATE_CHECK=1 Disable automatic GitHub release check prompt on startup

Switcher Keys:
  Type to fuzzy-filter    Enter: switch / create     Tab: toggle last session
  Up/Down / Ctrl+P/N      Alt+Enter / Ctrl+O: new    Ctrl+R: rename
  Ctrl+X: kill            Ctrl+Q: detach             Esc: cancel / quit
  Alt/Shift+Up/Down / Ctrl+B/F / Mouse wheel: scroll preview pane
  Press trigger key twice to send the literal key to your session.
`)
}
