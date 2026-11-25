package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/EricWF/mplex/internal/daemon"
)

const defaultProtocolVersion = "2024-11-05"

// Standard JSON-RPC 2.0 error codes.
const (
	errCodeParseError     = -32700
	errCodeInvalidRequest = -32600
	errCodeMethodNotFound = -32601
	errCodeInvalidParams  = -32602
	errCodeInternalError  = -32603
)

// Options configures an MCP stdio Server.
type Options struct {
	SocketPath string
	Version    string
	ErrOut     io.Writer
	Scrollback int
}

// Server implements a Model Context Protocol (MCP) server over newline-delimited
// JSON-RPC 2.0 stdio streams backed by the mplex daemon.
type Server struct {
	socketPath string
	version    string
	errOut     io.Writer
	scrollback int
}

// NewServer constructs an MCP Server with the given options.
func NewServer(opts Options) *Server {
	sock := opts.SocketPath
	if sock == "" {
		sock = daemon.DefaultSocketPath()
	}
	ver := opts.Version
	if ver == "" {
		ver = "dev"
	}
	errOut := opts.ErrOut
	if errOut == nil {
		errOut = io.Discard
	}
	sb := opts.Scrollback
	if sb <= 0 {
		sb = 10000
	}
	return &Server{
		socketPath: sock,
		version:    ver,
		errOut:     errOut,
		scrollback: sb,
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToolDef describes a single MCP tool exposed via tools/list.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolContent represents a single content item returned by tools/call.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is the MCP result envelope returned by tools/call.
type CallToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Serve runs the JSON-RPC 2.0 MCP loop reading newline-delimited messages from
// in and writing newline-delimited JSON-RPC responses strictly to out.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	return s.ServeContext(context.Background(), in, out)
}

// ServeContext is like Serve but stops cleanly when ctx is canceled and passes
// ctx through to long-running tools such as wait_session.
func (s *Server) ServeContext(ctx context.Context, in io.Reader, out io.Writer) error {
	type readResult struct {
		line []byte
		err  error
	}

	lineCh := make(chan readResult, 1)
	stopRead := make(chan struct{})
	defer close(stopRead)

	go func() {
		defer close(lineCh)
		br := bufio.NewReader(in)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				cp := append([]byte(nil), line...)
				select {
				case lineCh <- readResult{line: cp}:
				case <-stopRead:
					return
				}
			}
			if err != nil {
				select {
				case lineCh <- readResult{err: err}:
				case <-stopRead:
				}
				return
			}
		}
	}()

	var writeMu sync.Mutex
	writeResp := func(resp rpcResponse) error {
		data, err := json.Marshal(resp)
		if err != nil {
			s.logf("marshal response: %v", err)
			return err
		}
		data = append(data, '\n')
		writeMu.Lock()
		_, err = out.Write(data)
		writeMu.Unlock()
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case rr, ok := <-lineCh:
			if !ok {
				return nil
			}
			if rr.err != nil {
				if errors.Is(rr.err, io.EOF) {
					return nil
				}
				return rr.err
			}

			trimmed := bytes.TrimSpace(rr.line)
			if len(trimmed) == 0 {
				continue
			}

			resp, shouldReply := s.handleMessage(ctx, trimmed)
			if shouldReply {
				if err := writeResp(resp); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Server) handleMessage(ctx context.Context, raw []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.logf("parse error: %v", err)
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error: &rpcError{
				Code:    errCodeParseError,
				Message: "parse error: " + err.Error(),
			},
		}, true
	}

	isNotification := len(bytes.TrimSpace(req.ID)) == 0

	if req.Method == "" || (req.JSONRPC != "" && req.JSONRPC != "2.0") {
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcError{
				Code:    errCodeInvalidRequest,
				Message: "invalid JSON-RPC 2.0 request",
			},
		}, true
	}

	switch req.Method {
	case "initialize":
		var params initializeParams
		if err := decodeParams(req.Params, &params); err != nil {
			if isNotification {
				return rpcResponse{}, false
			}
			return errorResp(req.ID, errCodeInvalidParams, "invalid initialize params: "+err.Error()), true
		}
		protoVer := params.ProtocolVersion
		if protoVer == "" {
			protoVer = defaultProtocolVersion
		}
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": protoVer,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "mplex",
					"version": s.version,
				},
			},
		}, true

	case "notifications/initialized":
		// Notification — no response required.
		return rpcResponse{}, false

	case "ping":
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}, true

	case "tools/list":
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": s.toolDefinitions(),
			},
		}, true

	case "tools/call":
		var params callToolParams
		if err := decodeParams(req.Params, &params); err != nil {
			if isNotification {
				return rpcResponse{}, false
			}
			return errorResp(req.ID, errCodeInvalidParams, "invalid tools/call params: "+err.Error()), true
		}
		if params.Name == "" {
			if isNotification {
				return rpcResponse{}, false
			}
			return errorResp(req.ID, errCodeInvalidParams, "tool name is required"), true
		}
		result, rpcErr := s.dispatchTool(ctx, params.Name, params.Arguments)
		if isNotification {
			return rpcResponse{}, false
		}
		if rpcErr != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   rpcErr,
			}, true
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}, true

	default:
		if isNotification {
			// Ignore unknown notifications per JSON-RPC 2.0.
			return rpcResponse{}, false
		}
		return errorResp(req.ID, errCodeMethodNotFound, fmt.Sprintf("method %q not found", req.Method)), true
	}
}

func errorResp(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: msg,
		},
	}
}

func decodeParams(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return json.Unmarshal(trimmed, dst)
}

func (s *Server) logf(format string, args ...any) {
	if s.errOut != nil {
		_, _ = fmt.Fprintf(s.errOut, "mplex mcp: "+format+"\n", args...)
	}
}
