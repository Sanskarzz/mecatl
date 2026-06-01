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

	// resume reports whether session/load is supported (a session store is
	// configured). When false, loadSession is advertised false in initialize and
	// session/load returns an error. The composition root sets it via WithResume.
	resume bool
}

// AgentOption configures an Agent at construction.
type AgentOption func(*Agent)

// WithResume advertises session/load support (loadSession:true) and enables the
// session/load handler. The composition root passes true only when a durable
// session store is configured (mecated --store-dir), so resume is offered only
// when it can actually work.
func WithResume(enabled bool) AgentOption {
	return func(a *Agent) { a.resume = enabled }
}

// NewAgent constructs an Agent over svc. The Conn is set by Serve so the Agent
// can issue outbound request_permission calls and session/update notifications.
func NewAgent(svc *server.Service, opts ...AgentOption) *Agent {
	a := &Agent{
		svc:      svc,
		inFlight: make(map[string]struct{}),
		info:     implementation{Name: "mecatl", Version: "acp-phase3"},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
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
	case methodSessionLoad:
		return a.handleSessionLoad(ctx, params)
	case methodSessionSetMode:
		return a.handleSetMode(ctx, params)
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
// streaming-HTTP MCP, never a client stdio server), loadSession reflecting
// whether a session store is configured (WithResume), and echoes the protocol
// version it implements.
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
			LoadSession:        a.resume,
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
	if err := validateCwd(req.Cwd, "session/new"); err != nil {
		return nil, err
	}
	if err := rejectClientMCP(req.McpServers, "session/new"); err != nil {
		return nil, err
	}

	sess, err := a.svc.CreateSession(ctx, req.Cwd, session.ModeDefault, session.Limits{})
	if err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/new: "+err.Error())
	}
	// Advertise the slash commands for this workspace as an available_commands_update
	// so the editor can offer them in its input palette. Best-effort: a discovery
	// fault or an empty list simply means no (or an empty) update — it must not fail
	// session creation. Sent AFTER the session exists so the notification's
	// sessionId is valid.
	a.notifyAvailableCommands(ctx, string(sess.ID), sess.Workspace)

	return newSessionResponse{
		SessionID: string(sess.ID),
		Modes:     modeStateFor(sess.Mode),
	}, nil
}

// modeStateFor builds the ACP sessionModeState advertising mecatl's three
// permission modes with current as the session's CURRENT mode. It is shared by
// session/new and session/load so both seed the editor's mode picker identically.
func modeStateFor(current session.PermissionMode) *sessionModeState {
	return &sessionModeState{
		CurrentModeID: string(current),
		AvailableModes: []sessionMode{
			{ID: string(session.ModeDefault), Name: "Default", Description: "Standard deny/ask/allow permissions"},
			{ID: string(session.ModePlan), Name: "Plan", Description: "Read-only planning (no mutations)"},
			{ID: string(session.ModeAccept), Name: "Accept Edits", Description: "Auto-accept edits"},
		},
	}
}

// notifyAvailableCommands lists the slash commands for the workspace via the
// Service seam and, when any are found, pushes an available_commands_update. A
// nil lister, an empty result, or a discovery fault yields NO update (the palette
// stays empty) — command discovery must never break the session.
func (a *Agent) notifyAvailableCommands(ctx context.Context, sessionID, workspace string) {
	cmds, err := a.svc.ListCommands(ctx, workspace)
	if err != nil {
		slog.Debug("acp: list commands failed", "session", sessionID, "err", err)
		return
	}
	if len(cmds) == 0 {
		return
	}
	out := make([]availableCommand, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, availableCommand{Name: c.Name, Description: c.Description})
	}
	a.notifyUpdate(sessionID, availableCommandsUpdate{
		SessionUpdate:     updateAvailableCommands,
		AvailableCommands: out,
	})
}

// handleSessionLoad resumes a previously-persisted session so the next
// session/prompt continues it. It validates cwd exactly like session/new, rejects
// any client-provided MCP server the same way, then loads (and, if the session
// had cleanly completed, reopens) the session via Service.LoadSession. It does NOT
// replay prior turns' events: a full conversation replay is bounded only by
// history size and would re-issue every tool_call/diff/result the editor already
// rendered; mecatl restores the session state so the NEXT prompt continues with
// the full conversation context, and the editor keeps whatever transcript it
// persisted. (Replay-on-load is documented as a remaining nice-to-have in the
// ADR.) It returns the resumed mode state so the editor seeds its mode picker.
//
// When resume is disabled (no session store — WithResume(false)), the handler is
// effectively unreachable because loadSession is advertised false; we still guard
// it so a client that calls it anyway gets a clean method error rather than a
// surprising load against an in-memory store.
func (a *Agent) handleSessionLoad(ctx context.Context, params json.RawMessage) (any, error) {
	if !a.resume {
		return nil, newMethodErr(codeMethodNotFound, "acp: session/load: not supported (no session store configured)")
	}
	var req loadSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/load: "+err.Error())
	}
	if req.SessionID == "" {
		return nil, newMethodErr(codeInvalidParams, "acp: session/load: sessionId is required")
	}
	if err := validateCwd(req.Cwd, "session/load"); err != nil {
		return nil, err
	}
	if err := rejectClientMCP(req.McpServers, "session/load"); err != nil {
		return nil, err
	}

	sess, err := a.svc.LoadSession(ctx, session.SessionID(req.SessionID))
	if err != nil {
		// An unknown/never-persisted session (incl. the in-memory store after a
		// restart) is a client error: the id does not resolve.
		return nil, newMethodErr(codeInvalidParams, "acp: session/load: "+err.Error())
	}
	return loadSessionResponse{Modes: modeStateFor(sess.Mode)}, nil
}

// handleSetMode applies a session/set_mode by mapping the ACP modeId to a mecatl
// PermissionMode and calling Service.SetMode. On a successful change it emits a
// current_mode_update so the editor's picker reflects the new selection. An
// unknown modeId is rejected; a mid-turn change is rejected by the aggregate
// (Service.SetMode wraps the illegal transition) — the client must defer it to
// the next prompt. The ACP result is the empty SetSessionModeResponse.
func (a *Agent) handleSetMode(ctx context.Context, params json.RawMessage) (any, error) {
	var req setModeRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/set_mode: "+err.Error())
	}
	if req.SessionID == "" {
		return nil, newMethodErr(codeInvalidParams, "acp: session/set_mode: sessionId is required")
	}
	mode, ok := modeFromACP(req.ModeID)
	if !ok {
		return nil, newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: session/set_mode: unknown modeId %q", req.ModeID))
	}
	sess, err := a.svc.SetMode(ctx, session.SessionID(req.SessionID), mode)
	if err != nil {
		return nil, newMethodErr(codeInvalidParams, "acp: session/set_mode: "+err.Error())
	}
	// Confirm the change to the editor. SetMode is a no-op when the mode is
	// unchanged, but emitting the update unconditionally keeps the picker
	// authoritative and is harmless (the editor sets the value it already has).
	a.notifyUpdate(req.SessionID, currentModeUpdate{
		SessionUpdate: updateCurrentMode,
		CurrentModeID: string(sess.Mode),
	})
	return setModeResponse{}, nil
}

// modeFromACP maps an ACP modeId to a mecatl PermissionMode. The ids are mecatl's
// own mode strings (advertised verbatim as the availableModes ids on
// session/new), so this is an identity-with-validation map: an unrecognized id
// yields ok=false so the handler can reject it.
func modeFromACP(modeID string) (session.PermissionMode, bool) {
	switch session.PermissionMode(modeID) {
	case session.ModeDefault:
		return session.ModeDefault, true
	case session.ModePlan:
		return session.ModePlan, true
	case session.ModeAccept:
		return session.ModeAccept, true
	default:
		return "", false
	}
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
				// Persist the awaiting snapshot so a session/load after a restart can
				// re-attach to a paused session (mirrors the gRPC/HTTP adapters). With
				// session/load now landed this is no longer a dead snapshot.
				a.svc.Persist(ctx, session.SessionID(req.SessionID))
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
	// The event channel has closed, so the run reached a terminal state. Persist
	// the final snapshot (conversation + counters + terminal state) BEFORE the
	// deferred FinishRun deregisters the run — Persist is a no-op once the run is
	// gone. This is what makes session/load resume a continuable session: the
	// engine mutates the session in place, and only this Save captures the
	// completed turn's history durably.
	a.svc.Persist(ctx, session.SessionID(req.SessionID))
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

// validateCwd enforces ACP's absolute-existing-directory cwd contract, shared by
// session/new and session/load. ACP contracts cwd as an ABSOLUTE path, and
// CreateSession -> osfs would silently CREATE a missing dir; we fail loudly
// instead, so a non-absolute or typo'd/nonexistent cwd is rejected rather than
// spawning a session rooted at an accidentally-created directory. (os.Root
// confines tool I/O, so this is input validation, CWE-20, not an escape.) method
// is the ACP method name for the error prefix.
func validateCwd(cwd, method string) error {
	if strings.TrimSpace(cwd) == "" {
		return newMethodErr(codeInvalidParams, "acp: "+method+": cwd is required (absolute path)")
	}
	if !filepath.IsAbs(cwd) {
		return newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: %s: cwd %q must be an absolute path", method, cwd))
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: %s: cwd %q must be an existing directory", method, cwd))
	}
	return nil
}

// rejectClientMCP rejects any client-provided MCP server, shared by session/new
// and session/load. A stdio server (a command, no URL) is hard-rejected (mecatl
// never spawns stdio MCP); a URL server is also rejected this phase (client MCP
// delegation is deferred) but the message distinguishes the two so the gap is
// observable. It rejects on the FIRST entry — any client MCP server is
// unsupported this phase.
func rejectClientMCP(servers []mcpServer, method string) error {
	if len(servers) == 0 {
		return nil
	}
	m := servers[0]
	if m.URL == "" {
		return newMethodErr(codeInvalidParams,
			fmt.Sprintf("acp: %s: stdio MCP server %q rejected (mecatl is streaming-HTTP MCP only)", method, m.Name))
	}
	return newMethodErr(codeInvalidParams,
		fmt.Sprintf("acp: %s: client-provided MCP server %q not supported yet (deferred)", method, m.Name))
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
