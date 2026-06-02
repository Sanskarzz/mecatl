package ui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// renderInterval is the coalescing window for streamed deltas: one frame at
// ~60fps. Streamed assistant/reasoning deltas arrive far faster than the eye can
// see, and each one would otherwise trigger a full conversation re-render (and a
// fresh glamour render of the live, growing block — O(n²) over a turn). Instead a
// delta only marks the view dirty; a renderTickMsg fired on this interval flushes
// the dirty view at most once per frame. The visible cadence is unchanged.
const renderInterval = 16 * time.Millisecond

// renderTickMsg is the one-shot frame-cadence flush signal. A streamed delta arms
// it (via markDirty) when none is pending; the renderTickMsg handler flushes the
// dirty view and disarms, re-arming only if more deltas arrived in the meantime.
type renderTickMsg struct{}

// renderTickCmd schedules a single frame-cadence flush.
func (Model) renderTickCmd() tea.Cmd {
	return tea.Tick(renderInterval, func(time.Time) tea.Msg { return renderTickMsg{} })
}

// markDirty records that a streamed delta mutated the conversation and returns the
// updated model plus the command to drive the coalesced flush. It arms a one-shot
// renderTickMsg ONLY when none is already pending (tickArmed) — so a burst of deltas
// schedules exactly one tick, not one per delta — and returns the reader re-arm
// otherwise. The flush itself happens in the renderTickMsg handler, never per delta.
//
// It returns (Model, tea.Cmd) — the well-defined form — rather than mutating via a
// pointer receiver and being called as `return m.markDirty()`. In that operand
// form the Go spec leaves the order of evaluating the returned `m` value and the
// `m.markDirty()` call UNSPECIFIED, so the dirty/armed flags the method sets could
// be snapshotted into the return value BEFORE they are set — silently losing the
// arm and stalling the coalesced flush until the next boundary. Returning the model
// makes the flag updates land on exactly the model the caller returns, deterministically.
func (m Model) markDirty() (Model, tea.Cmd) {
	m.viewDirty = true
	if m.tickArmed {
		return m, m.waitCmd()
	}
	m.tickArmed = true
	return m, tea.Batch(m.waitCmd(), m.renderTickCmd())
}

// Update is the Elm reducer. It is split by message type; all model mutation and
// all glamour rendering happen here on the single update goroutine (the stream
// reader never touches the model). After most state changes it calls refreshView
// to re-render the conversation into the viewport.
//
// Streamed deltas (AssistantDeltaMsg/ReasoningDeltaMsg) do NOT refreshView per
// token: they append to the conversation and mark the view dirty (markDirty),
// which arms a single one-shot renderTickMsg (~16ms ≈ one 60fps frame) that flushes
// the dirty view at most once per frame and disarms. The invariant is "ONLY the
// delta cases defer; every other transition force-flushes" — turn/tool/result/error
// AND the permission.ask gate all flush (via afterEvent/endRun), so the pending tail
// is always rendered before any boundary (the final frame and event ordering are
// unchanged; only the per-token re-render churn is coalesced). refreshView clears
// viewDirty, making "rendered ⟺ not dirty" an invariant.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	// Test-only deterministic progress observer (nil in production). Fired on the
	// update goroutine after the message is reduced so teatest can sequence on the
	// reducer's actual phase, not on the CPU-starved output flush. See Deps.onPhase.
	if m.deps.onPhase != nil {
		if mm, ok := model.(Model); ok {
			m.deps.onPhase(mm.phase)
		}
	}
	return model, cmd
}

// update is the body of the Elm reducer (see Update, which wraps it with the
// test-only onPhase observer).
func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.onResize(msg)

	case tea.KeyPressMsg:
		return m.onKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case renderTickMsg:
		// Frame-cadence flush of coalesced deltas. The one-shot tick has fired:
		// disarm, flush if a delta dirtied the view (refreshView clears viewDirty),
		// and re-arm a fresh one-shot only if more deltas are still pending AND a run
		// is streaming — so the ticker idles to zero when the stream goes quiet and
		// self-terminates at run end. No flush depends on this tick (every boundary
		// force-flushes), so a dropped/late tick can never lose the tail.
		m.tickArmed = false
		if m.viewDirty {
			m.refreshView()
		}
		if m.viewDirty && m.phase == phaseRunning {
			m.tickArmed = true
			return m, m.renderTickCmd()
		}
		return m, nil

	// Lifecycle / transport.
	case client.SessionReadyMsg:
		m.sessionID = msg.SessionID
		m.caps = msg.Capabilities // stored for Phase B; unrendered this phase
		m.phase = phaseIdle
		m.statusMsg = "connected"
		return m, nil
	case client.ConnectErrMsg:
		m.phase = phaseFatal
		m.fatalErr = msg.Err.Error()
		return m, nil
	case client.StreamErrMsg:
		m.conv.addError("stream error: " + msg.Err.Error())
		return m.endRun(stopError), m.refreshCmd()
	case client.StreamClosedMsg:
		// Clean close. If a run was still active (no terminal result seen),
		// finalise it; otherwise it's the expected post-result close (no-op).
		if m.phase == phaseRunning || m.phase == phaseAwaitingApproval {
			return m.endRun("closed"), m.refreshCmd()
		}
		return m, nil

	case client.CommandsMsg:
		// Slash-command discovery landed: store the set (a failure degrades quietly
		// to an empty palette) and re-sync so the palette reflects it immediately if
		// the input is still a command line.
		m.palette.commands = msg.Commands
		mm, cmd := m.syncPalette()
		return mm, cmd

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
		m.toolProgress = ""
		return m.afterEvent()
	case client.AssistantDeltaMsg:
		// Append only; markDirty arms a one-shot frame-cadence flush. No per-token
		// refreshView (that re-runs glamour on the growing live block every token —
		// O(n²) over a turn). The next turn/tool/result boundary force-flushes too.
		m.conv.appendAssistant(msg.Text)
		return m.markDirty()
	case client.ReasoningDeltaMsg:
		// Same coalescing as the assistant delta: append, markDirty, no refreshView.
		m.conv.appendReasoning(msg.Text)
		return m.markDirty()
	case client.TurnEndMsg:
		// The turn's model exchange is done: freeze any live "reasoning…"
		// affordance, then append a muted stat line unless the turn was trivial.
		m.conv.endReasoningStream()
		if !trivialTurn(msg) {
			m.conv.addTurnStat(turnStatLine(msg))
		}
		return m.afterEvent()
	case client.ToolCallMsg:
		m.conv.addTool(msg.ID, msg.Name, msg.Args)
		m.activeTool = msg.Name
		m.toolProgress = ""
		// Accumulate the file path for the session changed-files summary. Tracked
		// at call time (not on the result) so the header reflects intent the moment
		// the mutation is announced; mutatedPath gates to Edit/Write + dedupes.
		if p, ok := mutatedPath(msg.Name, msg.Args); ok {
			m.recordFileChange(p)
		}
		return m.afterEvent()
	case client.ToolResultMsg:
		if !m.conv.resolveTool(msg.CallID, msg.Content, msg.IsError) {
			m.conv.addNotice("orphan tool result for " + msg.CallID)
		}
		m.activeTool = ""
		m.toolProgress = ""
		return m.afterEvent()
	case client.ToolProgressMsg:
		// Transient advisory line from a long-running tool: show it beside the
		// spinner while the tool runs. It is cleared on the next tool.result or
		// turn boundary; it never enters the transcript.
		m.toolProgress = msg.Text
		return m.afterEvent()
	case client.PermissionAskMsg:
		m.phase = phaseAwaitingApproval
		m.activeTool = ""
		m.toolProgress = ""
		m.ask = pendingAsk{
			AskID:        msg.AskID,
			Tool:         msg.Tool,
			Args:         msg.Args,
			Reason:       msg.Reason,
			allowFocused: true,
		}
		// Force-flush via afterEvent (refreshView + reader re-arm), like every other
		// non-delta boundary: any pending coalesced assistant tail must be rendered
		// into the viewport BEFORE the modal opens, so the transcript behind the modal
		// is current the moment it closes (the same "content flushes before a gate"
		// value as the toolcall-card-before-gate invariant). refreshView updating m.vp
		// underneath the centred modal overlay is harmless — View() shows the modal for
		// phaseAwaitingApproval regardless. Without this, the tail's flush would depend
		// on an already-armed one-shot tick happening to survive the phase change — the
		// one boundary that previously did NOT flush, breaking the "only deltas defer"
		// invariant.
		return m.afterEvent()
	case client.HookMsg:
		m.conv.addHook(msg.Text, msg.Phase, msg.Tool, string(msg.Decision))
		return m.afterEvent()
	case client.SubagentMsg:
		m.applySubagent(msg)
		return m.afterEvent()
	case client.TeamMsg:
		m.applyTeam(msg)
		return m.afterEvent()
	case client.CompactionMsg:
		m.conv.addNotice("history compacted" + suffix(msg.Text))
		return m.afterEvent()
	case client.ResultMsg:
		m.usage = sumUsage(m.usage, msg.Usage)
		// The latest turn's prompt size is the current context occupancy
		// (InputTokens already includes cache-served tokens).
		m.contextTokens = msg.Usage.InputTokens
		if msg.Stop == stopError && msg.Error != "" {
			m.conv.addError(msg.Error)
		}
		return m.endRun(msg.Stop), m.refreshCmd()
	default:
		return m, nil
	}
}

// applySubagent attributes a REDACTED subagent projection to its Task tool card by
// ParentCallID (id match, like resolveTool). A miss is silently dropped: the card
// carries no child content either way, so a lost subagent event only costs the
// trace, never correctness or isolation.
func (m *Model) applySubagent(msg client.SubagentMsg) {
	switch msg.Kind {
	case client.SubagentStart:
		m.conv.setSubagentStart(msg.ParentCallID, msg.Goal)
	case client.SubagentTool:
		m.conv.addSubagentTool(msg.ParentCallID, msg.ToolName, msg.IsError, msg.ToolCount)
	case client.SubagentEnd:
		m.conv.setSubagentEnd(msg.ParentCallID, msg.Usage, msg.ToolCount, msg.Stop, msg.DurationMs)
	}
}

// applyTeam attributes a BOUNDED team projection to its Team tool card by
// ParentCallID (id match, like applySubagent). A miss is silently dropped: the
// member transcripts never enter the parent conversation either way, so a lost
// team.* event only costs the lane trace, never correctness or isolation.
func (m *Model) applyTeam(msg client.TeamMsg) {
	switch msg.Kind {
	case client.TeamStart:
		m.conv.setTeamStart(msg.ParentCallID, msg.TeamID, msg.Roster)
	case client.TeamMember:
		m.conv.addTeamMember(msg)
	case client.TeamTasks:
		m.conv.setTeamTasks(msg.ParentCallID, msg.Tasks)
	case client.TeamEnd:
		m.conv.setTeamEnd(msg.ParentCallID, msg.TeamID, msg.Rounds, msg.Stop, msg.Usage)
		// team.end carries the terminal task snapshot too, so the task sub-view lands
		// the final state even if no member event followed the last transition.
		m.conv.setTeamTasks(msg.ParentCallID, msg.Tasks)
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

	// An open agent-team overlay likewise owns the keyboard (idle-only). It steps
	// back from focus → roster → closed on esc internally, so route here before the
	// phase switch (and before the ctrl+t toggle, so esc/enter belong to it).
	if mm, cmd, handled := m.onAgentsKey(msg); handled {
		return mm, cmd
	}

	// An open help overlay owns the keyboard: "?" or esc closes it, everything
	// else is swallowed. Routed AFTER the MCP/agents overlays (they never coexist;
	// those handlers return handled=false when closed) and BEFORE the ctrl+t
	// toggle and the phase switch — so a "?" pressed while help is up closes it
	// rather than reopening or leaking to the textarea.
	if m.showHelp {
		if key.Matches(msg, m.keys.Help) || key.Matches(msg, m.keys.Close) {
			m.showHelp = false
			_ = m.ta.Focus()
		}
		return m, nil
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
//
// The slash-command palette is woven in BEFORE the textarea path: while it is
// open it claims ↑/↓ (move selection), tab/enter (complete), and esc (dismiss)
// so those keys drive completion instead of the normal idle bindings. When it is
// closed every key falls through unchanged, and after any key that may have
// edited the input the palette is re-synced (open/filter/fetch) from the new
// content — so it appears the moment the input becomes "/…" and tracks the
// typed prefix.
func (m Model) onIdleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.palette.open {
		if mm, cmd, handled := m.onPaletteKey(msg); handled {
			return mm, cmd
		}
	}
	switch {
	case key.Matches(msg, m.keys.Help) && strings.TrimSpace(m.ta.Value()) == "":
		// "?" is printable: open help only on an empty prompt so "?" in prose still
		// inserts literally. The overlay claims the keyboard via the m.showHelp gate
		// in onKey; blur the input while it is up.
		m.showHelp = true
		m.ta.Blur()
		return m, nil
	case key.Matches(msg, m.keys.MCPPanel):
		return m.openMCP(mcpPanel)
	case key.Matches(msg, m.keys.Resources):
		return m.openMCP(mcpResources)
	case key.Matches(msg, m.keys.Prompts):
		return m.openMCP(mcpPrompts)
	case key.Matches(msg, m.keys.Agents):
		return m.openAgents()
	case key.Matches(msg, m.keys.Newline):
		m.ta.InsertRune('\n')
		return m.afterInputEdit(nil)
	case key.Matches(msg, m.keys.Submit):
		return m.submitPrompt()
	case key.Matches(msg, m.keys.ScrollU), key.Matches(msg, m.keys.ScrollD):
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	default:
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m.afterInputEdit(cmd)
	}
}

// onPaletteKey handles keys while the slash-command palette is open. It returns
// handled=false for keys the palette does not claim, so the caller falls through
// to the normal idle handling (and the key still reaches the textarea). Most of
// the palette's actions only mutate model state (no command); the exception is
// enter/tab over a BUILT-IN row, which runs the built-in directly (it may issue
// a command, e.g. opening an overlay).
func (m Model) onPaletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	switch msg.String() {
	case "up":
		m.paletteMoveUp()
		return m, nil, true
	case "down":
		m.paletteMoveDown()
		return m, nil, true
	case "tab", "enter":
		if mm, cmd, ran := m.runSelectedBuiltin(); ran {
			return mm, cmd, true
		}
		return m.paletteComplete(), nil, true
	case "esc":
		return m.paletteDismiss(), nil, true
	}
	return m, nil, false
}

// runSelectedBuiltin runs the currently-selected palette row IF it is a built-in
// command, closing the palette and resetting the input first, and reports ran=
// true. For a non-built-in (workspace) row it reports ran=false so the caller
// falls back to paletteComplete (text-completion). Running built-ins directly on
// palette-enter — rather than text-completing them — is deliberate:
// paletteComplete writes "/<name> " with a TRAILING SPACE, which makes
// commandPrefix false and would slip the line past the submitPrompt built-in
// intercept; for /clear the user expects enter to act, not to pre-fill the input.
func (m Model) runSelectedBuiltin() (tea.Model, tea.Cmd, bool) {
	if !m.palette.open || m.palette.cursor >= len(m.palette.filtered) {
		return m, nil, false
	}
	row := m.palette.filtered[m.palette.cursor]
	if !row.Builtin {
		return m, nil, false
	}
	b, found := builtinByName(m.caps, m.deps.MCP != nil, row.Name)
	if !found {
		return m, nil, false
	}
	// Close the palette and clear the input before acting (mirrors the bare-line
	// submit intercept), then dispatch.
	m.palette.open = false
	m.palette.filtered = nil
	m.palette.cursor = 0
	m.ta.Reset()
	mm, cmd := b.run(m)
	return mm, cmd, true
}

// afterInputEdit re-syncs the palette from the (possibly changed) textarea
// content and batches the palette's fetch command with cmd (the textarea's own
// command, e.g. a cursor blink). It is the single funnel every idle key path that
// edits the input runs through, so the palette can never get out of step with the
// input.
func (m Model) afterInputEdit(cmd tea.Cmd) (tea.Model, tea.Cmd) {
	mm, fetch := m.syncPalette()
	if fetch == nil {
		return mm, cmd
	}
	if cmd == nil {
		return mm, fetch
	}
	return mm, tea.Batch(cmd, fetch)
}

// submitPrompt opens a fresh Converse run for the textarea text, sends the
// mandatory prompt frame, starts the reader goroutine, and arms WaitForMsg.
func (m Model) submitPrompt() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" || m.sessionID == "" {
		return m, nil
	}
	// A BARE built-in command line ("/clear", "/help", …) is intercepted here —
	// before addUser / stream-open — and dispatched to its Model action, so the
	// built-in text never reaches the model. A non-built-in "/…" falls through to
	// the normal send (preserving bare workspace-command invocation), and a
	// "/name arg" line has a space → commandPrefix is false → also falls through
	// (workspace commands expand server-side from the full line).
	if name, ok := commandPrefix(text); ok {
		if b, found := builtinByName(m.caps, m.deps.MCP != nil, name); found {
			m.ta.Reset()
			return b.run(m)
		}
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
		return m.endRun(stopError), nil
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

// afterEvent re-renders the conversation and re-arms the reader, returning the
// updated model and the re-arm command. Used by events that change the scrollback
// but don't need bespoke handling.
//
// It returns (Model, tea.Cmd) and is called as `return m.afterEvent()` — the
// well-defined form — rather than mutating via a pointer receiver and being called
// as `return m.afterEvent()`. In that operand form the Go spec leaves the order
// of evaluating the returned `m` value and the `m.afterEvent()` call UNSPECIFIED,
// so the refreshView (and its viewDirty clear) could be snapshotted into the return
// value BEFORE it runs — silently dropping the boundary force-flush the delta-
// coalescing relies on (deltas only mark dirty; turn/tool/result boundaries MUST
// flush). Returning the model makes every afterEvent boundary a genuine, deterministic
// flush on exactly the model the caller returns.
func (m Model) afterEvent() (Model, tea.Cmd) {
	m.refreshView()
	return m, m.waitCmd()
}

// waitCmd re-arms the fan-in command on the current run channel. Returns nil when
// no run is active (defensive).
func (m Model) waitCmd() tea.Cmd {
	if m.streamCh == nil {
		return nil
	}
	return client.WaitForMsg(m.streamCh)
}

// refreshCmd is the command returned on the run-completion paths, after endRun +
// refreshView have already settled the final frame. It forces a full repaint
// (tea.ClearScreen erases and redraws from scratch) so the terminating frame the
// user is left reading is reconciled cleanly against any genuine diff dirt the
// differential renderer left behind while diffing a fast, reflowing stream. It does
// NOT heal the streaming scramble: that is a deterministic width-method layout
// error (glamour wraps on GraphemeWidth, the renderer paints on WcWidth — see
// render.go's normalizeEmojiWidth), so a re-paint just reproduces the same wrong
// layout. The scramble is fixed at the source by normalizeEmojiWidth; this repaint
// is retained only for ordinary stale cells. It fires once per run end, never per
// delta, so the cost is a single repaint when a turn settles.
func (Model) refreshCmd() tea.Cmd { return tea.ClearScreen }

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
		text, slot := stopReasonLabel(stop)
		m.statusMsg = m.deps.Theme.Style(slot).Render(text)
	}
	m.refreshView()
	return m
}

// refreshView re-renders the conversation into the viewport, keeping the view
// pinned to the bottom unless the user has scrolled up. It clears m.viewDirty, so
// every render path (afterEvent, endRun, onResize, the frame-cadence renderTickMsg)
// settles the flag — making "rendered ⟺ not dirty" an invariant and force-flushing
// the tail at every turn/tool/result/error boundary regardless of tick timing.
func (m *Model) refreshView() {
	m.viewDirty = false
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
		id, caps, err := deps.Session.CreateSession(deps.Ctx)
		if err != nil {
			return client.ConnectErrMsg{Err: err}
		}
		return client.SessionReadyMsg{SessionID: id, Capabilities: caps}
	}
}
