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

	OpenSwitcherCh  chan protocol.OpenSwitcherEvent
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
			rc.WriteStdoutRaw(frame.Payload)

		case protocol.MsgOpenSwitcher:
			var ev protocol.OpenSwitcherEvent
			_ = protocol.DecodeJSON(frame.Payload, &ev)
			select {
			case rc.OpenSwitcherCh <- ev:
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
	var resp protocol.PreviewResp
	err := rc.call(protocol.MsgPreviewReq, protocol.PreviewReq{Target: target, Rows: rows, Cols: cols}, protocol.MsgPreviewResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Lines, resp.InAltScreen, err
}

func (rc *RPCConn) TriggerSwitcher(clientID, sessionID string) error {
	var resp protocol.TriggerSwitcherResp
	err := rc.call(protocol.MsgTriggerSwitcher, protocol.TriggerSwitcherReq{ClientID: clientID, SessionID: sessionID}, protocol.MsgTriggerSwitcherResp, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return err
}
