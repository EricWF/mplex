package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/EricWF/mplex/internal/protocol"
)

// RPCConn manages a framed Unix socket connection to the mplex daemon with
// a single reader goroutine that demultiplexes async stream/event frames from
// synchronous RPC responses without races.
type RPCConn struct {
	conn net.Conn

	writeMu sync.Mutex

	rpcMu      sync.Mutex
	pending    map[byte]chan protocol.Frame
	closedErr  error
	closedChan chan struct{}
	closeOnce  sync.Once

	// Async handlers for interactive clients
	outMu                  sync.Mutex
	outWriter              io.Writer
	suppressOutput         atomic.Bool
	droppedWhileSuppressed atomic.Bool
	windowTitleEnabled     atomic.Bool
	titleSessionName       string
	titleInner             string

	OpenSwitcherCh  chan protocol.OpenSwitcherEvent
	SwitchSessionCh chan protocol.SwitchSessionEvent
	SessionExitedCh chan protocol.SessionExitedEvent
	DetachedCh      chan protocol.DetachedEvent
}

// Dial connects to an existing daemon at socketPath.
func Dial(socketPath string, out io.Writer) (*RPCConn, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	rc := &RPCConn{
		conn:            conn,
		pending:         make(map[byte]chan protocol.Frame),
		closedChan:      make(chan struct{}),
		outWriter:       out,
		OpenSwitcherCh:  make(chan protocol.OpenSwitcherEvent, 4),
		SwitchSessionCh: make(chan protocol.SwitchSessionEvent, 4),
		SessionExitedCh: make(chan protocol.SessionExitedEvent, 4),
		DetachedCh:      make(chan protocol.DetachedEvent, 4),
	}
	go rc.readLoop()
	return rc, nil
}

// DialOrStart connects to the daemon at socketPath, automatically spawning a
// background daemon process if one is not yet running.
func DialOrStart(socketPath string, out io.Writer) (*RPCConn, error) {
	if rc, err := Dial(socketPath, out); err == nil {
		return rc, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}

	cmd := exec.Command(exe, "daemon", "--socket", socketPath)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	_ = cmd.Process.Release()

	// Wait briefly for socket to become ready.
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		rc, err := Dial(socketPath, out)
		if err == nil {
			return rc, nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for daemon at %s: %w", socketPath, lastErr)
}

func (rc *RPCConn) Close() error {
	rc.failAll(io.EOF)
	return rc.conn.Close()
}

func (rc *RPCConn) Done() <-chan struct{} {
	return rc.closedChan
}

func (rc *RPCConn) SetSuppressOutput(suppress bool) {
	if suppress {
		rc.droppedWhileSuppressed.Store(false)
	}
	rc.suppressOutput.Store(suppress)
}

func (rc *RPCConn) ConsumeDroppedFlag() bool {
	return rc.droppedWhileSuppressed.Swap(false)
}

func (rc *RPCConn) WriteStdoutRaw(p []byte) {
	if len(p) == 0 || rc.outWriter == nil {
		return
	}
	rc.outMu.Lock()
	_, _ = rc.outWriter.Write(p)
	rc.outMu.Unlock()
}

// SetWindowTitleEnabled toggles automatic OSC 0 outer window title tagging
// after attach/switch/restore replays and on explicit UpdateWindowTitle calls.
func (rc *RPCConn) SetWindowTitleEnabled(enabled bool) {
	rc.windowTitleEnabled.Store(enabled)
}

// UpdateWindowTitle updates the tracked session title and immediately emits
// an OSC 0 title sequence to the outer terminal when window title tagging is enabled.
func (rc *RPCConn) UpdateWindowTitle(sessionName, innerTitle string) {
	rc.outMu.Lock()
	rc.titleSessionName = sessionName
	rc.titleInner = innerTitle
	if rc.windowTitleEnabled.Load() && rc.outWriter != nil {
		if osc := formatWindowTitleOSC(sessionName, innerTitle); len(osc) > 0 {
			_, _ = rc.outWriter.Write(osc)
		}
	}
	rc.outMu.Unlock()
}

func (rc *RPCConn) failAll(err error) {
	rc.closeOnce.Do(func() {
		rc.rpcMu.Lock()
		rc.closedErr = err
		for k, ch := range rc.pending {
			close(ch)
			delete(rc.pending, k)
		}
		rc.rpcMu.Unlock()
		close(rc.closedChan)
	})
}

func (rc *RPCConn) readLoop() {
	for {
		frame, err := protocol.ReadFrame(rc.conn)
		if err != nil {
			rc.failAll(err)
			return
		}

		switch frame.Type {
		case protocol.MsgStdout:
			if rc.suppressOutput.Load() {
				rc.droppedWhileSuppressed.Store(true)
				continue
			}
			rc.WriteStdoutRaw(frame.Payload)

		case protocol.MsgReplay:
			rc.outMu.Lock()
			if rc.outWriter != nil {
				if len(frame.Payload) > 0 {
					_, _ = rc.outWriter.Write(frame.Payload)
				}
				if rc.windowTitleEnabled.Load() && rc.titleSessionName != "" {
					if osc := formatWindowTitleOSC(rc.titleSessionName, rc.titleInner); len(osc) > 0 {
						_, _ = rc.outWriter.Write(osc)
					}
				}
			}
			rc.outMu.Unlock()

		case protocol.MsgOpenSwitcher:
			var ev protocol.OpenSwitcherEvent
			_ = protocol.DecodeJSON(frame.Payload, &ev)
			select {
			case rc.OpenSwitcherCh <- ev:
			default:
			}

		case protocol.MsgSwitchSession:
			var ev protocol.SwitchSessionEvent
			_ = protocol.DecodeJSON(frame.Payload, &ev)
			select {
			case rc.SwitchSessionCh <- ev:
			default:
			}

		case protocol.MsgSessionExited:
			var ev protocol.SessionExitedEvent
			_ = protocol.DecodeJSON(frame.Payload, &ev)
			select {
			case rc.SessionExitedCh <- ev:
			default:
			}

		case protocol.MsgDetached:
			var ev protocol.DetachedEvent
			_ = protocol.DecodeJSON(frame.Payload, &ev)
			select {
			case rc.DetachedCh <- ev:
			default:
			}

		default:
			if rc.windowTitleEnabled.Load() {
				switch frame.Type {
				case protocol.MsgAttachResp:
					var resp protocol.AttachResp
					if protocol.DecodeJSON(frame.Payload, &resp) == nil && resp.Error == "" && resp.Session.Name != "" {
						rc.outMu.Lock()
						rc.titleSessionName = resp.Session.Name
						rc.titleInner = resp.Session.Title
						rc.outMu.Unlock()
					}
				case protocol.MsgRestoreResp:
					var resp protocol.RestoreResp
					if protocol.DecodeJSON(frame.Payload, &resp) == nil && resp.Error == "" && resp.Session != nil && resp.Session.Name != "" {
						rc.outMu.Lock()
						rc.titleSessionName = resp.Session.Name
						rc.titleInner = resp.Session.Title
						rc.outMu.Unlock()
					}
				}
			}
			rc.rpcMu.Lock()
			ch, ok := rc.pending[frame.Type]
			if ok {
				delete(rc.pending, frame.Type)
			}
			rc.rpcMu.Unlock()
			if ok {
				ch <- frame
			}
		}
	}
}

func (rc *RPCConn) SendStdin(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	return protocol.WriteFrame(rc.conn, protocol.MsgStdin, data)
}

func (rc *RPCConn) SendResize(sz protocol.WinSize) error {
	rc.writeMu.Lock()
	defer rc.writeMu.Unlock()
	return protocol.WriteFrame(rc.conn, protocol.MsgResize, protocol.EncodeResize(sz))
}

func (rc *RPCConn) call(reqType byte, req any, respType byte, resp any) error {
	ch := make(chan protocol.Frame, 1)

	rc.rpcMu.Lock()
	if rc.closedErr != nil {
		err := rc.closedErr
		rc.rpcMu.Unlock()
		return err
	}
	rc.pending[respType] = ch
	rc.rpcMu.Unlock()

	rc.writeMu.Lock()
	err := protocol.WriteJSON(rc.conn, reqType, req)
	rc.writeMu.Unlock()
	if err != nil {
		rc.rpcMu.Lock()
		delete(rc.pending, respType)
		rc.rpcMu.Unlock()
		return err
	}

	select {
	case frame, ok := <-ch:
		if !ok {
			return errors.New("connection closed while waiting for response")
		}
		if resp != nil {
			return protocol.DecodeJSON(frame.Payload, resp)
		}
		return nil
	case <-time.After(10 * time.Second):
		rc.rpcMu.Lock()
		delete(rc.pending, respType)
		rc.rpcMu.Unlock()
		return errors.New("daemon request timed out")
	}
}

func (rc *RPCConn) Hello(req protocol.HelloReq) (protocol.HelloResp, error) {
	var resp protocol.HelloResp
	err := rc.call(protocol.MsgHello, req, protocol.MsgHelloResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) List() ([]protocol.SessionInfo, error) {
	var resp protocol.ListResp
	err := rc.call(protocol.MsgListReq, protocol.ListReq{}, protocol.MsgListResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Sessions, err
}

func (rc *RPCConn) Create(req protocol.CreateReq) (protocol.SessionInfo, error) {
	var resp protocol.CreateResp
	err := rc.call(protocol.MsgCreateReq, req, protocol.MsgCreateResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Session, err
}

func (rc *RPCConn) Attach(req protocol.AttachReq) (protocol.AttachResp, error) {
	var resp protocol.AttachResp
	err := rc.call(protocol.MsgAttachReq, req, protocol.MsgAttachResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) Detach(clientID, sessionID string) error {
	var resp protocol.DetachResp
	err := rc.call(protocol.MsgDetachReq, protocol.DetachReq{ClientID: clientID, SessionID: sessionID}, protocol.MsgDetachResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return err
}

func (rc *RPCConn) Pause(paused bool) error {
	var resp protocol.PauseResp
	err := rc.call(protocol.MsgPauseReq, protocol.PauseReq{Paused: paused}, protocol.MsgPauseResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return err
}

func (rc *RPCConn) Restore(req protocol.RestoreReq) (protocol.RestoreResp, error) {
	var resp protocol.RestoreResp
	err := rc.call(protocol.MsgRestoreReq, req, protocol.MsgRestoreResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) Kill(target string, all bool) ([]string, error) {
	var resp protocol.KillResp
	err := rc.call(protocol.MsgKillReq, protocol.KillReq{Target: target, All: all}, protocol.MsgKillResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Killed, err
}

func (rc *RPCConn) Rename(target, newName string) (protocol.SessionInfo, error) {
	var resp protocol.RenameResp
	err := rc.call(protocol.MsgRenameReq, protocol.RenameReq{Target: target, NewName: newName}, protocol.MsgRenameResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Session, err
}

func (rc *RPCConn) Preview(target string, rows, cols int) ([]string, bool, error) {
	resp, err := rc.PreviewScrolled(target, rows, cols, 0)
	return resp.Lines, resp.InAltScreen, err
}

func (rc *RPCConn) PreviewScrolled(target string, rows, cols, scrollOffset int) (protocol.PreviewResp, error) {
	var resp protocol.PreviewResp
	err := rc.call(protocol.MsgPreviewReq, protocol.PreviewReq{
		Target:       target,
		Rows:         rows,
		Cols:         cols,
		ScrollOffset: scrollOffset,
	}, protocol.MsgPreviewResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) TriggerSwitcher(clientID, sessionID string) error {
	var resp protocol.TriggerSwitcherResp
	err := rc.call(protocol.MsgTriggerSwitcher, protocol.TriggerSwitcherReq{ClientID: clientID, SessionID: sessionID}, protocol.MsgTriggerSwitcherResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return err
}

func (rc *RPCConn) SwitchSession(clientID, sessionID, target string) (protocol.TriggerSwitcherResp, error) {
	var resp protocol.TriggerSwitcherResp
	err := rc.call(protocol.MsgTriggerSwitcher, protocol.TriggerSwitcherReq{
		ClientID:  clientID,
		SessionID: sessionID,
		Target:    target,
	}, protocol.MsgTriggerSwitcherResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) Capture(req protocol.CaptureReq) (protocol.CaptureResp, error) {
	var resp protocol.CaptureResp
	err := rc.call(protocol.MsgCaptureReq, req, protocol.MsgCaptureResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) SendToSession(target string, data []byte) (protocol.SendResp, error) {
	var resp protocol.SendResp
	err := rc.call(protocol.MsgSendReq, protocol.SendReq{Target: target, Data: data}, protocol.MsgSendResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}

func (rc *RPCConn) Follow(req protocol.FollowReq) (protocol.FollowResp, error) {
	var resp protocol.FollowResp
	err := rc.call(protocol.MsgFollowReq, req, protocol.MsgFollowResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp, err
}
