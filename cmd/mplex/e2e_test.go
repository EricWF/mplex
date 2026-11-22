package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/EricWF/mplex/internal/vt"
)

func TestFullInteractiveCLIInRealPTY(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "mplex")
	sock := filepath.Join(tmp, "mplex.sock")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
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
	_, _ = ptmx.Write([]byte("i=1; while [ $i -le 40 ]; do echo MAIN_SCROLL_$i; i=$((i+1)); done; echo MAIN_DONE\n"))
	waitCond("main session output + native scrollback", func() bool {
		mu.Lock()
		inAlt := outer.InAltScreen
		mu.Unlock()
		return !inAlt && strings.Contains(screenText(), "MAIN_DONE") && strings.Contains(scrollbackText(), "MAIN_SCROLL_1")
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
