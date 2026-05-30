package ui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// Update is the Elm reducer. It is split by message type; all model mutation and
// all glamour rendering happen here on the single update goroutine (the stream
// reader never touches the model). After most state changes it calls refreshView
// to re-render the conversation into the viewport.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.onResize(msg)

	case tea.KeyPressMsg:
		return m.onKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	// Lifecycle / transport.
	case client.SessionReadyMsg:
		m.sessionID = msg.SessionID
		m.phase = phaseIdle
		m.statusMsg = "connected"
		return m, nil
	case client.ConnectErrMsg:
		m.phase = phaseFatal
		m.fatalErr = msg.Err.Error()
		return m, nil
	case client.StreamErrMsg:
		m.conv.addError("stream error: " + msg.Err.Error())
		return m.endRun("error"), m.refreshCmd()
	case client.StreamClosedMsg:
		// Clean close. If a run was still active (no terminal result seen),
		// finalise it; otherwise it's the expected post-result close (no-op).
		if m.phase == phaseRunning || m.phase == phaseAwaitingApproval {
			return m.endRun("closed"), m.refreshCmd()
		}
		return m, nil

	default:
		// MCP overlay result/error msgs (Stage D) are reduced first; if it's not
		// one of those, fall through to the stream-event handler.
		if mm, cmd, handled := m.updateMCPMsg(msg); handled {
			return mm, cmd
		}
		// Stream events (session.init / turn.start / deltas / tool.* /
		// permission.ask / hook / compaction / result) are handled separately to
		// keep this reducer's branch count in check.
		return m.updateStreamEvent(msg)
	}
}

// updateStreamEvent reduces the per-event stream msgs into the conversation. It
// is the back half of Update, split out so the cyclomatic complexity of each
// stays manageable. Unknown msgs are a no-op.
func (m Model) updateStreamEvent(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case client.SessionInitMsg:
		return m, m.waitCmd()
	case client.TurnStartMsg:
		m.conv.startAssistant()
		m.activeTool = ""
		return m, m.afterEvent()
	case client.AssistantDeltaMsg:
		m.conv.appendAssistant(msg.Text)
		m.refreshView()
		return m, m.waitCmd()
	case client.ReasoningDeltaMsg:
		m.conv.appendReasoning(msg.Text)
		m.refreshView()
		return m, m.waitCmd()
	case client.TurnEndMsg:
		// The turn's model exchange is done: freeze any live "reasoning…"
		// affordance, then append a muted stat line unless the turn was trivial.
		m.conv.endReasoningStream()
		if !trivialTurn(msg) {
			m.conv.addTurnStat(turnStatLine(msg))
		}
		return m, m.afterEvent()
	case client.ToolCallMsg:
		m.conv.addTool(msg.ID, msg.Name, msg.Args)
		m.activeTool = msg.Name
		// Accumulate the file path for the session changed-files summary. Tracked
		// at call time (not on the result) so the header reflects intent the moment
		// the mutation is announced; mutatedPath gates to Edit/Write + dedupes.
		if p, ok := mutatedPath(msg.Name, msg.Args); ok {
			m.recordFileChange(p)
		}
		return m, m.afterEvent()
	case client.ToolResultMsg:
		if !m.conv.resolveTool(msg.CallID, msg.Content, msg.IsError) {
			m.conv.addNotice("orphan tool result for " + msg.CallID)
		}
		m.activeTool = ""
		return m, m.afterEvent()
	case client.PermissionAskMsg:
		m.phase = phaseAwaitingApproval
		m.activeTool = ""
		m.ask = pendingAsk{
			AskID:        msg.AskID,
			Tool:         msg.Tool,
			Args:         msg.Args,
			Reason:       msg.Reason,
			allowFocused: true,
		}
		return m, m.waitCmd()
	case client.HookMsg:
		m.conv.addHook(msg.Text, msg.Phase, msg.Tool, string(msg.Decision))
		return m, m.afterEvent()
	case client.CompactionMsg:
		m.conv.addNotice("history compacted" + suffix(msg.Text))
		return m, m.afterEvent()
	case client.ResultMsg:
		m.usage = sumUsage(m.usage, msg.Usage)
		// The latest turn's prompt size is the current context occupancy
		// (InputTokens already includes cache-served tokens).
		m.contextTokens = msg.Usage.InputTokens
		if msg.Stop == "error" && msg.Error != "" {
			m.conv.addError(msg.Error)
		}
		m.statusMsg = "stop: " + msg.Stop
		return m.endRun(msg.Stop), m.refreshCmd()
	default:
		return m, nil
	}
}

// onResize updates widget dimensions and invalidates the glamour width cache via
// the renderer width, then re-renders.
func (m Model) onResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height

	taH := 4
	footerH := 2
	headerH := 2
	vpH := m.height - taH - footerH - headerH
	if vpH < 1 {
		vpH = 1
	}
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(vpH)
	m.ta.SetWidth(m.width)
	m.rend.setWidth(m.width)
	m.refreshView()
	return m, nil
}

// onKey routes key presses by phase. Global quit (ctrl+c) always wins.
func (m Model) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Quit) {
		if m.cancelRun != nil {
			m.cancelRun()
		}
		return m, tea.Quit
	}

	// An open MCP overlay owns the keyboard (it only opens while idle). It steps
	// back / closes on esc internally, so route here before the phase switch.
	if mm, cmd, handled := m.onMCPKey(msg); handled {
		return mm, cmd
	}

	// ctrl+t is a global render toggle (full vs capped tool output); it works in
	// any phase and never feeds the textarea.
	if key.Matches(msg, m.keys.ExpandTools) {
		m.expandTools = !m.expandTools
		m.refreshView()
		return m, nil
	}

	switch m.phase {
	case phaseAwaitingApproval:
		return m.onApprovalKey(msg)
	case phaseRunning:
		return m.onRunningKey(msg)
	case phaseIdle:
		return m.onIdleKey(msg)
	default:
		return m, nil
	}
}

// onApprovalKey resolves the open permission modal. Left/right (or tab) toggle
// the focused button; allow/deny keys send ResumeApproval with the exact ask_id.
func (m Model) onApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "left", "right", "tab":
		m.ask.allowFocused = !m.ask.allowFocused
		return m, nil
	case "enter":
		return m.resolveAsk(m.ask.allowFocused)
	}
	if key.Matches(msg, m.keys.Allow) {
		return m.resolveAsk(true)
	}
	if key.Matches(msg, m.keys.Deny) {
		return m.resolveAsk(false)
	}
	return m, nil
}

// resolveAsk sends the approval/denial on the SAME stream (ask_id correlation),
// closes the modal, and resumes the run (spinner restarts). The send is wrapped
// in a command so a send error surfaces as a StreamErrMsg.
func (m Model) resolveAsk(allow bool) (tea.Model, tea.Cmd) {
	askID := m.ask.AskID
	stream := m.stream
	m.ask = pendingAsk{}
	m.phase = phaseRunning

	verb := "denied"
	if allow {
		verb = "allowed"
	}
	m.conv.addNotice("permission " + verb)
	m.refreshView()

	send := func() tea.Msg {
		if stream == nil {
			return nil
		}
		if err := stream.SendApproval(askID, allow); err != nil {
			return client.StreamErrMsg{Err: err}
		}
		return nil
	}
	return m, tea.Batch(send, m.waitCmd())
}

// onRunningKey handles keys while a run streams: esc sends Cancel (the run ends
// with stop "cancelled"; we keep the stream open until that result). Other keys
// scroll the viewport.
func (m Model) onRunningKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Cancel) {
		stream := m.stream
		m.statusMsg = "cancelling…"
		return m, func() tea.Msg {
			if stream != nil {
				_ = stream.SendCancel()
			}
			return nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// onIdleKey handles keys while idle: enter submits the prompt, shift+enter (and
// ctrl+j) inserts a newline, everything else feeds the textarea (or scrolls).
func (m Model) onIdleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.MCPPanel):
		return m.openMCP(mcpPanel)
	case key.Matches(msg, m.keys.Resources):
		return m.openMCP(mcpResources)
	case key.Matches(msg, m.keys.Prompts):
		return m.openMCP(mcpPrompts)
	case key.Matches(msg, m.keys.Newline):
		m.ta.InsertRune('\n')
		return m, nil
	case key.Matches(msg, m.keys.Submit):
		return m.submitPrompt()
	case key.Matches(msg, m.keys.ScrollU), key.Matches(msg, m.keys.ScrollD):
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	default:
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}
}

// submitPrompt opens a fresh Converse run for the textarea text, sends the
// mandatory prompt frame, starts the reader goroutine, and arms WaitForMsg.
func (m Model) submitPrompt() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" || m.sessionID == "" {
		return m, nil
	}
	m.conv.addUser(text)
	m.ta.Reset()
	m.ta.Blur() // input is disabled while running; reflect it visually
	m.phase = phaseRunning
	m.statusMsg = "running…"
	m.refreshView()

	// Open the run stream synchronously so the model can own the channel and
	// cancel func (commands run AFTER Update returns and cannot mutate the
	// model). The Converse open is a lazy gRPC stream create — cheap. The
	// blocking Recv loop runs on its own goroutine via ReadLoop.
	runCtx, cancel := context.WithCancel(m.deps.Ctx)
	stream, err := m.deps.Conv.OpenConverse(runCtx)
	if err != nil {
		cancel()
		m.conv.addError("open run: " + err.Error())
		return m.endRun("error"), nil
	}
	ch := make(chan tea.Msg, 64)
	m.stream = stream
	m.streamCh = ch
	m.cancelRun = cancel
	// ReadLoop selects on runCtx so it can never wedge if endRun stops draining
	// ch; endRun calls cancelRun, which unblocks and exits the reader.
	go stream.ReadLoop(runCtx, ch)

	send := func() tea.Msg {
		if err := stream.SendPrompt(m.sessionID, text); err != nil {
			return client.StreamErrMsg{Err: err}
		}
		return nil
	}
	return m, tea.Batch(send, m.waitCmd(), m.sp.Tick)
}

// afterEvent re-renders the conversation and re-arms the reader. Used by events
// that change the scrollback but don't need bespoke handling.
func (m Model) afterEvent() tea.Cmd {
	m.refreshView()
	return m.waitCmd()
}

// waitCmd re-arms the fan-in command on the current run channel. Returns nil when
// no run is active (defensive).
func (m Model) waitCmd() tea.Cmd {
	if m.streamCh == nil {
		return nil
	}
	return client.WaitForMsg(m.streamCh)
}

// refreshCmd is a no-op command used where Update must return a Cmd after a
// refreshView already mutated the viewport.
func (Model) refreshCmd() tea.Cmd { return nil }

// endRun tears down the current run: clears the stream/channel/cancel, returns to
// idle, and re-focuses input. The stop reason updates the status line.
func (m Model) endRun(stop string) Model {
	if m.cancelRun != nil {
		m.cancelRun()
		m.cancelRun = nil
	}
	m.stream = nil
	m.streamCh = nil
	m.activeTool = ""
	m.phase = phaseIdle
	// Re-enable input now the run is done. Focus() returns a cursor-blink cmd we
	// don't thread back here; the focus state itself is what matters for the
	// "input disabled while running" affordance, and the next keypress re-arms
	// the blink anyway.
	_ = m.ta.Focus()
	if stop != "" {
		m.statusMsg = "stop: " + stop
	}
	m.refreshView()
	return m
}

// refreshView re-renders the conversation into the viewport, keeping the view
// pinned to the bottom unless the user has scrolled up.
func (m *Model) refreshView() {
	content := m.rend.renderConversation(&m.conv, m.expandTools)
	// When the global details toggle is on, fold the session's changed-files list
	// in beneath the scrollback so the muted "Δ N files" header indicator has a
	// discoverable, scannable expansion — without a dedicated key or overlay.
	if m.expandTools {
		if list := m.rend.renderChangedFiles(m.filesChanged); list != "" {
			content += "\n" + list
		}
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(content)
	if m.stuck || atBottom {
		m.vp.GotoBottom()
	}
}

// suffix appends ": <text>" when text is non-empty, for notice formatting.
func suffix(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return ": " + text
}

// sumUsage accumulates token usage across a session for the footer.
func sumUsage(a, b client.Usage) client.Usage {
	return client.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
}

// createSessionCmd runs CreateSession off the update goroutine; result arrives as
// SessionReadyMsg or ConnectErrMsg.
func (m Model) createSessionCmd() tea.Cmd {
	deps := m.deps
	return func() tea.Msg {
		id, err := deps.Session.CreateSession(deps.Ctx)
		if err != nil {
			return client.ConnectErrMsg{Err: err}
		}
		return client.SessionReadyMsg{SessionID: id}
	}
}
