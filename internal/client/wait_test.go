package client

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EricWF/mplex/internal/protocol"
)

type fakeWaitClient struct {
	mu          sync.Mutex
	sessions    []protocol.SessionInfo
	captureFunc func(req protocol.CaptureReq) (protocol.CaptureResp, error)
}

func (f *fakeWaitClient) List() ([]protocol.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]protocol.SessionInfo, len(f.sessions))
	copy(cp, f.sessions)
	return cp, nil
}

func (f *fakeWaitClient) Capture(req protocol.CaptureReq) (protocol.CaptureResp, error) {
	f.mu.Lock()
	fn := f.captureFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return protocol.CaptureResp{}, errors.New("session not found")
}

func TestWaitValidation(t *testing.T) {
	cli := &fakeWaitClient{}

	if _, err := Wait(context.Background(), cli, WaitOptions{}); err == nil {
		t.Fatal("expected error for missing target")
	}
	if _, err := Wait(context.Background(), cli, WaitOptions{Target: "main"}); err == nil {
		t.Fatal("expected error when no condition is provided")
	}
	if _, err := Wait(context.Background(), cli, WaitOptions{Target: "main", Match: "["}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if _, err := Wait(context.Background(), cli, WaitOptions{Target: "main", Idle: -time.Second}); err == nil {
		t.Fatal("expected error for negative idle")
	}
	if _, err := Wait(context.Background(), cli, WaitOptions{Target: "main", Exit: true, Timeout: -time.Second}); err == nil {
		t.Fatal("expected error for negative timeout")
	}
}

func TestWaitMatch(t *testing.T) {
	calls := 0
	var gotScrollback bool
	cli := &fakeWaitClient{
		captureFunc: func(req protocol.CaptureReq) (protocol.CaptureResp, error) {
			calls++
			gotScrollback = req.IncludeScrollback
			lines := []string{"starting..."}
			if calls >= 3 {
				lines = append(lines, "BUILD_OK: 42ms")
			}
			return protocol.CaptureResp{
				Session: protocol.SessionInfo{ID: "s1", Name: "build", LastActiveAt: time.Now()},
				Lines:   lines,
				Text:    strings.Join(lines, "\n"),
			}, nil
		},
	}

	res, err := Wait(context.Background(), cli, WaitOptions{
		Target:            "build",
		Match:             `BUILD_OK:\s+\d+ms`,
		IncludeScrollback: true,
		Timeout:           2 * time.Second,
		PollInterval:      10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Wait match failed: %v", err)
	}
	if !res.Matched || res.Reason != "match" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.MatchLine != "BUILD_OK: 42ms" {
		t.Fatalf("MatchLine = %q, want %q", res.MatchLine, "BUILD_OK: 42ms")
	}
	if res.Session == nil || res.Session.Name != "build" {
		t.Fatalf("unexpected session info: %+v", res.Session)
	}
	if !gotScrollback {
		t.Fatal("expected IncludeScrollback=true to be forwarded")
	}
}

func TestWaitIdle(t *testing.T) {
	sess := protocol.SessionInfo{ID: "s1", Name: "worker", LastActiveAt: time.Now()}
	cli := &fakeWaitClient{
		sessions: []protocol.SessionInfo{sess},
		captureFunc: func(req protocol.CaptureReq) (protocol.CaptureResp, error) {
			return protocol.CaptureResp{
				Session: sess,
				Lines:   []string{"idle prompt$"},
				Text:    "idle prompt$",
			}, nil
		},
	}

	res, err := Wait(context.Background(), cli, WaitOptions{
		Target:       "worker",
		Idle:         80 * time.Millisecond,
		Timeout:      2 * time.Second,
		PollInterval: 15 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Wait idle failed: %v", err)
	}
	if !res.Matched || res.Reason != "idle" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Text != "idle prompt$" {
		t.Fatalf("expected captured text on idle, got %q", res.Text)
	}
}

func TestWaitExit(t *testing.T) {
	cli := &fakeWaitClient{
		sessions: []protocol.SessionInfo{{ID: "s1", Name: "job", LastActiveAt: time.Now()}},
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cli.mu.Lock()
		cli.sessions = nil
		cli.mu.Unlock()
	}()

	res, err := Wait(context.Background(), cli, WaitOptions{
		Target:       "job",
		Exit:         true,
		Timeout:      2 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Wait exit failed: %v", err)
	}
	if !res.Matched || res.Reason != "exit" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Session == nil || res.Session.Name != "job" {
		t.Fatalf("expected last known session metadata, got %+v", res.Session)
	}
}

func TestWaitTimeout(t *testing.T) {
	cli := &fakeWaitClient{
		captureFunc: func(req protocol.CaptureReq) (protocol.CaptureResp, error) {
			return protocol.CaptureResp{
				Session: protocol.SessionInfo{ID: "s1", Name: "slow", LastActiveAt: time.Now()},
				Lines:   []string{"still working..."},
				Text:    "still working...",
			}, nil
		},
	}

	res, err := Wait(context.Background(), cli, WaitOptions{
		Target:       "slow",
		Match:        "NEVER_APPEARS",
		Timeout:      75 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
	})
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("expected ErrWaitTimeout, got %v", err)
	}
	if res.Matched || res.Reason != "timeout" {
		t.Fatalf("unexpected timeout result: %+v", res)
	}
	if res.Text != "still working..." {
		t.Fatalf("expected last text on timeout, got %q", res.Text)
	}
}
