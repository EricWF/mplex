package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/protocol"
	"github.com/EricWF/mplex/internal/vt"
	"github.com/EricWF/mplex/internal/workspace"
)

// shortTempSock creates a short Unix socket path under /tmp so macOS stays well
// under its 104-byte sockaddr_un.sun_path limit.
func shortTempSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mplex-e2e-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "m.sock")
}

// waitSessionAttached polls `mplex list --json` until the named session exists and is attached.
func waitSessionAttached(t *testing.T, bin, sock, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command(bin, "--socket", sock, "list", "--json")
		cmd.Env = append(os.Environ(), "MPLEX_SESSION=", "MPLEX_SESSION_ID=", "MPLEX_SOCKET=", "MPLEX_NO_UPDATE_CHECK=1")
		if out, err := cmd.CombinedOutput(); err == nil {
			var list []protocol.SessionInfo
			if json.Unmarshal(out, &list) == nil {
				for _, s := range list {
					if s.Name == name && s.Attached {
						return
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session %q to be attached", name)
}

func TestFullInteractiveCLIInRealPTY(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	// Ensure daemon cleanup at end of test.
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	// Launch real `mplex` inside a real 24x90 PTY in a named project directory
	// so default cwd naming produces "demo"!
	projDir := filepath.Join(tmp, "demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projDir: %v", err)
	}
	cmd := exec.Command(bin, "--socket", sock)
	cmd.Dir = projDir
	cmd.Env = append(os.Environ(),
		"SHELL=/bin/sh",
		"PS1=TESTPROMPT$ ",
		"TERM=xterm-256color",
		"MPLEX_SESSION=",
		"MPLEX_SOCKET=",
		"MPLEX_NAMING=",
		"MPLEX_WINDOW_TITLE=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 90})
	if err != nil {
		t.Fatalf("pty.StartWithSize: %v", err)
	}
	defer ptmx.Close()

	// Feed real PTY output into a shadow VT representing the user's outer terminal emulator.
	var mu sync.Mutex
	outer := vt.New(24, 90, 2000)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				outer.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	screenText := func() string {
		mu.Lock()
		defer mu.Unlock()
		s := outer.Active()
		var b strings.Builder
		for r := 0; r < s.Rows; r++ {
			for _, c := range s.Lines[r].Cells {
				if c.Width == 0 {
					continue
				}
				if c.Rune == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(c.Rune)
				}
			}
			b.WriteByte('\n')
		}
		return b.String()
	}

	scrollbackText := func() string {
		mu.Lock()
		defer mu.Unlock()
		var b strings.Builder
		for _, l := range outer.Main.Scrollback {
			for _, c := range l.Cells {
				if c.Width == 0 {
					continue
				}
				if c.Rune == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(c.Rune)
				}
			}
			b.WriteByte('\n')
		}
		return b.String()
	}

	waitCond := func(desc string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s\n--- SCREEN ---\n%s", desc, screenText())
	}

	// 1. Verify initial shell starts on main screen buffer with cwd basename "demo" and tags outer window title!
	waitSessionAttached(t, bin, sock, "demo")
	_, _ = ptmx.Write([]byte("i=1; while [ $i -le 40 ]; do echo MAIN_SCROLL_$i; i=$((i+1)); done; echo MAIN_DONE:$MPLEX_SESSION\n"))
	waitCond("demo session output + native scrollback + outer window title", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		title := outer.Title
		mu.Unlock()
		return !inAlt && title == "[demo]" && strings.Contains(screenText(), "MAIN_DONE:demo") && strings.Contains(scrollbackText(), "MAIN_SCROLL_1")
	})

	// 2. Press Ctrl+\ (0x1c) to open the fzf-style switcher!
	_, _ = ptmx.Write([]byte{0x1c})
	waitCond("switcher UI visible on alt screen", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		txt := screenText()
		return inAlt && strings.Contains(txt, "mplex") && strings.Contains(txt, "demo")
	})

	// 3. Type "api" and hit Enter to create and switch to a brand-new session "api"!
	_, _ = ptmx.Write([]byte("api"))
	waitCond("create new session 'api' option highlighted", func() bool {
		return strings.Contains(screenText(), `Create new session "api"`)
	})
	_, _ = ptmx.Write([]byte{'\r'})

	// Wait until we are back on main screen in the fresh "api" session with outer title "[api]"!
	waitCond("switched to fresh api session", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		sbLen := len(outer.Main.Scrollback)
		title := outer.Title
		mu.Unlock()
		return !inAlt && sbLen == 0 && title == "[api]" && !strings.Contains(screenText(), "MAIN_DONE")
	})

	// Run a command inside "api" to verify it's an independent live shell!
	_, _ = ptmx.Write([]byte("echo INSIDE_API_SESSION:$MPLEX_SESSION\n"))
	waitCond("api session command output", func() bool {
		return strings.Contains(screenText(), "INSIDE_API_SESSION:api")
	})

	// 4. Press Ctrl+\ then Tab to quick-toggle back to "demo"!
	_, _ = ptmx.Write([]byte{0x1c})
	waitCond("switcher reopened", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		return inAlt && strings.Contains(screenText(), "api") && strings.Contains(screenText(), "demo")
	})
	_, _ = ptmx.Write([]byte{'\t'})

	// Verify we are back in "demo" AND outer terminal's NATIVE scrollback + window title are restored!
	waitCond("quick-toggled back to demo with native scrollback restored", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		title := outer.Title
		mu.Unlock()
		return !inAlt && title == "[demo]" && strings.Contains(screenText(), "MAIN_DONE") && strings.Contains(scrollbackText(), "MAIN_SCROLL_1")
	})

	// 5. Open switcher and press Ctrl+Q to detach cleanly!
	_, _ = ptmx.Write([]byte{0x1c})
	waitCond("switcher open before detach", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return outer.InAltScreen
	})
	_, _ = ptmx.Write([]byte{0x11}) // Ctrl+Q

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("mplex exited with error after detach: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("mplex did not exit cleanly after Ctrl+Q detach")
	}

	// Wait for all final output bytes (including ResetOuterModes + title reset) to drain from the PTY!
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("PTY reader did not drain after mplex exit")
	}

	// Verify outer terminal was NOT left stuck in alt-screen, mouse mode, or stale window title!
	mu.Lock()
	if outer.InAltScreen {
		t.Fatalf("outer terminal left in alt screen after detach!")
	}
	if outer.MouseMode != 0 {
		t.Fatalf("outer terminal left in mouse mode %d after detach!", outer.MouseMode)
	}
	if outer.Title != "" {
		t.Fatalf("outer terminal window title was not reset on detach: %q", outer.Title)
	}
	mu.Unlock()

	// 6. Verify both sessions are still alive via `mplex list`!
	listOut, err := exec.Command(bin, "--socket", sock, "list").CombinedOutput()
	if err != nil {
		t.Fatalf("mplex list failed: %v\n%s", err, listOut)
	}
	ls := string(listOut)
	if !strings.Contains(ls, "demo") || !strings.Contains(ls, "api") {
		t.Fatalf("expected both 'demo' and 'api' sessions alive after detach:\n%s", ls)
	}
	fmt.Printf("Full PTY interactive CLI test passed! Active sessions:\n%s", ls)
}

func TestMultiWindowIndependentSessions(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	type ptyWindow struct {
		cmd    *exec.Cmd
		ptmx   *os.File
		mu     sync.Mutex
		outer  *vt.Terminal
		doneCh chan error
	}

	workDir := filepath.Join(tmp, "webapp")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	launchWindow := func(args ...string) *ptyWindow {
		t.Helper()
		fullArgs := append([]string{"--socket", sock}, args...)
		cmd := exec.Command(bin, fullArgs...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(),
			"SHELL=/bin/sh",
			"PS1=W$ ",
			"TERM=xterm-256color",
			"MPLEX_SESSION=",
			"MPLEX_SOCKET=",
			"MPLEX_NAMING=",
			"MPLEX_WINDOW_TITLE=",
			"MPLEX_NO_UPDATE_CHECK=1",
		)
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 100})
		if err != nil {
			t.Fatalf("pty.StartWithSize: %v", err)
		}
		w := &ptyWindow{
			cmd:    cmd,
			ptmx:   ptmx,
			outer:  vt.New(24, 100, 1000),
			doneCh: make(chan error, 1),
		}
		go func() {
			buf := make([]byte, 8192)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					w.mu.Lock()
					w.outer.Write(buf[:n])
					w.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
		go func() {
			w.doneCh <- cmd.Wait()
		}()
		t.Cleanup(func() {
			_ = ptmx.Close()
		})
		return w
	}

	screenOf := func(w *ptyWindow) string {
		w.mu.Lock()
		defer w.mu.Unlock()
		s := w.outer.Active()
		var b strings.Builder
		for r := 0; r < s.Rows; r++ {
			for _, c := range s.Lines[r].Cells {
				if c.Width == 0 {
					continue
				}
				if c.Rune == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(c.Rune)
				}
			}
			b.WriteByte('\n')
		}
		return b.String()
	}

	inAltOf := func(w *ptyWindow) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.outer.InAltScreen
	}

	waitCond := func(w *ptyWindow, desc string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s\n--- SCREEN ---\n%s", desc, screenOf(w))
	}

	// 1. Window 1 starts mplex with no existing sessions -> creates & attaches to "webapp".
	win1 := launchWindow()
	waitSessionAttached(t, bin, sock, "webapp")
	_, _ = win1.ptmx.Write([]byte("echo WIN1_READY:$MPLEX_SESSION\n"))
	waitCond(win1, "win1 webapp session ready", func() bool {
		return !inAltOf(win1) && strings.Contains(screenOf(win1), "WIN1_READY:webapp")
	})

	// 2. Window 2 runs plain `mplex` -> must open session creation/switcher screen with
	// "+ New session" selected, NOT steal "webapp" from Window 1!
	win2 := launchWindow()
	waitCond(win2, "win2 creation/switcher screen open", func() bool {
		txt := screenOf(win2)
		return inAltOf(win2) &&
			strings.Contains(txt, "New Session") &&
			strings.Contains(txt, "+ New session") &&
			strings.Contains(txt, "webapp")
	})

	// Verify Window 1 was NOT detached or killed!
	select {
	case err := <-win1.doneCh:
		t.Fatalf("Window 1 exited unexpectedly when Window 2 ran mplex (err=%v)", err)
	default:
	}

	// 3. Pressing Enter in Window 2 immediately creates a brand-new session ("webapp-2")!
	_, _ = win2.ptmx.Write([]byte{'\r'})
	waitCond(win2, "win2 entered new session", func() bool {
		return !inAltOf(win2)
	})
	_, _ = win2.ptmx.Write([]byte("echo WIN2_READY:$MPLEX_SESSION\n"))
	waitCond(win2, "win2 command output", func() bool {
		return strings.Contains(screenOf(win2), "WIN2_READY:webapp-2")
	})

	// 4. Verify Window 1 is STILL independently live in "webapp"!
	_, _ = win1.ptmx.Write([]byte("echo WIN1_STILL_ALIVE:$MPLEX_SESSION\n"))
	waitCond(win1, "win1 still live in webapp", func() bool {
		return strings.Contains(screenOf(win1), "WIN1_STILL_ALIVE:webapp")
	})
	if strings.Contains(screenOf(win1), "WIN2_READY") || strings.Contains(screenOf(win2), "WIN1_STILL_ALIVE") {
		t.Fatalf("output leaked across independent windows")
	}

	// 5. Verify `mplex list` reports BOTH sessions as attached!
	listOut, err := exec.Command(bin, "--socket", sock, "list").CombinedOutput()
	if err != nil {
		t.Fatalf("mplex list failed: %v\n%s", err, listOut)
	}
	ls := string(listOut)
	if strings.Count(ls, "attached") < 2 {
		t.Fatalf("expected 2 attached sessions in mplex list:\n%s", ls)
	}

	// 6. Window 3 opens plain `mplex` and presses Esc -> quits cleanly without touching win1/win2!
	win3 := launchWindow()
	waitCond(win3, "win3 switcher open", func() bool {
		return inAltOf(win3) && strings.Contains(screenOf(win3), "New Session")
	})
	_, _ = win3.ptmx.Write([]byte{0x1b})
	select {
	case err := <-win3.doneCh:
		if err != nil {
			t.Fatalf("win3 exited with error on Esc: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("win3 did not exit on Esc")
	}

	// Cleanly detach win1 and win2 via kill --all in defer.
}

func TestNewDetachedFlagAfterName(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)
	workDir := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	// Documented README syntax: flags (-d, -c) AFTER positional session name before `--`.
	out, err := exec.Command(bin, "--socket", sock, "new", "build", "-d", "-c", workDir, "--", "/bin/sh", "-c", "sleep 30").CombinedOutput()
	if err != nil {
		t.Fatalf("mplex new build -d failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `Created session "build"`) {
		t.Fatalf("unexpected output: %s", out)
	}

	listOut, err := exec.Command(bin, "--socket", sock, "list").CombinedOutput()
	if err != nil {
		t.Fatalf("mplex list failed: %v\n%s", err, listOut)
	}
	if !strings.Contains(string(listOut), "build") {
		t.Fatalf("expected session 'build' in list:\n%s", listOut)
	}
}

func TestLLMFriendlyCLIEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	runCLI := func(args ...string) []byte {
		t.Helper()
		full := append([]string{"--socket", sock}, args...)
		cmd := exec.Command(bin, full...)
		cmd.Env = append(os.Environ(), "MPLEX_SESSION=", "MPLEX_SESSION_ID=", "MPLEX_SOCKET=", "MPLEX_NO_UPDATE_CHECK=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mplex %v failed: %v\n%s", args, err, out)
		}
		return out
	}

	// 1. `mplex list --json` with no daemon running returns empty JSON array `[]`.
	emptyListOut := runCLI("list", "--json")
	var sessions []protocol.SessionInfo
	if err := json.Unmarshal(emptyListOut, &sessions); err != nil {
		t.Fatalf("unmarshal empty list json: %v\n%s", err, emptyListOut)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}

	// 2. `mplex new worker -d --json -- /bin/sh` creates a detached session with JSON output.
	newOut := runCLI("new", "worker", "-d", "--json", "--", "/bin/sh")
	var created protocol.SessionInfo
	if err := json.Unmarshal(newOut, &created); err != nil {
		t.Fatalf("unmarshal new json: %v\n%s", err, newOut)
	}
	if created.Name != "worker" || created.ID == "" {
		t.Fatalf("unexpected created session: %+v", created)
	}

	// 3. `mplex send worker ... --enter --json` drives the session non-interactively.
	sendOut := runCLI("send", "worker", "i=1; while [ $i -le 30 ]; do echo LOG_LINE_$i; i=$((i+1)); done; echo WORKER_READY:$MPLEX_SESSION", "--enter", "--json")
	var sendResp protocol.SendResp
	if err := json.Unmarshal(sendOut, &sendResp); err != nil {
		t.Fatalf("unmarshal send json: %v\n%s", err, sendOut)
	}
	if sendResp.Bytes == 0 || sendResp.Session.Name != "worker" {
		t.Fatalf("unexpected send response: %+v", sendResp)
	}

	// 4. `mplex capture worker --scrollback --json` reads scrollback + screen output!
	var capResp protocol.CaptureResp
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		capOut := runCLI("capture", "worker", "--scrollback", "--json")
		if err := json.Unmarshal(capOut, &capResp); err != nil {
			t.Fatalf("unmarshal capture json: %v\n%s", err, capOut)
		}
		if strings.Contains(capResp.Text, "WORKER_READY:worker") && strings.Contains(capResp.Text, "LOG_LINE_1") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !strings.Contains(capResp.Text, "WORKER_READY:worker") || !strings.Contains(capResp.Text, "LOG_LINE_1") {
		t.Fatalf("capture did not contain expected scrollback & marker:\n%s", capResp.Text)
	}

	// Also verify `--lines 5` limits captured lines.
	tailOut := runCLI("capture", "worker", "--scrollback", "--lines", "5", "--json")
	var tailResp protocol.CaptureResp
	if err := json.Unmarshal(tailOut, &tailResp); err != nil {
		t.Fatalf("unmarshal tail capture: %v", err)
	}
	if len(tailResp.Lines) > 5 {
		t.Fatalf("expected <= 5 lines, got %d", len(tailResp.Lines))
	}

	// 5. Launch an interactive PTY window attached to "main", then remotely switch it to "worker"
	// using non-TTY `mplex switch worker --json`!
	cmd := exec.Command(bin, "--socket", sock, "new", "main")
	cmd.Env = append(os.Environ(),
		"SHELL=/bin/sh",
		"PS1=P$ ",
		"TERM=xterm-256color",
		"MPLEX_SESSION=",
		"MPLEX_SOCKET=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 90})
	if err != nil {
		t.Fatalf("pty.StartWithSize: %v", err)
	}
	defer ptmx.Close()

	var mu sync.Mutex
	outer := vt.New(24, 90, 1000)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				outer.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	outerContains := func(substr string) bool {
		mu.Lock()
		defer mu.Unlock()
		s := outer.Active()
		for r := 0; r < s.Rows; r++ {
			var b strings.Builder
			for _, c := range s.Lines[r].Cells {
				if c.Width == 0 {
					continue
				}
				if c.Rune == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(c.Rune)
				}
			}
			if strings.Contains(b.String(), substr) {
				return true
			}
		}
		return false
	}

	waitSessionAttached(t, bin, sock, "main")
	_, _ = ptmx.Write([]byte("echo MAIN_WIN:$MPLEX_SESSION\n"))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !outerContains("MAIN_WIN:main") {
		time.Sleep(25 * time.Millisecond)
	}
	if !outerContains("MAIN_WIN:main") {
		t.Fatalf("interactive window did not start in main session")
	}

	// Non-TTY remote switch from external CLI (LLM tool call)!
	swOut := runCLI("switch", "worker", "--json")
	var swResp struct {
		Switched bool                  `json:"switched"`
		Client   string                `json:"client"`
		Session  *protocol.SessionInfo `json:"session"`
	}
	if err := json.Unmarshal(swOut, &swResp); err != nil {
		t.Fatalf("unmarshal switch json: %v\n%s", err, swOut)
	}
	if !swResp.Switched || swResp.Session == nil || swResp.Session.Name != "worker" {
		t.Fatalf("unexpected switch response: %+v", swResp)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !outerContains("WORKER_READY:worker") {
		time.Sleep(25 * time.Millisecond)
	}
	if !outerContains("WORKER_READY:worker") {
		t.Fatalf("interactive window did not switch to worker session")
	}

	// 6. Send special key (--key ctrl-c) without stealing the user's window.
	keyOut := runCLI("send", "worker", "--key", "ctrl-c", "--json")
	if err := json.Unmarshal(keyOut, &sendResp); err != nil || sendResp.Bytes != 1 {
		t.Fatalf("unexpected key send response: %s (err=%v)", keyOut, err)
	}

	// 7. Rename and Kill with `--json`.
	renOut := runCLI("rename", "worker", "api", "--json")
	var renamed protocol.SessionInfo
	if err := json.Unmarshal(renOut, &renamed); err != nil || renamed.Name != "api" {
		t.Fatalf("unexpected rename json: %s (err=%v)", renOut, err)
	}

	killOut := runCLI("kill", "api", "--json")
	var killResp struct {
		Killed []string `json:"killed"`
	}
	if err := json.Unmarshal(killOut, &killResp); err != nil || len(killResp.Killed) != 1 || killResp.Killed[0] != "api" {
		t.Fatalf("unexpected kill json: %s (err=%v)", killOut, err)
	}
}

func TestMultipleShellsSupported(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	// Prepare clean HOME/ZDOTDIR so zsh doesn't launch newuser wizard and fish has writable config dir.
	fakeHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(fakeHome, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	_ = os.WriteFile(filepath.Join(fakeHome, ".zshrc"), []byte("# test\n"), 0o644)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	candidates := []string{"sh", "bash", "zsh", "fish"}
	tested := 0
	for _, shName := range candidates {
		shPath, err := exec.LookPath(shName)
		if err != nil {
			continue
		}
		tested++
		t.Run(shName, func(t *testing.T) {
			sessName := "sess-" + shName
			env := append(os.Environ(),
				"HOME="+fakeHome,
				"ZDOTDIR="+fakeHome,
				"SHELL="+shPath,
				"TERM=xterm-256color",
				"MPLEX_SESSION=",
				"MPLEX_SESSION_ID=",
				"MPLEX_SOCKET=",
				"MPLEX_NO_UPDATE_CHECK=1",
			)

			// Create session relying purely on the caller's SHELL env (even on a shared daemon).
			mk := exec.Command(bin, "--socket", sock, "new", sessName, "-d", "--json")
			mk.Env = env
			if out, err := mk.CombinedOutput(); err != nil {
				t.Fatalf("new %s failed: %v\n%s", shName, err, out)
			}

			// Send a command valid across sh, bash, zsh, AND fish!
			send := exec.Command(bin, "--socket", sock, "send", sessName, "echo SHELL_MULTI:$MPLEX_SESSION", "--enter")
			send.Env = env
			if out, err := send.CombinedOutput(); err != nil {
				t.Fatalf("send %s failed: %v\n%s", shName, err, out)
			}

			want := "SHELL_MULTI:" + sessName
			deadline := time.Now().Add(5 * time.Second)
			var lastCap string
			for time.Now().Before(deadline) {
				cp := exec.Command(bin, "--socket", sock, "capture", sessName)
				cp.Env = env
				if out, err := cp.CombinedOutput(); err == nil {
					lastCap = string(out)
					if strings.Contains(lastCap, want) {
						return
					}
				}
				time.Sleep(40 * time.Millisecond)
			}
			t.Fatalf("shell %s (%s) did not produce %q\n--- CAPTURE ---\n%s", shName, shPath, want, lastCap)
		})
	}
	if tested == 0 {
		t.Fatalf("no shells found to test")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWaitAndFollowCLIEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	cleanEnv := append(os.Environ(),
		"SHELL=/bin/sh",
		"PS1=P$ ",
		"TERM=xterm-256color",
		"MPLEX_SESSION=",
		"MPLEX_SESSION_ID=",
		"MPLEX_SOCKET=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)

	runCLI := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"--socket", sock}, args...)...)
		cmd.Env = cleanEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mplex %v failed: %v\n%s", args, err, out)
		}
		return out
	}

	// 1. Create a detached worker session.
	runCLI("new", "worker", "-d", "--", "/bin/sh")

	// 2. `mplex wait` with NO conditions returns a clear usage error (non-zero exit).
	noCond := exec.Command(bin, "--socket", sock, "wait", "worker")
	noCond.Env = cleanEnv
	if out, err := noCond.CombinedOutput(); err == nil {
		t.Fatalf("expected error when mplex wait has no conditions, got output: %s", out)
	}

	// 3. `mplex wait --match` + `--scrollback` with flags in arbitrary order!
	runCLI("send", "worker", "i=1; while [ $i -le 30 ]; do echo W_LINE_$i; i=$((i+1)); done; sleep 0.1; echo BUILD_READY_99", "--enter")
	waitMatchOut := runCLI("wait", "worker", "--match", `BUILD_READY_\d+`, "--scrollback", "--timeout", "5s", "--json")
	var matchRes client.WaitResult
	if err := json.Unmarshal(waitMatchOut, &matchRes); err != nil {
		t.Fatalf("unmarshal wait match json: %v\n%s", err, waitMatchOut)
	}
	if !matchRes.Matched || matchRes.Reason != "match" || !strings.Contains(matchRes.MatchLine, "BUILD_READY_99") {
		t.Fatalf("unexpected wait match result: %+v", matchRes)
	}
	if !strings.Contains(matchRes.Text, "W_LINE_1") {
		t.Fatalf("expected scrollback W_LINE_1 in match text:\n%s", matchRes.Text)
	}

	// Also verify flags BEFORE positional target work.
	waitBeforeOut := runCLI("wait", "--match", "BUILD_READY_99", "--timeout", "3s", "worker", "--json")
	var matchBeforeRes client.WaitResult
	if err := json.Unmarshal(waitBeforeOut, &matchBeforeRes); err != nil || !matchBeforeRes.Matched {
		t.Fatalf("wait with flags before target failed: err=%v out=%s", err, waitBeforeOut)
	}

	// 4. `mplex wait --idle` waits until output settles.
	runCLI("send", "worker", "sleep 0.1; echo IDLE_DONE_OK", "--enter")
	waitIdleOut := runCLI("wait", "worker", "--idle", "200ms", "--timeout", "5s", "--json")
	var idleRes client.WaitResult
	if err := json.Unmarshal(waitIdleOut, &idleRes); err != nil {
		t.Fatalf("unmarshal wait idle json: %v\n%s", err, waitIdleOut)
	}
	if !idleRes.Matched || idleRes.Reason != "idle" || !strings.Contains(idleRes.Text, "IDLE_DONE_OK") {
		t.Fatalf("unexpected wait idle result: %+v", idleRes)
	}

	// 5. `mplex wait` timeout exits non-zero AND emits structured JSON when `--json` is passed.
	timeoutCmd := exec.Command(bin, "--socket", sock, "wait", "worker", "--match", "NEVER_GONNA_APPEAR", "--timeout", "150ms", "--json")
	timeoutCmd.Env = cleanEnv
	var timeoutStdout, timeoutStderr bytes.Buffer
	timeoutCmd.Stdout = &timeoutStdout
	timeoutCmd.Stderr = &timeoutStderr
	if err := timeoutCmd.Run(); err == nil {
		t.Fatalf("expected non-zero exit on wait timeout, stdout=%s stderr=%s", timeoutStdout.String(), timeoutStderr.String())
	}
	var timeoutRes client.WaitResult
	if err := json.Unmarshal(timeoutStdout.Bytes(), &timeoutRes); err != nil {
		t.Fatalf("unmarshal timeout json: %v\nstdout=%s\nstderr=%s", err, timeoutStdout.String(), timeoutStderr.String())
	}
	if timeoutRes.Matched || timeoutRes.Reason != "timeout" {
		t.Fatalf("unexpected timeout result: %+v", timeoutRes)
	}

	// 6. `mplex wait --exit` waits until a short-lived session exits cleanly.
	runCLI("new", "shortjob", "-d", "--", "/bin/sh", "-c", "sleep 0.15; echo JOB_DONE")
	waitExitOut := runCLI("wait", "shortjob", "--exit", "--timeout", "5s", "--json")
	var exitRes client.WaitResult
	if err := json.Unmarshal(waitExitOut, &exitRes); err != nil {
		t.Fatalf("unmarshal wait exit json: %v\n%s", err, waitExitOut)
	}
	if !exitRes.Matched || exitRes.Reason != "exit" {
		t.Fatalf("unexpected wait exit result: %+v", exitRes)
	}

	// 7. Read-Only Live Follow (`mplex follow` AND `mplex logs -f`) WITHOUT detaching an active PTY client!
	activeCmd := exec.Command(bin, "--socket", sock, "new", "app")
	activeCmd.Env = cleanEnv
	ptmx, err := pty.StartWithSize(activeCmd, &pty.Winsize{Rows: 24, Cols: 90})
	if err != nil {
		t.Fatalf("pty.StartWithSize: %v", err)
	}
	defer ptmx.Close()

	var outerMu sync.Mutex
	outer := vt.New(24, 90, 1000)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				outerMu.Lock()
				outer.Write(buf[:n])
				outerMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	activeDone := make(chan error, 1)
	go func() { activeDone <- activeCmd.Wait() }()

	outerHas := func(substr string) bool {
		outerMu.Lock()
		defer outerMu.Unlock()
		s := outer.Active()
		for r := 0; r < s.Rows; r++ {
			var b strings.Builder
			for _, c := range s.Lines[r].Cells {
				if c.Width == 0 {
					continue
				}
				if c.Rune == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(c.Rune)
				}
			}
			if strings.Contains(b.String(), substr) {
				return true
			}
		}
		return false
	}

	waitSessionAttached(t, bin, sock, "app")
	_, _ = ptmx.Write([]byte("echo APP_HISTORY_SEED\n"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !outerHas("APP_HISTORY_SEED") {
		time.Sleep(25 * time.Millisecond)
	}
	if !outerHas("APP_HISTORY_SEED") {
		t.Fatalf("active interactive client did not receive APP_HISTORY_SEED")
	}

	// Start `mplex follow app --history` AND `mplex logs -f app` concurrently.
	followBuf := &lockedBuffer{}
	followCmd := exec.Command(bin, "--socket", sock, "follow", "app", "--history")
	followCmd.Env = cleanEnv
	followCmd.Stdout = followBuf
	if err := followCmd.Start(); err != nil {
		t.Fatalf("start mplex follow: %v", err)
	}
	followDone := make(chan error, 1)
	go func() { followDone <- followCmd.Wait() }()

	logsBuf := &lockedBuffer{}
	logsCmd := exec.Command(bin, "--socket", sock, "logs", "-f", "app")
	logsCmd.Env = cleanEnv
	logsCmd.Stdout = logsBuf
	if err := logsCmd.Start(); err != nil {
		t.Fatalf("start mplex logs -f: %v", err)
	}
	logsDone := make(chan error, 1)
	go func() { logsDone <- logsCmd.Wait() }()

	// Wait until follow --history receives the initial history snapshot (confirms both followers have connected).
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(followBuf.String(), "APP_HISTORY_SEED") {
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(followBuf.String(), "APP_HISTORY_SEED") {
		t.Fatalf("mplex follow --history did not emit initial history; got %q", followBuf.String())
	}

	// Emit live output via the active interactive PTY (also proving it was NOT detached!).
	// Retry once briefly in case logs -f was still finishing its handshake.
	deadline = time.Now().Add(5 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if i == 0 || (i%10 == 0 && !strings.Contains(logsBuf.String(), "LIVE_FOLLOW_TOKEN")) {
			_, _ = ptmx.Write([]byte("echo LIVE_FOLLOW_TOKEN\n"))
		}
		if outerHas("LIVE_FOLLOW_TOKEN") &&
			strings.Contains(followBuf.String(), "LIVE_FOLLOW_TOKEN") &&
			strings.Contains(logsBuf.String(), "LIVE_FOLLOW_TOKEN") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !outerHas("LIVE_FOLLOW_TOKEN") {
		t.Fatalf("active interactive client lost LIVE_FOLLOW_TOKEN (detached?)")
	}
	if !strings.Contains(followBuf.String(), "LIVE_FOLLOW_TOKEN") {
		t.Fatalf("mplex follow did not stream LIVE_FOLLOW_TOKEN; got %q", followBuf.String())
	}
	if !strings.Contains(logsBuf.String(), "LIVE_FOLLOW_TOKEN") {
		t.Fatalf("mplex logs -f did not stream LIVE_FOLLOW_TOKEN; got %q", logsBuf.String())
	}

	// Verify active PTY client is STILL alive and attached!
	select {
	case err := <-activeDone:
		t.Fatalf("active interactive client exited unexpectedly while being followed (err=%v)", err)
	default:
	}

	// Send SIGINT to `mplex logs -f` -> must exit cleanly with code 0!
	if err := logsCmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT to mplex logs -f: %v", err)
	}
	select {
	case err := <-logsDone:
		if err != nil {
			t.Fatalf("mplex logs -f exited non-zero on SIGINT: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("mplex logs -f did not exit after SIGINT")
	}

	// Active interactive client STILL works after follower disconnected!
	_, _ = ptmx.Write([]byte("echo STILL_ATTACHED_AFTER_UNFOLLOW\n"))
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !outerHas("STILL_ATTACHED_AFTER_UNFOLLOW") {
		time.Sleep(25 * time.Millisecond)
	}
	if !outerHas("STILL_ATTACHED_AFTER_UNFOLLOW") {
		t.Fatalf("active interactive client did not receive output after follower disconnected")
	}

	// Exit the session -> `mplex follow` must exit cleanly with code 0!
	_, _ = ptmx.Write([]byte("exit 0\n"))
	select {
	case err := <-followDone:
		if err != nil {
			t.Fatalf("mplex follow exited non-zero on session exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("mplex follow did not exit when session exited")
	}
	// Since "worker" is still alive, activeCmd opens the switcher; press Esc to exit cleanly.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outerMu.Lock()
		inAlt := outer.InAltScreen
		outerMu.Unlock()
		if inAlt {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	_, _ = ptmx.Write([]byte{0x1b})
	select {
	case <-activeDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("active client did not exit after Esc on switcher")
	}

	// 8. Smoke test `mplex mcp` subprocess stdio: strictly valid JSON-RPC 2.0 on stdout!
	mcpCmd := exec.Command(bin, "--socket", sock, "mcp")
	mcpCmd.Env = cleanEnv
	mcpIn := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`,
	}, "\n") + "\n"
	mcpCmd.Stdin = strings.NewReader(mcpIn)
	var mcpOut, mcpErr bytes.Buffer
	mcpCmd.Stdout = &mcpOut
	mcpCmd.Stderr = &mcpErr
	if err := mcpCmd.Run(); err != nil {
		t.Fatalf("mplex mcp failed: %v\nstderr=%s\nstdout=%s", err, mcpErr.String(), mcpOut.String())
	}

	scanner := bufio.NewScanner(&mcpOut)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		var resp map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			t.Fatalf("non-JSON-RPC line %d on mplex mcp stdout: %v\nline=%q", lineCount, err, scanner.Text())
		}
		if resp["jsonrpc"] != "2.0" {
			t.Fatalf("line %d missing jsonrpc=2.0: %v", lineCount, resp)
		}
	}
	if lineCount != 4 {
		t.Fatalf("expected exactly 4 JSON-RPC responses (notification must be silent), got %d:\n%s", lineCount, mcpOut.String())
	}
}

func TestWorkspaceSaveRestoreEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	xdgState := filepath.Join(tmp, "state")
	fakeHome := filepath.Join(tmp, "home")
	dirA := filepath.Join(tmp, "proj-alpha")
	dirB := filepath.Join(tmp, "proj-beta")
	for _, d := range []string{xdgState, fakeHome, dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Resolve symlinks (e.g. /var -> /private/var on macOS) for exact CWD assertions.
	if r, err := filepath.EvalSymlinks(dirA); err == nil {
		dirA = r
	}
	if r, err := filepath.EvalSymlinks(dirB); err == nil {
		dirB = r
	}
	if r, err := filepath.EvalSymlinks(fakeHome); err == nil {
		fakeHome = r
	}

	baseEnv := append(os.Environ(),
		"HOME="+fakeHome,
		"XDG_STATE_HOME="+xdgState,
		"MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"),
		"SHELL=/bin/sh",
		"MPLEX_SESSION=",
		"MPLEX_SESSION_ID=",
		"MPLEX_SOCKET=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)

	runCLI := func(args ...string) []byte {
		t.Helper()
		full := append([]string{"--socket", sock}, args...)
		cmd := exec.Command(bin, full...)
		cmd.Env = baseEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mplex %v failed: %v\n%s", args, err, out)
		}
		return out
	}

	// 1. Create two sessions in distinct directories.
	runCLI("new", "alpha", "-d", "-c", dirA, "--json", "--", "/bin/sh")
	runCLI("new", "beta", "-d", "-c", dirB, "--json", "--", "/bin/sh")

	// Emit output in alpha so --with-scrollback has content to capture.
	runCLI("send", "alpha", "echo SAVED_SCROLLBACK_MARKER", "--enter")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		capOut := runCLI("capture", "alpha")
		if strings.Contains(string(capOut), "SAVED_SCROLLBACK_MARKER") {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	// 2. Save named workspace "work" with scrollback tail.
	saveOut := runCLI("save", "work", "--with-scrollback", "--json")
	var saveResp struct {
		Path string `json:"path"`
		workspace.Snapshot
	}
	if err := json.Unmarshal(saveOut, &saveResp); err != nil {
		t.Fatalf("unmarshal save json: %v\n%s", err, saveOut)
	}
	wantPath := filepath.Join(xdgState, "mplex", "workspaces", "work.json")
	if saveResp.Path != wantPath {
		t.Fatalf("save path = %q, want %q", saveResp.Path, wantPath)
	}
	if saveResp.Version != workspace.CurrentVersion || saveResp.Name != "work" || len(saveResp.Sessions) != 2 {
		t.Fatalf("unexpected saved snapshot: %+v", saveResp.Snapshot)
	}
	foundMarker := false
	for _, s := range saveResp.Sessions {
		if s.Name == "alpha" && strings.Contains(strings.Join(s.ScrollbackTail, "\n"), "SAVED_SCROLLBACK_MARKER") {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatalf("expected SAVED_SCROLLBACK_MARKER in alpha scrollback_tail: %+v", saveResp.Sessions)
	}

	// Also save via explicit --file path.
	explicitFile := filepath.Join(tmp, "custom-snap.json")
	runCLI("save", "--file", explicitFile, "--json")
	if _, err := os.Stat(explicitFile); err != nil {
		t.Fatalf("expected explicit snapshot file at %s: %v", explicitFile, err)
	}

	waitNoSessions := func() {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		for time.Now().Before(dl) {
			out := runCLI("list", "--json")
			var ls []protocol.SessionInfo
			if json.Unmarshal(out, &ls) == nil && len(ls) == 0 {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for all sessions to exit after kill --all")
	}

	// 3. Kill all sessions and wait for them to exit.
	runCLI("kill", "--all", "--json")
	waitNoSessions()

	// 4. Restore workspace "work" and verify both sessions recreated in exact CWDs!
	restoreOut := runCLI("restore", "work", "--json")
	var restoreResp struct {
		Created []string `json:"created"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(restoreOut, &restoreResp); err != nil {
		t.Fatalf("unmarshal restore json: %v\n%s", err, restoreOut)
	}
	if len(restoreResp.Created) != 2 || len(restoreResp.Skipped) != 0 {
		t.Fatalf("unexpected restore response: %+v", restoreResp)
	}

	listOut := runCLI("list", "--json")
	var listed []protocol.SessionInfo
	if err := json.Unmarshal(listOut, &listed); err != nil {
		t.Fatalf("unmarshal list json: %v\n%s", err, listOut)
	}
	byName := make(map[string]protocol.SessionInfo, len(listed))
	for _, s := range listed {
		byName[s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("expected 2 restored sessions, got %d: %+v", len(byName), listed)
	}
	for name, wantDir := range map[string]string{"alpha": dirA, "beta": dirB} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("missing restored session %q", name)
		}
		gotCwd := s.Cwd
		if r, err := filepath.EvalSymlinks(gotCwd); err == nil {
			gotCwd = r
		}
		if gotCwd != wantDir {
			t.Errorf("restored session %q Cwd = %q, want %q", name, gotCwd, wantDir)
		}
		// Verify live shell PWD inside the restored session matches exact CWD.
		runCLI("send", name, "echo LIVE_PWD:$(pwd)", "--enter")
		wantLine := "LIVE_PWD:" + wantDir
		dl := time.Now().Add(5 * time.Second)
		matched := false
		var lastCap string
		for time.Now().Before(dl) {
			out := runCLI("capture", name)
			lastCap = string(out)
			if strings.Contains(lastCap, wantLine) {
				matched = true
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if !matched {
			t.Fatalf("restored session %q live pwd did not match %q:\n%s", name, wantLine, lastCap)
		}
	}

	// 5. Restoring again without --force must skip existing sessions.
	restoreAgainOut := runCLI("restore", "work", "--json")
	restoreResp = struct {
		Created []string `json:"created"`
		Skipped []string `json:"skipped"`
	}{}
	if err := json.Unmarshal(restoreAgainOut, &restoreResp); err != nil {
		t.Fatalf("unmarshal second restore json: %v\n%s", err, restoreAgainOut)
	}
	if len(restoreResp.Created) != 0 || len(restoreResp.Skipped) != 2 {
		t.Fatalf("expected 0 created & 2 skipped, got %+v", restoreResp)
	}

	// 6. Restoring with --force must create deduplicated sessions (alpha-2, beta-2).
	restoreForceOut := runCLI("restore", "work", "--force", "--json")
	restoreResp = struct {
		Created []string `json:"created"`
		Skipped []string `json:"skipped"`
	}{}
	if err := json.Unmarshal(restoreForceOut, &restoreResp); err != nil {
		t.Fatalf("unmarshal force restore json: %v\n%s", err, restoreForceOut)
	}
	if len(restoreResp.Created) != 2 || len(restoreResp.Skipped) != 0 {
		t.Fatalf("expected 2 deduplicated created & 0 skipped, got %+v", restoreResp)
	}

	// 7. Missing CWD falls back to $HOME on restore.
	runCLI("kill", "--all", "--json")
	waitNoSessions()
	if err := os.RemoveAll(dirB); err != nil {
		t.Fatalf("remove dirB: %v", err)
	}
	runCLI("restore", "--file", explicitFile, "--json")
	listOut = runCLI("list", "--json")
	listed = nil
	if err := json.Unmarshal(listOut, &listed); err != nil {
		t.Fatalf("unmarshal list json after missing cwd restore: %v", err)
	}
	for _, s := range listed {
		if s.Name == "beta" {
			gotCwd := s.Cwd
			if r, err := filepath.EvalSymlinks(gotCwd); err == nil {
				gotCwd = r
			}
			if gotCwd != fakeHome {
				t.Errorf("restored session beta with deleted dir got Cwd=%q, want HOME=%q", gotCwd, fakeHome)
			}
		}
	}
}

func TestConfigPrecedenceAndCommandsEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(tmp, "cfg", "config.toml")
	cleanEnv := func(extra ...string) []string {
		env := []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + tmp,
			"MPLEX_CONFIG=" + cfgPath,
		}
		return append(env, extra...)
	}

	// 1. `mplex config --path`
	cmd := exec.Command(bin, "config", "--path")
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config --path failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != cfgPath {
		t.Fatalf("config --path = %q, want %q", strings.TrimSpace(string(out)), cfgPath)
	}

	// 2. `mplex config --init` writes starter config, refuses overwrite without --force.
	cmd = exec.Command(bin, "config", "--init", "--json")
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config --init failed: %v\n%s", err, out)
	}
	cmd = exec.Command(bin, "config", "--init")
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected second config --init without --force to fail, got: %s", out)
	}

	// 3. Write custom config file and verify config file overrides built-in defaults.
	customTOML := `
key = "alt-s"
socket = "/tmp/from-cfg.sock"
scrollback = 5000
replay_lines = 2500
auto_update = false
window_title = false
naming = "numeric"
`
	if err := os.WriteFile(cfgPath, []byte(customTOML), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	readEffective := func(extraEnv []string, cliArgs ...string) effectiveConfig {
		t.Helper()
		args := append(cliArgs, "config", "--json")
		c := exec.Command(bin, args...)
		c.Env = cleanEnv(extraEnv...)
		raw, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("mplex %v failed: %v\n%s", args, err, raw)
		}
		var eff effectiveConfig
		if err := json.Unmarshal(raw, &eff); err != nil {
			t.Fatalf("unmarshal config json: %v\n%s", err, raw)
		}
		return eff
	}

	fromFile := readEffective(nil)
	if !fromFile.ConfigExists || fromFile.Key != "alt-s" || fromFile.Socket != "/tmp/from-cfg.sock" ||
		fromFile.Scrollback != 5000 || fromFile.ReplayLines != 2500 ||
		fromFile.AutoUpdate != false || fromFile.WindowTitle != false || fromFile.Naming != "numeric" {
		t.Fatalf("unexpected config from file: %+v", fromFile)
	}

	// 4. Env vars override config file.
	envOverrides := []string{
		"MPLEX_KEY=ctrl-a",
		"MPLEX_SOCKET=/tmp/from-env.sock",
		"MPLEX_SCROLLBACK=7777",
		"MPLEX_REPLAY_LINES=3333",
		"MPLEX_AUTO_UPDATE=true",
		"MPLEX_WINDOW_TITLE=true",
		"MPLEX_NAMING=cwd",
	}
	fromEnv := readEffective(envOverrides)
	if fromEnv.Key != "ctrl-a" || fromEnv.Socket != "/tmp/from-env.sock" ||
		fromEnv.Scrollback != 7777 || fromEnv.ReplayLines != 3333 ||
		fromEnv.AutoUpdate != true || fromEnv.WindowTitle != true || fromEnv.Naming != "cwd" {
		t.Fatalf("env did not override config file: %+v", fromEnv)
	}

	// 5. CLI flags override env vars!
	fromCLI := readEffective(envOverrides,
		"--key", "ctrl-]",
		"--socket", "/tmp/from-cli.sock",
		"--scrollback", "9999",
		"--replay-lines", "4444",
		"--window-title=false",
		"--naming", "numeric",
	)
	if fromCLI.Key != "ctrl-]" || fromCLI.Socket != "/tmp/from-cli.sock" ||
		fromCLI.Scrollback != 9999 || fromCLI.ReplayLines != 4444 ||
		fromCLI.WindowTitle != false || fromCLI.Naming != "numeric" {
		t.Fatalf("CLI flags did not override env vars: %+v", fromCLI)
	}

	// 6. `--init --force` recovers even from a malformed config file.
	if err := os.WriteFile(cfgPath, []byte("scrollback = invalid\n"), 0o644); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	cmd = exec.Command(bin, "config", "--json")
	cmd.Env = cleanEnv()
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected error on malformed config file")
	}
	cmd = exec.Command(bin, "config", "--init", "--force")
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config --init --force failed: %v\n%s", err, out)
	}
	recovered := readEffective(nil)
	if recovered.Key != `ctrl-\` || recovered.Scrollback != 10000 {
		t.Fatalf("unexpected recovered config: %+v", recovered)
	}
}

func TestCompletionAndDynamicSessionCompletionEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	env := append(os.Environ(),
		"MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"),
		"SHELL=/bin/sh",
		"MPLEX_SESSION=",
		"MPLEX_SESSION_ID=",
		"MPLEX_SOCKET=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)

	// 1. `mplex completion <fish|zsh|bash>` succeeds; unknown shell fails.
	for _, sh := range []string{"fish", "zsh", "bash"} {
		cmd := exec.Command(bin, "completion", sh)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mplex completion %s failed: %v\n%s", sh, err, out)
		}
		if !strings.Contains(string(out), "__complete-sessions") {
			t.Fatalf("mplex completion %s missing __complete-sessions", sh)
		}
	}
	badCmd := exec.Command(bin, "completion", "tcsh")
	badCmd.Env = env
	if _, err := badCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected mplex completion tcsh to fail")
	}

	// 2. `__complete-sessions` when daemon is NOT running exits 0 silently with empty output.
	start := time.Now()
	emptyCmd := exec.Command(bin, "--socket", sock, "__complete-sessions")
	emptyCmd.Env = env
	out, err := emptyCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("__complete-sessions without daemon exited non-zero: %v\n%s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty output without daemon, got %q", string(out))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("__complete-sessions without daemon took too long: %v", elapsed)
	}

	// 3. Start two sessions and verify plain and `--describe` output.
	for _, name := range []string{"alpha", "beta"} {
		mk := exec.Command(bin, "--socket", sock, "new", name, "-d", "--json", "--", "/bin/sh")
		mk.Env = env
		if out, err := mk.CombinedOutput(); err != nil {
			t.Fatalf("new %s failed: %v\n%s", name, err, out)
		}
	}

	plainCmd := exec.Command(bin, "--socket", sock, "__complete-sessions")
	plainCmd.Env = env
	plainOut, err := plainCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("__complete-sessions failed: %v\n%s", err, plainOut)
	}
	lines := strings.Split(strings.TrimSpace(string(plainOut)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 session completion lines, got %q", string(plainOut))
	}
	if !strings.Contains(string(plainOut), "alpha") || !strings.Contains(string(plainOut), "beta") {
		t.Fatalf("unexpected completion output: %q", string(plainOut))
	}

	descCmd := exec.Command(bin, "--socket", sock, "__complete-sessions", "--describe")
	descCmd.Env = env
	descOut, err := descCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("__complete-sessions --describe failed: %v\n%s", err, descOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(descOut)), "\n") {
		if !strings.Contains(line, "\t") {
			t.Errorf("expected tab-separated description in %q", line)
		}
	}
}

func TestNumericNamingAndWindowTitleDisabled(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)
	projDir := filepath.Join(tmp, "myrepo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	// Verify MPLEX_NAMING=numeric creates "main" and "1" regardless of cwd basename.
	mk1 := exec.Command(bin, "--socket", sock, "new", "-d", "--json", "--", "/bin/sh", "-c", "sleep 30")
	mk1.Dir = projDir
	mk1.Env = append(os.Environ(), "MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"), "MPLEX_NAMING=numeric", "MPLEX_NO_UPDATE_CHECK=1")
	out1, err := mk1.CombinedOutput()
	if err != nil {
		t.Fatalf("new numeric 1 failed: %v\n%s", err, out1)
	}
	var s1 protocol.SessionInfo
	if err := json.Unmarshal(out1, &s1); err != nil || s1.Name != "main" {
		t.Fatalf("expected numeric session 'main', got %+v (err=%v)", s1, err)
	}

	mk2 := exec.Command(bin, "--socket", sock, "--naming=numeric", "new", "-d", "--json", "--", "/bin/sh", "-c", "sleep 30")
	mk2.Dir = projDir
	mk2.Env = append(os.Environ(), "MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"), "MPLEX_NAMING=", "MPLEX_NO_UPDATE_CHECK=1")
	out2, err := mk2.CombinedOutput()
	if err != nil {
		t.Fatalf("new numeric 2 failed: %v\n%s", err, out2)
	}
	var s2 protocol.SessionInfo
	if err := json.Unmarshal(out2, &s2); err != nil || s2.Name != "1" {
		t.Fatalf("expected numeric session '1', got %+v (err=%v)", s2, err)
	}

	// Verify --window-title=false does not overwrite the outer terminal's existing title.
	cmd := exec.Command(bin, "--socket", sock, "--window-title=false", "attach", "main")
	cmd.Dir = projDir
	cmd.Env = append(os.Environ(),
		"MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"),
		"SHELL=/bin/sh",
		"TERM=xterm-256color",
		"MPLEX_SESSION=",
		"MPLEX_SOCKET=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 90})
	if err != nil {
		t.Fatalf("pty.StartWithSize: %v", err)
	}
	defer ptmx.Close()

	var mu sync.Mutex
	outer := vt.New(24, 90, 500)
	outer.Title = "KEEP_ME"
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				outer.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	waitSessionAttached(t, bin, sock, "main")
	// Detach cleanly via external CLI.
	_ = exec.Command(bin, "--socket", sock, "detach").Run()
	_ = cmd.Wait()

	mu.Lock()
	gotTitle := outer.Title
	mu.Unlock()
	if gotTitle != "KEEP_ME" {
		t.Fatalf("expected outer title unchanged ('KEEP_ME') with --window-title=false, got %q", gotTitle)
	}
}

func TestStolenSessionDropsOldClientIntoMenu(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	type ptyWin struct {
		cmd    *exec.Cmd
		ptmx   *os.File
		mu     sync.Mutex
		outer  *vt.Terminal
		doneCh chan error
	}

	launch := func(shellPath string, extraEnv []string, args ...string) *ptyWin {
		t.Helper()
		fullArgs := append([]string{"--socket", sock}, args...)
		cmd := exec.Command(bin, fullArgs...)
		env := append(os.Environ(),
			"SHELL="+shellPath,
			"PS1=P$ ",
			"TERM=xterm-256color",
			"MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"),
			"MPLEX_SESSION=",
			"MPLEX_SESSION_ID=",
			"MPLEX_SOCKET=",
			"MPLEX_WINDOW_TITLE=1",
			"MPLEX_NO_UPDATE_CHECK=1",
		)
		env = append(env, extraEnv...)
		cmd.Env = env
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 110})
		if err != nil {
			t.Fatalf("pty.StartWithSize: %v", err)
		}
		w := &ptyWin{
			cmd:    cmd,
			ptmx:   ptmx,
			outer:  vt.New(24, 110, 1000),
			doneCh: make(chan error, 1),
		}
		// Simulate an outer terminal emulator replying to DA1/CPR queries (needed by fish 4).
		w.outer.OnQueryReply = func(reply []byte) {
			_, _ = ptmx.Write(reply)
		}
		go func() {
			buf := make([]byte, 8192)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					w.mu.Lock()
					w.outer.Write(buf[:n])
					w.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
		go func() {
			w.doneCh <- cmd.Wait()
		}()
		t.Cleanup(func() {
			_ = ptmx.Close()
		})
		return w
	}

	screenOf := func(w *ptyWin) string {
		w.mu.Lock()
		defer w.mu.Unlock()
		s := w.outer.Active()
		var b strings.Builder
		for r := 0; r < s.Rows; r++ {
			for _, c := range s.Lines[r].Cells {
				if c.Width == 0 {
					continue
				}
				if c.Rune == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(c.Rune)
				}
			}
			b.WriteByte('\n')
		}
		return b.String()
	}

	inAltOf := func(w *ptyWin) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.outer.InAltScreen
	}

	titleOf := func(w *ptyWin) string {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.outer.Title
	}

	waitCond := func(w *ptyWin, desc string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s\n--- SCREEN ---\n%s", desc, screenOf(w))
	}

	// 1. Window 1 starts mplex and creates session "foo".
	win1 := launch("/bin/sh", nil, "new", "foo")
	waitSessionAttached(t, bin, sock, "foo")
	_, _ = win1.ptmx.Write([]byte("echo WIN1_IN_FOO:$MPLEX_SESSION\n"))
	waitCond(win1, "win1 in foo", func() bool {
		return !inAltOf(win1) && strings.Contains(screenOf(win1), "WIN1_IN_FOO:foo")
	})

	// 2. Window 2 connects to "foo" (stealing it from Window 1) and writes to it!
	win2 := launch("/bin/sh", nil, "attach", "foo")
	waitCond(win2, "win2 attached to foo with replay", func() bool {
		return !inAltOf(win2) && strings.Contains(screenOf(win2), "WIN1_IN_FOO:foo")
	})
	_, _ = win2.ptmx.Write([]byte("echo WIN2_WROTE_FOO\n"))
	waitCond(win2, "win2 output in foo", func() bool {
		return strings.Contains(screenOf(win2), "WIN2_WROTE_FOO")
	})

	// 3. Window 1 MUST NOT exit or freeze — it must drop cleanly into the mplex menu
	// with a notice explaining that "foo" was attached by another client!
	select {
	case err := <-win1.doneCh:
		t.Fatalf("win1 exited instead of dropping into mplex menu (err=%v)", err)
	default:
	}
	waitCond(win1, "win1 dropped into mplex menu with notice", func() bool {
		txt := screenOf(win1)
		return inAltOf(win1) &&
			strings.Contains(txt, "mplex") &&
			strings.Contains(txt, `Session "foo" attached by another client`) &&
			strings.Contains(txt, "+ New session") &&
			titleOf(win1) == ""
	})

	// 4. Verify Window 1 is responsive in the menu: pressing Enter creates a fresh session!
	_, _ = win1.ptmx.Write([]byte{'\r'})
	waitCond(win1, "win1 entered new session from menu", func() bool {
		return !inAltOf(win1)
	})
	_, _ = win1.ptmx.Write([]byte("echo WIN1_NEW_ALIVE:$MPLEX_SESSION\n"))
	waitCond(win1, "win1 typing works in new session", func() bool {
		return strings.Contains(screenOf(win1), "WIN1_NEW_ALIVE:") && !strings.Contains(screenOf(win1), "WIN1_NEW_ALIVE:foo")
	})

	// 5. Verify running `mplex new bar` INSIDE Window 1 switches Window 1 directly to "bar"
	// instead of accidentally nesting a second mplex client!
	_, _ = win1.ptmx.Write([]byte(bin + " new bar\n"))
	waitSessionAttached(t, bin, sock, "bar")
	waitCond(win1, "win1 switched to bar without nesting", func() bool {
		return !inAltOf(win1) && titleOf(win1) == "[bar]"
	})
	_, _ = win1.ptmx.Write([]byte("echo IN_BAR:$MPLEX_SESSION\n"))
	waitCond(win1, "bar responsive", func() bool {
		return strings.Contains(screenOf(win1), "IN_BAR:bar")
	})

	// 6. Test stealing while Window 1 ALREADY has the switcher menu open!
	_, _ = win1.ptmx.Write([]byte{0x1c}) // Ctrl-\
	waitCond(win1, "win1 opened switcher", func() bool {
		return inAltOf(win1) && strings.Contains(screenOf(win1), "bar")
	})
	// Window 2 steals "bar" while Window 1 is in the switcher!
	// First detach win2 by switching win2 via Ctrl-\ -> filter "bar" -> Enter.
	_, _ = win2.ptmx.Write([]byte{0x1c})
	waitCond(win2, "win2 opened switcher", func() bool {
		return inAltOf(win2) && strings.Contains(screenOf(win2), "bar")
	})
	_, _ = win2.ptmx.Write([]byte("bar\r"))
	waitCond(win2, "win2 stole bar", func() bool {
		return !inAltOf(win2) && strings.Contains(screenOf(win2), "IN_BAR:bar")
	})
	// Window 1 (already in switcher) must update in place with the stolen notice,
	// and pressing Esc must exit cleanly instead of freezing!
	waitCond(win1, "win1 switcher updated live with stolen notice", func() bool {
		return inAltOf(win1) && strings.Contains(screenOf(win1), `Session "bar" attached by another client`)
	})
	_, _ = win1.ptmx.Write([]byte{0x1b}) // Esc -> exit cleanly
	select {
	case err := <-win1.doneCh:
		if err != nil {
			t.Fatalf("win1 exited with error on Esc after steal: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("win1 froze on Esc after session was stolen while switcher was open")
	}

	// Cleanly exit win2 via external detach.
	_ = exec.Command(bin, "--socket", sock, "detach").Run()
	select {
	case err := <-win2.doneCh:
		if err != nil {
			t.Fatalf("win2 exited with error on detach: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("win2 did not exit on detach")
	}

	// 7. If fish is installed, run the exact fish steal-and-write scenario end-to-end!
	if fishPath, err := exec.LookPath("fish"); err == nil {
		t.Run("fish", func(t *testing.T) {
			fakeHome := filepath.Join(tmp, "fish-home")
			if err := os.MkdirAll(fakeHome, 0o755); err != nil {
				t.Fatalf("mkdir fakeHome: %v", err)
			}
			extra := []string{"HOME=" + fakeHome}

			f1 := launch(fishPath, extra, "new", "fishfoo")
			waitSessionAttached(t, bin, sock, "fishfoo")
			_, _ = f1.ptmx.Write([]byte("echo FISH_ONE:$MPLEX_SESSION\r"))
			waitCond(f1, "fish win1 output", func() bool {
				return !inAltOf(f1) && strings.Contains(screenOf(f1), "FISH_ONE:fishfoo")
			})

			f2 := launch(fishPath, extra, "attach", "fishfoo")
			waitCond(f2, "fish win2 attached", func() bool {
				return !inAltOf(f2) && strings.Contains(screenOf(f2), "FISH_ONE:fishfoo")
			})
			_, _ = f2.ptmx.Write([]byte("echo FISH_TWO_WRITE\r"))
			waitCond(f2, "fish win2 wrote to fishfoo", func() bool {
				return strings.Contains(screenOf(f2), "FISH_TWO_WRITE")
			})

			// f1 must drop cleanly into the mplex menu and be fully interactive (not frozen!).
			waitCond(f1, "fish win1 dropped into mplex menu", func() bool {
				txt := screenOf(f1)
				return inAltOf(f1) &&
					strings.Contains(txt, `Session "fishfoo" attached by another client`) &&
					strings.Contains(txt, "+ New session")
			})

			// Create a new fish session directly from f1's menu and type in it!
			_, _ = f1.ptmx.Write([]byte{'\r'})
			waitCond(f1, "fish win1 created new session", func() bool {
				return !inAltOf(f1)
			})
			_, _ = f1.ptmx.Write([]byte("echo FISH_NEW_OK:$MPLEX_SESSION\r"))
			waitCond(f1, "fish win1 new session interactive", func() bool {
				return strings.Contains(screenOf(f1), "FISH_NEW_OK:")
			})

			// f2 is still live in fishfoo, and stealing fishfoo back from f1 drops f2 into the menu!
			_, _ = f2.ptmx.Write([]byte("echo FISH_TWO_STILL_OK:$MPLEX_SESSION\r"))
			waitCond(f2, "fish win2 still interactive", func() bool {
				return strings.Contains(screenOf(f2), "FISH_TWO_STILL_OK:fishfoo")
			})

			_, _ = f1.ptmx.Write([]byte{0x1c})
			waitCond(f1, "fish win1 opened switcher", func() bool {
				return inAltOf(f1) && strings.Contains(screenOf(f1), "fishfoo")
			})
			_, _ = f1.ptmx.Write([]byte("fishfoo\r"))
			waitCond(f1, "fish win1 stole fishfoo back", func() bool {
				return !inAltOf(f1) && strings.Contains(screenOf(f1), "FISH_TWO_STILL_OK:fishfoo")
			})
			waitCond(f2, "fish win2 dropped into mplex menu on ping-pong steal", func() bool {
				txt := screenOf(f2)
				return inAltOf(f2) && strings.Contains(txt, `Session "fishfoo" attached by another client`)
			})

			// Pressing Esc in f2's menu exits cleanly; Ctrl+\ -> Ctrl+Q in f1 detaches cleanly.
			_, _ = f2.ptmx.Write([]byte{0x1b})
			select {
			case err := <-f2.doneCh:
				if err != nil {
					t.Fatalf("fish win2 exited with error on Esc: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("fish win2 did not exit cleanly on Esc")
			}

			_, _ = f1.ptmx.Write([]byte{0x1c})
			waitCond(f1, "fish win1 switcher open for detach", func() bool {
				return inAltOf(f1)
			})
			_, _ = f1.ptmx.Write([]byte{0x11}) // Ctrl+Q
			select {
			case err := <-f1.doneCh:
				if err != nil {
					t.Fatalf("fish win1 exited with error on Ctrl+Q: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("fish win1 did not exit cleanly on Ctrl+Q")
			}
		})
	}
}

func TestStatusCommandInsideAndOutsideSession(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	cleanEnv := append(os.Environ(),
		"MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"),
		"MPLEX_SESSION=",
		"MPLEX_SESSION_ID=",
		"MPLEX_SOCKET=",
		"MPLEX_NO_UPDATE_CHECK=1",
	)

	// 1. Outside a session: exits 1, reports not in session, and -q prints nothing.
	cmdOut := exec.Command(bin, "--socket", sock, "status")
	cmdOut.Env = cleanEnv
	out, err := cmdOut.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit outside session, got 0 (out=%q)", out)
	}
	if !strings.Contains(string(out), "Not in an mplex session") || strings.Contains(string(out), "mplex:") {
		t.Fatalf("unexpected outside status output: %q", out)
	}

	cmdQuiet := exec.Command(bin, "--socket", sock, "status", "-q")
	cmdQuiet.Env = cleanEnv
	if qOut, qErr := cmdQuiet.CombinedOutput(); qErr == nil || len(qOut) != 0 {
		t.Fatalf("expected silent exit 1 for status -q outside session, got err=%v out=%q", qErr, qOut)
	}

	cmdJSONOut := exec.Command(bin, "--socket", sock, "status", "--json")
	cmdJSONOut.Env = cleanEnv
	jOut, jErr := cmdJSONOut.CombinedOutput()
	if jErr == nil {
		t.Fatalf("expected exit 1 for status --json outside session")
	}
	var stOut struct {
		InSession bool `json:"in_session"`
		Stale     bool `json:"stale"`
	}
	if err := json.Unmarshal(jOut, &stOut); err != nil || stOut.InSession || stOut.Stale {
		t.Fatalf("unexpected outside status JSON: %s (err=%v)", jOut, err)
	}

	// 2. Stale MPLEX_SESSION env pointing at non-existent daemon -> exit 1 + stale=true.
	cmdStale := exec.Command(bin, "--socket", sock, "status", "--json")
	cmdStale.Env = append(cleanEnv, "MPLEX_SESSION=ghost", "MPLEX_SESSION_ID=s99", "MPLEX_SOCKET="+sock)
	staleOut, staleErr := cmdStale.CombinedOutput()
	if staleErr == nil {
		t.Fatalf("expected exit 1 for stale MPLEX_SESSION")
	}
	if err := json.Unmarshal(staleOut, &stOut); err != nil || stOut.InSession || !stOut.Stale {
		t.Fatalf("expected stale=true in_session=false, got: %s (err=%v)", staleOut, err)
	}

	// 3. Create a live session and verify `mplex status` inside it (including after rename!).
	mk := exec.Command(bin, "--socket", sock, "new", "alpha", "-d", "--json", "--", "/bin/sh")
	mk.Env = cleanEnv
	mkOut, err := mk.CombinedOutput()
	if err != nil {
		t.Fatalf("create alpha: %v\n%s", err, mkOut)
	}
	var created protocol.SessionInfo
	if err := json.Unmarshal(mkOut, &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}

	// Simulate running inside the session (same MPLEX_SESSION / MPLEX_SESSION_ID / MPLEX_SOCKET).
	inEnv := append(cleanEnv,
		"MPLEX_SESSION="+created.Name,
		"MPLEX_SESSION_ID="+created.ID,
		"MPLEX_SOCKET="+sock,
	)

	cmdIn := exec.Command(bin, "status")
	cmdIn.Env = inEnv
	inOut, err := cmdIn.CombinedOutput()
	if err != nil {
		t.Fatalf("status inside session failed: %v\n%s", err, inOut)
	}
	if !strings.Contains(string(inOut), `In mplex session "alpha"`) || !strings.Contains(string(inOut), created.ID) {
		t.Fatalf("unexpected inside status output: %q", inOut)
	}

	cmdQuietIn := exec.Command(bin, "status", "-q")
	cmdQuietIn.Env = inEnv
	if qOut, qErr := cmdQuietIn.CombinedOutput(); qErr != nil || len(qOut) != 0 {
		t.Fatalf("expected silent exit 0 for status -q inside session, got err=%v out=%q", qErr, qOut)
	}

	// Rename session to "beta" -> `mplex status --name` & `--json` must report live name "beta"!
	ren := exec.Command(bin, "--socket", sock, "rename", "alpha", "beta")
	ren.Env = cleanEnv
	if out, err := ren.CombinedOutput(); err != nil {
		t.Fatalf("rename failed: %v\n%s", err, out)
	}

	cmdName := exec.Command(bin, "whoami", "--name")
	cmdName.Env = inEnv
	nameOut, err := cmdName.CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --name failed: %v\n%s", err, nameOut)
	}
	if strings.TrimSpace(string(nameOut)) != "beta" {
		t.Fatalf("expected live renamed name 'beta', got %q", nameOut)
	}

	cmdJSONIn := exec.Command(bin, "status", "--json")
	cmdJSONIn.Env = inEnv
	jInOut, err := cmdJSONIn.CombinedOutput()
	if err != nil {
		t.Fatalf("status --json inside session failed: %v\n%s", err, jInOut)
	}
	var stIn struct {
		InSession   bool                  `json:"in_session"`
		SessionName string                `json:"session_name"`
		SessionID   string                `json:"session_id"`
		Socket      string                `json:"socket"`
		Session     *protocol.SessionInfo `json:"session"`
	}
	if err := json.Unmarshal(jInOut, &stIn); err != nil {
		t.Fatalf("unmarshal status json: %v\n%s", err, jInOut)
	}
	if !stIn.InSession || stIn.SessionName != "beta" || stIn.SessionID != created.ID || stIn.Socket != sock || stIn.Session == nil {
		t.Fatalf("unexpected inside status json: %+v", stIn)
	}
}

func TestExitingFinalShellWithBackgroundJobDoesNotHang(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := shortTempSock(t)

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	defer func() {
		_ = exec.Command(bin, "--socket", sock, "kill", "--all").Run()
	}()

	runShellExit := func(t *testing.T, shellPath, bgCmd string) {
		t.Helper()
		fakeHome := filepath.Join(tmp, "home-"+filepath.Base(shellPath))
		if err := os.MkdirAll(fakeHome, 0o755); err != nil {
			t.Fatalf("mkdir home: %v", err)
		}
		cmd := exec.Command(bin, "--socket", sock, "new", "solo-"+filepath.Base(shellPath))
		cmd.Env = append(os.Environ(),
			"HOME="+fakeHome,
			"SHELL="+shellPath,
			"PS1=P$ ",
			"TERM=xterm-256color",
			"MPLEX_CONFIG="+filepath.Join(tmp, "no-config.toml"),
			"MPLEX_SESSION=",
			"MPLEX_SESSION_ID=",
			"MPLEX_SOCKET=",
			"MPLEX_NO_UPDATE_CHECK=1",
		)
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 90})
		if err != nil {
			t.Fatalf("pty.StartWithSize: %v", err)
		}
		defer ptmx.Close()

		var mu sync.Mutex
		outer := vt.New(24, 90, 500)
		outer.OnQueryReply = func(reply []byte) {
			_, _ = ptmx.Write(reply)
		}
		go func() {
			buf := make([]byte, 8192)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					mu.Lock()
					outer.Write(buf[:n])
					mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()

		waitSessionAttached(t, bin, sock, "solo-"+filepath.Base(shellPath))
		_, _ = ptmx.Write([]byte(bgCmd + "\r"))
		time.Sleep(150 * time.Millisecond)
		_, _ = ptmx.Write([]byte("exit\r"))

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()
		select {
		case err := <-doneCh:
			if err != nil {
				t.Fatalf("mplex exited with error after shell exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("mplex hung after exiting final %s shell with background job", shellPath)
		}
	}

	t.Run("sh", func(t *testing.T) {
		runShellExit(t, "/bin/sh", "sleep 30 &")
	})
	if fishPath, err := exec.LookPath("fish"); err == nil {
		t.Run("fish", func(t *testing.T) {
			runShellExit(t, fishPath, "sleep 30 &; disown")
		})
	}
}
