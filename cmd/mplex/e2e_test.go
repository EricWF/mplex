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
