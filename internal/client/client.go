package client

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/EricWF/mplex/internal/protocol"
	"github.com/EricWF/mplex/internal/switcher"
	"github.com/EricWF/mplex/internal/vt"
)

// Options configures an interactive terminal client session.
type Options struct {
	SocketPath      string
	Trigger         TriggerKey
	InitialTarget   string // optional session name/ID to attach immediately
	CreateIfMissing bool
	CreateName      string
	CreateCwd       string
	CreateCmd       []string
	OpenPicker      bool // open switcher immediately on startup
	Scrollback      int
	ReplayLines     int
	WindowTitle     bool   // tag outer terminal window title with [session] name
	NamingMode      string // default session naming mode ("cwd" or "numeric")
}

// RunInteractive connects to the daemon, enters raw terminal mode, and proxies
// the outer terminal transparently to the active session with on-demand switching.
func RunInteractive(opts Options) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("mplex interactive mode requires a TTY on stdin and stdout")
	}

	rc, err := DialOrStart(opts.SocketPath, os.Stdout)
	if err != nil {
		return err
	}
	defer rc.Close()
	rc.SetWindowTitleEnabled(opts.WindowTitle)

	sz := readWinSize(os.Stdin)
	helloResp, err := rc.Hello(protocol.HelloReq{
		Size:        sz,
		Term:        os.Getenv("TERM"),
		ColorTerm:   os.Getenv("COLORTERM"),
		ReplayLines: opts.ReplayLines,
		Interactive: true,
	})
	if err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}
	_ = helloResp

	// Save original outer terminal state and switch to raw mode.
	stdinFd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("enable raw mode: %w", err)
	}

	var exitMessage string
	defer func() {
		rc.SetWindowTitleEnabled(false)
		// Always leave the outer terminal in a clean, usable native state.
		// ResetOuterModes(true) uses ?1047l which exits alt-screen if active
		// and is a safe no-op if already on the main screen.
		rc.WriteStdoutRaw(vt.ResetOuterModes(true))
		if opts.WindowTitle {
			rc.WriteStdoutRaw([]byte("\x1b]0;\x07"))
		}
		_ = term.Restore(stdinFd, oldState)
		if exitMessage != "" {
			fmt.Fprintln(os.Stderr, exitMessage)
		}
	}()

	// Signal handling: SIGWINCH for live resizes, SIGTERM/SIGHUP for clean restore.
	winchCh := make(chan os.Signal, 4)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	termSigCh := make(chan os.Signal, 2)
	signal.Notify(termSigCh, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(termSigCh)

	// Shared state between stdin goroutine and main loop.
	var (
		inSwitcher atomic.Bool
		sizeMu     sync.Mutex
		curSize    = sz
	)

	getSize := func() protocol.WinSize {
		sizeMu.Lock()
		defer sizeMu.Unlock()
		return curSize
	}

	switcherInputCh := make(chan []byte, 64)
	switcherResizeCh := make(chan protocol.WinSize, 8)
	switcherRefreshCh := make(chan string, 8)
	triggerOpenCh := make(chan []byte, 4)
	stdinErrCh := make(chan error, 1)

	// Resize forwarder goroutine.
	stopResize := make(chan struct{})
	defer close(stopResize)
	go func() {
		for {
			select {
			case <-stopResize:
				return
			case <-winchCh:
				nsz := readWinSize(os.Stdin)
				sizeMu.Lock()
				curSize = nsz
				sizeMu.Unlock()
				if inSwitcher.Load() {
					select {
					case switcherResizeCh <- nsz:
					default:
					}
				} else {
					_ = rc.SendResize(nsz)
				}
			}
		}
	}()

	// Dedicated stdin reader goroutine (started once we're either attached to a
	// session or entering the switcher, so type-ahead during startup is never lost).
	var startStdinOnce sync.Once
	startStdinReader := func() {
		startStdinOnce.Do(func() {
			go func() {
				buf := make([]byte, 32*1024)
				for {
					n, err := os.Stdin.Read(buf)
					if n > 0 {
						chunk := make([]byte, n)
						copy(chunk, buf[:n])

						if inSwitcher.Load() {
							switcherInputCh <- chunk
							continue
						}

						before, matched, after, found := opts.Trigger.Match(chunk)
						if !found {
							if sendErr := rc.SendStdin(chunk); sendErr != nil {
								stdinErrCh <- sendErr
								return
							}
							continue
						}

						if len(before) > 0 {
							_ = rc.SendStdin(before)
						}
						// Switch routing immediately so subsequent keystrokes go into switcher!
						inSwitcher.Store(true)
						select {
						case triggerOpenCh <- matched:
						default:
						}
						if len(after) > 0 {
							switcherInputCh <- after
						}
					}
					if err != nil {
						stdinErrCh <- err
						return
					}
				}
			}()
		})
	}

	var (
		currentSession protocol.SessionInfo
		hasSession     bool
		lastSessionID  string
	)

	attachSession := func(target string) error {
		rc.SetSuppressOutput(false)
		resp, err := rc.Attach(protocol.AttachReq{
			Target:      target,
			Size:        getSize(),
			ReplayLines: opts.ReplayLines,
		})
		if err != nil {
			return err
		}
		if hasSession && currentSession.ID != resp.Session.ID {
			lastSessionID = currentSession.ID
		}
		currentSession = resp.Session
		hasSession = true
		return nil
	}

	createAndAttach := func(name, cwd string, cmd []string) error {
		if cwd == "" {
			if hasSession && currentSession.Cwd != "" {
				cwd = currentSession.Cwd
			} else if opts.CreateCwd != "" {
				cwd = opts.CreateCwd
			} else if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}
		sess, err := rc.Create(protocol.CreateReq{
			Name:       name,
			Cwd:        cwd,
			Shell:      os.Getenv("SHELL"),
			Cmd:        cmd,
			Env:        os.Environ(),
			Size:       getSize(),
			Scrollback: opts.Scrollback,
			NamingMode: opts.NamingMode,
		})
		if err != nil {
			return err
		}
		return attachSession(sess.ID)
	}

	type switcherOpts struct {
		notice       string
		allowCancel  bool
		preferCreate bool
		exitOnEsc    bool
		openedWith   []byte
	}

	runSwitcher := func(sopt switcherOpts) (exitClient bool, err error) {
		inSwitcher.Store(true)
		startStdinReader()
		rc.SetSuppressOutput(true)
		if hasSession {
			_ = rc.Pause(true)
		}

		// Drain stale switcher resize notifications before opening.
		drainWinSizes(switcherResizeCh)
		drainStrings(switcherRefreshCh)

		defaultCwd := opts.CreateCwd
		if hasSession && currentSession.Cwd != "" {
			defaultCwd = currentSession.Cwd
		} else if defaultCwd == "" {
			defaultCwd, _ = os.Getwd()
		}

		s := getSize()
		res := switcher.Run(switcher.Config{
			CurrentSessionID: currentSession.ID,
			LastSessionID:    lastSessionID,
			TriggerLabel:     opts.Trigger.Label,
			Notice:           sopt.notice,
			DefaultCwd:       defaultCwd,
			Rows:             int(s.Rows),
			Cols:             int(s.Cols),
			AllowCancel:      sopt.allowCancel && hasSession,
			PreferCreate:     sopt.preferCreate,
			ExitOnEsc:        sopt.exitOnEsc,
			InputCh:          switcherInputCh,
			ResizeCh:         switcherResizeCh,
			RefreshCh:        switcherRefreshCh,
			Out:              os.Stdout,
			Callbacks: switcher.Callbacks{
				ListSessions:       rc.List,
				GetPreview:         rc.Preview,
				GetPreviewScrolled: rc.PreviewScrolled,
				KillSession: func(target string) error {
					_, err := rc.Kill(target, false)
					return err
				},
				Rename: func(target, newName string) (protocol.SessionInfo, error) {
					sess, err := rc.Rename(target, newName)
					if err == nil && hasSession && sess.ID == currentSession.ID {
						currentSession = sess
						rc.UpdateWindowTitle(sess.Name, sess.Title)
					}
					return sess, err
				},
				MatchTrigger: func(chunk []byte) ([]byte, bool) {
					before, matched, after, ok := opts.Trigger.Match(chunk)
					if ok && len(before) == 0 && len(after) == 0 {
						return matched, true
					}
					return nil, false
				},
			},
		})

		// If the user killed the currently attached session inside the switcher,
		// mark hasSession = false.
		if res.Action == switcher.ActionCancel && !hasSession {
			res.Action = switcher.ActionDetach
		}

		switch res.Action {
		case switcher.ActionCancel, switcher.ActionSendLiteral:
			dropped := rc.ConsumeDroppedFlag()
			rc.SetSuppressOutput(false)
			inSwitcher.Store(false)
			restResp, err := rc.Restore(protocol.RestoreReq{
				Size:        getSize(),
				ReplayLines: opts.ReplayLines,
				ForceReplay: dropped,
			})
			if err != nil {
				// Current session may have exited while switcher was open!
				hasSession = false
				return false, nil
			}
			if restResp.Session != nil {
				currentSession = *restResp.Session
			}
			if res.Action == switcher.ActionSendLiteral {
				lit := sopt.openedWith
				if len(lit) == 0 {
					lit = res.Literal
				}
				if len(lit) > 0 {
					_ = rc.SendStdin(lit)
				}
			}
			return false, nil

		case switcher.ActionSelect:
			inSwitcher.Store(false)
			if err := attachSession(res.Target); err != nil {
				if !hasSession {
					return true, err
				}
				return false, nil
			}
			return false, nil

		case switcher.ActionCreate:
			inSwitcher.Store(false)
			if err := createAndAttach(res.Target, defaultCwd, nil); err != nil {
				return false, err
			}
			return false, nil

		case switcher.ActionDetach:
			inSwitcher.Store(false)
			if hasSession {
				_ = rc.Detach("", "")
				exitMessage = "mplex: detached"
			}
			return true, nil
		}

		inSwitcher.Store(false)
		return false, nil
	}

	// --- Initial startup behavior ---
	switch {
	case opts.OpenPicker:
		sessions, err := rc.List()
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			if err := createAndAttach(opts.CreateName, opts.CreateCwd, opts.CreateCmd); err != nil {
				return err
			}
		} else {
			exitClient, err := runSwitcher(switcherOpts{exitOnEsc: true})
			if err != nil || exitClient {
				return err
			}
		}

	case opts.CreateName != "" || len(opts.CreateCmd) > 0:
		if err := createAndAttach(opts.CreateName, opts.CreateCwd, opts.CreateCmd); err != nil {
			return err
		}

	case opts.InitialTarget != "":
		if err := attachSession(opts.InitialTarget); err != nil {
			if opts.CreateIfMissing {
				if err := createAndAttach(opts.InitialTarget, opts.CreateCwd, opts.CreateCmd); err != nil {
					return err
				}
			} else {
				return err
			}
		}

	default:
		sessions, err := rc.List()
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			if err := createAndAttach("", opts.CreateCwd, opts.CreateCmd); err != nil {
				return err
			}
		} else {
			anyAttached := false
			for _, s := range sessions {
				if s.Attached {
					anyAttached = true
					break
				}
			}
			if anyAttached {
				// Another terminal window already has an active mplex session.
				// Open the switcher/creation screen with "+ New session" selected
				// instead of stealing the active session from that window.
				exitClient, err := runSwitcher(switcherOpts{
					preferCreate: true,
					exitOnEsc:    true,
				})
				if err != nil || exitClient {
					return err
				}
			} else {
				// No other window is attached; seamlessly resume the most recent session.
				if err := attachSession(sessions[0].ID); err != nil {
					return err
				}
			}
		}
	}

	// Start reading stdin now that we are attached to an active session.
	startStdinReader()

	// --- Main event loop ---
	for {
		select {
		case <-rc.Done():
			exitMessage = "mplex: daemon disconnected"
			return nil

		case <-termSigCh:
			_ = rc.Detach("", "")
			return nil

		case err := <-stdinErrCh:
			_ = rc.Detach("", "")
			return err

		case ev := <-rc.DetachedCh:
			if ev.Reason != "" {
				exitMessage = "mplex: " + ev.Reason
			} else {
				exitMessage = "mplex: detached"
			}
			return nil

		case matched := <-triggerOpenCh:
			exitClient, err := runSwitcher(switcherOpts{allowCancel: true, openedWith: matched})
			if err != nil || exitClient {
				return err
			}

		case <-rc.OpenSwitcherCh:
			exitClient, err := runSwitcher(switcherOpts{allowCancel: true})
			if err != nil || exitClient {
				return err
			}

		case ev := <-rc.SwitchSessionCh:
			if ev.Target != "" && (!hasSession || ev.Target != currentSession.ID) {
				_ = attachSession(ev.Target)
			}

		case ev := <-rc.SessionExitedCh:
			// Ignore exit events for sessions we already switched away from or killed.
			if !hasSession || ev.SessionID != currentSession.ID {
				continue
			}
			hasSession = false
			sessions, err := rc.List()
			if err != nil || len(sessions) == 0 {
				exitMessage = fmt.Sprintf("mplex: session %q exited (code %d)", ev.SessionName, ev.ExitCode)
				return nil
			}
			notice := fmt.Sprintf("Session %q exited (code %d)", ev.SessionName, ev.ExitCode)
			exitClient, err := runSwitcher(switcherOpts{notice: notice, exitOnEsc: true})
			if err != nil || exitClient {
				return err
			}
		}
	}
}

func readWinSize(f *os.File) protocol.WinSize {
	ws, err := pty.GetsizeFull(f)
	if err != nil || ws.Rows == 0 || ws.Cols == 0 {
		return protocol.WinSize{Rows: 24, Cols: 80}
	}
	return protocol.WinSize{
		Rows:   ws.Rows,
		Cols:   ws.Cols,
		XPixel: ws.X,
		YPixel: ws.Y,
	}
}

func drainWinSizes(ch <-chan protocol.WinSize) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func drainStrings(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func formatWindowTitle(sessionName, innerTitle string) string {
	name := strings.TrimSpace(sessionName)
	title := strings.TrimSpace(innerTitle)
	if name == "" {
		return title
	}
	prefix := "[" + name + "]"
	if title == "" {
		return prefix
	}
	if strings.HasPrefix(title, prefix) {
		return title
	}
	return prefix + " " + title
}

func formatWindowTitleOSC(sessionName, innerTitle string) []byte {
	formatted := formatWindowTitle(sessionName, innerTitle)
	if formatted == "" {
		return nil
	}
	return []byte("\x1b]0;" + formatted + "\x07")
}
