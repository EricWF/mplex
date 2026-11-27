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

func TestUnreadOutputBellTrackingAndScrolledPreview(t *testing.T) {
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

	var rc *client.RPCConn
	outer := newOuterTermSink(10, 60)
	waitFor(t, 2*time.Second, "daemon socket", func() bool {
		c, err := client.Dial(sock, outer)
		if err != nil {
			return false
		}
		rc = c
		return true
	})
	defer rc.Close()

	// Enable outer window title tagging on rc to also verify OSC 0 emission after replay.
	rc.SetWindowTitleEnabled(true)

	sz := protocol.WinSize{Rows: 10, Cols: 60}
	if _, err := rc.Hello(protocol.HelloReq{Size: sz, Term: "xterm-256color", Interactive: true}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	cli, err := client.Dial(sock, nil)
	if err != nil {
		t.Fatalf("dial cli: %v", err)
	}
	defer cli.Close()

	findInfo := func(id string) protocol.SessionInfo {
		t.Helper()
		list, err := cli.List()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, s := range list {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("session %q not found", id)
		return protocol.SessionInfo{}
	}

	// Create a detached session that prints 30 lines, sets an inner title, and waits for stdin.
	sess, err := cli.Create(protocol.CreateReq{
		Name: "worker",
		Cmd:  []string{"/bin/sh", "-c", `printf "\033]2;build-runner\007"; i=1; while [ $i -le 30 ]; do printf "SCROLL_%02d\n" "$i"; i=$((i+1)); done; printf "READY\n"; exec cat`},
		Size: sz,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Detached session receives output -> UnreadOutput must become true, HasBell false.
	waitFor(t, 3*time.Second, "detached unread output", func() bool {
		info := findInfo(sess.ID)
		return info.UnreadOutput && !info.HasBell && info.Scrollback >= 20
	})

	// Test PreviewScrolled into scrollback over RPC!
	bottomPrev, err := cli.PreviewScrolled(sess.ID, 5, 40, 0)
	if err != nil {
		t.Fatalf("PreviewScrolled(0): %v", err)
	}
	if bottomPrev.ScrollOffset != 0 || bottomPrev.TotalLines < 30 {
		t.Fatalf("unexpected bottom preview metadata: %+v", bottomPrev)
	}
	if !strings.Contains(strings.Join(bottomPrev.Lines, "\n"), "READY") {
		t.Fatalf("bottom preview missing READY: %v", bottomPrev.Lines)
	}

	midPrev, err := cli.PreviewScrolled(sess.ID, 5, 40, 20)
	if err != nil {
		t.Fatalf("PreviewScrolled(20): %v", err)
	}
	if midPrev.ScrollOffset != 20 {
		t.Fatalf("expected ScrollOffset=20, got %d", midPrev.ScrollOffset)
	}
	if strings.Contains(strings.Join(midPrev.Lines, "\n"), "READY") {
		t.Fatalf("scrolled preview should not still show READY: %v", midPrev.Lines)
	}

	topPrev, err := cli.PreviewScrolled(sess.ID, 5, 40, 9999)
	if err != nil {
		t.Fatalf("PreviewScrolled(9999): %v", err)
	}
	if topPrev.ScrollOffset >= topPrev.TotalLines || !strings.Contains(strings.Join(topPrev.Lines, "\n"), "SCROLL_01") {
		t.Fatalf("top clamped preview unexpected: %+v", topPrev)
	}

	// Send BEL (0x07) while still detached -> HasBell must become true.
	if _, err := cli.SendToSession(sess.ID, []byte("ALERT\x07\n")); err != nil {
		t.Fatalf("send BEL: %v", err)
	}
	waitFor(t, 2*time.Second, "detached bell flag", func() bool {
		info := findInfo(sess.ID)
		return info.UnreadOutput && info.HasBell
	})

	// Attach -> both UnreadOutput and HasBell must clear immediately!
	// And outer window title must be "[worker] build-runner" (not clobbered by replay's OSC 2!).
	if _, err := rc.Attach(protocol.AttachReq{Target: sess.ID, Size: sz, ReplayLines: 500}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	infoAfterAttach := findInfo(sess.ID)
	if infoAfterAttach.UnreadOutput || infoAfterAttach.HasBell {
		t.Fatalf("expected unread/bell cleared on attach, got unread=%v bell=%v", infoAfterAttach.UnreadOutput, infoAfterAttach.HasBell)
	}
	waitFor(t, 2*time.Second, "outer window title tagged after replay", func() bool {
		outer.mu.Lock()
		defer outer.mu.Unlock()
		return outer.term.Title == "[worker] build-runner"
	})

	// Pause client (simulating switcher open), send output + BEL -> both flags trigger while paused!
	if err := rc.Pause(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := cli.SendToSession(sess.ID, []byte("PAUSED_BELL\x07\n")); err != nil {
		t.Fatalf("send while paused: %v", err)
	}
	waitFor(t, 2*time.Second, "paused unread+bell flags", func() bool {
		info := findInfo(sess.ID)
		return info.UnreadOutput && info.HasBell
	})

	// Restore -> flags clear again!
	restResp, err := rc.Restore(protocol.RestoreReq{Size: sz, ReplayLines: 500})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restResp.Session == nil || restResp.Session.Name != "worker" {
		t.Fatalf("expected RestoreResp.Session populated, got %+v", restResp.Session)
	}
	infoAfterRestore := findInfo(sess.ID)
	if infoAfterRestore.UnreadOutput || infoAfterRestore.HasBell {
		t.Fatalf("expected unread/bell cleared on restore, got unread=%v bell=%v", infoAfterRestore.UnreadOutput, infoAfterRestore.HasBell)
	}

	_, _ = cli.Kill("", true)
}

func TestPausedQueryAutoReplyAndStolenDetachEvent(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "mplex-dq-*")
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

	outer1 := newOuterTermSink(24, 80)
	var c1 *client.RPCConn
	waitFor(t, 2*time.Second, "daemon socket", func() bool {
		c, err := client.Dial(sock, outer1)
		if err != nil {
			return false
		}
		c1 = c
		return true
	})
	defer c1.Close()

	sz := protocol.WinSize{Rows: 24, Cols: 80}
	if _, err := c1.Hello(protocol.HelloReq{Size: sz, Term: "xterm-256color", Interactive: true}); err != nil {
		t.Fatalf("c1 hello: %v", err)
	}

	// Shell script that emits DA1 (\x1b[0c) and blocks reading stdin until a 'c' arrives,
	// mimicking fish 4's blocking DA1 terminal capability query.
	script := `stty -icanon min 1 time 0 -echo; echo READY_SCRIPT; read -r line; printf '\033[0c'; while :; do ch=$(dd bs=1 count=1 2>/dev/null); [ "$ch" = "c" ] && break; done; echo DA1_UNBLOCKED; exec cat`
	sess, err := c1.Create(protocol.CreateReq{
		Name: "foo",
		Cmd:  []string{"/bin/sh", "-c", script},
		Size: sz,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c1.Attach(protocol.AttachReq{Target: sess.ID, Size: sz, ReplayLines: 200}); err != nil {
		t.Fatalf("c1 attach: %v", err)
	}
	waitFor(t, 2*time.Second, "script ready", func() bool {
		return outer1.screenContains("READY_SCRIPT")
	})

	// Pause c1 (simulating switcher open), trigger the DA1 query from the session:
	// daemon MUST auto-answer while paused so the session does not freeze!
	if err := c1.Pause(true); err != nil {
		t.Fatalf("c1 pause: %v", err)
	}
	if _, err := c1.SendToSession(sess.ID, []byte("go\n")); err != nil {
		t.Fatalf("trigger query: %v", err)
	}
	waitFor(t, 3*time.Second, "paused session unblocked by daemon DA1 reply", func() bool {
		capResp, err := c1.Capture(protocol.CaptureReq{Target: sess.ID})
		return err == nil && strings.Contains(capResp.Text, "DA1_UNBLOCKED")
	})

	// Now client 2 steals "foo" from c1 -> c1 receives DetachedEvent with Stolen=true!
	outer2 := newOuterTermSink(24, 80)
	c2, err := client.Dial(sock, outer2)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer c2.Close()
	if _, err := c2.Hello(protocol.HelloReq{Size: sz, Term: "xterm-256color", Interactive: true}); err != nil {
		t.Fatalf("c2 hello: %v", err)
	}
	if _, err := c2.Attach(protocol.AttachReq{Target: "foo", Size: sz, ReplayLines: 200}); err != nil {
		t.Fatalf("c2 attach: %v", err)
	}

	select {
	case ev := <-c1.DetachedCh:
		if !ev.Stolen || ev.SessionID != sess.ID || ev.SessionName != "foo" {
			t.Fatalf("unexpected DetachedEvent on steal: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("c1 did not receive DetachedEvent when c2 stole foo")
	}

	// c2 writes to the stolen session and receives live output.
	if err := c2.SendStdin([]byte("STOLEN_WRITE_OK\r\n")); err != nil {
		t.Fatalf("c2 SendStdin: %v", err)
	}
	waitFor(t, 2*time.Second, "c2 sees live output", func() bool {
		return outer2.screenContains("STOLEN_WRITE_OK")
	})

	_, _ = c2.Kill("", true)
}
