package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/daemon"
	"github.com/EricWF/mplex/internal/protocol"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mplex: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	socketPath := daemon.DefaultSocketPath()
	keySpec := os.Getenv("MPLEX_KEY")
	if keySpec == "" {
		keySpec = "ctrl-\\"
	}
	scrollback := envInt("MPLEX_SCROLLBACK", 10000)
	replayLines := envInt("MPLEX_REPLAY_LINES", 10000)

	// Parse leading global flags before subcommand.
	fs := flag.NewFlagSet("mplex", flag.ContinueOnError)
	fs.Usage = printUsage
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.StringVar(&keySpec, "key", keySpec, "switcher trigger key (e.g. ctrl-\\, ctrl-space, alt-s, none)")
	fs.IntVar(&scrollback, "scrollback", scrollback, "max scrollback lines kept per session")
	fs.IntVar(&replayLines, "replay-lines", replayLines, "max scrollback lines replayed on switch")
	openPicker := fs.Bool("pick", false, "open session switcher immediately")
	nested := fs.Bool("nested", false, "allow nesting mplex inside an existing mplex session")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Printf("mplex %s\n", version)
		return nil
	}

	rest := fs.Args()
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
		rest = rest[1:]
	}

	switch sub {
	case "version":
		fmt.Printf("mplex %s\n", version)
		return nil

	case "help", "-h", "--help":
		printUsage()
		return nil

	case "daemon":
		return runDaemonCmd(socketPath, rest)

	case "ls", "list":
		return runListCmd(socketPath, rest)

	case "new", "n":
		return runNewCmd(socketPath, keySpec, scrollback, replayLines, rest)

	case "attach", "a":
		return runAttachCmd(socketPath, keySpec, scrollback, replayLines, rest)

	case "switch", "pick", "s":
		return runSwitchCmd(socketPath, keySpec, scrollback, replayLines, rest)

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
			return runSwitchCmd(socketPath, keySpec, scrollback, replayLines, nil)
		}
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
		})

	default:
		// Shorthand: `mplex <name>` attaches to (or creates) session <name>.
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		fmt.Println("No running mplex sessions.")
		return nil
	}
	defer rc.Close()

	sessions, err := rc.List()
	if err != nil {
		return err
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

func runNewCmd(socketPath, keySpec string, scrollback, replayLines int, args []string) error {
	fs := flag.NewFlagSet("mplex new", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.StringVar(&keySpec, "key", keySpec, "switcher trigger key")
	detached := fs.Bool("d", false, "create session detached without attaching")
	cwd := fs.String("c", "", "starting directory for the new session")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Support: `mplex new [name] [-- cmd args...]`
	var name string
	var cmdArgs []string
	rem := fs.Args()
	for i, a := range rem {
		if a == "--" {
			cmdArgs = rem[i+1:]
			rem = rem[:i]
			break
		}
	}
	if len(rem) > 0 {
		name = rem[0]
		if len(rem) > 1 && len(cmdArgs) == 0 {
			cmdArgs = rem[1:]
		}
	}

	startDir := *cwd
	if startDir == "" {
		startDir, _ = os.Getwd()
	}

	if *detached {
		rc, err := client.DialOrStart(socketPath, nil)
		if err != nil {
			return err
		}
		defer rc.Close()

		sess, err := rc.Create(protocol.CreateReq{
			Name:       name,
			Cwd:        startDir,
			Cmd:        cmdArgs,
			Env:        os.Environ(),
			Size:       protocol.WinSize{Rows: 24, Cols: 80},
			Scrollback: scrollback,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Created session %q (id=%s)\n", sess.Name, sess.ID)
		return nil
	}

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
	})
}

func runAttachCmd(socketPath, keySpec string, scrollback, replayLines int, args []string) error {
	fs := flag.NewFlagSet("mplex attach", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	fs.StringVar(&keySpec, "key", keySpec, "switcher trigger key")
	create := fs.Bool("create", false, "create the session if it does not exist")
	pick := fs.Bool("pick", false, "open session switcher")
	if err := fs.Parse(args); err != nil {
		return err
	}

	trig, err := client.ParseTriggerKey(keySpec)
	if err != nil {
		return err
	}

	target := ""
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	return client.RunInteractive(client.Options{
		SocketPath:      socketPath,
		Trigger:         trig,
		InitialTarget:   target,
		CreateIfMissing: *create,
		OpenPicker:      *pick,
		Scrollback:      scrollback,
		ReplayLines:     replayLines,
	})
}

func runSwitchCmd(socketPath, keySpec string, scrollback, replayLines int, args []string) error {
	fs := flag.NewFlagSet("mplex switch", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	clientID := fs.String("client", "", "specific client ID to open switcher on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// First try out-of-band trigger on an already attached interactive client!
	if rc, err := client.Dial(socketPath, nil); err == nil {
		err = rc.TriggerSwitcher(*clientID, os.Getenv("MPLEX_SESSION_ID"))
		_ = rc.Close()
		if err == nil {
			return nil
		}
	}

	// If no interactive client is attached yet, start one straight into the switcher!
	trig, err := client.ParseTriggerKey(keySpec)
	if err != nil {
		return err
	}
	return client.RunInteractive(client.Options{
		SocketPath:  socketPath,
		Trigger:     trig,
		OpenPicker:  true,
		Scrollback:  scrollback,
		ReplayLines: replayLines,
	})
}

func runKillCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex kill", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	all := fs.Bool("all", false, "kill all sessions")
	if err := fs.Parse(args); err != nil {
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
	if len(killed) > 0 {
		fmt.Printf("Killed: %s\n", strings.Join(killed, ", "))
	}
	return nil
}

func runRenameCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex rename", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	if err := fs.Parse(args); err != nil {
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
	fmt.Printf("Renamed session to %q\n", sess.Name)
	return nil
}

func runDetachCmd(socketPath string, args []string) error {
	fs := flag.NewFlagSet("mplex detach", flag.ContinueOnError)
	fs.StringVar(&socketPath, "socket", socketPath, "daemon Unix socket path")
	clientID := fs.String("client", "", "client ID to detach")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rc, err := client.Dial(socketPath, nil)
	if err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	defer rc.Close()
	return rc.Detach(*clientID, os.Getenv("MPLEX_SESSION_ID"))
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
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
  mplex [flags]                     Attach to latest session (or create one; inside mplex, opens switcher)
  mplex <name>                      Attach to session <name>, creating it if needed
  mplex new [name] [-d] [-c dir]    Create a new session (optionally: -- <cmd...>)
  mplex attach [name]               Attach to an existing session
  mplex switch                      Trigger session switcher (works out-of-band for terminal keybindings!)
  mplex list                        List active sessions
  mplex rename <old> <new>          Rename a session
  mplex kill <name> | --all         Kill one or all sessions
  mplex detach                      Detach the active client

Flags & Environment:
  --key <spec>            Switcher trigger key (default: ctrl-\, env: MPLEX_KEY)
                          Examples: ctrl-\, ctrl-space, ctrl-], alt-s, none
                          Use --key=none with a terminal hotkey bound to "mplex switch"
                          for 100% zero-byte keyboard interception.
  --pick                  Open the session switcher immediately on attach
  --socket <path>         Daemon Unix socket path (env: MPLEX_SOCKET)
  --scrollback <lines>    Max scrollback lines tracked per session (default: 10000)
  --replay-lines <lines>  Max scrollback lines replayed on switch (default: 10000)

Switcher Keys:
  Type to fuzzy-filter    Enter: switch / create     Tab: toggle last session
  Up/Down / Ctrl+P/N      Alt+Enter / Ctrl+O: new    Ctrl+R: rename
  Ctrl+X: kill            Ctrl+Q: detach             Esc: cancel
  Press trigger key twice to send the literal key to your session.
`)
}
