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

	// Launch real `mplex` inside a real 24x90 PTY!
	cmd := exec.Command(bin, "--socket", sock)
	cmd.Env = append(os.Environ(),
		"SHELL=/bin/sh",
		"PS1=TESTPROMPT$ ",
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

	// 1. Verify initial shell starts on main screen buffer and runs commands natively!
	waitSessionAttached(t, bin, sock, "main")
	_, _ = ptmx.Write([]byte("i=1; while [ $i -le 40 ]; do echo MAIN_SCROLL_$i; i=$((i+1)); done; echo MAIN_DONE:$MPLEX_SESSION\n"))
	waitCond("main session output + native scrollback", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		return !inAlt && strings.Contains(screenText(), "MAIN_DONE:main") && strings.Contains(scrollbackText(), "MAIN_SCROLL_1")
	})

	// 2. Press Ctrl+\ (0x1c) to open the fzf-style switcher!
	_, _ = ptmx.Write([]byte{0x1c})
	waitCond("switcher UI visible on alt screen", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		txt := screenText()
		return inAlt && strings.Contains(txt, "mplex") && strings.Contains(txt, "main")
	})

	// 3. Type "api" and hit Enter to create and switch to a brand-new session "api"!
	_, _ = ptmx.Write([]byte("api"))
	waitCond("create new session 'api' option highlighted", func() bool {
		return strings.Contains(screenText(), `Create new session "api"`)
	})
	_, _ = ptmx.Write([]byte{'\r'})

	// Wait until we are back on main screen in the fresh "api" session!
	waitCond("switched to fresh api session", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		sbLen := len(outer.Main.Scrollback)
		mu.Unlock()
		return !inAlt && sbLen == 0 && !strings.Contains(screenText(), "MAIN_DONE")
	})

	// Run a command inside "api" to verify it's an independent live shell!
	_, _ = ptmx.Write([]byte("echo INSIDE_API_SESSION:$MPLEX_SESSION\n"))
	waitCond("api session command output", func() bool {
		return strings.Contains(screenText(), "INSIDE_API_SESSION:api")
	})

	// 4. Press Ctrl+\ then Tab to quick-toggle back to "main"!
	_, _ = ptmx.Write([]byte{0x1c})
	waitCond("switcher reopened", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		return inAlt && strings.Contains(screenText(), "api") && strings.Contains(screenText(), "main")
	})
	_, _ = ptmx.Write([]byte{'\t'})

	// Verify we are back in "main" AND outer terminal's NATIVE scrollback has MAIN_SCROLL_1!
	waitCond("quick-toggled back to main with native scrollback restored", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		return !inAlt && strings.Contains(screenText(), "MAIN_DONE") && strings.Contains(scrollbackText(), "MAIN_SCROLL_1")
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

	// Wait for all final output bytes (including ResetOuterModes) to drain from the PTY!
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("PTY reader did not drain after mplex exit")
	}

	// Verify outer terminal was NOT left stuck in alt-screen or mouse mode!
	mu.Lock()
	if outer.InAltScreen {
		t.Fatalf("outer terminal left in alt screen after detach!")
	}
	if outer.MouseMode != 0 {
		t.Fatalf("outer terminal left in mouse mode %d after detach!", outer.MouseMode)
	}
	mu.Unlock()

	// 6. Verify both sessions are still alive via `mplex list`!
	listOut, err := exec.Command(bin, "--socket", sock, "list").CombinedOutput()
	if err != nil {
		t.Fatalf("mplex list failed: %v\n%s", err, listOut)
	}
	ls := string(listOut)
	if !strings.Contains(ls, "main") || !strings.Contains(ls, "api") {
		t.Fatalf("expected both 'main' and 'api' sessions alive after detach:\n%s", ls)
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

	launchWindow := func(args ...string) *ptyWindow {
		t.Helper()
		fullArgs := append([]string{"--socket", sock}, args...)
		cmd := exec.Command(bin, fullArgs...)
		cmd.Env = append(os.Environ(),
			"SHELL=/bin/sh",
			"PS1=W$ ",
			"TERM=xterm-256color",
			"MPLEX_SESSION=",
			"MPLEX_SOCKET=",
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

	// 1. Window 1 starts mplex with no existing sessions -> creates & attaches to "main".
	win1 := launchWindow()
	waitSessionAttached(t, bin, sock, "main")
	_, _ = win1.ptmx.Write([]byte("echo WIN1_READY:$MPLEX_SESSION\n"))
	waitCond(win1, "win1 main session ready", func() bool {
		return !inAltOf(win1) && strings.Contains(screenOf(win1), "WIN1_READY:main")
	})

	// 2. Window 2 runs plain `mplex` -> must open session creation/switcher screen with
	// "+ New session" selected, NOT steal "main" from Window 1!
	win2 := launchWindow()
	waitCond(win2, "win2 creation/switcher screen open", func() bool {
		txt := screenOf(win2)
		return inAltOf(win2) &&
			strings.Contains(txt, "New Session") &&
			strings.Contains(txt, "+ New session") &&
			strings.Contains(txt, "main")
	})

	// Verify Window 1 was NOT detached or killed!
	select {
	case err := <-win1.doneCh:
		t.Fatalf("Window 1 exited unexpectedly when Window 2 ran mplex (err=%v)", err)
	default:
	}

	// 3. Pressing Enter in Window 2 immediately creates a brand-new session (because
	// "+ New session" is pre-selected)!
	_, _ = win2.ptmx.Write([]byte{'\r'})
	waitCond(win2, "win2 entered new session", func() bool {
		return !inAltOf(win2)
	})
	_, _ = win2.ptmx.Write([]byte("echo WIN2_READY:$MPLEX_SESSION\n"))
	waitCond(win2, "win2 command output", func() bool {
		return strings.Contains(screenOf(win2), "WIN2_READY:1")
	})

	// 4. Verify Window 1 is STILL independently live in "main"!
	_, _ = win1.ptmx.Write([]byte("echo WIN1_STILL_ALIVE:$MPLEX_SESSION\n"))
	waitCond(win1, "win1 still live in main", func() bool {
		return strings.Contains(screenOf(win1), "WIN1_STILL_ALIVE:main")
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
