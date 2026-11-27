package switcher

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EricWF/mplex/internal/protocol"
)

func TestSwitcherPreferCreateAndExitOnEsc(t *testing.T) {
	sessions := []protocol.SessionInfo{
		{ID: "s1", Name: "main", Attached: true},
		{ID: "s2", Name: "api", Attached: false},
	}

	// 1. PreferCreate=true: pressing Enter immediately creates a new session
	// instead of selecting/stealing "main".
	inCh := make(chan []byte, 4)
	inCh <- []byte{'\r'}
	var out bytes.Buffer
	res := Run(Config{
		PreferCreate: true,
		ExitOnEsc:    true,
		Rows:         24,
		Cols:         100,
		InputCh:      inCh,
		Out:          &out,
		Callbacks: Callbacks{
			ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
		},
	})
	if res.Action != ActionCreate || res.Target != "" {
		t.Fatalf("expected ActionCreate with empty target, got %+v", res)
	}

	// 2. PreferCreate=false when CurrentSessionID=="": prefers detached "api"
	// over attached "main".
	inCh2 := make(chan []byte, 4)
	inCh2 <- []byte{'\r'}
	out.Reset()
	res2 := Run(Config{
		PreferCreate: false,
		ExitOnEsc:    true,
		Rows:         24,
		Cols:         100,
		InputCh:      inCh2,
		Out:          &out,
		Callbacks: Callbacks{
			ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
		},
	})
	if res2.Action != ActionSelect || res2.Target != "s2" {
		t.Fatalf("expected detached session s2 to be pre-selected, got %+v", res2)
	}

	// 3. ExitOnEsc=true: first Esc clears query, second Esc exits cleanly (ActionDetach).
	inCh3 := make(chan []byte, 4)
	inCh3 <- []byte("foo")
	inCh3 <- []byte{0x1b}
	inCh3 <- []byte{0x1b}
	out.Reset()
	res3 := Run(Config{
		PreferCreate: true,
		ExitOnEsc:    true,
		Rows:         24,
		Cols:         100,
		InputCh:      inCh3,
		Out:          &out,
		Callbacks: Callbacks{
			ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
		},
	})
	if res3.Action != ActionDetach {
		t.Fatalf("expected ActionDetach on Esc with empty query, got %+v", res3)
	}
}

func TestSwitcherPreviewScrollAndResetOnSelectionChange(t *testing.T) {
	sessions := []protocol.SessionInfo{
		{ID: "s1", Name: "logs", Scrollback: 25},
		{ID: "s2", Name: "api", Scrollback: 10},
	}

	// Simulate 30 total lines for s1, 15 for s2.
	makePreview := func(target string, rows, cols, scrollOffset int) (protocol.PreviewResp, error) {
		total := 30
		if target == "s2" {
			total = 15
		}
		maxOff := total - rows
		if maxOff < 0 {
			maxOff = 0
		}
		if scrollOffset < 0 {
			scrollOffset = 0
		}
		if scrollOffset > maxOff {
			scrollOffset = maxOff
		}
		end := total - scrollOffset
		start := end - rows
		if start < 0 {
			start = 0
		}
		lines := make([]string, 0, end-start)
		for i := start + 1; i <= end; i++ {
			lines = append(lines, fmt.Sprintf("%s-line-%02d", target, i))
		}
		return protocol.PreviewResp{
			Lines:        lines,
			TotalLines:   total,
			ScrollOffset: scrollOffset,
		}, nil
	}

	var out bytes.Buffer
	st := &state{
		cfg: Config{
			CurrentSessionID: "s1",
			Rows:             16, // preview viewport = 10 rows
			Cols:             100,
			AllowCancel:      true,
			Out:              &out,
			Callbacks: Callbacks{
				ListSessions:       func() ([]protocol.SessionInfo, error) { return sessions, nil },
				GetPreviewScrolled: makePreview,
			},
		},
		rows:         16,
		cols:         100,
		previewCache: make(map[string]previewCacheEntry),
	}
	st.reloadSessions()
	st.rebuildEntries()
	st.selectInitial()
	st.syncSelectionScroll()
	st.ensurePreview()
	st.render()

	if st.previewScroll != 0 {
		t.Fatalf("initial previewScroll = %d, want 0", st.previewScroll)
	}
	if strings.Contains(out.String(), "↑") {
		t.Fatalf("unexpected scroll badge at offset 0")
	}

	// Alt+Up (\x1b[1;3A) scrolls preview up 1 line.
	st.handleInput([]byte("\x1b[1;3A"))
	st.ensurePreview()
	if st.previewScroll != 1 {
		t.Fatalf("after Alt+Up: previewScroll = %d, want 1", st.previewScroll)
	}

	// Shift+Up (\x1b[1;2A) scrolls up 1 more line (-> 2).
	st.handleInput([]byte("\x1b[1;2A"))
	st.ensurePreview()
	if st.previewScroll != 2 {
		t.Fatalf("after Shift+Up: previewScroll = %d, want 2", st.previewScroll)
	}

	// Ctrl+B (0x02) pages preview up by 5 lines (-> 7).
	st.handleInput([]byte{0x02})
	st.ensurePreview()
	if st.previewScroll != 7 {
		t.Fatalf("after Ctrl+B: previewScroll = %d, want 7", st.previewScroll)
	}
	out.Reset()
	st.render()
	if !strings.Contains(out.String(), "↑7 lines") {
		t.Fatalf("expected '↑7 lines' scroll indicator in preview header, got:\n%s", out.String())
	}

	// Ctrl+F (0x06) pages preview down by 5 lines (-> 2).
	st.handleInput([]byte{0x06})
	st.ensurePreview()
	if st.previewScroll != 2 {
		t.Fatalf("after Ctrl+F: previewScroll = %d, want 2", st.previewScroll)
	}

	// Alt+Down (\x1b[1;3B) and Shift+Down (\x1b[1;2B) scroll down to 0.
	st.handleInput([]byte("\x1b[1;3B\x1b[1;2B"))
	st.ensurePreview()
	if st.previewScroll != 0 {
		t.Fatalf("after Alt+Down + Shift+Down: previewScroll = %d, want 0", st.previewScroll)
	}

	// Mouse wheel up over preview pane (x > leftW) scrolls preview, NOT session list!
	leftW, _ := st.splitWidths()
	wheelUpRight := fmt.Sprintf("\x1b[<64;%d;10M", leftW+5)
	st.handleInput([]byte(wheelUpRight))
	st.ensurePreview()
	if st.selected != 0 || st.previewScroll != 3 {
		t.Fatalf("after wheel up on preview: selected=%d previewScroll=%d, want 0, 3", st.selected, st.previewScroll)
	}

	// Overshooting scroll clamps cleanly to maxOffset (30 - 10 = 20).
	for i := 0; i < 10; i++ {
		st.handleInput([]byte{0x02}) // Ctrl+B (+5 each)
		st.ensurePreview()
	}
	if st.previewScroll != 20 {
		t.Fatalf("expected clamped previewScroll=20, got %d", st.previewScroll)
	}

	// Mouse wheel down over left session list (x <= leftW) moves selection AND resets previewScroll to 0!
	wheelDownLeft := fmt.Sprintf("\x1b[<65;%d;10M", leftW-2)
	st.handleInput([]byte(wheelDownLeft))
	st.ensurePreview()
	if st.selected != 1 {
		t.Fatalf("after wheel down on list: selected=%d, want 1", st.selected)
	}
	if st.previewScroll != 0 {
		t.Fatalf("expected previewScroll reset to 0 on selection change, got %d", st.previewScroll)
	}
}

func TestSwitcherActivityAndBellBadges(t *testing.T) {
	sessions := []protocol.SessionInfo{
		{ID: "s1", Name: "cur", Attached: true},
		{ID: "s2", Name: "other", Attached: true},
		{ID: "s3", Name: "unread", Attached: false, UnreadOutput: true},
		{ID: "s4", Name: "alert", Attached: false, UnreadOutput: true, HasBell: true},
		{ID: "s5", Name: "idle", Attached: false},
	}

	var out bytes.Buffer
	st := &state{
		cfg: Config{
			CurrentSessionID: "s1",
			Rows:             24,
			Cols:             110,
			AllowCancel:      true,
			Out:              &out,
			Callbacks: Callbacks{
				ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
				GetPreviewScrolled: func(target string, rows, cols, scrollOffset int) (protocol.PreviewResp, error) {
					return protocol.PreviewResp{Lines: []string{"preview-" + target}}, nil
				},
			},
		},
		rows:         24,
		cols:         110,
		previewCache: make(map[string]previewCacheEntry),
	}
	st.reloadSessions()
	st.rebuildEntries()
	st.selectInitial()
	st.ensurePreview()
	st.render()

	frame := out.String()
	// Green dot preserved for current session, yellow dot preserved for attached-in-another-window.
	if !strings.Contains(frame, "\x1b[32m●\x1b[0m") {
		t.Fatalf("missing green current-session dot")
	}
	if !strings.Contains(frame, "\x1b[33m●\x1b[0m") {
		t.Fatalf("missing yellow attached-elsewhere dot")
	}
	// Cyan dot + new badge for unread session.
	if !strings.Contains(frame, "\x1b[36m●\x1b[0m") || !strings.Contains(frame, "\x1b[36mnew\x1b[0m") {
		t.Fatalf("missing cyan unread indicator/badge")
	}
	// Red bell symbol + ! badge for bell session.
	if !strings.Contains(frame, "\x1b[1;31m◆\x1b[0m") || !strings.Contains(frame, "\x1b[1;31m!\x1b[0m") {
		t.Fatalf("missing bell indicator/badge")
	}

	// Move selection to "unread" (s3) -> preview header must show "● new".
	st.selected = 2
	st.ensurePreview()
	out.Reset()
	st.render()
	if !strings.Contains(out.String(), "· ● new") {
		t.Fatalf("expected unread badge in preview header, got:\n%s", out.String())
	}

	// Move selection to "alert" (s4) -> preview header must show "! bell".
	st.selected = 3
	st.ensurePreview()
	out.Reset()
	st.render()
	if !strings.Contains(out.String(), "· ! bell") {
		t.Fatalf("expected bell badge in preview header, got:\n%s", out.String())
	}
}

type syncWriteCounter struct {
	mu     sync.Mutex
	writes int
	buf    bytes.Buffer
}

func (w *syncWriteCounter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	return w.buf.Write(p)
}

func (w *syncWriteCounter) snapshot() (int, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes, w.buf.String()
}

func TestSwitcherLiveRefreshRerendersOnlyOnChange(t *testing.T) {
	var dataMu sync.Mutex
	curSessions := []protocol.SessionInfo{
		{ID: "s1", Name: "build", Attached: false},
	}
	curPreview := []string{"compiling..."}

	inCh := make(chan []byte, 4)
	out := &syncWriteCounter{}

	doneCh := make(chan Result, 1)
	go func() {
		doneCh <- Run(Config{
			ExitOnEsc:       true,
			Rows:            20,
			Cols:            100,
			RefreshInterval: 20 * time.Millisecond,
			InputCh:         inCh,
			Out:             out,
			Callbacks: Callbacks{
				ListSessions: func() ([]protocol.SessionInfo, error) {
					dataMu.Lock()
					defer dataMu.Unlock()
					cp := make([]protocol.SessionInfo, len(curSessions))
					copy(cp, curSessions)
					return cp, nil
				},
				GetPreviewScrolled: func(target string, rows, cols, scrollOffset int) (protocol.PreviewResp, error) {
					dataMu.Lock()
					defer dataMu.Unlock()
					cp := append([]string(nil), curPreview...)
					return protocol.PreviewResp{Lines: cp, TotalLines: len(cp)}, nil
				},
			},
		})
	}()

	// Wait for initial render (1 init escape + 1 initial frame = 2 writes).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, s := out.snapshot()
		if strings.Contains(s, "compiling...") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	initialWrites, _ := out.snapshot()

	// Wait through several 20ms ticks with NO data changes: write count must NOT increase!
	time.Sleep(90 * time.Millisecond)
	afterIdleWrites, _ := out.snapshot()
	if afterIdleWrites != initialWrites {
		t.Fatalf("expected 0 extra renders while idle, writes grew from %d to %d", initialWrites, afterIdleWrites)
	}

	// Now mutate preview and bell status -> ticker must detect change and re-render!
	dataMu.Lock()
	curSessions[0].HasBell = true
	curSessions[0].UnreadOutput = true
	curPreview = []string{"BUILD PASSED"}
	dataMu.Unlock()

	deadline = time.Now().Add(time.Second)
	updated := false
	for time.Now().Before(deadline) {
		_, s := out.snapshot()
		if strings.Contains(s, "BUILD PASSED") && strings.Contains(s, "! bell") {
			updated = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !updated {
		t.Fatalf("live refresh did not re-render updated preview & bell badge")
	}

	// Exit cleanly via Esc.
	inCh <- []byte{0x1b}
	select {
	case res := <-doneCh:
		if res.Action != ActionDetach {
			t.Fatalf("expected ActionDetach, got %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("switcher did not exit cleanly")
	}
}

func TestSwitcherAsyncExternalEvents(t *testing.T) {
	// 1. EventSessionLost while open: clears AllowCancel, updates notice, enables ExitOnEsc.
	var mu sync.Mutex
	sessions := []protocol.SessionInfo{
		{ID: "s1", Name: "foo", Attached: true},
	}
	inCh := make(chan []byte, 4)
	evCh := make(chan ExternalEvent, 4)
	out := &syncWriteCounter{}
	doneCh := make(chan Result, 1)

	go func() {
		doneCh <- Run(Config{
			CurrentSessionID: "s1",
			AllowCancel:      true,
			ExitOnEsc:        false,
			Rows:             24,
			Cols:             100,
			RefreshInterval:  -1,
			InputCh:          inCh,
			EventCh:          evCh,
			Out:              out,
			Callbacks: Callbacks{
				ListSessions: func() ([]protocol.SessionInfo, error) {
					mu.Lock()
					defer mu.Unlock()
					cp := make([]protocol.SessionInfo, len(sessions))
					copy(cp, sessions)
					return cp, nil
				},
			},
		})
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, s := out.snapshot(); strings.Contains(s, "foo") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	evCh <- ExternalEvent{
		Kind:   EventSessionLost,
		Notice: `Session "foo" attached by another client`,
	}

	deadline = time.Now().Add(time.Second)
	sawNotice := false
	for time.Now().Before(deadline) {
		if _, s := out.snapshot(); strings.Contains(s, `Session "foo" attached by another client`) {
			sawNotice = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawNotice {
		t.Fatalf("switcher did not render EventSessionLost notice")
	}

	// Pressing Esc must now exit cleanly (ActionDetach), NOT ActionCancel!
	inCh <- []byte{0x1b}
	select {
	case res := <-doneCh:
		if res.Action != ActionDetach {
			t.Fatalf("expected ActionDetach after EventSessionLost + Esc, got %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("switcher did not exit on Esc after EventSessionLost")
	}

	// 2. EventForceDetach immediately exits with ForcedDetach=true.
	evCh2 := make(chan ExternalEvent, 2)
	doneCh2 := make(chan Result, 1)
	go func() {
		doneCh2 <- Run(Config{
			Rows:            24,
			Cols:            80,
			RefreshInterval: -1,
			InputCh:         make(chan []byte),
			EventCh:         evCh2,
			Out:             &syncWriteCounter{},
			Callbacks: Callbacks{
				ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
			},
		})
	}()
	evCh2 <- ExternalEvent{Kind: EventForceDetach, Notice: "mplex: detached"}
	select {
	case res := <-doneCh2:
		if res.Action != ActionDetach || !res.ForcedDetach || res.Notice != "mplex: detached" {
			t.Fatalf("unexpected force-detach result: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("switcher did not exit on EventForceDetach")
	}

	// 3. EventDaemonClosed immediately exits with DaemonClosed=true.
	evCh3 := make(chan ExternalEvent, 2)
	doneCh3 := make(chan Result, 1)
	go func() {
		doneCh3 <- Run(Config{
			Rows:            24,
			Cols:            80,
			RefreshInterval: -1,
			InputCh:         make(chan []byte),
			EventCh:         evCh3,
			Out:             &syncWriteCounter{},
			Callbacks: Callbacks{
				ListSessions: func() ([]protocol.SessionInfo, error) { return sessions, nil },
			},
		})
	}()
	evCh3 <- ExternalEvent{Kind: EventDaemonClosed}
	select {
	case res := <-doneCh3:
		if res.Action != ActionDetach || !res.DaemonClosed {
			t.Fatalf("unexpected daemon-closed result: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("switcher did not exit on EventDaemonClosed")
	}
}
