package mcp_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EricWF/mplex/internal/client"
	"github.com/EricWF/mplex/internal/daemon"
	"github.com/EricWF/mplex/internal/mcp"
	"github.com/EricWF/mplex/internal/protocol"
)

func shortTempSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mplex-mcp-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "m.sock")
}

func waitDaemonReady(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rc, err := client.Dial(sock, nil); err == nil {
			_ = rc.Close()
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("daemon did not become ready at %s", sock)
}

type jsonRPCResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type mcpHarness struct {
	t      *testing.T
	inW    *io.PipeWriter
	outR   *bufio.Reader
	doneCh chan error
	nextID int
}

func startMCPHarness(t *testing.T, sock string) *mcpHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	var stderrBuf bytes.Buffer
	srv := mcp.NewServer(mcp.Options{
		SocketPath: sock,
		Version:    "test-1.0.0",
		ErrOut:     &stderrBuf,
	})

	doneCh := make(chan error, 1)
	go func() {
		err := srv.Serve(inR, outW)
		_ = outW.Close()
		doneCh <- err
	}()

	h := &mcpHarness{
		t:      t,
		inW:    inW,
		outR:   bufio.NewReader(outR),
		doneCh: doneCh,
	}
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case err := <-doneCh:
			if err != nil {
				t.Errorf("mcp Serve exited with error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("mcp Serve did not exit after stdin closed")
		}
	})
	return h
}

func (h *mcpHarness) sendRaw(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.inW, line+"\n"); err != nil {
		h.t.Fatalf("write to mcp stdin: %v", err)
	}
}

func (h *mcpHarness) readResp() jsonRPCResp {
	h.t.Helper()
	line, err := h.outR.ReadBytes('\n')
	if err != nil {
		h.t.Fatalf("read from mcp stdout: %v", err)
	}
	var resp jsonRPCResp
	if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
		h.t.Fatalf("invalid JSON-RPC response on stdout: %v\nraw: %s", err, line)
	}
	if resp.JSONRPC != "2.0" {
		h.t.Fatalf("expected jsonrpc=2.0, got %q", resp.JSONRPC)
	}
	return resp
}

func (h *mcpHarness) call(method string, params any) jsonRPCResp {
	h.t.Helper()
	h.nextID++
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      h.nextID,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		h.t.Fatalf("marshal request: %v", err)
	}
	h.sendRaw(string(b))
	return h.readResp()
}

func (h *mcpHarness) notify(method string, params any) {
	h.t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		h.t.Fatalf("marshal notification: %v", err)
	}
	h.sendRaw(string(b))
}

func (h *mcpHarness) callTool(name string, args any) mcp.CallToolResult {
	h.t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	resp := h.call("tools/call", params)
	if resp.Error != nil {
		h.t.Fatalf("tools/call %s returned JSON-RPC error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		h.t.Fatalf("unmarshal CallToolResult for %s: %v\nraw: %s", name, err, resp.Result)
	}
	return res
}

func TestMCPEndToEnd(t *testing.T) {
	sock := shortTempSock(t)
	d := daemon.New(sock)
	go func() {
		_ = d.Start()
	}()
	defer d.Close()
	waitDaemonReady(t, sock)

	h := startMCPHarness(t, sock)

	// 1. initialize
	initResp := h.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "0.1"},
	})
	if initResp.Error != nil {
		t.Fatalf("initialize error: %+v", initResp.Error)
	}
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if initResult.ProtocolVersion != "2024-11-05" {
		t.Fatalf("protocolVersion = %q, want 2024-11-05", initResult.ProtocolVersion)
	}
	if initResult.Capabilities.Tools == nil {
		t.Fatalf("expected capabilities.tools to be present")
	}
	if initResult.ServerInfo.Name != "mplex" || initResult.ServerInfo.Version != "test-1.0.0" {
		t.Fatalf("unexpected serverInfo: %+v", initResult.ServerInfo)
	}

	// 2. notifications/initialized (must produce NO response) + ping (next response must match ping ID)
	h.notify("notifications/initialized", nil)
	pingResp := h.call("ping", nil)
	if pingResp.Error != nil {
		t.Fatalf("ping error: %+v", pingResp.Error)
	}
	if string(bytes.TrimSpace(pingResp.Result)) != "{}" {
		t.Fatalf("ping result = %s, want {}", pingResp.Result)
	}

	// 3. tools/list
	listToolsResp := h.call("tools/list", nil)
	if listToolsResp.Error != nil {
		t.Fatalf("tools/list error: %+v", listToolsResp.Error)
	}
	var toolsList struct {
		Tools []mcp.ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(listToolsResp.Result, &toolsList); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	wantTools := map[string]bool{
		"list_sessions":   false,
		"create_session":  false,
		"capture_session": false,
		"send_input":      false,
		"switch_session":  false,
		"wait_session":    false,
		"rename_session":  false,
		"kill_session":    false,
	}
	for _, td := range toolsList.Tools {
		if _, ok := wantTools[td.Name]; ok {
			wantTools[td.Name] = true
		}
		if td.InputSchema["type"] != "object" {
			t.Errorf("tool %q inputSchema.type = %v, want object", td.Name, td.InputSchema["type"])
		}
	}
	for name, seen := range wantTools {
		if !seen {
			t.Fatalf("missing expected tool %q in tools/list", name)
		}
	}

	// 4. list_sessions (empty)
	lsEmpty := h.callTool("list_sessions", nil)
	if lsEmpty.IsError || len(lsEmpty.Content) != 1 {
		t.Fatalf("unexpected list_sessions result: %+v", lsEmpty)
	}
	var sessions []protocol.SessionInfo
	if err := json.Unmarshal([]byte(lsEmpty.Content[0].Text), &sessions); err != nil {
		t.Fatalf("unmarshal empty sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}

	// 5. create_session
	workDir := t.TempDir()
	createdRes := h.callTool("create_session", map[string]any{
		"name": "worker",
		"cwd":  workDir,
		"cmd":  []string{"/bin/sh"},
	})
	if createdRes.IsError {
		t.Fatalf("create_session error: %+v", createdRes)
	}
	var created protocol.SessionInfo
	if err := json.Unmarshal([]byte(createdRes.Content[0].Text), &created); err != nil {
		t.Fatalf("unmarshal created session: %v", err)
	}
	if created.Name != "worker" || created.ID == "" {
		t.Fatalf("unexpected created session: %+v", created)
	}

	// 6. send_input (text + enter)
	sendRes := h.callTool("send_input", map[string]any{
		"target": "worker",
		"text":   "i=1; while [ $i -le 30 ]; do echo MCP_LOG_$i; i=$((i+1)); done; echo MCP_READY:$MPLEX_SESSION",
		"enter":  true,
	})
	if sendRes.IsError {
		t.Fatalf("send_input error: %+v", sendRes)
	}
	var sendPayload protocol.SendResp
	if err := json.Unmarshal([]byte(sendRes.Content[0].Text), &sendPayload); err != nil {
		t.Fatalf("unmarshal send_input: %v", err)
	}
	if sendPayload.Bytes == 0 {
		t.Fatalf("expected >0 bytes sent")
	}

	// 7. wait_session (--match + --scrollback)
	waitMatchRes := h.callTool("wait_session", map[string]any{
		"target":     "worker",
		"match":      `MCP_READY:worker`,
		"scrollback": true,
		"timeout":    "5s",
	})
	if waitMatchRes.IsError {
		t.Fatalf("wait_session match error: %+v", waitMatchRes)
	}
	var waitMatch client.WaitResult
	if err := json.Unmarshal([]byte(waitMatchRes.Content[0].Text), &waitMatch); err != nil {
		t.Fatalf("unmarshal wait_session match: %v", err)
	}
	if !waitMatch.Matched || waitMatch.Reason != "match" || !strings.Contains(waitMatch.MatchLine, "MCP_READY:worker") {
		t.Fatalf("unexpected wait match result: %+v", waitMatch)
	}
	if !strings.Contains(waitMatch.Text, "MCP_LOG_1") {
		t.Fatalf("expected scrollback line MCP_LOG_1 in wait text:\n%s", waitMatch.Text)
	}

	// 8. capture_session (scrollback + lines limit)
	capRes := h.callTool("capture_session", map[string]any{
		"target":     "worker",
		"scrollback": true,
		"lines":      5,
	})
	if capRes.IsError {
		t.Fatalf("capture_session error: %+v", capRes)
	}
	var capPayload protocol.CaptureResp
	if err := json.Unmarshal([]byte(capRes.Content[0].Text), &capPayload); err != nil {
		t.Fatalf("unmarshal capture_session: %v", err)
	}
	if len(capPayload.Lines) == 0 || len(capPayload.Lines) > 5 {
		t.Fatalf("expected 1..5 captured lines, got %d", len(capPayload.Lines))
	}

	// 9. wait_session (idle)
	waitIdleRes := h.callTool("wait_session", map[string]any{
		"target":  "worker",
		"idle":    "120ms",
		"timeout": "3s",
	})
	if waitIdleRes.IsError {
		t.Fatalf("wait_session idle error: %+v", waitIdleRes)
	}
	var waitIdle client.WaitResult
	if err := json.Unmarshal([]byte(waitIdleRes.Content[0].Text), &waitIdle); err != nil {
		t.Fatalf("unmarshal wait_session idle: %v", err)
	}
	if !waitIdle.Matched || waitIdle.Reason != "idle" {
		t.Fatalf("unexpected wait idle result: %+v", waitIdle)
	}

	// 10. wait_session timeout returns isError=true with structured WaitResult
	waitTimeoutRes := h.callTool("wait_session", map[string]any{
		"target":  "worker",
		"match":   "THIS_WILL_NEVER_APPEAR",
		"timeout": "100ms",
	})
	if !waitTimeoutRes.IsError {
		t.Fatalf("expected isError=true on wait timeout, got %+v", waitTimeoutRes)
	}
	var waitTimeout client.WaitResult
	if err := json.Unmarshal([]byte(waitTimeoutRes.Content[0].Text), &waitTimeout); err != nil {
		t.Fatalf("unmarshal wait timeout result: %v", err)
	}
	if waitTimeout.Matched || waitTimeout.Reason != "timeout" {
		t.Fatalf("unexpected wait timeout payload: %+v", waitTimeout)
	}

	// 11. send_input with special keys
	keyRes := h.callTool("send_input", map[string]any{
		"target": "worker",
		"keys":   []string{"ctrl-c"},
	})
	if keyRes.IsError {
		t.Fatalf("send_input ctrl-c error: %+v", keyRes)
	}

	// 12. rename_session
	renRes := h.callTool("rename_session", map[string]any{
		"target":   "worker",
		"new_name": "api",
	})
	if renRes.IsError {
		t.Fatalf("rename_session error: %+v", renRes)
	}
	var renamed protocol.SessionInfo
	if err := json.Unmarshal([]byte(renRes.Content[0].Text), &renamed); err != nil {
		t.Fatalf("unmarshal rename_session: %v", err)
	}
	if renamed.Name != "api" {
		t.Fatalf("renamed.Name = %q, want api", renamed.Name)
	}

	// 13. list_sessions reflects renamed session
	lsRes := h.callTool("list_sessions", nil)
	if lsRes.IsError {
		t.Fatalf("list_sessions error: %+v", lsRes)
	}
	if err := json.Unmarshal([]byte(lsRes.Content[0].Text), &sessions); err != nil {
		t.Fatalf("unmarshal list_sessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "api" {
		t.Fatalf("expected single session 'api', got %+v", sessions)
	}

	// 14. switch_session triggers active interactive client
	interactiveClient, err := client.Dial(sock, nil)
	if err != nil {
		t.Fatalf("dial interactive client: %v", err)
	}
	defer interactiveClient.Close()
	if _, err := interactiveClient.Hello(protocol.HelloReq{
		Size:        protocol.WinSize{Rows: 24, Cols: 80},
		Term:        "xterm-256color",
		Interactive: true,
	}); err != nil {
		t.Fatalf("interactive hello: %v", err)
	}

	swRes := h.callTool("switch_session", map[string]any{
		"target": "api",
	})
	if swRes.IsError {
		t.Fatalf("switch_session error: %+v", swRes)
	}
	select {
	case ev := <-interactiveClient.SwitchSessionCh:
		if ev.Target != renamed.ID {
			t.Fatalf("SwitchSessionCh target = %q, want %q", ev.Target, renamed.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("interactive client did not receive MsgSwitchSession")
	}

	// 15. kill_session + wait_session(--exit)
	killRes := h.callTool("kill_session", map[string]any{
		"target": "api",
	})
	if killRes.IsError {
		t.Fatalf("kill_session error: %+v", killRes)
	}
	var killPayload struct {
		Killed []string `json:"killed"`
	}
	if err := json.Unmarshal([]byte(killRes.Content[0].Text), &killPayload); err != nil {
		t.Fatalf("unmarshal kill_session: %v", err)
	}
	if len(killPayload.Killed) != 1 || killPayload.Killed[0] != "api" {
		t.Fatalf("unexpected kill payload: %+v", killPayload)
	}

	waitExitRes := h.callTool("wait_session", map[string]any{
		"target":  "api",
		"exit":    true,
		"timeout": "3s",
	})
	if waitExitRes.IsError {
		t.Fatalf("wait_session exit error: %+v", waitExitRes)
	}
	var waitExit client.WaitResult
	if err := json.Unmarshal([]byte(waitExitRes.Content[0].Text), &waitExit); err != nil {
		t.Fatalf("unmarshal wait_session exit: %v", err)
	}
	if !waitExit.Matched || waitExit.Reason != "exit" {
		t.Fatalf("unexpected wait exit result: %+v", waitExit)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	sock := shortTempSock(t)
	h := startMCPHarness(t, sock)

	// Invalid JSON -> parse error -32700
	h.sendRaw("{not valid json")
	parseResp := h.readResp()
	if parseResp.Error == nil || parseResp.Error.Code != -32700 {
		t.Fatalf("expected -32700 parse error, got %+v", parseResp)
	}

	// Unknown method -> -32601
	unknownMethod := h.call("nonexistent/method", nil)
	if unknownMethod.Error == nil || unknownMethod.Error.Code != -32601 {
		t.Fatalf("expected -32601 method not found, got %+v", unknownMethod)
	}

	// Unknown tool -> -32602
	unknownTool := h.call("tools/call", map[string]any{"name": "does_not_exist"})
	if unknownTool.Error == nil || unknownTool.Error.Code != -32602 {
		t.Fatalf("expected -32602 invalid params for unknown tool, got %+v", unknownTool)
	}

	// Tool error: wait_session with no condition returns isError=true
	badWait := h.callTool("wait_session", map[string]any{"target": "foo"})
	if !badWait.IsError {
		t.Fatalf("expected isError=true for wait_session without conditions")
	}

	// Tool error: list_sessions when daemon not running still succeeds with []
	lsNoDaemon := h.callTool("list_sessions", nil)
	if lsNoDaemon.IsError || strings.TrimSpace(lsNoDaemon.Content[0].Text) != "[]" {
		t.Fatalf("expected [] when daemon not running, got %+v", lsNoDaemon)
	}
}
