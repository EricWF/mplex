package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Message types for the length-prefixed binary Unix socket protocol.
// Frame layout: [1B type][4B big-endian length][payload].
const (
	// Fast-path binary frames (zero JSON overhead).
	MsgStdin  byte = 1
	MsgStdout byte = 2
	MsgReplay byte = 3
	MsgResize byte = 4

	// Control / RPC frames (JSON payloads).
	MsgHello               byte = 10
	MsgHelloResp           byte = 11
	MsgListReq             byte = 12
	MsgListResp            byte = 13
	MsgCreateReq           byte = 14
	MsgCreateResp          byte = 15
	MsgAttachReq           byte = 16
	MsgAttachResp          byte = 17
	MsgDetachReq           byte = 18
	MsgDetachResp          byte = 19
	MsgKillReq             byte = 20
	MsgKillResp            byte = 21
	MsgRenameReq           byte = 22
	MsgRenameResp          byte = 23
	MsgPreviewReq          byte = 24
	MsgPreviewResp         byte = 25
	MsgTriggerSwitcher     byte = 26
	MsgTriggerSwitcherResp byte = 27
	MsgOpenSwitcher        byte = 28
	MsgSessionExited       byte = 29
	MsgDetached            byte = 30
	MsgRestoreReq          byte = 31
	MsgRestoreResp         byte = 32
	MsgPauseReq            byte = 33
	MsgPauseResp           byte = 34
	MsgCaptureReq          byte = 35
	MsgCaptureResp         byte = 36
	MsgSendReq             byte = 37
	MsgSendResp            byte = 38
	MsgSwitchSession       byte = 39
)

// MaxFrameSize guards against malformed frames (64 MiB allows huge scrollback replays).
const MaxFrameSize = 64 * 1024 * 1024

// Frame is a single binary message over the daemon/client Unix socket.
type Frame struct {
	Type    byte
	Payload []byte
}

// WriteFrame writes one framed message atomically into w.
func WriteFrame(w io.Writer, msgType byte, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var hdr [5]byte
	hdr[0] = msgType
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if err := writeFull(w, hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if err := writeFull(w, payload); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON marshals v as JSON and writes it as a frame.
func WriteJSON(w io.Writer, msgType byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, msgType, data)
}

// ReadFrame reads a single length-prefixed frame from r.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > MaxFrameSize {
		return Frame{}, fmt.Errorf("incoming frame too large: %d", length)
	}
	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Type: hdr[0], Payload: payload}, nil
}

// DecodeJSON unmarshals a frame's payload into dst.
func DecodeJSON(payload []byte, dst any) error {
	if len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, dst)
}

// EncodeResize encodes terminal dimensions (cells + pixels) into 8 bytes.
func EncodeResize(sz WinSize) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint16(buf[0:2], sz.Rows)
	binary.BigEndian.PutUint16(buf[2:4], sz.Cols)
	binary.BigEndian.PutUint16(buf[4:6], sz.XPixel)
	binary.BigEndian.PutUint16(buf[6:8], sz.YPixel)
	return buf[:]
}

// DecodeResize decodes 8-byte resize payload.
func DecodeResize(payload []byte) (WinSize, error) {
	if len(payload) != 8 {
		return WinSize{}, errors.New("invalid resize payload length")
	}
	return WinSize{
		Rows:   binary.BigEndian.Uint16(payload[0:2]),
		Cols:   binary.BigEndian.Uint16(payload[2:4]),
		XPixel: binary.BigEndian.Uint16(payload[4:6]),
		YPixel: binary.BigEndian.Uint16(payload[6:8]),
	}, nil
}

func writeFull(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	return nil
}

// WinSize mirrors struct winsize (rows, cols, pixel width, pixel height).
type WinSize struct {
	Rows   uint16 `json:"rows"`
	Cols   uint16 `json:"cols"`
	XPixel uint16 `json:"xpixel"`
	YPixel uint16 `json:"ypixel"`
}

// SessionInfo summarizes a running session for listing and the switcher UI.
type SessionInfo struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	PID          int       `json:"pid"`
	FgCommand    string    `json:"fg_command"`
	Cwd          string    `json:"cwd"`
	Title        string    `json:"title"`
	Rows         int       `json:"rows"`
	Cols         int       `json:"cols"`
	InAltScreen  bool      `json:"in_alt_screen"`
	Attached     bool      `json:"attached"`
	AttachedBy   string    `json:"attached_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	Scrollback   int       `json:"scrollback_lines"`
	UnreadOutput bool      `json:"unread_output,omitempty"`
	HasBell      bool      `json:"has_bell,omitempty"`
}

type HelloReq struct {
	ClientID    string  `json:"client_id"`
	Size        WinSize `json:"size"`
	Term        string  `json:"term"`
	ColorTerm   string  `json:"colorterm"`
	ReplayLines int     `json:"replay_lines"`
	Interactive bool    `json:"interactive"`
}

type HelloResp struct {
	ClientID string `json:"client_id"`
	Error    string `json:"error,omitempty"`
}

type ListReq struct{}

type ListResp struct {
	Sessions []SessionInfo `json:"sessions"`
	Error    string        `json:"error,omitempty"`
}

type CreateReq struct {
	Name       string   `json:"name,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	Shell      string   `json:"shell,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	Size       WinSize  `json:"size"`
	Scrollback int      `json:"scrollback,omitempty"`
	NamingMode string   `json:"naming_mode,omitempty"`
}

type CreateResp struct {
	Session SessionInfo `json:"session"`
	Error   string      `json:"error,omitempty"`
}

type AttachReq struct {
	Target      string  `json:"target"`
	Size        WinSize `json:"size"`
	ReplayLines int     `json:"replay_lines"`
}

type AttachResp struct {
	Session     SessionInfo `json:"session"`
	InAltScreen bool        `json:"in_alt_screen"`
	Error       string      `json:"error,omitempty"`
}

type DetachReq struct {
	ClientID  string `json:"client_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type DetachResp struct {
	Error string `json:"error,omitempty"`
}

type KillReq struct {
	Target string `json:"target,omitempty"`
	All    bool   `json:"all,omitempty"`
}

type KillResp struct {
	Killed []string `json:"killed,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type RenameReq struct {
	Target  string `json:"target"`
	NewName string `json:"new_name"`
}

type RenameResp struct {
	Session SessionInfo `json:"session"`
	Error   string      `json:"error,omitempty"`
}

type PreviewReq struct {
	Target       string `json:"target"`
	Rows         int    `json:"rows"`
	Cols         int    `json:"cols"`
	ScrollOffset int    `json:"scroll_offset,omitempty"`
}

type PreviewResp struct {
	Lines        []string `json:"lines"`
	InAltScreen  bool     `json:"in_alt_screen"`
	TotalLines   int      `json:"total_lines,omitempty"`
	ScrollOffset int      `json:"scroll_offset,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type TriggerSwitcherReq struct {
	ClientID  string `json:"client_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Target    string `json:"target,omitempty"` // if set, switch client directly to this session
}

type TriggerSwitcherResp struct {
	Client  string       `json:"client,omitempty"`
	Session *SessionInfo `json:"session,omitempty"`
	Error   string       `json:"error,omitempty"`
}

type OpenSwitcherEvent struct {
	Reason string `json:"reason,omitempty"`
}

type SwitchSessionEvent struct {
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
}

type SessionExitedEvent struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	ExitCode    int    `json:"exit_code"`
}

type DetachedEvent struct {
	Reason string `json:"reason,omitempty"`
}

type RestoreReq struct {
	Size        WinSize `json:"size"`
	ReplayLines int     `json:"replay_lines"`
	ForceReplay bool    `json:"force_replay"`
}

type RestoreResp struct {
	Replayed    bool         `json:"replayed"`
	InAltScreen bool         `json:"in_alt_screen"`
	Session     *SessionInfo `json:"session,omitempty"`
	Error       string       `json:"error,omitempty"`
}

type PauseReq struct {
	Paused bool `json:"paused"`
}

type PauseResp struct {
	Error string `json:"error,omitempty"`
}

type CaptureReq struct {
	Target            string `json:"target"`
	Lines             int    `json:"lines,omitempty"`
	IncludeScrollback bool   `json:"include_scrollback,omitempty"`
	ANSI              bool   `json:"ansi,omitempty"`
	RawWrap           bool   `json:"raw_wrap,omitempty"`
}

type CaptureResp struct {
	Session     SessionInfo `json:"session"`
	Lines       []string    `json:"lines"`
	Text        string      `json:"text"`
	InAltScreen bool        `json:"in_alt_screen"`
	Error       string      `json:"error,omitempty"`
}

type SendReq struct {
	Target string `json:"target"`
	Data   []byte `json:"data"`
}

type SendResp struct {
	Session SessionInfo `json:"session"`
	Bytes   int         `json:"bytes"`
	Error   string      `json:"error,omitempty"`
}
