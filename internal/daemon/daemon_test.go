package daemon_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/daemon"
	"github.com/EricWF/mplex/internal/protocol"
	"github.com/EricWF/mplex/internal/vt"
)

// outerTermSink simulates a real outer terminal emulator (Ghostty/Kitty/Alacritty)
// receiving raw bytes from mplex over the client stdout stream.
type outerTermSink struct {
	mu   sync.Mutex
	term *vt.Terminal
}

func newOuterTermSink(rows, cols int) *outerTermSink {
	return &outerTermSink{term: vt.New(rows, cols, 5000)}
}

func (o *outerTermSink) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.term.Write(p)
	o.mu.Unlock()
	return len(p), nil
}

func (o *outerTermSink) screenContains(substr string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.term.Active()
	for r := 0; r < s.Rows; r++ {
		if strings.Contains(rowString(s.Lines[r]), substr) {
			return true
		}
	}
	return false
}

func (o *outerTermSink) scrollbackContains(substr string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, l := range o.term.Main.Scrollback {
		if strings.Contains(rowString(l), substr) {
			return true
		}
	}
	return false
}

func rowString(l vt.Line) string {
	var b strings.Builder
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
	return strings.TrimRight(b.String(), " ")
}

func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

func TestEndToEndDaemonSwitchRestoresScrollbackAndAltScreenModes(t *testing.T) {
	// Use a short /tmp dir so macOS Unix socket paths stay well under the 104-byte sun_path limit.
	sockDir, err := os.MkdirTemp("/tmp", "mplex-dt-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "m.sock")
	d := daemon.New(sock)
	go func() {
		_ = d.Start()
	}()
	defer d.Close()

	// Wait for daemon listener to bind.
	var rc *client.RPCConn
	outer := newOuterTermSink(12, 60)
	waitFor(t, 2*time.Second, "daemon socket", func() bool {
		c, err := client.Dial(sock, outer)
		if err != nil {
			return false
		}
		rc = c
		return true
	})
	defer rc.Close()

	sz := protocol.WinSize{Rows: 12, Cols: 60}
	if _, err := rc.Hello(protocol.HelloReq{Size: sz, Term: "xterm-256color", Interactive: true}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// Create Session A: emits 60 lines (populates native scrollback) and stays alive.
	sessA, err := rc.Create(protocol.CreateReq{
		Name: "logs",
		Cmd:  []string{"/bin/sh", "-c", `i=1; while [ $i -le 60 ]; do printf "LOG_ROW_%02d\n" "$i"; i=$((i+1)); done; printf "READY_A\n"; exec cat`},
		Size: sz,
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}

	// Create Session B: enters alt-screen, enables mouse + Kitty keyboard + draws TUI.
	sessB, err := rc.Create(protocol.CreateReq{
		Name: "editor",
		Cmd:  []string{"/bin/sh", "-c", `printf "BEFORE_ALT_B\n\033[?1049h\033[?1000h\033[?1006h\033[>5u\033[2;3HEDITOR_TUI_ACTIVE"; exec cat`},
		Size: sz,
	})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Attach to Session A and wait for all 60 lines + READY_A.
	if _, err := rc.Attach(protocol.AttachReq{Target: sessA.ID, Size: sz, ReplayLines: 500}); err != nil {
		t.Fatalf("attach A: %v", err)
	}
	waitFor(t, 3*time.Second, "Session A output", func() bool {
		return outer.screenContains("READY_A") && outer.scrollbackContains("LOG_ROW_01")
	})

	// Switch to Session B (fullscreen TUI in alt screen with mouse & Kitty keyboard!).
	if _, err := rc.Attach(protocol.AttachReq{Target: sessB.ID, Size: sz, ReplayLines: 500}); err != nil {
		t.Fatalf("attach B: %v", err)
	}
	waitFor(t, 3*time.Second, "Session B alt screen", func() bool {
		outer.mu.Lock()
		defer outer.mu.Unlock()
		return outer.term.InAltScreen &&
			outer.term.MouseMode == 1000 &&
			outer.term.MouseEncoding == 1006 &&
			len(outer.term.Alt.KittyStack) == 1 &&
			outer.term.Alt.KittyStack[0] == 5
	})
	if !outer.screenContains("EDITOR_TUI_ACTIVE") {
		t.Fatalf("Session B alt screen content missing")
	}

	// Switch BACK to Session A! Outer terminal MUST exit alt-screen, reset mouse & Kitty
	// keyboard, AND repopulate native outer scrollback with LOG_ROW_01..LOG_ROW_60!
	if _, err := rc.Attach(protocol.AttachReq{Target: sessA.ID, Size: sz, ReplayLines: 500}); err != nil {
		t.Fatalf("reattach A: %v", err)
	}
	waitFor(t, 3*time.Second, "Session A restored with native scrollback", func() bool {
		outer.mu.Lock()
		inAlt := outer.term.InAltScreen
		mouse := outer.term.MouseMode
		kitty := len(outer.term.Main.KittyStack)
		outer.mu.Unlock()
		return !inAlt && mouse == 0 && kitty == 0 &&
			outer.screenContains("READY_A") &&
			outer.scrollbackContains("LOG_ROW_01") &&
			outer.scrollbackContains("LOG_ROW_30")
	})

	// Verify live input passthrough still works after switching back.
	if err := rc.SendStdin([]byte("LIVE_ECHO_AFTER_SWITCH\n")); err != nil {
		t.Fatalf("SendStdin: %v", err)
	}
	waitFor(t, 2*time.Second, "live input echo", func() bool {
		return outer.screenContains("LIVE_ECHO_AFTER_SWITCH")
	})

	// Test Pause -> background output while switcher open -> Restore catches missed output!
	if err := rc.Pause(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := rc.SendStdin([]byte("PRINTED_WHILE_PAUSED\n")); err != nil {
		t.Fatalf("stdin while paused: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if outer.screenContains("PRINTED_WHILE_PAUSED") {
		t.Fatalf("output leaked while client paused for switcher!")
	}
	restResp, err := rc.Restore(protocol.RestoreReq{Size: sz, ReplayLines: 500})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !restResp.Replayed {
		t.Fatalf("expected Replay=true because background output arrived while paused")
	}
	waitFor(t, 2*time.Second, "missed paused output restored", func() bool {
		return outer.screenContains("PRINTED_WHILE_PAUSED")
	})

	// Test out-of-band `mplex switch` trigger from a second CLI connection!
	cli2, err := client.Dial(sock, nil)
	if err != nil {
		t.Fatalf("dial cli2: %v", err)
	}
	defer cli2.Close()

	if err := cli2.TriggerSwitcher("", ""); err != nil {
		t.Fatalf("TriggerSwitcher: %v", err)
	}
	select {
	case <-rc.OpenSwitcherCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("interactive client did not receive MsgOpenSwitcher")
	}

	// Test Preview, Rename, and List!
	lines, inAlt, err := cli2.Preview(sessB.ID, 10, 40)
	if err != nil || !inAlt || len(lines) == 0 {
		t.Fatalf("preview B failed: err=%v inAlt=%v lines=%v", err, inAlt, lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "EDITOR_TUI_ACTIVE") {
		t.Fatalf("preview B did not contain expected text: %v", lines)
	}

	renamed, err := cli2.Rename("logs", "prod-logs")
	if err != nil || renamed.Name != "prod-logs" {
		t.Fatalf("rename failed: %+v err=%v", renamed, err)
	}

	all, err := cli2.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("list failed: %d err=%v", len(all), err)
	}

	// Kill both cleanly.
	killed, err := cli2.Kill("", true)
	if err != nil || len(killed) != 2 {
		t.Fatalf("kill all failed: killed=%v err=%v", killed, err)
	}
	fmt.Printf("end-to-end daemon test verified: scrollback, alt-screen, modes, pause/restore, out-of-band switcher\n")
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFollowReadOnlyDoesNotDetachActiveClient(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "mplex-flw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "m.sock")

	d := daemon.New(sock)
	go func() {
		_ = d.Start()
	}()
	defer d.Close()

	// Connect interactive client A.
	var active *client.RPCConn
	outer := newOuterTermSink(20, 80)
	waitFor(t, 2*time.Second, "daemon socket", func() bool {
		c, err := client.Dial(sock, outer)
		if err != nil {
			return false
		}
		active = c
		return true
	})
	defer active.Close()

	sz := protocol.WinSize{Rows: 20, Cols: 80}
	hello, err := active.Hello(protocol.HelloReq{Size: sz, Term: "xterm-256color", Interactive: true})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}

	sess, err := active.Create(protocol.CreateReq{
		Name: "live",
		Cmd:  []string{"/bin/sh"},
		Size: sz,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := active.Attach(protocol.AttachReq{Target: sess.ID, Size: sz, ReplayLines: 500}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Emit output before followers join.
	if err := active.SendStdin([]byte("echo BEFORE_FOLLOW_HISTORY\n")); err != nil {
		t.Fatalf("SendStdin: %v", err)
	}
	waitFor(t, 3*time.Second, "BEFORE_FOLLOW_HISTORY on active client", func() bool {
		return outer.screenContains("BEFORE_FOLLOW_HISTORY")
	})

	// Follower 1 joins WITH history.
	bufHist := &syncBuffer{}
	followerHist, err := client.Dial(sock, bufHist)
	if err != nil {
		t.Fatalf("dial followerHist: %v", err)
	}
	defer followerHist.Close()

	resp1, err := followerHist.Follow(protocol.FollowReq{
		Target:         "live",
		IncludeHistory: true,
		Lines:          50,
	})
	if err != nil {
		t.Fatalf("Follow (history): %v", err)
	}
	if !resp1.Session.Attached || resp1.Session.AttachedBy != hello.ClientID {
		t.Fatalf("expected active client %q to remain attached, got %+v", hello.ClientID, resp1.Session)
	}

	// Follower 2 joins WITHOUT history.
	bufLive := &syncBuffer{}
	followerLive, err := client.Dial(sock, bufLive)
	if err != nil {
		t.Fatalf("dial followerLive: %v", err)
	}
	defer followerLive.Close()

	if _, err := followerLive.Follow(protocol.FollowReq{
		Target:         "live",
		IncludeHistory: false,
	}); err != nil {
		t.Fatalf("Follow (live only): %v", err)
	}

	// Follower 1 should receive the history snapshot; active client must NOT be detached.
	waitFor(t, 2*time.Second, "followerHist receives initial history", func() bool {
		return strings.Contains(bufHist.String(), "BEFORE_FOLLOW_HISTORY")
	})
	if strings.Contains(bufLive.String(), "BEFORE_FOLLOW_HISTORY") {
		t.Fatalf("follower without IncludeHistory unexpectedly received old history: %q", bufLive.String())
	}
	select {
	case ev := <-active.DetachedCh:
		t.Fatalf("active client was detached by follower: %+v", ev)
	default:
	}

	// Broadcast live output -> active client AND both followers receive it.
	if err := active.SendStdin([]byte("echo LIVE_BROADCAST_OK\n")); err != nil {
		t.Fatalf("SendStdin live: %v", err)
	}
	waitFor(t, 3*time.Second, "active client receives LIVE_BROADCAST_OK", func() bool {
		return outer.screenContains("LIVE_BROADCAST_OK")
	})
	waitFor(t, 3*time.Second, "followerHist receives LIVE_BROADCAST_OK", func() bool {
		return strings.Contains(bufHist.String(), "LIVE_BROADCAST_OK")
	})
	waitFor(t, 3*time.Second, "followerLive receives LIVE_BROADCAST_OK", func() bool {
		return strings.Contains(bufLive.String(), "LIVE_BROADCAST_OK")
	})

	// Disconnect follower 1 (triggers Unfollow cleanup) and verify streaming continues cleanly.
	_ = followerHist.Close()
	if err := active.SendStdin([]byte("echo AFTER_UNFOLLOW_OK\n")); err != nil {
		t.Fatalf("SendStdin after unfollow: %v", err)
	}
	waitFor(t, 3*time.Second, "followerLive receives AFTER_UNFOLLOW_OK", func() bool {
		return strings.Contains(bufLive.String(), "AFTER_UNFOLLOW_OK")
	})

	// Exit the session -> BOTH active client and followerLive receive MsgSessionExited!
	if err := active.SendStdin([]byte("exit 0\n")); err != nil {
		t.Fatalf("SendStdin exit: %v", err)
	}
	select {
	case ev := <-active.SessionExitedCh:
		if ev.SessionID != sess.ID {
			t.Fatalf("active SessionExitedCh id=%q, want %q", ev.SessionID, sess.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("active client did not receive MsgSessionExited")
	}
	select {
	case ev := <-followerLive.SessionExitedCh:
		if ev.SessionID != sess.ID {
			t.Fatalf("followerLive SessionExitedCh id=%q, want %q", ev.SessionID, sess.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("followerLive did not receive MsgSessionExited")
	}
}
