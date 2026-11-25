package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/protocol"
)

func (s *Server) toolDefinitions() []ToolDef {
	return []ToolDef{
		{
			Name:        "list_sessions",
			Description: "List all active mplex terminal sessions with their metadata (id, name, cwd, fg_command, attached status, scrollback lines, last_active_at).",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		{
			Name:        "create_session",
			Description: "Create a new detached mplex terminal session, automatically starting the daemon if needed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Optional name for the new session (a unique name is allocated if omitted or already taken).",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Optional starting working directory for the session.",
					},
					"cmd": map[string]any{
						"type":        "array",
						"description": "Optional command and arguments to run instead of the default login shell (e.g. [\"/bin/sh\", \"-c\", \"npm run dev\"]).",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        "capture_session",
			Description: "Capture the current visible screen (and optionally scrollback history) of a running mplex session as plain text or ANSI-styled lines.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "Session name or ID to capture.",
					},
					"lines": map[string]any{
						"type":        "integer",
						"description": "Optional maximum number of trailing lines to return (0 returns all lines).",
						"minimum":     0,
					},
					"scrollback": map[string]any{
						"type":        "boolean",
						"description": "Include scrollback buffer history before the visible screen.",
					},
					"ansi": map[string]any{
						"type":        "boolean",
						"description": "Preserve ANSI color and style escape sequences in output.",
					},
				},
				"required":             []string{"target"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "send_input",
			Description: "Send text and/or special keys (such as enter, ctrl-c, ctrl-d, esc, tab, up, down) to a running mplex session without attaching or disrupting the user's window.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "Session name or ID to receive the input.",
					},
					"text": map[string]any{
						"type":        "string",
						"description": "Literal text to write to the session's PTY.",
					},
					"keys": map[string]any{
						"type":        "array",
						"description": "Optional list of special key names to send after text (e.g. [\"ctrl-c\"], [\"up\", \"enter\"]).",
						"items": map[string]any{
							"type": "string",
						},
					},
					"enter": map[string]any{
						"type":        "boolean",
						"description": "Append a carriage return (Enter) after text and keys.",
					},
				},
				"required":             []string{"target"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "switch_session",
			Description: "Switch an attached interactive mplex terminal window to display the target session.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "Session name or ID to switch the interactive client to.",
					},
					"client_id": map[string]any{
						"type":        "string",
						"description": "Optional specific interactive client ID to target (defaults to the most recently active interactive client).",
					},
				},
				"required":             []string{"target"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "wait_session",
			Description: "Wait for a mplex session to match a regular expression, become idle for a duration, or exit. At least one of match, idle, or exit is required.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "Session name or ID to wait on.",
					},
					"match": map[string]any{
						"type":        "string",
						"description": "Regular expression to wait for in the session's captured output.",
					},
					"idle": map[string]any{
						"type":        "string",
						"description": "Go duration string (e.g. \"2s\", \"500ms\") to wait until the session has no new activity.",
					},
					"exit": map[string]any{
						"type":        "boolean",
						"description": "Wait until the session exits.",
					},
					"scrollback": map[string]any{
						"type":        "boolean",
						"description": "Include scrollback history when matching the regex.",
					},
					"timeout": map[string]any{
						"type":        "string",
						"description": "Maximum duration to wait as a Go duration string (default \"30s\").",
					},
				},
				"required":             []string{"target"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "rename_session",
			Description: "Rename an existing mplex session.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "Current session name or ID.",
					},
					"new_name": map[string]any{
						"type":        "string",
						"description": "New unique name for the session.",
					},
				},
				"required":             []string{"target", "new_name"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "kill_session",
			Description: "Terminate one mplex session by name/ID or kill all active sessions.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "Session name or ID to kill (required unless all is true).",
					},
					"all": map[string]any{
						"type":        "boolean",
						"description": "If true, terminate all active sessions.",
					},
				},
				"additionalProperties": false,
			},
		},
	}
}

func (s *Server) dispatchTool(ctx context.Context, name string, rawArgs json.RawMessage) (CallToolResult, *rpcError) {
	switch name {
	case "list_sessions":
		return s.toolListSessions(rawArgs), nil
	case "create_session":
		return s.toolCreateSession(rawArgs), nil
	case "capture_session":
		return s.toolCaptureSession(rawArgs), nil
	case "send_input":
		return s.toolSendInput(rawArgs), nil
	case "switch_session":
		return s.toolSwitchSession(rawArgs), nil
	case "wait_session":
		return s.toolWaitSession(ctx, rawArgs), nil
	case "rename_session":
		return s.toolRenameSession(rawArgs), nil
	case "kill_session":
		return s.toolKillSession(rawArgs), nil
	default:
		return CallToolResult{}, &rpcError{
			Code:    errCodeInvalidParams,
			Message: fmt.Sprintf("unknown tool %q", name),
		}
	}
}

func (s *Server) toolListSessions(rawArgs json.RawMessage) CallToolResult {
	var args struct{}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		return toolJSON([]protocol.SessionInfo{})
	}
	defer rc.Close()

	sessions, err := rc.List()
	if err != nil {
		return toolError(err.Error())
	}
	if sessions == nil {
		sessions = []protocol.SessionInfo{}
	}
	return toolJSON(sessions)
}

func (s *Server) toolCreateSession(rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Name string   `json:"name"`
		Cwd  string   `json:"cwd"`
		Cmd  []string `json:"cmd"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}

	startDir := args.Cwd
	if startDir == "" {
		startDir, _ = os.Getwd()
	}

	rc, err := client.DialOrStart(s.socketPath, nil)
	if err != nil {
		return toolError(err.Error())
	}
	defer rc.Close()

	sess, err := rc.Create(protocol.CreateReq{
		Name:       args.Name,
		Cwd:        startDir,
		Shell:      os.Getenv("SHELL"),
		Cmd:        args.Cmd,
		Env:        os.Environ(),
		Size:       protocol.WinSize{Rows: 24, Cols: 80},
		Scrollback: s.scrollback,
	})
	if err != nil {
		return toolError(err.Error())
	}
	return toolJSON(sess)
}

func (s *Server) toolCaptureSession(rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Target     string `json:"target"`
		Lines      int    `json:"lines"`
		Scrollback bool   `json:"scrollback"`
		ANSI       bool   `json:"ansi"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}
	if strings.TrimSpace(args.Target) == "" {
		return toolError("target is required")
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		return toolError(fmt.Sprintf("daemon not running: %v", err))
	}
	defer rc.Close()

	resp, err := rc.Capture(protocol.CaptureReq{
		Target:            args.Target,
		Lines:             args.Lines,
		IncludeScrollback: args.Scrollback,
		ANSI:              args.ANSI,
	})
	if err != nil {
		return toolError(err.Error())
	}
	if resp.Lines == nil {
		resp.Lines = []string{}
	}
	return toolJSON(resp)
}

func (s *Server) toolSendInput(rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Target string   `json:"target"`
		Text   string   `json:"text"`
		Keys   []string `json:"keys"`
		Enter  bool     `json:"enter"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}
	if strings.TrimSpace(args.Target) == "" {
		return toolError("target is required")
	}

	var payload bytes.Buffer
	if args.Text != "" {
		payload.WriteString(args.Text)
	}
	for _, ks := range args.Keys {
		b, err := client.ParseSendKey(ks)
		if err != nil {
			return toolError(err.Error())
		}
		payload.Write(b)
	}
	if args.Enter {
		payload.WriteByte('\r')
	}
	if payload.Len() == 0 {
		return toolError("nothing to send (provide text, keys, or enter=true)")
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		return toolError(fmt.Sprintf("daemon not running: %v", err))
	}
	defer rc.Close()

	resp, err := rc.SendToSession(args.Target, payload.Bytes())
	if err != nil {
		return toolError(err.Error())
	}
	return toolJSON(resp)
}

func (s *Server) toolSwitchSession(rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Target   string `json:"target"`
		ClientID string `json:"client_id"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}
	if strings.TrimSpace(args.Target) == "" {
		return toolError("target is required")
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		return toolError(fmt.Sprintf("daemon not running: %v", err))
	}
	defer rc.Close()

	resp, err := rc.SwitchSession(args.ClientID, os.Getenv("MPLEX_SESSION_ID"), args.Target)
	if err != nil {
		return toolError(err.Error())
	}
	return toolJSON(map[string]any{
		"switched": true,
		"client":   resp.Client,
		"session":  resp.Session,
	})
}

func (s *Server) toolWaitSession(ctx context.Context, rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Target     string `json:"target"`
		Match      string `json:"match"`
		Idle       string `json:"idle"`
		Exit       bool   `json:"exit"`
		Scrollback bool   `json:"scrollback"`
		Timeout    string `json:"timeout"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}
	if strings.TrimSpace(args.Target) == "" {
		return toolError("target is required")
	}

	var idleDur time.Duration
	if strings.TrimSpace(args.Idle) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(args.Idle))
		if err != nil {
			return toolError(fmt.Sprintf("invalid idle duration %q: %v", args.Idle, err))
		}
		if d <= 0 {
			return toolError("idle duration must be positive")
		}
		idleDur = d
	}

	timeoutDur := client.DefaultWaitTimeout
	if strings.TrimSpace(args.Timeout) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(args.Timeout))
		if err != nil {
			return toolError(fmt.Sprintf("invalid timeout duration %q: %v", args.Timeout, err))
		}
		if d <= 0 {
			return toolError("timeout duration must be positive")
		}
		timeoutDur = d
	}

	if args.Match == "" && idleDur == 0 && !args.Exit {
		return toolError("at least one wait condition (match, idle, or exit) is required")
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		if args.Exit {
			return toolJSON(client.WaitResult{
				Matched: true,
				Reason:  "exit",
			})
		}
		return toolError(fmt.Sprintf("daemon not running: %v", err))
	}
	defer rc.Close()

	res, err := client.Wait(ctx, rc, client.WaitOptions{
		Target:            args.Target,
		Match:             args.Match,
		Idle:              idleDur,
		Exit:              args.Exit,
		IncludeScrollback: args.Scrollback,
		Timeout:           timeoutDur,
	})
	if err != nil {
		if errors.Is(err, client.ErrWaitTimeout) {
			return toolJSONWithError(res)
		}
		return toolError(err.Error())
	}
	return toolJSON(res)
}

func (s *Server) toolRenameSession(rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Target  string `json:"target"`
		NewName string `json:"new_name"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}
	if strings.TrimSpace(args.Target) == "" || strings.TrimSpace(args.NewName) == "" {
		return toolError("target and new_name are required")
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		return toolError(fmt.Sprintf("daemon not running: %v", err))
	}
	defer rc.Close()

	sess, err := rc.Rename(args.Target, args.NewName)
	if err != nil {
		return toolError(err.Error())
	}
	return toolJSON(sess)
}

func (s *Server) toolKillSession(rawArgs json.RawMessage) CallToolResult {
	var args struct {
		Target string `json:"target"`
		All    bool   `json:"all"`
	}
	if err := decodeParams(rawArgs, &args); err != nil {
		return toolError(fmt.Sprintf("invalid arguments: %v", err))
	}
	if !args.All && strings.TrimSpace(args.Target) == "" {
		return toolError("target or all=true is required")
	}

	rc, err := client.Dial(s.socketPath, nil)
	if err != nil {
		return toolError(fmt.Sprintf("daemon not running: %v", err))
	}
	defer rc.Close()

	killed, err := rc.Kill(args.Target, args.All)
	if err != nil {
		return toolError(err.Error())
	}
	if killed == nil {
		killed = []string{}
	}
	return toolJSON(map[string]any{"killed": killed})
}

func toolJSON(v any) CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("marshal result: %v", err))
	}
	return CallToolResult{
		Content: []ToolContent{{Type: "text", Text: string(b)}},
	}
}

func toolJSONWithError(v any) CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("marshal result: %v", err))
	}
	return CallToolResult{
		Content: []ToolContent{{Type: "text", Text: string(b)}},
		IsError: true,
	}
}

func toolError(msg string) CallToolResult {
	return CallToolResult{
		Content: []ToolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}
