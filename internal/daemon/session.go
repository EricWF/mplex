package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/EricWF/mplex/internal/protocol"
	"github.com/EricWF/mplex/internal/vt"
)

const defaultScrollback = 10000

// Session owns a single persistent PTY shell/process and its shadow VT emulator.
type Session struct {
	id         string
	daemon     *Daemon
	ptmx       *os.File
	cmd        *exec.Cmd
	createdAt  time.Time
	initialCwd string

	mu              sync.Mutex
	name            string
	vt              *vt.Terminal
	size            protocol.WinSize
	client          *ClientConn
	paused          bool
	pausedGen       uint64
	wasInAltAtPause bool
	lastActiveAt    time.Time
	exited          bool
	exitCode        int

	queryReplyCh chan []byte
	done         chan struct{}
	closeOnce    sync.Once
}

func newSession(d *Daemon, id, name string, req protocol.CreateReq, socketPath string) (*Session, error) {
	rows := int(req.Size.Rows)
	cols := int(req.Size.Cols)
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	scrollback := req.Scrollback
	if scrollback <= 0 {
		scrollback = defaultScrollback
	}

	var argv []string
	if len(req.Cmd) > 0 {
		argv = req.Cmd
	} else {
		shell := req.Shell
		if shell == "" {
			shell = os.Getenv("SHELL")
		}
		if shell == "" {
			shell = "/bin/sh"
		}
		argv = []string{shell}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cwd := req.Cwd
	if cwd == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = home
		}
	}
	if cwd != "" {
		if st, err := os.Stat(cwd); err == nil && st.IsDir() {
			cmd.Dir = cwd
		}
	}

	env := buildSessionEnv(req.Env, id, name, socketPath)
	cmd.Env = env

	winSize := &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
		X:    req.Size.XPixel,
		Y:    req.Size.YPixel,
	}

	ptmx, err := pty.StartWithSize(cmd, winSize)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	now := time.Now()
	s := &Session{
		id:           id,
		daemon:       d,
		ptmx:         ptmx,
		cmd:          cmd,
		createdAt:    now,
		initialCwd:   cwd,
		name:         name,
		vt:           vt.New(rows, cols, scrollback),
		size:         protocol.WinSize{Rows: uint16(rows), Cols: uint16(cols), XPixel: req.Size.XPixel, YPixel: req.Size.YPixel},
		lastActiveAt: now,
		queryReplyCh: make(chan []byte, 64),
		done:         make(chan struct{}),
	}

	go s.queryReplyLoop()
	go s.readLoop()
	return s, nil
}

func buildSessionEnv(clientEnv []string, id, name, socketPath string) []string {
	base := clientEnv
	if len(base) == 0 {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+3)
	for _, kv := range base {
		if strings.HasPrefix(kv, "MPLEX_SESSION=") ||
			strings.HasPrefix(kv, "MPLEX_SESSION_ID=") ||
			strings.HasPrefix(kv, "MPLEX_SOCKET=") ||
			strings.HasPrefix(kv, "MPLEX_CLIENT_ID=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"MPLEX_SESSION="+name,
		"MPLEX_SESSION_ID="+id,
		"MPLEX_SOCKET="+socketPath,
	)
	return out
}

func (s *Session) queryReplyLoop() {
	for {
		select {
		case <-s.done:
			return
		case reply := <-s.queryReplyCh:
			if len(reply) > 0 {
				_, _ = s.ptmx.Write(reply)
			}
		}
	}
}

func (s *Session) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			s.mu.Lock()
			if s.client == nil {
				s.vt.OnQueryReply = func(reply []byte) {
					cp := append([]byte(nil), reply...)
					select {
					case s.queryReplyCh <- cp:
					default:
					}
				}
			} else {
				s.vt.OnQueryReply = nil
			}
			s.vt.Write(chunk)
			s.lastActiveAt = time.Now()
			client := s.client
			paused := s.paused
			if client != nil && !paused {
				client.enqueueFrame(protocol.MsgStdout, chunk)
			}
			s.mu.Unlock()
		}
		if err != nil {
			// On Linux PTY masters, EIO indicates the slave side closed (normal process exit).
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
				break
			}
			break
		}
	}

	exitCode := 0
	if err := s.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.ptmx.Close()
	})

	s.mu.Lock()
	s.exited = true
	s.exitCode = exitCode
	client := s.client
	name := s.name
	s.client = nil
	s.mu.Unlock()

	if client != nil {
		client.clearSession(s)
		client.enqueueJSON(protocol.MsgSessionExited, protocol.SessionExitedEvent{
			SessionID:   s.id,
			SessionName: name,
			ExitCode:    exitCode,
		})
	}

	s.daemon.removeSession(s.id)
}

// WriteInput writes raw bytes from the attached client directly to the PTY.
func (s *Session) WriteInput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	s.vt.SnoopClientInput(data)
	s.lastActiveAt = time.Now()
	s.mu.Unlock()

	_, err := s.ptmx.Write(data)
	return err
}

// Resize updates PTY window size (including pixel dimensions) and reflows the shadow VT.
func (s *Session) Resize(sz protocol.WinSize) {
	if sz.Rows == 0 || sz.Cols == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resizeLocked(sz)
}

func (s *Session) resizeLocked(sz protocol.WinSize) {
	if sz.Rows == 0 || sz.Cols == 0 {
		return
	}
	if sz == s.size {
		return
	}
	s.size = sz
	_ = pty.Setsize(s.ptmx, &pty.Winsize{
		Rows: sz.Rows,
		Cols: sz.Cols,
		X:    sz.XPixel,
		Y:    sz.YPixel,
	})
	s.vt.Resize(int(sz.Rows), int(sz.Cols))
}

// Attach atomically attaches client to this session, resizes if needed, builds
// the full synchronized replay, and enqueues MsgAttachResp + MsgReplay strictly
// before any subsequent MsgStdout frames.
func (s *Session) Attach(c *ClientConn, sz protocol.WinSize, replayLines int) (protocol.SessionInfo, error) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return protocol.SessionInfo{}, errors.New("session has exited")
	}

	prevClient := s.client
	if prevClient != nil && prevClient != c {
		s.client = nil
		s.paused = false
		prevClient.clearSession(s)
		prevClient.enqueueJSON(protocol.MsgDetached, protocol.DetachedEvent{
			Reason: fmt.Sprintf("session %q attached by another client", s.name),
		})
	}

	if sz.Rows > 0 && sz.Cols > 0 {
		s.resizeLocked(sz)
	}

	if replayLines <= 0 {
		replayLines = defaultScrollback
	}
	replay := s.vt.BuildReplay(replayLines)
	inAlt := s.vt.InAltScreen
	s.client = c
	s.paused = false
	s.vt.OnQueryReply = nil
	s.lastActiveAt = time.Now()
	info := s.infoLocked()

	c.enqueueJSON(protocol.MsgAttachResp, protocol.AttachResp{
		Session:     info,
		InAltScreen: inAlt,
	})
	c.enqueueFrame(protocol.MsgReplay, replay)
	s.mu.Unlock()

	return info, nil
}

// Detach disconnects client if it is currently attached to this session.
func (s *Session) Detach(c *ClientConn) {
	s.mu.Lock()
	if s.client == c {
		s.client = nil
		s.paused = false
	}
	s.mu.Unlock()
}

// SetPaused pauses/unpauses live stdout streaming while the client displays the switcher UI.
func (s *Session) SetPaused(c *ClientConn, paused bool) {
	s.mu.Lock()
	if s.client == c {
		if paused && !s.paused {
			s.paused = true
			s.pausedGen = s.vt.Generation
			s.wasInAltAtPause = s.vt.InAltScreen
		} else if !paused {
			s.paused = false
		}
	}
	s.mu.Unlock()
}

// Restore resumes live streaming after canceling the switcher. If the screen did
// not change and is on the main buffer, emits only mode restoration (zero redraw).
// Otherwise atomically emits a full synchronized replay.
func (s *Session) Restore(c *ClientConn, req protocol.RestoreReq) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.exited {
		return errors.New("session has exited")
	}
	if s.client != c {
		return errors.New("client is not attached to this session")
	}

	sizeChanged := req.Size.Rows > 0 && req.Size.Cols > 0 && req.Size != s.size
	if sizeChanged {
		s.resizeLocked(req.Size)
	}

	needReplay := req.ForceReplay ||
		sizeChanged ||
		s.vt.InAltScreen ||
		s.wasInAltAtPause ||
		s.vt.Generation != s.pausedGen

	s.paused = false
	s.lastActiveAt = time.Now()

	var payload []byte
	if needReplay {
		lines := req.ReplayLines
		if lines <= 0 {
			lines = defaultScrollback
		}
		payload = s.vt.BuildReplay(lines)
	} else {
		payload = s.vt.BuildModeRestore()
	}

	c.enqueueJSON(protocol.MsgRestoreResp, protocol.RestoreResp{
		Replayed:    needReplay,
		InAltScreen: s.vt.InAltScreen,
	})
	c.enqueueFrame(protocol.MsgReplay, payload)
	return nil
}

// Preview returns ANSI-styled preview lines of the session's active screen.
func (s *Session) Preview(rows, cols int) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vt.RenderPreview(rows, cols), s.vt.InAltScreen
}

// Capture extracts the session's screen (and optionally scrollback) as plain or ANSI lines.
func (s *Session) Capture(req protocol.CaptureReq) (protocol.SessionInfo, []string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := s.vt.Capture(vt.CaptureOptions{
		IncludeScrollback: req.IncludeScrollback,
		ANSI:              req.ANSI,
		JoinWrapped:       !req.RawWrap,
		MaxLines:          req.Lines,
	})
	return s.infoLocked(), lines, s.vt.InAltScreen
}

// Info returns a snapshot of session metadata, enriched with live Linux /proc foreground info.
func (s *Session) Info() protocol.SessionInfo {
	s.mu.Lock()
	info := s.infoLocked()
	s.mu.Unlock()
	return info
}

func (s *Session) infoLocked() protocol.SessionInfo {
	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	fgCmd, procCwd := inspectForegroundProc(s.ptmx, pid)
	cwd := procCwd
	if cwd == "" {
		cwd = s.vt.Cwd
	}
	if cwd == "" {
		cwd = s.initialCwd
	}

	var attachedBy string
	if s.client != nil {
		attachedBy = s.client.id
	}

	return protocol.SessionInfo{
		ID:           s.id,
		Name:         s.name,
		PID:          pid,
		FgCommand:    fgCmd,
		Cwd:          cwd,
		Title:        s.vt.Title,
		Rows:         s.vt.Rows(),
		Cols:         s.vt.Cols(),
		InAltScreen:  s.vt.InAltScreen,
		Attached:     s.client != nil,
		AttachedBy:   attachedBy,
		CreatedAt:    s.createdAt,
		LastActiveAt: s.lastActiveAt,
		Scrollback:   len(s.vt.Main.Scrollback),
	}
}

func (s *Session) Rename(newName string) {
	s.mu.Lock()
	s.name = newName
	s.mu.Unlock()
}

// Kill terminates the session's process group cleanly.
func (s *Session) Kill() {
	if s.cmd != nil && s.cmd.Process != nil {
		// Send SIGHUP to the entire foreground/session process group.
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGHUP)
		_ = s.cmd.Process.Signal(syscall.SIGHUP)
	}
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.ptmx.Close()
	})
}
