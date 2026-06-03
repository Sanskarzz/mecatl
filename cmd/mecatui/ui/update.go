package ui

import (
	"context"
	"fmt"
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

	case tea.PasteMsg:
		return m.onPaste(msg)

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
		// An error PAUSES the queue (shouldDrain(stopError) is false): the staged
		// follow-ups are kept intact and marked paused (m.queuePaused) so the queue
		// card says why, not auto-sent into a broken run. drainQueue records the pause
		// here, but the policy lives in one place.
		m.conv.addError("stream error: " + msg.Err.Error())
		m = m.endRun(stopError)
		mm, drainCmd := m.drainQueue(stopError)
		return mm, tea.Batch(m.refreshCmd(), drainCmd)
	case client.StreamClosedMsg:
		// Clean close. If a run was still active (no terminal result seen),
		// finalise it; otherwise it's the expected post-result close (no-op).
		if m.phase == phaseRunning || m.phase == phaseAwaitingApproval {
			m = m.endRun("closed")
			mm, drainCmd := m.drainQueue("closed")
			return mm, tea.Batch(m.refreshCmd(), drainCmd)
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
		m = m.endRun(msg.Stop)
		mm, drainCmd := m.drainQueue(msg.Stop)
		return mm, tea.Batch(m.refreshCmd(), drainCmd)
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

// onPaste routes a bracketed-paste payload to the prompt input. Bubble Tea v2
// emits tea.PasteMsg (not KeyPressMsg) for a paste, so it does NOT pass through
// onKey — this mirrors onKey's overlay/help/phase gating itself. A paste is
// dropped while any overlay/help/approval owns the keyboard (it must not leak
// into the input behind the modal); otherwise it is accepted ONLY in the
// input-accepting phases (idle or running — both keep the textarea focused for
// compose/enqueue) and forwarded to the textarea, which inserts the runes
// internally, then run through afterInputEdit so the slash palette re-syncs
// (e.g. pasting "/cl" opens the palette) — the same funnel typed input uses.
func (m Model) onPaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.showHelp || m.phase == phaseAwaitingApproval ||
		m.mcp.view != mcpNone || m.agents.view != agentsNone {
		return m, nil
	}
	if m.phase != phaseIdle && m.phase != phaseRunning {
		return m, nil
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m.afterInputEdit(cmd)
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

// onRunningKey handles keys while a run streams. Type-while-running: the textarea
// stays focused so the user can compose and ENQUEUE a follow-up. It mirrors
// onIdleKey's precedence so the input behaves the same mid-run as at idle, with
// two differences — enter ENQUEUES (instead of submitting), and esc has a layered
// meaning before it falls through to cancel:
//
//	(1) an open palette claims its NAVIGATION keys (↑/↓/tab/esc) so /-typing shows
//	    the dropdown while running — but NOT enter: mid-run enter must always ENQUEUE
//	    uniformly (the locked decision). So a "/clear" line composed mid-run is staged
//	    as the literal text "/clear" and dispatched as a built-in at DRAIN time (where
//	    the phase is idle and /clear's idle-guard is satisfied) via submitPrompt's
//	    existing intercept — never run immediately mid-run via the palette;
//	(2) esc/Cancel: non-empty input → clear the input (and resync the palette);
//	    else non-empty queue → clear the queue (status "queue cleared"); else →
//	    SendCancel (today's behaviour: the run ends with stop "cancelled");
//	(3) shift+enter (Newline) → insert a newline;
//	(4) enter (Submit) → enqueuePrompt (the locked decision: enter mid-run stages a
//	    follow-up, it does not submit a second concurrent run);
//	(5) pgup/pgdn → scroll the viewport;
//	(6) anything else → feed the textarea (+ palette resync via afterInputEdit).
//
// ctrl+t (expand) and ctrl+c (quit) are handled globally in onKey before this.
func (m Model) onRunningKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Let the palette claim its navigation keys while running, but NOT enter — enter
	// mid-run always enqueues (uniform), so it must fall through to the Submit case
	// below rather than completing/running a palette row. (esc is handled by the
	// Cancel branch below, which layers clear-input/clear-queue/cancel — the palette's
	// own esc-dismiss would shadow that, so it is excluded here too.)
	if m.palette.open && !key.Matches(msg, m.keys.Submit) && !key.Matches(msg, m.keys.Cancel) {
		if mm, cmd, handled := m.onPaletteKey(msg); handled {
			return mm, cmd
		}
	}
	// The @-mention menu, like the palette, claims its navigation/complete keys
	// while running EXCEPT enter (which must enqueue uniformly) and esc (the
	// Cancel branch layers clear-input/clear-queue/cancel below).
	if m.mention.open && !key.Matches(msg, m.keys.Submit) && !key.Matches(msg, m.keys.Cancel) {
		if mm, handled := m.onMentionKey(msg); handled {
			return mm, nil
		}
	}
	switch {
	case key.Matches(msg, m.keys.Cancel):
		if strings.TrimSpace(m.ta.Value()) != "" {
			// Staged-but-unsent input: esc clears it first (mirrors a text editor's
			// "esc clears the line"), leaving the queue and the run untouched.
			m.ta.Reset()
			return m.afterInputEdit(nil)
		}
		if len(m.queued) > 0 {
			// No live input but staged follow-ups: esc drops the queue before it would
			// cancel the run, so a user who changed their mind can clear the backlog
			// without killing the in-flight turn.
			m.queued = nil
			m.statusMsg = m.deps.Theme.Style("muted").Render("queue cleared")
			m.refreshView()
			return m, nil
		}
		// Nothing staged: esc cancels the run (today's behaviour — the run ends with
		// stop "cancelled"; the stream stays open until that terminal result).
		stream := m.stream
		m.statusMsg = "cancelling…"
		return m, func() tea.Msg {
			if stream != nil {
				_ = stream.SendCancel()
			}
			return nil
		}
	case key.Matches(msg, m.keys.Newline):
		m.ta.InsertRune('\n')
		return m.afterInputEdit(nil)
	case key.Matches(msg, m.keys.Submit):
		return m.enqueuePrompt()
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

// enqueuePrompt stages the current textarea text as a follow-up to drain when the
// running turn ends (see drainQueue). It trims the input; an empty line is a no-op
// (so enter on a blank prompt mid-run does nothing). At the cap it rejects with a
// muted "queue full" status and KEEPS the input (the user can edit it down or wait
// for a drain to free a slot). Otherwise it appends, resets the input, sets a
// "queued (N)" status, and re-syncs the palette (afterInputEdit) so the dropdown
// closes now that the "/" line is gone.
//
// Crucially it does NOT open a stream or send anything — the queued text becomes a
// real prompt only when drainQueue later hands it to submitPrompt (the existing
// send path, which maps to the server-side StartRunContent reopen). There is no
// second send path.
func (m Model) enqueuePrompt() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return m, nil
	}
	if len(m.queued) >= maxQueued {
		m.statusMsg = m.deps.Theme.Style("muted").Render(fmt.Sprintf("queue full (%d)", maxQueued))
		return m, nil
	}
	m.queued = append(m.queued, text)
	m.ta.Reset()
	m.statusMsg = m.deps.Theme.Style("muted").Render(fmt.Sprintf("queued (%d)", len(m.queued)))
	m.refreshView()
	return m.afterInputEdit(nil)
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
	// The @-mention menu (mutually exclusive with the palette) claims the same
	// navigation/complete keys while it is open.
	if m.mention.open {
		if mm, handled := m.onMentionKey(msg); handled {
			return mm, nil
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
	case key.Matches(msg, m.keys.Cancel) && m.queuePaused != "":
		// A run ended on a non-clean stop with staged follow-ups still queued (the
		// paused state). Mirror the running-phase esc layering: a non-empty input is
		// cleared first; otherwise esc drops the paused queue. (With no input and an
		// empty queue queuePaused is already "", so this branch never strands esc.)
		if strings.TrimSpace(m.ta.Value()) != "" {
			m.ta.Reset()
			return m.afterInputEdit(nil)
		}
		m.queued = nil
		m.queuePaused = ""
		m.statusMsg = m.deps.Theme.Style("muted").Render("queue cleared")
		m.refreshView()
		return m, nil
	case key.Matches(msg, m.keys.Newline):
		m.ta.InsertRune('\n')
		return m.afterInputEdit(nil)
	case key.Matches(msg, m.keys.Submit):
		// While paused, enter on an EMPTY line RESUMES: fire the next staged prompt
		// manually. A non-empty line falls through to a normal submit (which also
		// clears the pause and lets the queue drain at the new run's clean end).
		if m.queuePaused != "" && len(m.queued) > 0 && strings.TrimSpace(m.ta.Value()) == "" {
			return m.resumeQueue()
		}
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
	case keyMenuUp:
		m.paletteMoveUp()
		return m, nil, true
	case keyMenuDown:
		m.paletteMoveDown()
		return m, nil, true
	case keyMenuTab, keyMenuEnter:
		if mm, cmd, ran := m.runSelectedBuiltin(); ran {
			return mm, cmd, true
		}
		return m.paletteComplete(), nil, true
	case keyMenuDismiss:
		return m.paletteDismiss(), nil, true
	}
	return m, nil, false
}

// onMentionKey handles keys while the @-mention file menu is open. Like
// onPaletteKey it reports handled=false for keys it does not claim (so the key
// still reaches the textarea). Unlike onPaletteKey it issues no command —
// completing a mention only rewrites the input, never runs a built-in — so it
// returns just (Model, handled). up/down move the selection; tab/enter complete
// the highlighted path; esc dismisses. The two menus never coexist (mutually
// exclusive tokens), so the caller routes to whichever is open.
func (m Model) onMentionKey(msg tea.KeyPressMsg) (Model, bool) {
	switch msg.String() {
	case keyMenuUp:
		m.mentionMoveUp()
		return m, true
	case keyMenuDown:
		m.mentionMoveDown()
		return m, true
	case keyMenuTab, keyMenuEnter:
		return m.mentionComplete(), true
	case keyMenuDismiss:
		return m.mentionDismiss(), true
	}
	return m, false
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
	// The @-mention menu syncs from the SAME input edit. It is mutually exclusive
	// with the palette (mentionToken returns false for a "/" line and for a
	// multi-line input), so at most one opens; the sync is synchronous (no fetch).
	mm = mm.syncMention()
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
	if m.sessionID == "" {
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
	// Expand any @-mentions that resolve to an existing REGULAR FILE into media
	// parts (image/audio) and inlined text-file bodies. attachableMentions is the
	// stat-filter gate: a token that is not a real file (prose like "@oncall", a
	// directory, a dangling link) is left as literal text and never reaches
	// ExpandMentions. The remaining filesystem + proto work lives behind
	// client.ExpandMentions (the ui passes only resolved path strings + the
	// proto-free caps, and reads back proto-free Descriptors/InlineText — the proto
	// Parts stay opaque, kept in the MediaResult and handed straight to SendPrompt,
	// so the ui never names a proto type). ANY error LOUD-rejects: surface it in the
	// transcript, keep the input intact, send NOTHING.
	var media client.MediaResult
	if files := attachableMentions(m.deps.Workspace, text); len(files) > 0 {
		res, err := client.ExpandMentions(files, m.caps)
		if err != nil {
			m.conv.addError("attach: " + err.Error())
			m.refreshView()
			return m, nil
		}
		media = res
		for _, body := range res.InlineText {
			text = strings.TrimSpace(text + "\n\n" + body)
		}
	}
	// A media-only prompt (empty text but at least one part) still sends — the proto
	// allows text OR parts, and the server enforces "at least one non-empty". A
	// truly empty submit (no text AND no parts) is the no-op early-return.
	if text == "" && len(media.Parts) == 0 {
		return m, nil
	}
	if len(media.Descriptors) > 0 {
		m.conv.addUserWithMedia(text, media.Descriptors)
	} else {
		m.conv.addUser(text)
	}
	m.ta.Reset()
	// A fresh run clears any queue pause: whether this is the auto-drain (popAndSubmit
	// already cleared it) or a manual send while paused, the queue now gets a new
	// chance to drain at this run's clean completion, so it is no longer "paused".
	m.queuePaused = ""
	// The textarea stays FOCUSED while running so the user can type a follow-up and
	// enqueue it (see enqueuePrompt / onRunningKey). It used to Blur here to signal
	// "input disabled while running"; type-while-running supersedes that.
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
		if err := stream.SendPrompt(m.sessionID, text, media.Parts); err != nil {
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
	// Ensure the input is focused now the run is done. With type-while-running the
	// input is already focused during a run, so this is a no-op on the common path;
	// it still matters as the single re-focus point after a blur the run may have
	// crossed (e.g. an overlay that blurred the textarea). Focus() returns a
	// cursor-blink cmd we don't thread back here; the next keypress re-arms the blink.
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

// drainQueue pops the oldest staged follow-up and submits it, one at a time. It is
// called on every run-completion path (ResultMsg / StreamErrMsg / StreamClosedMsg)
// AFTER endRun has settled the model back to idle.
//
// It fires on a HEALTHY stop (shouldDrain): end_turn, the empty reason, or a size
// limit (max_turns / max_tool_calls). On a non-healthy stop — error, a user-cancel
// ("cancelled"), max_consecutive_failures, or a stream close — it does NOT fire:
// instead it records the stop reason in m.queuePaused and KEEPS the queue, so the
// run that died never silently fires the next staged prompt. The user then resumes
// with enter on an empty line (resumeQueue) or clears with esc — renderQueue shows
// that affordance. The phase==phaseIdle guard is belt-and-braces (endRun always
// lands idle on these paths) so a future caller can't drain into a still-running
// model.
//
// The submit goes through the EXISTING submitPrompt path — the same one a typed
// prompt uses — so the queued prompt reopens the completed session server-side
// (StartRunContent) exactly like a manual follow-up; there is no separate send
// path. submitPrompt sets phaseRunning and opens a fresh stream whose own ResultMsg
// re-enters drainQueue, giving a one-at-a-time FIFO drain.
func (m Model) drainQueue(stop string) (tea.Model, tea.Cmd) {
	if len(m.queued) == 0 {
		m.queuePaused = ""
		return m, nil
	}
	if !shouldDrain(stop) {
		// A non-clean stop (error / user-cancel / repeated failures / stream close)
		// with staged follow-ups: PAUSE and KEEP the queue, but record the reason so
		// renderQueue can say so loudly (and the idle keys can resume/clear it) — a
		// silent "N queued" after the run died reads as a hang. The user resumes with
		// enter on an empty line (resumeQueue) or clears with esc.
		m.queuePaused = stop
		return m, nil
	}
	if m.phase != phaseIdle {
		return m, nil // belt-and-braces: never drain into a still-running model.
	}
	return m.popAndSubmit()
}

// popAndSubmit pops the oldest staged follow-up, clears any pause, and submits it
// through the EXISTING submitPrompt path (server-side StartRunContent reopen) — the
// single shared body behind both the auto-drain (drainQueue) and the manual resume
// (resumeQueue). submitPrompt sets phaseRunning and opens a fresh stream whose own
// ResultMsg re-enters drainQueue, giving a one-at-a-time FIFO drain.
func (m Model) popAndSubmit() (tea.Model, tea.Cmd) {
	m.queuePaused = ""
	next := m.queued[0]
	m.queued = m.queued[1:]
	m.ta.SetValue(next)
	return m.submitPrompt()
}

// resumeQueue is the MANUAL counterpart to the auto-drain: it fires the next staged
// follow-up when the user presses enter on an empty line while the queue is paused
// (a run ended on a non-clean stop; see onIdleKey). It shares popAndSubmit with
// drainQueue so the two triggers — automatic on a healthy completion, manual on
// resume — go through one body and one send path. Callers gate on a non-empty,
// paused queue; this assumes m.queued is non-empty.
func (m Model) resumeQueue() (tea.Model, tea.Cmd) {
	return m.popAndSubmit()
}

// shouldDrain reports whether a terminal stop reason should auto-fire the next
// staged follow-up. It drains on a HEALTHY stop — one where the model was either
// done or merely hit a SIZE bound, so feeding the next staged prompt simply
// continues the work the user lined up:
//
//   - ""        — treated as a clean end_turn throughout (cf. stopReasonLabel).
//   - end_turn  — the model finished without requesting more tools.
//   - max_turns / max_tool_calls — the run hit a per-run budget. The model was
//     healthy; it just ran out of room. A queued follow-up ("continue", or the next
//     step) is exactly what's wanted here, and firing it reopens the session with a
//     fresh budget — so these DRAIN (issue: a silent pause-on-limit read as a hang).
//
// Everything else PAUSES the drain and keeps the queue intact, because the run
// stopped for a bad reason or the user intervened — firing a follow-up into it would
// be surprising:
//
//   - max_consecutive_failures — the run was failing repeatedly; don't pile on.
//   - cancelled — the USER stopped the run; auto-resuming would fight that intent.
//   - error / "closed" — the run broke or the stream ended abnormally.
//
// The vocabulary is the session.StopReason set plus the "closed" StreamClosed
// sentinel; stopReasonLabel renders the same set for the footer.
func shouldDrain(stop string) bool {
	switch stop {
	case "", "end_turn", "max_turns", "max_tool_calls":
		return true
	default:
		return false
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
