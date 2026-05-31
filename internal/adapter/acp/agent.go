package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/session"
)

// Agent is the ACP adapter's request handler: it bridges the JSON-RPC Conn to the
// surface-agnostic *server.Service (the SAME service the gRPC/HTTP adapters
// consume). It owns the one-prompt-per-session serialization and the outbound
// request_permission round-trip. It is the ACP equivalent of server.HarnessServer.
type Agent struct {
	svc  *server.Service
	conn *Conn

	// inFlight guards the "one in-flight prompt per session" rule: a session id is
	// present while its session/prompt is running, so a second prompt for the same
	// session is rejected.
	mu       sync.Mutex
	inFlight map[string]struct{}

	// info is the agent identity returned on initialize.
	info implementation
}

// NewAgent constructs an Agent over svc. The Conn is set by Serve so the Agent
// can issue outbound request_permission calls and session/update notifications.
func NewAgent(svc *server.Service) *Agent {
	return &Agent{
		svc:      svc,
		inFlight: make(map[string]struct{}),
		info:     implementation{Name: "mecatl", Version: "acp-phase1"},
	}
}

// Serve runs the ACP stdio session over conn and blocks until the input stream
// ends or ctx is cancelled. It is the entry the composition root calls under
// `mecated --acp` (passing a Conn over os.Stdin/os.Stdout).
//
// ORDERING (load-bearing): Serve records conn as a.conn BEFORE conn.Serve starts
// dispatching inbound frames. a.conn is the outbound channel the handlers
// dereference — handleSessionPrompt's requestPermission issues an outbound Call,
// and notifyUpdate sends notifications, both through a.conn. Because the same
// conn handles inbound dispatch and a.conn is set first on this goroutine before
// any inbound frame is read, a handler can never observe a nil a.conn. Do not
// move the assignment after conn.Serve, and call Serve exactly once per Agent.
func (a *Agent) Serve(ctx context.Context, conn *Conn) error {
	a.conn = conn
	return conn.Serve(ctx)
}

// Handle is the JSON-RPC Handler: it routes inbound ACP methods. A request
// (isRequest true) returns a result/error; a notification (session/cancel) acts
// and returns nil. An unknown method returns codeMethodNotFound.
func (a *Agent) Handle(ctx context.Context, method string, params json.RawMessage, isRequest bool) (any, error) {
	switch method {
	case methodInitialize:
		return a.handleInitialize(params)
	case methodSessionNew:
		return a.handleSessionNew(ctx, params)
	case methodSessionPrompt:
		return a.handleSessionPrompt(ctx, params)
	case methodSessionCancel:
		a.handleSessionCancel(ctx, params)
		return nil, nil
	default:
		if !isRequest {
			return nil, nil // ignore unknown notifications
		}
		return nil, newMethodErr(codeMethodNotFound, "acp: unknown method "+method)
	}
}

// handleInitialize negotiates capabilities. mecatl advertises NO image/audio/
// embeddedContext prompt support (text only this phase), NO MCP transports
// (client-provided MCP servers are rejected — mecatl connects its own
// streaming-HTTP MCP, never a client stdio server), loadSession:false, and echoes
// the protocol version it implements.
func (a *Agent) handleInitialize(params json.RawMessage) (any, error) {
	var req initializeRequest
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, newMethodErr(codeInvalidParams, "acp: initialize: "+err.Error())
		}
	}
	return initializeResponse{
		ProtocolVersion: protocolVersion,
		AgentCapabilities: agentCapabilities{
			LoadSession:        false,
			McpCapabilities:    mcpCapabilities{HTTP: false, SSE: false},
			PromptCapabilities: promptCapabilities{Audio: false, EmbeddedContext: false, Image: false},
		},
		AuthMethods: []any{},
		AgentInfo:   &a.info,
	}, nil
}

// handleSessionNew creates a mecatl session rooted at the client's cwd via
// Service.CreateSession (the SAME entry the gRPC CreateSession RPC uses), and
// returns its id. It REJECTS any client-provided MCP server: mecatl connects only
// its own streaming-HTTP MCP servers (CLAUDE.md: no stdio MCP, ever), and client
// MCP delegation is deferred. The session mode is always default this phase; the
// available modes are reflected so the editor can show them.
func (a *Agent) handleSessionNew(ctx context.Context, params json.RawMessage) (any, error) {
	var req newSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/new: "+err.Error())
	}
	// Validate cwd: ACP contracts it as an ABSOLUTE path, and CreateSession ->
	// osfs would silently CREATE a missing dir. We fail loudly instead, so a
	// non-absolute or typo'd/nonexistent cwd is rejected rather than spawning a
	// session rooted at an accidentally-created directory. (os.Root confines tool
	// I/O, so this is not an escape — it is input validation, CWE-20.)
	if strings.TrimSpace(req.Cwd) == "" {
		return nil, newMethodErr(codeInvalidParams, "acp: session/new: cwd is required (absolute path)")
	}
	if !filepath.IsAbs(req.Cwd) {
		return nil, newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: session/new: cwd %q must be an absolute path", req.Cwd))
	}
	if info, statErr := os.Stat(req.Cwd); statErr != nil || !info.IsDir() {
		return nil, newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: session/new: cwd %q must be an existing directory", req.Cwd))
	}
	if len(req.McpServers) > 0 {
		// A stdio MCP server (a command, no URL) is hard-rejected: mecatl never
		// spawns stdio MCP. A URL server is also rejected this phase — client MCP
		// delegation is deferred — but the message distinguishes the two so the gap
		// is observable. We reject on the FIRST entry (any client MCP server is
		// unsupported this phase).
		m := req.McpServers[0]
		if m.URL == "" {
			return nil, newMethodErr(codeInvalidParams,
				fmt.Sprintf("acp: session/new: stdio MCP server %q rejected (mecatl is streaming-HTTP MCP only)", m.Name))
		}
		return nil, newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: session/new: client-provided MCP server %q not supported yet (deferred)", m.Name))
	}

	sess, err := a.svc.CreateSession(ctx, req.Cwd, session.ModeDefault, session.Limits{})
	if err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/new: "+err.Error())
	}
	return newSessionResponse{
		SessionID: string(sess.ID),
		Modes: &sessionModeState{
			CurrentModeID: string(session.ModeDefault),
			AvailableModes: []sessionMode{
				{ID: string(session.ModeDefault), Name: "Default", Description: "Standard deny/ask/allow permissions"},
				{ID: string(session.ModePlan), Name: "Plan", Description: "Read-only planning (no mutations)"},
				{ID: string(session.ModeAccept), Name: "Accept Edits", Description: "Auto-accept edits"},
			},
		},
	}, nil
}

// handleSessionPrompt runs one prompt to completion. It enforces one in-flight
// prompt per session, flattens the text content blocks into the prompt text,
// starts a run via Service.StartRun, drains the run's Events projecting each to a
// session/update notification (and handling permission.ask out of band as an
// outbound request_permission), and returns {stopReason} only when the terminal
// EvResult arrives. It BLOCKS for the whole turn, exactly as ACP requires.
func (a *Agent) handleSessionPrompt(ctx context.Context, params json.RawMessage) (any, error) {
	var req promptRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/prompt: "+err.Error())
	}
	if req.SessionID == "" {
		return nil, newMethodErr(codeInvalidParams, "acp: session/prompt: sessionId is required")
	}
	text := flattenPrompt(req.Prompt)
	if strings.TrimSpace(text) == "" {
		return nil, newMethodErr(codeInvalidParams, "acp: session/prompt: prompt has no text content")
	}

	if !a.acquire(req.SessionID) {
		return nil, newMethodErr(codeInvalidParams, "acp: session/prompt: a prompt is already in flight for this session")
	}
	defer a.release(req.SessionID)

	run, err := a.svc.StartRun(ctx, session.SessionID(req.SessionID), text)
	if err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/prompt: "+err.Error())
	}
	// Remove the run from the Service registry once we finish draining its events,
	// mirroring the gRPC/HTTP adapters' `defer ... FinishRun(...)`. Without this a
	// long-lived editor session leaks a dead run per prompt.
	defer a.svc.FinishRun(session.SessionID(req.SessionID), run)

	stop := stopEndTurn
	for ev := range run.Events() {
		switch ev.Type {
		case session.EvPermissionAsk:
			if ev.Ask != nil {
				// Out-of-band: ask the editor, then resolve the run. Run on its own
				// goroutine so draining the event channel never blocks behind the
				// editor's reply (the loop is paused awaiting Approve anyway, but
				// concurrent tool results from a parallel read still flow).
				a.requestPermission(ctx, req.SessionID, run, *ev.Ask)
			}
		case session.EvResult:
			if ev.Result != nil {
				stop = stopReasonFor(ev.Result.Stop)
			}
		default:
			if update, ok := projectUpdate(ev); ok {
				a.notifyUpdate(req.SessionID, update)
			}
		}
	}
	return promptResponse{StopReason: stop}, nil
}

// handleSessionCancel cancels the in-flight run for the session. The blocked
// session/prompt then resolves with stopReason:"cancelled".
func (a *Agent) handleSessionCancel(ctx context.Context, params json.RawMessage) {
	var n cancelNotification
	if err := json.Unmarshal(params, &n); err != nil || n.SessionID == "" {
		return
	}
	if err := a.svc.Cancel(ctx, session.SessionID(n.SessionID)); err != nil {
		slog.Debug("acp: session/cancel", "session", n.SessionID, "err", err)
	}
}

// requestPermission issues the outbound session/request_permission, awaits the
// editor's reply, and resolves the paused run with the mapped verdict. A
// transport error or a cancelled context denies the ask (fail-safe), so the run
// never hangs waiting on an approval that will not come.
func (a *Agent) requestPermission(ctx context.Context, sessionID string, run runApprover, ask session.PendingAsk) {
	go func() {
		var resp requestPermissionResponse
		err := a.conn.Call(ctx, methodRequestPermission, permissionRequestFor(sessionID, ask), &resp)
		if err != nil {
			slog.Debug("acp: request_permission failed; denying", "session", sessionID, "ask", ask.AskID, "err", err)
			run.Approve(ask.AskID, false)
			return
		}
		run.Approve(ask.AskID, approvalFor(resp.Outcome))
	}()
}

// notifyUpdate pushes one session/update notification, logging (not failing) a
// write error: a dropped notification must not abort the in-flight prompt.
func (a *Agent) notifyUpdate(sessionID string, update any) {
	if err := a.conn.Notify(methodSessionUpdate, sessionNotification{SessionID: sessionID, Update: update}); err != nil {
		slog.Debug("acp: session/update notify failed", "session", sessionID, "err", err)
	}
}

// acquire marks a session's prompt in flight, returning false if one already is.
func (a *Agent) acquire(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.inFlight[sessionID]; busy {
		return false
	}
	a.inFlight[sessionID] = struct{}{}
	return true
}

func (a *Agent) release(sessionID string) {
	a.mu.Lock()
	delete(a.inFlight, sessionID)
	a.mu.Unlock()
}

// runApprover is the subset of *agent.Run the permission round-trip needs,
// narrowed so requestPermission is unit-testable with a fake.
type runApprover interface {
	Approve(askID string, allow bool)
}

// flattenPrompt concatenates the text of every text ContentBlock with newlines,
// dropping non-text blocks (image/audio/resource are unsupported this phase).
func flattenPrompt(blocks []contentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
