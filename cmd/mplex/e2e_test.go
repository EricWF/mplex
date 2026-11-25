package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

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
		"MPLEX_NAMING=git",
	}
	fromEnv := readEffective(envOverrides)
	if fromEnv.Key != "ctrl-a" || fromEnv.Socket != "/tmp/from-env.sock" ||
		fromEnv.Scrollback != 7777 || fromEnv.ReplayLines != 3333 ||
		fromEnv.AutoUpdate != true || fromEnv.WindowTitle != true || fromEnv.Naming != "git" {
		t.Fatalf("env did not override config file: %+v", fromEnv)
	}

	// 5. CLI flags override env vars!
	fromCLI := readEffective(envOverrides,
		"--key", "ctrl-]",
		"--socket", "/tmp/from-cli.sock",
		"--scrollback", "9999",
		"--replay-lines", "4444",
	)
	if fromCLI.Key != "ctrl-]" || fromCLI.Socket != "/tmp/from-cli.sock" ||
		fromCLI.Scrollback != 9999 || fromCLI.ReplayLines != 4444 {
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
