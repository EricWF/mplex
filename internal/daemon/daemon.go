package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EricWF/mplex/internal/protocol"
)

const maxClientQueueBytes = 32 * 1024 * 1024

// Daemon manages all persistent sessions and client Unix socket connections.
type Daemon struct {
	socketPath      string
	listener        net.Listener
	AutoExitOnEmpty bool

	mu          sync.RWMutex
	sessions    map[string]*Session
	clients     map[string]*ClientConn
	hadSession  bool
	nextSessNum uint64
	nextCliNum  uint64

	closed atomic.Bool
}

// DefaultSocketPath returns $MPLEX_SOCKET or $XDG_RUNTIME_DIR/mplex/mplex.sock
// (falling back to /tmp/mplex-<uid>/mplex.sock so macOS stays short under the
// 104-byte sun_path limit and consistent across GUI terminals and SSH).
func DefaultSocketPath() string {
	if p := os.Getenv("MPLEX_SOCKET"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "mplex", "mplex.sock")
	}
	return filepath.Join("/tmp", fmt.Sprintf("mplex-%d", os.Getuid()), "mplex.sock")
}

// New creates a new Daemon bound to socketPath.
func New(socketPath string) *Daemon {
	return &Daemon{
		socketPath: socketPath,
		sessions:   make(map[string]*Session),
		clients:    make(map[string]*ClientConn),
	}
}

// Start creates the Unix socket and accepts client connections until Close is called.
func (d *Daemon) Start() error {
	dir := filepath.Dir(d.socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	// Remove stale socket if no daemon is listening on it.
	if _, err := os.Stat(d.socketPath); err == nil {
		conn, dialErr := net.DialTimeout("unix", d.socketPath, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return fmt.Errorf("daemon already running at %s", d.socketPath)
		}
		_ = os.Remove(d.socketPath)
	}

	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", d.socketPath, err)
	}
	_ = os.Chmod(d.socketPath, 0o600)
	d.listener = ln

	for {
		conn, err := ln.Accept()
		if err != nil {
			if d.closed.Load() {
				return nil
			}
			continue
		}
		go d.handleConn(conn)
	}
}

// Close shuts down the listener, disconnects clients, kills sessions, and removes the socket.
func (d *Daemon) Close() error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if d.listener != nil {
		_ = d.listener.Close()
	}

	d.mu.Lock()
	clients := make([]*ClientConn, 0, len(d.clients))
	for _, c := range d.clients {
		clients = append(clients, c)
	}
	sessions := make([]*Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		sessions = append(sessions, s)
	}
	d.mu.Unlock()

	for _, c := range clients {
		c.close()
	}
	for _, s := range sessions {
		s.Kill()
	}
	_ = os.Remove(d.socketPath)
	return nil
}

func (d *Daemon) removeSession(id string) {
	d.mu.Lock()
	delete(d.sessions, id)
	shouldCheck := d.AutoExitOnEmpty && d.hadSession && len(d.sessions) == 0 && len(d.clients) == 0
	d.mu.Unlock()
	if shouldCheck {
		d.scheduleEmptyExit()
	}
}

func (d *Daemon) scheduleEmptyExit() {
	time.AfterFunc(300*time.Millisecond, func() {
		d.mu.RLock()
		empty := d.AutoExitOnEmpty && d.hadSession && len(d.sessions) == 0 && len(d.clients) == 0
		d.mu.RUnlock()
		if empty {
			_ = d.Close()
		}
	})
}

func (d *Daemon) findSession(target string) *Session {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if target == "" {
		return nil
	}
	if s, ok := d.sessions[target]; ok {
		return s
	}
	for _, s := range d.sessions {
		s.mu.Lock()
		name := s.name
		s.mu.Unlock()
		if name == target {
			return s
		}
	}
	return nil
}

func (d *Daemon) listSessions() []protocol.SessionInfo {
	d.mu.RLock()
	list := make([]*Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		list = append(list, s)
	}
	d.mu.RUnlock()

	infos := make([]protocol.SessionInfo, 0, len(list))
	for _, s := range list {
		infos = append(infos, s.Info())
	}
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].LastActiveAt.Equal(infos[j].LastActiveAt) {
			return infos[i].LastActiveAt.After(infos[j].LastActiveAt)
		}
		return infos[i].CreatedAt.Before(infos[j].CreatedAt)
	})
	return infos
}

func (d *Daemon) allocateUniqueName(desired string) string {
	desired = strings.TrimSpace(desired)
	nameExists := func(cand string) bool {
		for _, s := range d.sessions {
			s.mu.Lock()
			n := s.name
			s.mu.Unlock()
			if n == cand {
				return true
			}
		}
		return false
	}

	if desired == "" {
		for i := 1; ; i++ {
			cand := strconv.Itoa(i)
			if i == 1 && !nameExists("main") {
				return "main"
			}
			if !nameExists(cand) {
				return cand
			}
		}
	}
	if !nameExists(desired) {
		return desired
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", desired, i)
		if !nameExists(cand) {
			return cand
		}
	}
}

func (d *Daemon) handleConn(conn net.Conn) {
	cliNum := atomic.AddUint64(&d.nextCliNum, 1)
	clientID := fmt.Sprintf("c%d", cliNum)

	c := newClientConn(clientID, conn)
	d.mu.Lock()
	d.clients[clientID] = c
	d.mu.Unlock()

	defer func() {
		if sess := c.currentSession(); sess != nil {
			sess.Detach(c)
		}
		d.mu.Lock()
		delete(d.clients, c.id)
		shouldCheck := d.AutoExitOnEmpty && d.hadSession && len(d.sessions) == 0 && len(d.clients) == 0
		d.mu.Unlock()
		c.close()
		if shouldCheck {
			d.scheduleEmptyExit()
		}
	}()

	for {
		frame, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		d.dispatchFrame(c, frame)
	}
}

func (d *Daemon) dispatchFrame(c *ClientConn, frame protocol.Frame) {
	switch frame.Type {
	case protocol.MsgStdin:
		c.touch()
		if sess := c.currentSession(); sess != nil {
			_ = sess.WriteInput(frame.Payload)
		}

	case protocol.MsgResize:
		if sz, err := protocol.DecodeResize(frame.Payload); err == nil {
			c.setSize(sz)
			if sess := c.currentSession(); sess != nil {
				sess.Resize(sz)
			}
		}

	case protocol.MsgHello:
		var req protocol.HelloReq
		if err := protocol.DecodeJSON(frame.Payload, &req); err != nil {
			c.enqueueJSON(protocol.MsgHelloResp, protocol.HelloResp{Error: err.Error()})
			return
		}
		c.setHello(req)
		c.enqueueJSON(protocol.MsgHelloResp, protocol.HelloResp{ClientID: c.id})

	case protocol.MsgListReq:
		c.enqueueJSON(protocol.MsgListResp, protocol.ListResp{
			Sessions: d.listSessions(),
		})

	case protocol.MsgCreateReq:
		var req protocol.CreateReq
		if err := protocol.DecodeJSON(frame.Payload, &req); err != nil {
			c.enqueueJSON(protocol.MsgCreateResp, protocol.CreateResp{Error: err.Error()})
			return
		}
		d.mu.Lock()
		id := fmt.Sprintf("s%d", atomic.AddUint64(&d.nextSessNum, 1))
		name := d.allocateUniqueName(req.Name)
		d.mu.Unlock()

		sess, err := newSession(d, id, name, req, d.socketPath)
		if err != nil {
			c.enqueueJSON(protocol.MsgCreateResp, protocol.CreateResp{Error: err.Error()})
			return
		}

		d.mu.Lock()
		// Re-check uniqueness in case of concurrent creation with the same desired name.
		if unique := d.allocateUniqueName(name); unique != name {
			sess.Rename(unique)
		}
		d.sessions[id] = sess
		d.hadSession = true
		d.mu.Unlock()

		c.enqueueJSON(protocol.MsgCreateResp, protocol.CreateResp{Session: sess.Info()})

	case protocol.MsgAttachReq:
		var req protocol.AttachReq
		if err := protocol.DecodeJSON(frame.Payload, &req); err != nil {
			c.enqueueJSON(protocol.MsgAttachResp, protocol.AttachResp{Error: err.Error()})
			return
		}
		sess := d.findSession(req.Target)
		if sess == nil {
			c.enqueueJSON(protocol.MsgAttachResp, protocol.AttachResp{
				Error: fmt.Sprintf("session %q not found", req.Target),
			})
			return
		}
		// Detach from any previous session first.
		if prev := c.currentSession(); prev != nil && prev != sess {
			prev.Detach(c)
		}
		c.setSession(sess)
		if _, err := sess.Attach(c, req.Size, req.ReplayLines); err != nil {
			c.clearSession(sess)
			c.enqueueJSON(protocol.MsgAttachResp, protocol.AttachResp{Error: err.Error()})
			return
		}

	case protocol.MsgDetachReq:
		var req protocol.DetachReq
		_ = protocol.DecodeJSON(frame.Payload, &req)
		if req.ClientID != "" || req.SessionID != "" || !c.isInteractive() {
			target := d.findInteractiveClient(req.ClientID, req.SessionID, c.id)
			if target == nil {
				c.enqueueJSON(protocol.MsgDetachResp, protocol.DetachResp{Error: "no active interactive client found"})
				return
			}
			if sess := target.currentSession(); sess != nil {
				sess.Detach(target)
				target.clearSession(sess)
			}
			target.enqueueJSON(protocol.MsgDetached, protocol.DetachedEvent{Reason: "detached"})
			c.enqueueJSON(protocol.MsgDetachResp, protocol.DetachResp{})
			return
		}
		if sess := c.currentSession(); sess != nil {
			sess.Detach(c)
			c.clearSession(sess)
		}
		c.enqueueJSON(protocol.MsgDetachResp, protocol.DetachResp{})

	case protocol.MsgPauseReq:
		var req protocol.PauseReq
		_ = protocol.DecodeJSON(frame.Payload, &req)
		if sess := c.currentSession(); sess != nil {
			sess.SetPaused(c, req.Paused)
		}
		c.enqueueJSON(protocol.MsgPauseResp, protocol.PauseResp{})

	case protocol.MsgRestoreReq:
		var req protocol.RestoreReq
		_ = protocol.DecodeJSON(frame.Payload, &req)
		sess := c.currentSession()
		if sess == nil {
			c.enqueueJSON(protocol.MsgRestoreResp, protocol.RestoreResp{Error: "no attached session"})
			return
		}
		if err := sess.Restore(c, req); err != nil {
			c.enqueueJSON(protocol.MsgRestoreResp, protocol.RestoreResp{Error: err.Error()})
		}

	case protocol.MsgKillReq:
		var req protocol.KillReq
		if err := protocol.DecodeJSON(frame.Payload, &req); err != nil {
			c.enqueueJSON(protocol.MsgKillResp, protocol.KillResp{Error: err.Error()})
			return
		}
		var killed []string
		if req.All {
			d.mu.RLock()
			all := make([]*Session, 0, len(d.sessions))
			for _, s := range d.sessions {
				all = append(all, s)
			}
			d.mu.RUnlock()
			for _, s := range all {
				killed = append(killed, s.Info().Name)
				s.Kill()
			}
		} else {
			sess := d.findSession(req.Target)
			if sess == nil {
				c.enqueueJSON(protocol.MsgKillResp, protocol.KillResp{
					Error: fmt.Sprintf("session %q not found", req.Target),
				})
				return
			}
			killed = append(killed, sess.Info().Name)
			sess.Kill()
		}
		c.enqueueJSON(protocol.MsgKillResp, protocol.KillResp{Killed: killed})

	case protocol.MsgRenameReq:
		var req protocol.RenameReq
		if err := protocol.DecodeJSON(frame.Payload, &req); err != nil {
			c.enqueueJSON(protocol.MsgRenameResp, protocol.RenameResp{Error: err.Error()})
			return
		}
		newName := strings.TrimSpace(req.NewName)
		if newName == "" {
			c.enqueueJSON(protocol.MsgRenameResp, protocol.RenameResp{Error: "new name cannot be empty"})
			return
		}
		sess := d.findSession(req.Target)
		if sess == nil {
			c.enqueueJSON(protocol.MsgRenameResp, protocol.RenameResp{Error: fmt.Sprintf("session %q not found", req.Target)})
			return
		}
		if existing := d.findSession(newName); existing != nil && existing != sess {
			c.enqueueJSON(protocol.MsgRenameResp, protocol.RenameResp{Error: fmt.Sprintf("session %q already exists", newName)})
			return
		}
		sess.Rename(newName)
		c.enqueueJSON(protocol.MsgRenameResp, protocol.RenameResp{Session: sess.Info()})

	case protocol.MsgPreviewReq:
		var req protocol.PreviewReq
		if err := protocol.DecodeJSON(frame.Payload, &req); err != nil {
			c.enqueueJSON(protocol.MsgPreviewResp, protocol.PreviewResp{Error: err.Error()})
			return
		}
		sess := d.findSession(req.Target)
		if sess == nil {
			c.enqueueJSON(protocol.MsgPreviewResp, protocol.PreviewResp{Error: "session not found"})
			return
		}
		lines, inAlt := sess.Preview(req.Rows, req.Cols)
		c.enqueueJSON(protocol.MsgPreviewResp, protocol.PreviewResp{
			Lines:       lines,
			InAltScreen: inAlt,
		})

	case protocol.MsgTriggerSwitcher:
		var req protocol.TriggerSwitcherReq
		_ = protocol.DecodeJSON(frame.Payload, &req)
		target := d.findInteractiveClient(req.ClientID, req.SessionID, c.id)
		if target == nil {
			c.enqueueJSON(protocol.MsgTriggerSwitcherResp, protocol.TriggerSwitcherResp{
				Error: "no active interactive client found",
			})
			return
		}
		target.enqueueJSON(protocol.MsgOpenSwitcher, protocol.OpenSwitcherEvent{Reason: "external"})
		c.enqueueJSON(protocol.MsgTriggerSwitcherResp, protocol.TriggerSwitcherResp{})
	}
}

func (d *Daemon) findInteractiveClient(clientID, sessionID, excludeID string) *ClientConn {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if clientID != "" {
		if c, ok := d.clients[clientID]; ok && c.isInteractive() {
			return c
		}
	}
	if sessionID != "" {
		for _, c := range d.clients {
			if c.id == excludeID || !c.isInteractive() {
				continue
			}
			if sess := c.currentSession(); sess != nil && sess.id == sessionID {
				return c
			}
		}
	}
	// Otherwise pick the most recently active interactive client.
	var best *ClientConn
	var bestTime time.Time
	for _, c := range d.clients {
		if c.id == excludeID || !c.isInteractive() {
			continue
		}
		t := c.lastActive()
		if best == nil || t.After(bestTime) {
			best = c
			bestTime = t
		}
	}
	return best
}

// ClientConn wraps a Unix socket client with a non-blocking, strictly FIFO write queue
// so session PTY reads and atomic attach replays never block or reorder.
type ClientConn struct {
	id   string
	conn net.Conn

	stateMu     sync.Mutex
	sess        *Session
	interactive bool
	size        protocol.WinSize
	lastAct     time.Time

	writeMu    sync.Mutex
	writeCond  *sync.Cond
	writeQueue []protocol.Frame
	queueBytes int
	closed     bool
}

func newClientConn(id string, conn net.Conn) *ClientConn {
	c := &ClientConn{
		id:      id,
		conn:    conn,
		lastAct: time.Now(),
	}
	c.writeCond = sync.NewCond(&c.writeMu)
	go c.writeLoop()
	return c
}

func (c *ClientConn) setHello(req protocol.HelloReq) {
	c.stateMu.Lock()
	c.interactive = req.Interactive
	c.size = req.Size
	c.lastAct = time.Now()
	c.stateMu.Unlock()
}

func (c *ClientConn) isInteractive() bool {
	c.stateMu.Lock()
	v := c.interactive
	c.stateMu.Unlock()
	return v
}

func (c *ClientConn) lastActive() time.Time {
	c.stateMu.Lock()
	t := c.lastAct
	c.stateMu.Unlock()
	return t
}

func (c *ClientConn) touch() {
	c.stateMu.Lock()
	c.lastAct = time.Now()
	c.stateMu.Unlock()
}

func (c *ClientConn) setSize(sz protocol.WinSize) {
	c.stateMu.Lock()
	c.size = sz
	c.lastAct = time.Now()
	c.stateMu.Unlock()
}

func (c *ClientConn) currentSession() *Session {
	c.stateMu.Lock()
	s := c.sess
	c.stateMu.Unlock()
	return s
}

func (c *ClientConn) setSession(s *Session) {
	c.stateMu.Lock()
	c.sess = s
	c.lastAct = time.Now()
	c.stateMu.Unlock()
}

func (c *ClientConn) clearSession(s *Session) {
	c.stateMu.Lock()
	if c.sess == s {
		c.sess = nil
	}
	c.stateMu.Unlock()
}

func (c *ClientConn) enqueueFrame(msgType byte, payload []byte) {
	c.writeMu.Lock()
	if c.closed {
		c.writeMu.Unlock()
		return
	}
	if c.queueBytes+len(payload) > maxClientQueueBytes && msgType == protocol.MsgStdout {
		// Protect daemon memory if a client hangs completely.
		c.writeMu.Unlock()
		c.close()
		return
	}
	c.writeQueue = append(c.writeQueue, protocol.Frame{Type: msgType, Payload: payload})
	c.queueBytes += len(payload)
	c.writeCond.Signal()
	c.writeMu.Unlock()
}

func (c *ClientConn) enqueueJSON(msgType byte, v any) {
	data, err := marshalJSON(v)
	if err != nil {
		return
	}
	c.enqueueFrame(msgType, data)
}

func (c *ClientConn) writeLoop() {
	for {
		c.writeMu.Lock()
		for len(c.writeQueue) == 0 && !c.closed {
			c.writeCond.Wait()
		}
		if c.closed && len(c.writeQueue) == 0 {
			c.writeMu.Unlock()
			return
		}
		batch := c.writeQueue
		c.writeQueue = nil
		c.queueBytes = 0
		c.writeMu.Unlock()

		for _, f := range batch {
			if err := protocol.WriteFrame(c.conn, f.Type, f.Payload); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *ClientConn) close() {
	c.writeMu.Lock()
	if !c.closed {
		c.closed = true
		c.writeCond.Broadcast()
		_ = c.conn.Close()
	}
	c.writeMu.Unlock()
}

func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
