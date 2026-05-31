package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/acp"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// scriptTool is a minimal mutating Tool: it returns a fixed body and records
// that it ran, so the e2e test can assert the approved tool actually executed.
type scriptTool struct {
	name     string
	readOnly bool
	content  string
}

func (s *scriptTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: s.name, Description: s.name, Schema: json.RawMessage(`{"type":"object"}`)}
}
func (s *scriptTool) ReadOnly() bool { return s.readOnly }
func (s *scriptTool) Execute(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	return session.NewToolResult(in.ID, s.content), nil
}

// newService wires a real *agent.Engine (mockllm + memfs + permpolicy) into a
// server.Service, mirroring the gRPC adapter's test harness.
func newService(t *testing.T, llm *mockllm.Provider, rules []governance.Rule, tools ...tool.Tool) *server.Service {
	t.Helper()
	cat := tool.NewCatalog()
	for _, tl := range tools {
		cat.MustRegister(tl)
	}
	engine := agent.NewEngine(agent.Deps{
		LLM:     llm,
		Catalog: cat,
		Policy:  permpolicy.NewPolicy(rules),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:        engine,
		Store:         memstore.New(),
		Workspaces:    func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits: session.Limits{MaxTurns: 10, MaxToolCalls: 20},
		Now:           func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// editor is the scripted ACP CLIENT side of the test: it owns the agent's stdin
// (it writes requests/responses there) and reads the agent's stdout (the agent's
// requests/notifications/responses). It speaks Content-Length framing.
type editor struct {
	t        *testing.T
	toAgent  *io.PipeWriter // editor -> agent stdin
	fromAgnt *bufio.Reader  // agent stdout -> editor

	mu    sync.Mutex
	next  int64
	pend  map[int64]chan rpcMsg
	notes chan rpcMsg // session/update notifications
	reqs  chan rpcMsg // agent-initiated requests (request_permission)
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (e *editor) writeFrame(m any) {
	body, _ := json.Marshal(m)
	_, err := fmt.Fprintf(e.toAgent, "Content-Length: %d\r\n\r\n%s", len(body), body)
	if err != nil {
		e.t.Errorf("editor write: %v", err)
	}
}

// call issues an editor->agent request and returns its result, blocking.
func (e *editor) call(method string, params any) json.RawMessage {
	e.mu.Lock()
	e.next++
	id := e.next
	ch := make(chan rpcMsg, 1)
	e.pend[id] = ch
	e.mu.Unlock()
	e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case m := <-ch:
		if len(m.Error) > 0 {
			e.t.Fatalf("%s error: %s", method, m.Error)
		}
		return m.Result
	case <-time.After(5 * time.Second):
		e.t.Fatalf("%s timed out", method)
		return nil
	}
}

func (e *editor) respond(id json.RawMessage, result any) {
	e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// readLoop reads agent frames, routing responses to pending calls, notifications
// to the notes channel, and agent requests to the reqs channel.
func (e *editor) readLoop() {
	for {
		length, err := readHdr(e.fromAgnt)
		if err != nil {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(e.fromAgnt, body); err != nil {
			return
		}
		var m rpcMsg
		if err := json.Unmarshal(body, &m); err != nil {
			return
		}
		switch {
		case m.Method == "" && len(m.ID) > 0: // response
			id, _ := strconv.ParseInt(strings.TrimSpace(string(m.ID)), 10, 64)
			e.mu.Lock()
			ch := e.pend[id]
			delete(e.pend, id)
			e.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "" && len(m.ID) > 0: // agent request
			e.reqs <- m
		case m.Method != "": // notification
			e.notes <- m
		}
	}
}

func readHdr(br *bufio.Reader) (int, error) {
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		t := strings.TrimRight(line, "\r\n")
		if t == "" {
			break
		}
		if name, val, ok := strings.Cut(t, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(val))
		}
	}
	if length < 0 {
		return 0, fmt.Errorf("missing content-length")
	}
	return length, nil
}

// TestEndToEndPromptWithPermission drives the full Phase 1 loop over an in-memory
// stdio pipe: initialize -> session/new -> session/prompt; the agent streams a
// message chunk, a tool_call, then issues a request_permission that the editor
// answers allow_once; the tool runs; a tool_call_update follows; and the prompt
// returns stopReason end_turn.
func TestEndToEndPromptWithPermission(t *testing.T) {
	write := &scriptTool{name: "Write", readOnly: false, content: "wrote"}
	llm := mockllm.New(
		// Turn 1: a text delta + a Write tool call (Write asks for permission).
		mockllm.ChunksTurn(
			port.Chunk{Kind: port.ChunkText, Text: "Working on it"},
			port.Chunk{Kind: port.ChunkToolCall, ToolCall: callP("c1", "Write", `{"path":"a.txt"}`)},
			port.Chunk{Kind: port.ChunkDone, Stop: session.StopEndTurn},
		),
		// Turn 2: after the tool result, a final message and end the turn.
		mockllm.TextTurn("All done"),
	)
	// nil rules => default decision is Ask, so Write triggers a permission.ask.
	svc := newService(t, llm, nil, write)
	cwd := t.TempDir() // session/new now requires an existing absolute dir

	// Wire the agent over a pair of pipes: editorIn -> agent stdin,
	// agent stdout -> editorOut.
	agentStdinR, editorToAgentW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()

	a := acp.NewAgent(svc)
	conn := acp.NewConn(agentStdinR, agentStdoutW, a.Handle)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.Serve(ctx, conn) }()

	e := &editor{
		t:        t,
		toAgent:  editorToAgentW,
		fromAgnt: bufio.NewReader(agentStdoutR),
		pend:     map[int64]chan rpcMsg{},
		notes:    make(chan rpcMsg, 64),
		reqs:     make(chan rpcMsg, 8),
	}
	go e.readLoop()

	// 1) initialize
	initRes := e.call("initialize", map[string]any{"protocolVersion": 1})
	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(initRes, &init); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	if init.ProtocolVersion != 1 || init.AgentCapabilities.LoadSession || init.AgentCapabilities.PromptCapabilities.Image {
		t.Fatalf("unexpected initialize result: %+v", init)
	}

	// 2) session/new
	newRes := e.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(newRes, &ns); err != nil || ns.SessionID == "" {
		t.Fatalf("session/new result: %s err=%v", newRes, err)
	}

	// 3) session/prompt — issue it concurrently (it blocks until the turn ends),
	// then service the permission request and drain notifications.
	promptDone := make(chan json.RawMessage, 1)
	go func() {
		promptDone <- e.call("session/prompt", map[string]any{
			"sessionId": ns.SessionID,
			"prompt":    []any{map[string]any{"type": "text", "text": "please write"}},
		})
	}()

	// 4) Answer the request_permission with allow_once.
	select {
	case req := <-e.reqs:
		if req.Method != "session/request_permission" {
			t.Fatalf("expected request_permission, got %q", req.Method)
		}
		var rp struct {
			ToolCall struct {
				ToolCallID string `json:"toolCallId"`
				Title      string `json:"title"`
			} `json:"toolCall"`
			Options []struct {
				OptionID string `json:"optionId"`
			} `json:"options"`
		}
		if err := json.Unmarshal(req.Params, &rp); err != nil {
			t.Fatalf("request_permission params: %v", err)
		}
		if rp.ToolCall.Title != "Write" || len(rp.Options) != 4 {
			t.Fatalf("unexpected request_permission: %+v", rp)
		}
		e.respond(req.ID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}})
	case <-time.After(5 * time.Second):
		t.Fatal("no request_permission arrived")
	}

	// 5) The prompt returns end_turn.
	var pr struct {
		StopReason string `json:"stopReason"`
	}
	select {
	case res := <-promptDone:
		if err := json.Unmarshal(res, &pr); err != nil {
			t.Fatalf("prompt result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session/prompt did not return")
	}
	if pr.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", pr.StopReason)
	}

	// No run-registry leak: once the prompt returns the run must be deregistered
	// (FinishRun), so LookupRun reports no in-flight run for the session.
	if _, ok := svc.LookupRun(session.SessionID(ns.SessionID)); ok {
		t.Fatalf("run leaked in registry after prompt completed")
	}

	// 6) Assert the streamed session/update sequence: a message chunk, a tool_call
	// (Write), and a tool_call_update completed.
	updates := drainUpdates(e.notes)
	assertHasUpdate(t, updates, "agent_message_chunk", "")
	assertHasUpdate(t, updates, "tool_call", "Write")
	assertHasToolStatus(t, updates, "tool_call_update", "completed")

	// Clean shutdown: closing the editor's writer ends the agent read loop.
	_ = editorToAgentW.Close()
	select {
	case <-serveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

// drainUpdates collects all session/update notifications currently buffered.
func drainUpdates(notes chan rpcMsg) []map[string]any {
	var out []map[string]any
	for {
		select {
		case n := <-notes:
			if n.Method != "session/update" {
				continue
			}
			var p struct {
				Update map[string]any `json:"update"`
			}
			if err := json.Unmarshal(n.Params, &p); err == nil {
				out = append(out, p.Update)
			}
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func assertHasUpdate(t *testing.T, updates []map[string]any, kind, title string) {
	t.Helper()
	for _, u := range updates {
		if u["sessionUpdate"] == kind {
			if title == "" || u["title"] == title {
				return
			}
		}
	}
	t.Fatalf("missing %s update (title %q) in %v", kind, title, updates)
}

func assertHasToolStatus(t *testing.T, updates []map[string]any, kind, status string) {
	t.Helper()
	for _, u := range updates {
		if u["sessionUpdate"] == kind && u["status"] == status {
			return
		}
	}
	t.Fatalf("missing %s update with status %q in %v", kind, status, updates)
}

// TestEndToEndEditDiffBlock drives a prompt whose model emits an Edit tool call
// (auto-allowed), and asserts the streamed tool_call carries an ACP diff content
// block synthesized from the Edit args (oldText/newText/path) so the editor can
// render a native inline diff.
func TestEndToEndEditDiffBlock(t *testing.T) {
	edit := &scriptTool{name: "Edit", readOnly: false, content: "edited"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Edit", `{"path":"main.go","old_string":"foo","new_string":"bar"}`)),
		mockllm.TextTurn("done"),
	)
	svc := newService(t, llm, allowRules(), edit) // allow => no permission ask

	agentStdinR, editorToAgentW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()
	a := acp.NewAgent(svc)
	conn := acp.NewConn(agentStdinR, agentStdoutW, a.Handle)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = a.Serve(ctx, conn) }()

	e := &editor{t: t, toAgent: editorToAgentW, fromAgnt: bufio.NewReader(agentStdoutR),
		pend: map[int64]chan rpcMsg{}, notes: make(chan rpcMsg, 64), reqs: make(chan rpcMsg, 8)}
	go e.readLoop()

	res := e.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &ns)

	out := e.call("session/prompt", map[string]any{"sessionId": ns.SessionID,
		"prompt": []any{map[string]any{"type": "text", "text": "edit it"}}})
	var pr struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(out, &pr); err != nil || pr.StopReason != "end_turn" {
		t.Fatalf("prompt stopReason %q err %v", pr.StopReason, err)
	}

	updates := drainUpdates(e.notes)
	// Find the Edit tool_call and assert its content carries a diff block.
	var found bool
	for _, u := range updates {
		if u["sessionUpdate"] != "tool_call" || u["title"] != "Edit" {
			continue
		}
		content, ok := u["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatalf("Edit tool_call has no content array: %v", u)
		}
		block := content[0].(map[string]any)
		if block["type"] != "diff" {
			t.Fatalf("Edit content block type = %v, want diff", block["type"])
		}
		if block["path"] != "main.go" || block["oldText"] != "foo" || block["newText"] != "bar" {
			t.Fatalf("diff block = %v", block)
		}
		found = true
	}
	if !found {
		t.Fatalf("no Edit tool_call with a diff block in %v", updates)
	}

	_ = editorToAgentW.Close()
}

// TestSessionNewRejectsStdioMCP asserts a client-provided stdio MCP server is
// rejected (mecatl is streaming-HTTP MCP only).
func TestSessionNewRejectsStdioMCP(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	// A valid existing cwd so the stdio-MCP rejection (not cwd validation) is the
	// reason the call fails.
	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"name":"local","command":"some-bin"}]}`, t.TempDir())
	_, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
	if err == nil || !strings.Contains(err.Error(), "stdio MCP") {
		t.Fatalf("expected stdio MCP rejection, got %v", err)
	}
}

// TestSessionNewRejectsBadCwd asserts cwd validation: a relative path and a
// nonexistent absolute path are both rejected (and no session is created).
func TestSessionNewRejectsBadCwd(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"empty", "", "cwd is required"},
		{"relative", "relative/dir", "must be an absolute path"},
		{"nonexistent", filepath.Join(t.TempDir(), "does-not-exist"), "must be an existing directory"},
		{"file not dir", writeTempFile(t), "must be an existing directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[]}`, tc.cwd)
			_, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cwd %q: want error containing %q, got %v", tc.cwd, tc.want, err)
			}
		})
	}
}

// writeTempFile creates a regular file and returns its path (a non-directory cwd
// must be rejected by the IsDir check).
func writeTempFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

// TestSecondPromptRejected asserts only one in-flight prompt per session.
func TestSecondPromptRejected(t *testing.T) {
	// A tool call requiring approval keeps the first prompt in flight (awaiting),
	// so the second prompt observes the in-flight guard.
	write := &scriptTool{name: "Write", content: "x"}
	llm := mockllm.New(mockllm.ToolCallTurn(call("c1", "Write", `{}`)), mockllm.TextTurn("done"))
	svc := newService(t, llm, nil, write)

	agentStdinR, editorToAgentW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()
	a := acp.NewAgent(svc)
	conn := acp.NewConn(agentStdinR, agentStdoutW, a.Handle)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = a.Serve(ctx, conn) }()

	e := &editor{t: t, toAgent: editorToAgentW, fromAgnt: bufio.NewReader(agentStdoutR),
		pend: map[int64]chan rpcMsg{}, notes: make(chan rpcMsg, 64), reqs: make(chan rpcMsg, 8)}
	go e.readLoop()

	res := e.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &ns)

	// First prompt blocks on the permission ask (we never answer it).
	go func() {
		_ = e.call("session/prompt", map[string]any{"sessionId": ns.SessionID,
			"prompt": []any{map[string]any{"type": "text", "text": "go"}}})
	}()
	// Wait until the agent has asked for permission (the first prompt is now in
	// flight and paused).
	select {
	case <-e.reqs:
	case <-time.After(5 * time.Second):
		t.Fatal("no permission ask from first prompt")
	}

	// Second prompt for the SAME session must be rejected.
	e.mu.Lock()
	e.next++
	id := e.next
	ch := make(chan rpcMsg, 1)
	e.pend[id] = ch
	e.mu.Unlock()
	e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt",
		"params": map[string]any{"sessionId": ns.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "again"}}}})
	select {
	case m := <-ch:
		if len(m.Error) == 0 {
			t.Fatalf("second prompt should have errored, got result %s", m.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second prompt did not respond")
	}
}

// TestSequentialPromptsNoRunLeak drives two prompts in sequence on one session
// and asserts the run registry holds no entry for the session after each
// completes — i.e. FinishRun deregisters the run, so a long-lived editor session
// does not leak a dead run per prompt.
func TestSequentialPromptsNoRunLeak(t *testing.T) {
	read := &scriptTool{name: "Read", readOnly: true, content: "body"}
	// Two prompts, each: a read tool call (auto-allowed) then a closing message.
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("done one"),
		mockllm.ToolCallTurn(call("c2", "Read", `{"path":"b"}`)),
		mockllm.TextTurn("done two"),
	)
	svc := newService(t, llm, allowRules(), read)

	agentStdinR, editorToAgentW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()
	a := acp.NewAgent(svc)
	conn := acp.NewConn(agentStdinR, agentStdoutW, a.Handle)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = a.Serve(ctx, conn) }()

	e := &editor{t: t, toAgent: editorToAgentW, fromAgnt: bufio.NewReader(agentStdoutR),
		pend: map[int64]chan rpcMsg{}, notes: make(chan rpcMsg, 128), reqs: make(chan rpcMsg, 8)}
	go e.readLoop()

	res := e.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &ns)
	sid := session.SessionID(ns.SessionID)

	for i, text := range []string{"first", "second"} {
		var pr struct {
			StopReason string `json:"stopReason"`
		}
		out := e.call("session/prompt", map[string]any{"sessionId": ns.SessionID,
			"prompt": []any{map[string]any{"type": "text", "text": text}}})
		if err := json.Unmarshal(out, &pr); err != nil || pr.StopReason != "end_turn" {
			t.Fatalf("prompt %d: stopReason %q err %v", i, pr.StopReason, err)
		}
		// The run for this session must be gone once the (blocking) prompt returns.
		if _, ok := svc.LookupRun(sid); ok {
			t.Fatalf("run leaked in registry after prompt %d completed", i)
		}
	}
}

// allowRules makes every tool auto-allowed (no permission ask), so a prompt runs
// straight to completion.
func allowRules() []governance.Rule {
	return []governance.Rule{{Effect: governance.Allow}}
}

// call / callP build session.ToolCall values for the tests.
func call(id, name, args string) session.ToolCall {
	return session.NewToolCall(session.ToolCallID(id), name, json.RawMessage(args))
}

func callP(id, name, args string) *session.ToolCall {
	c := call(id, name, args)
	return &c
}
