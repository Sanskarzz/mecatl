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
	"github.com/stacklok/mecatl/internal/adapter/mcp"
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
// server.Service, mirroring the gRPC adapter's test harness. Optional configFns
// mutate the server.Config before construction (e.g. to inject a CommandLister
// or a shared store).
func newService(t *testing.T, llm *mockllm.Provider, rules []governance.Rule, tools ...tool.Tool) *server.Service {
	t.Helper()
	return newServiceCfg(t, llm, rules, nil, tools...)
}

// newServiceCfg is newService with an extra hook to customize the server.Config.
func newServiceCfg(t *testing.T, llm *mockllm.Provider, rules []governance.Rule, configFn func(*server.Config), tools ...tool.Tool) *server.Service {
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
	cfg := server.Config{
		Engine:        engine,
		Store:         memstore.New(),
		Workspaces:    func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits: session.Limits{MaxTurns: 10, MaxToolCalls: 20},
		Now:           func() time.Time { return time.Unix(0, 0) },
	}
	if configFn != nil {
		configFn(&cfg)
	}
	svc, err := server.NewService(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// fakeLister is a CommandLister returning a fixed command set, for the
// available_commands_update test.
type fakeLister struct {
	cmds []server.Command
}

func (f fakeLister) List(_ context.Context, _ string) ([]server.Command, error) {
	return f.cmds, nil
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

// fakeSessionEngine records the specs it received and returns a stub engine plus
// a no-op close, so the accept-path tests can assert the factory was called with
// the right URL+headers WITHOUT connecting a real MCP server (offline).
type fakeSessionEngine struct {
	mu     sync.Mutex
	called int
	specs  []mcp.ServerConfig
	engine *agent.Engine
}

func (f *fakeSessionEngine) factory(_ context.Context, specs []mcp.ServerConfig) (*agent.Engine, func() error, error) {
	f.mu.Lock()
	f.called++
	f.specs = specs
	f.mu.Unlock()
	return f.engine, func() error { return nil }, nil
}

// stubEngine builds a minimal mockllm-backed engine the fake factory hands back as
// the per-session engine (it never has to actually run in the accept tests).
func stubEngine(t *testing.T) *agent.Engine {
	t.Helper()
	return agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("done")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(allowRules()),
		Model:   "test-model",
	})
}

// TestSessionNewAcceptsHTTPMCP asserts a client-provided streaming-HTTP MCP server
// is accepted on session/new: the per-session engine factory is invoked with one
// spec whose URL and headers map correctly, and a session id is returned.
func TestSessionNewAcceptsHTTPMCP(t *testing.T) {
	fake := &fakeSessionEngine{engine: stubEngine(t)}
	svc := newServiceCfg(t, mockllm.New(), nil, func(c *server.Config) { c.SessionEngine = fake.factory })
	a := acp.NewAgent(svc)

	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"type":"http","name":"docs","url":"https://example.test/mcp","headers":[{"name":"Authorization","value":"Bearer x"}]}]}`, t.TempDir())
	out, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
	if err != nil {
		t.Fatalf("session/new with http MCP: %v", err)
	}
	b, _ := json.Marshal(out)
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if jerr := json.Unmarshal(b, &ns); jerr != nil || ns.SessionID == "" {
		t.Fatalf("session/new result: %s err=%v", b, jerr)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.called != 1 {
		t.Fatalf("factory called %d times, want 1", fake.called)
	}
	if len(fake.specs) != 1 {
		t.Fatalf("factory got %d specs, want 1: %+v", len(fake.specs), fake.specs)
	}
	spec := fake.specs[0]
	if spec.Name != "docs" || spec.URL != "https://example.test/mcp" {
		t.Fatalf("spec name/url = %q/%q", spec.Name, spec.URL)
	}
	if spec.Headers["Authorization"] != "Bearer x" {
		t.Fatalf("spec headers = %v, want Authorization: Bearer x", spec.Headers)
	}
}

// TestSessionNewRejectsStdioMCP asserts a client-provided stdio MCP server is
// rejected (mecatl is streaming-HTTP MCP only), in both the command-shaped and the
// explicit type:"stdio" forms.
func TestSessionNewRejectsStdioMCP(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	cwd := t.TempDir()
	cases := []string{
		// command-shaped, no type.
		fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"name":"local","command":"some-bin"}]}`, cwd),
		// explicit type:"stdio".
		fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"name":"local","type":"stdio","command":"some-bin"}]}`, cwd),
	}
	for _, params := range cases {
		_, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
		if err == nil || !strings.Contains(err.Error(), "stdio MCP") {
			t.Fatalf("expected stdio MCP rejection, got %v (params=%s)", err, params)
		}
	}
}

// TestSessionNewRejectsSSEMCP asserts a type:"sse" client MCP server is rejected
// (streaming-HTTP only).
func TestSessionNewRejectsSSEMCP(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"name":"stream","type":"sse","url":"https://example.test/sse"}]}`, t.TempDir())
	_, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
	if err == nil || !strings.Contains(err.Error(), "sse") {
		t.Fatalf("expected sse rejection, got %v", err)
	}
}

// TestSessionNewRejectsBadScheme asserts an http MCP server with a non-allowed URL
// scheme (file://) is rejected by the SSRF scheme allowlist.
func TestSessionNewRejectsBadScheme(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"name":"bad","type":"http","url":"file:///etc/passwd"}]}`, t.TempDir())
	_, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected bad-scheme rejection, got %v", err)
	}
}

// TestSessionNewRejectsTooManyMCP asserts a client declaring more than the cap of
// MCP servers is rejected (CWE-400: the servers connect serially, so an unbounded
// count could stall session/new). It is rejected BEFORE the factory is consulted.
func TestSessionNewRejectsTooManyMCP(t *testing.T) {
	fake := &fakeSessionEngine{engine: stubEngine(t)}
	svc := newServiceCfg(t, mockllm.New(), nil, func(c *server.Config) { c.SessionEngine = fake.factory })
	a := acp.NewAgent(svc)

	var entries []string
	for i := 0; i < 9; i++ { // 9 > the cap of 8
		entries = append(entries, fmt.Sprintf(`{"type":"http","name":"s%d","url":"https://s%d.test/mcp"}`, i, i))
	}
	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[%s]}`, t.TempDir(), strings.Join(entries, ","))
	_, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
	if err == nil || !strings.Contains(err.Error(), "too many MCP servers") {
		t.Fatalf("expected too-many-servers rejection, got %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.called != 0 {
		t.Fatalf("factory called %d times; the cap must reject before any connect", fake.called)
	}
}

// TestSessionNewSetsClientMCPTimeout asserts each accepted spec carries the bounded
// per-server connect timeout (so a slow client server cannot hold session/new for
// the full operator budget).
func TestSessionNewSetsClientMCPTimeout(t *testing.T) {
	fake := &fakeSessionEngine{engine: stubEngine(t)}
	svc := newServiceCfg(t, mockllm.New(), nil, func(c *server.Config) { c.SessionEngine = fake.factory })
	a := acp.NewAgent(svc)

	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[{"type":"http","name":"docs","url":"https://example.test/mcp"}]}`, t.TempDir())
	if _, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(fake.specs))
	}
	if fake.specs[0].Timeout <= 0 || fake.specs[0].Timeout >= 30*time.Second {
		t.Fatalf("spec Timeout = %v, want a bounded client-path value (< operator 30s)", fake.specs[0].Timeout)
	}
}

// TestInitializeAdvertisesHTTPMCP asserts initialize advertises http:true / sse:false
// so an editor offers its streaming-HTTP MCP servers.
func TestInitializeAdvertisesHTTPMCP(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	out, err := a.Handle(context.Background(), "initialize", json.RawMessage(`{"protocolVersion":1}`), true)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	b, _ := json.Marshal(out)
	var init struct {
		AgentCapabilities struct {
			McpCapabilities struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if jerr := json.Unmarshal(b, &init); jerr != nil {
		t.Fatalf("initialize result: %v", jerr)
	}
	if !init.AgentCapabilities.McpCapabilities.HTTP || init.AgentCapabilities.McpCapabilities.SSE {
		t.Fatalf("mcpCapabilities = %+v, want http:true sse:false", init.AgentCapabilities.McpCapabilities)
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

// startAgent wires an Agent over a pipe pair, starts Serve on a goroutine, and
// returns a connected editor plus a cleanup that closes the editor's writer and
// waits for Serve. It centralizes the boilerplate the new Phase-3 tests share.
func startAgent(t *testing.T, svc *server.Service, opts ...acp.AgentOption) (*editor, func()) {
	t.Helper()
	agentStdinR, editorToAgentW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()
	a := acp.NewAgent(svc, opts...)
	conn := acp.NewConn(agentStdinR, agentStdoutW, a.Handle)
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() { _ = a.Serve(ctx, conn); close(serveDone) }()
	e := &editor{t: t, toAgent: editorToAgentW, fromAgnt: bufio.NewReader(agentStdoutR),
		pend: map[int64]chan rpcMsg{}, notes: make(chan rpcMsg, 128), reqs: make(chan rpcMsg, 8)}
	go e.readLoop()
	cleanup := func() {
		_ = editorToAgentW.Close()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
		}
		cancel()
	}
	return e, cleanup
}

// waitForUpdate drains notifications until one matching sessionUpdate==kind
// arrives (or it times out), returning the matched update map.
func waitForUpdate(t *testing.T, e *editor, kind string) map[string]any {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-e.notes:
			if n.Method != "session/update" {
				continue
			}
			var p struct {
				Update map[string]any `json:"update"`
			}
			if err := json.Unmarshal(n.Params, &p); err != nil {
				continue
			}
			if p.Update["sessionUpdate"] == kind {
				return p.Update
			}
		case <-deadline:
			t.Fatalf("no %q session/update arrived", kind)
			return nil
		}
	}
}

// TestAvailableCommandsUpdateOnSessionNew asserts that session/new emits an
// available_commands_update listing the workspace's slash commands (mapped from
// the injected CommandLister) so the editor's palette is seeded.
func TestAvailableCommandsUpdateOnSessionNew(t *testing.T) {
	lister := fakeLister{cmds: []server.Command{
		{Name: "plan", Description: "Draft a plan"},
		{Name: "review", Description: "Review the diff"},
	}}
	svc := newServiceCfg(t, mockllm.New(), nil, func(c *server.Config) { c.Commands = lister })
	e, cleanup := startAgent(t, svc)
	defer cleanup()

	res := e.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &ns); err != nil || ns.SessionID == "" {
		t.Fatalf("session/new: %s err=%v", res, err)
	}

	u := waitForUpdate(t, e, "available_commands_update")
	cmds, ok := u["availableCommands"].([]any)
	if !ok || len(cmds) != 2 {
		t.Fatalf("availableCommands = %v, want 2 entries", u["availableCommands"])
	}
	first := cmds[0].(map[string]any)
	if first["name"] != "plan" || first["description"] != "Draft a plan" {
		t.Fatalf("first command = %v", first)
	}
}

// TestNoAvailableCommandsUpdateWhenEmpty asserts that with no command lister (the
// default), session/new emits NO available_commands_update.
func TestNoAvailableCommandsUpdateWhenEmpty(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	e, cleanup := startAgent(t, svc)
	defer cleanup()

	_ = e.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})

	// Briefly drain: any available_commands_update within the window is a failure.
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case n := <-e.notes:
			if n.Method != "session/update" {
				continue
			}
			var p struct {
				Update map[string]any `json:"update"`
			}
			_ = json.Unmarshal(n.Params, &p)
			if p.Update["sessionUpdate"] == "available_commands_update" {
				t.Fatalf("unexpected available_commands_update with no lister")
			}
		case <-deadline:
			return
		}
	}
}

// TestSetModeAppliesAndEmitsCurrentModeUpdate asserts session/set_mode maps the
// modeId to the session's PermissionMode, persists it, and emits a
// current_mode_update reflecting the new mode.
func TestSetModeAppliesAndEmitsCurrentModeUpdate(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	e, cleanup := startAgent(t, svc)
	defer cleanup()

	res := e.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &ns)

	// Switch to plan mode.
	_ = e.call("session/set_mode", map[string]any{"sessionId": ns.SessionID, "modeId": "plan"})

	u := waitForUpdate(t, e, "current_mode_update")
	if u["currentModeId"] != "plan" {
		t.Fatalf("currentModeId = %v, want plan", u["currentModeId"])
	}

	// The persisted session reflects the new mode.
	sess, err := svc.GetSession(context.Background(), session.SessionID(ns.SessionID))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Mode != session.ModePlan {
		t.Fatalf("session mode = %q, want plan", sess.Mode)
	}
}

// TestSetModeRejectsUnknownMode asserts an unknown modeId is rejected.
func TestSetModeRejectsUnknownMode(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc)
	res := e2eSessionID(t, a)
	params := fmt.Sprintf(`{"sessionId":%q,"modeId":"bogus"}`, res)
	_, err := a.Handle(context.Background(), "session/set_mode", json.RawMessage(params), true)
	if err == nil || !strings.Contains(err.Error(), "unknown modeId") {
		t.Fatalf("expected unknown modeId rejection, got %v", err)
	}
}

// e2eSessionID creates a session through the Handle entrypoint and returns its
// id (used by the direct-Handle tests that do not need the full pipe harness).
func e2eSessionID(t *testing.T, a *acp.Agent) string {
	t.Helper()
	params := fmt.Sprintf(`{"cwd":%q,"mcpServers":[]}`, t.TempDir())
	out, err := a.Handle(context.Background(), "session/new", json.RawMessage(params), true)
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	// out is the adapter's newSessionResponse value; re-marshal to read the id.
	b, _ := json.Marshal(out)
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(b, &ns)
	return ns.SessionID
}

// TestSessionLoadRestoresPersistedSession asserts session/load (with resume
// enabled) restores a completed session so a subsequent session/prompt continues
// it, preserving the conversation history.
func TestSessionLoadRestoresPersistedSession(t *testing.T) {
	read := &scriptTool{name: "Read", readOnly: true, content: "body"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("first done"),
		mockllm.TextTurn("second done"),
	)
	// A shared store object persists across the two agent connections.
	store := memstore.New()
	svc := newServiceCfg(t, llm, allowRules(), func(c *server.Config) { c.Store = store }, read)

	// Connection 1: create + run one prompt to completion, then disconnect.
	e1, cleanup1 := startAgent(t, svc, acp.WithResume(true))
	res := e1.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &ns)
	out := e1.call("session/prompt", map[string]any{"sessionId": ns.SessionID,
		"prompt": []any{map[string]any{"type": "text", "text": "first"}}})
	var pr struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(out, &pr); err != nil || pr.StopReason != "end_turn" {
		t.Fatalf("first prompt stopReason %q err %v", pr.StopReason, err)
	}
	cleanup1()

	// Connection 2: load the session, then continue with a second prompt.
	e2, cleanup2 := startAgent(t, svc, acp.WithResume(true))
	defer cleanup2()
	loadRes := e2.call("session/load", map[string]any{"sessionId": ns.SessionID, "cwd": t.TempDir(), "mcpServers": []any{}})
	var lr struct {
		Modes *struct {
			CurrentModeID string `json:"currentModeId"`
		} `json:"modes"`
	}
	if err := json.Unmarshal(loadRes, &lr); err != nil || lr.Modes == nil {
		t.Fatalf("session/load result: %s err=%v", loadRes, err)
	}

	out2 := e2.call("session/prompt", map[string]any{"sessionId": ns.SessionID,
		"prompt": []any{map[string]any{"type": "text", "text": "second"}}})
	var pr2 struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(out2, &pr2); err != nil || pr2.StopReason != "end_turn" {
		t.Fatalf("second prompt stopReason %q err %v", pr2.StopReason, err)
	}

	// The continued session preserved its history: the second turn ran on top of
	// the first prompt + tool result + assistant message, so the conversation has
	// grown beyond a single prompt.
	sess, err := svc.GetSession(context.Background(), session.SessionID(ns.SessionID))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(sess.Conversation.Messages) < 4 {
		t.Fatalf("conversation has %d messages, want >=4 (history preserved across load)", len(sess.Conversation.Messages))
	}
}

// TestSessionLoadReplaysTranscript asserts session/load re-streams the persisted
// conversation as session/update notifications so a re-attaching editor rebuilds
// the transcript: it sees the assistant message chunk, the tool_call card (c1),
// and its tool_call_update (completed), with the tool_call arriving BEFORE the
// update (open-before-update). It also asserts no request_permission is issued
// during load (replay must never re-prompt for a historical, already-resolved
// approval).
func TestSessionLoadReplaysTranscript(t *testing.T) {
	read := &scriptTool{name: "Read", readOnly: true, content: "body"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("first done"),
	)
	store := memstore.New()
	svc := newServiceCfg(t, llm, allowRules(), func(c *server.Config) { c.Store = store }, read)

	// Connection 1: create + run a prompt that produces a tool_call(c1) + result +
	// final message, then disconnect.
	e1, cleanup1 := startAgent(t, svc, acp.WithResume(true))
	res := e1.call("session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	var ns struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &ns)
	out := e1.call("session/prompt", map[string]any{"sessionId": ns.SessionID,
		"prompt": []any{map[string]any{"type": "text", "text": "first"}}})
	var pr struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(out, &pr); err != nil || pr.StopReason != "end_turn" {
		t.Fatalf("first prompt stopReason %q err %v", pr.StopReason, err)
	}
	cleanup1()

	// Connection 2: load the session and collect the replayed notifications, in
	// arrival order.
	e2, cleanup2 := startAgent(t, svc, acp.WithResume(true))
	defer cleanup2()
	_ = e2.call("session/load", map[string]any{"sessionId": ns.SessionID, "cwd": t.TempDir(), "mcpServers": []any{}})

	updates := drainUpdates(e2.notes)

	// Security assertion: no request_permission was received during load.
	select {
	case req := <-e2.reqs:
		t.Fatalf("unexpected agent request during load: %s", req.Method)
	default:
	}

	// At least one agent_message_chunk (the "first done" reply).
	assertHasUpdate(t, updates, "agent_message_chunk", "")
	// A tool_call card for c1 and a completed tool_call_update for c1.
	callIdx, updateIdx := -1, -1
	for i, u := range updates {
		switch u["sessionUpdate"] {
		case "tool_call":
			if u["toolCallId"] == "c1" && callIdx < 0 {
				callIdx = i
			}
		case "tool_call_update":
			if u["toolCallId"] == "c1" && u["status"] == "completed" && updateIdx < 0 {
				updateIdx = i
			}
		}
	}
	if callIdx < 0 {
		t.Fatalf("no tool_call for c1 in replayed updates %v", updates)
	}
	if updateIdx < 0 {
		t.Fatalf("no completed tool_call_update for c1 in replayed updates %v", updates)
	}
	if callIdx >= updateIdx {
		t.Fatalf("tool_call (idx %d) must arrive before tool_call_update (idx %d) for c1", callIdx, updateIdx)
	}
}

// TestSessionLoadDisabledWithoutStore asserts that when resume is disabled (no
// durable store), initialize advertises loadSession:false and session/load
// returns a method error.
func TestSessionLoadDisabledWithoutStore(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	e, cleanup := startAgent(t, svc) // resume not enabled
	defer cleanup()

	initRes := e.call("initialize", map[string]any{"protocolVersion": 1})
	var init struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(initRes, &init)
	if init.AgentCapabilities.LoadSession {
		t.Fatalf("loadSession advertised true with resume disabled")
	}

	// A session/load call must error rather than load.
	e.mu.Lock()
	e.next++
	id := e.next
	ch := make(chan rpcMsg, 1)
	e.pend[id] = ch
	e.mu.Unlock()
	e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/load",
		"params": map[string]any{"sessionId": "whatever", "cwd": t.TempDir(), "mcpServers": []any{}}})
	select {
	case m := <-ch:
		if len(m.Error) == 0 {
			t.Fatalf("session/load should have errored with resume disabled, got %s", m.Result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session/load did not respond")
	}
}

// TestSessionLoadUnknownSession asserts session/load with resume enabled errors
// cleanly for a session id the store has never seen.
func TestSessionLoadUnknownSession(t *testing.T) {
	svc := newService(t, mockllm.New(), nil)
	a := acp.NewAgent(svc, acp.WithResume(true))
	params := fmt.Sprintf(`{"sessionId":"nope","cwd":%q,"mcpServers":[]}`, t.TempDir())
	_, err := a.Handle(context.Background(), "session/load", json.RawMessage(params), true)
	if err == nil {
		t.Fatal("expected session/load error for unknown session")
	}
}
