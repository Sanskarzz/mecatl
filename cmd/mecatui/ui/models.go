package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// models.go is the /models picker — the FIRST *selecting* overlay (every other
// inventory overlay is read-only / esc-only). It mirrors the mcp.go resource
// picker's cursor+Choose model, NOT the soul/usermodel read-only model. Selecting
// a row sets the ACTIVE selection, persists it via the injected SelectionStore,
// and shows a status notice. Per the slice's UX decision it does NOT recreate the
// live session: the selection is applied to the NEXT CreateSession (apply-on-next-
// create). The picker renders purely from client.ModelInfo — no proto in ui.

// modelsView is the active /models overlay (none = closed). Like the other
// inventory overlays it is idle-only and dismissed with esc; UNLIKE them it has a
// real cursor and an enter-to-select action.
type modelsView int

const (
	modelsNone  modelsView = iota // overlay closed
	modelsPanel                   // the grouped picker
)

// modelsState holds the /models overlay state on the Model. Value-embedded so the
// Model stays a plain struct Update copies; the slice is replaced wholesale on
// each RPC result.
type modelsState struct {
	view    modelsView
	loading bool  // the ListModels RPC is in flight
	err     error // last ListModels error, rendered distinctly
	models  []client.ModelInfo
	cursor  int                   // index into the FLATTENED model list (not counting group headers)
	active  client.ModelSelection // the persisted/active selection (drives the ● marker)
}

// openModels opens the picker and fires the ListModels RPC. Only callable while
// idle and when a model lister is wired; returns the model unchanged otherwise.
// The result arrives as a client.ModelsMsg handled in updateModelsMsg. The active
// selection is preserved across opens (it lives for the whole session), so the ●
// marker survives a close/reopen.
func (m Model) openModels() (tea.Model, tea.Cmd) {
	if m.phase != phaseIdle || m.deps.Models == nil {
		return m, nil
	}
	m.ta.Blur() // overlay owns the keyboard while open
	m.models.view = modelsPanel
	m.models.loading = true
	m.models.err = nil
	m.models.cursor = 0
	return m, client.ListModelsCmd(m.deps.Ctx, m.deps.Models)
}

// closeModels dismisses the overlay and returns focus to the prompt input. The
// active selection + the loaded list survive (the picker is reopened cheaply).
func (m Model) closeModels() (tea.Model, tea.Cmd) {
	m.models.view = modelsNone
	cmd := m.ta.Focus()
	return m, cmd
}

// onModelsKey routes key presses while the picker is open: up/down move the cursor
// across the flattened list, enter selects the cursor model (sets active +
// persists + notice), esc closes. Returns handled=false when closed so the caller
// falls through to normal idle key handling.
func (m Model) onModelsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if m.models.view == modelsNone {
		return m, nil, false
	}
	switch {
	case key.Matches(msg, m.keys.Close):
		mm, cmd := m.closeModels()
		return mm, cmd, true
	case key.Matches(msg, m.keys.Up):
		if m.models.cursor > 0 {
			m.models.cursor--
		}
		return m, nil, true
	case key.Matches(msg, m.keys.Down):
		if m.models.cursor < len(m.models.models)-1 {
			m.models.cursor++
		}
		return m, nil, true
	case key.Matches(msg, m.keys.Choose):
		mm, cmd := m.chooseModel()
		return mm, cmd, true
	}
	return m, nil, true
}

// chooseModel selects the cursor model: it sets the active selection, fires the
// persist Cmd (off the update goroutine, like every store write), and shows a
// success notice. It does NOT recreate the session (apply-on-next-create) — the
// notice says as much. A cursor past the list end is a no-op (defensive).
func (m Model) chooseModel() (tea.Model, tea.Cmd) {
	if m.models.cursor < 0 || m.models.cursor >= len(m.models.models) {
		return m, nil
	}
	chosen := m.models.models[m.models.cursor]
	sel := client.ModelSelection{ProviderID: chosen.ProviderID, ModelID: chosen.ID}
	m.models.active = sel
	m.activeModel = sel // header display reads from this once set (see renderHeader)
	m.statusMsg = m.deps.Theme.Style("success").Render(
		"model set: " + sanitizeTerminal(modelLabel(chosen)) + " — applies to the next session")
	return m, m.saveSelectionCmd(sel)
}

// saveSelectionCmd persists the selection via the injected SelectionStore off the
// update goroutine. A nil store (persistence disabled) or a write failure is
// fail-soft: the in-memory active selection still holds for this run, and the
// failure surfaces as a muted notice — never a crash, never an aborted pick. The
// result arrives as a selectionSavedMsg.
func (m Model) saveSelectionCmd(sel client.ModelSelection) tea.Cmd {
	store := m.deps.SelectionStore
	ws := m.deps.Workspace
	return func() tea.Msg {
		if store == nil {
			return selectionSavedMsg{}
		}
		return selectionSavedMsg{err: store.Save(ws, sel)}
	}
}

// selectionSavedMsg is the result of a SelectionStore.Save (nil err on success).
type selectionSavedMsg struct{ err error }

// updateModelsMsg reduces the client.ModelsMsg into the picker AND performs the
// key-removed reconcile (§4): when the loaded list does NOT contain the active
// selection, the active selection is CLEARED to the server default (so subsequent
// creates don't send a now-unavailable provider — which the server would reject
// with InvalidArgument) and a loud notice fires. The state file is NOT rewritten
// (the key may return next launch).
//
// During CONNECT (phaseConnecting), this is the first leg of the §4 sequence:
// ListModels lands, the persisted selection is reconciled, THEN it fires
// CreateSession (carrying the now-validated selection) — so the startup create
// never sends a removed provider. The returned Cmd is the create in that case, nil
// otherwise. handled=false for any other msg.
func (m Model) updateModelsMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case client.ModelsMsg:
		m.models.loading = false
		if msg.Err != nil {
			m.models.err = msg.Err
			// A connect-time ListModels error must NOT strand the UI at "connecting…":
			// proceed to create with the seeded (unreconciled) selection. The picker
			// still surfaces the error when opened. This is rare (the lister is the
			// dialled client), and degrading to "try the create" is safer than hanging.
			if m.phase == phaseConnecting {
				return m, m.createSessionCmd(), true
			}
			return m, nil, true
		}
		m.models.err = nil
		m.models.models = msg.Models
		if m.models.cursor >= len(m.models.models) {
			m.models.cursor = 0
		}
		m = m.reconcileSelection()
		if m.phase == phaseConnecting {
			// Reconcile done; now create the session with the validated selection.
			return m, m.createSessionCmd(), true
		}
		return m, nil, true
	case selectionSavedMsg:
		if msg.err != nil {
			m.statusMsg = m.deps.Theme.Style("warning").Render(
				"model set for this run, but could not persist: " + sanitizeTerminal(msg.err.Error()))
		}
		return m, nil, true
	default:
		return m, nil, false
	}
}

// reconcileSelection clears the active selection to the server default when it
// names a model absent from the loaded list (the key-removed fallback) and fires a
// loud notice. A zero active selection (server default already in use) or a still-
// available one is left untouched. It returns the model with the (possibly
// cleared) selection; the state file is never rewritten here.
func (m Model) reconcileSelection() Model {
	if m.models.active.IsZero() {
		return m
	}
	for _, mi := range m.models.models {
		if m.models.active.Matches(mi) {
			return m // still available — keep it
		}
	}
	// The persisted provider/model is gone (key removed since last launch). Fall
	// back to the server default for THIS run, loudly, without rewriting the state
	// file (the preference may come back next launch).
	gone := m.models.active.ModelID
	if gone == "" {
		gone = m.models.active.ProviderID
	}
	m.models.active = client.ModelSelection{}
	m.activeModel = client.ModelSelection{}
	m.statusMsg = m.deps.Theme.Style("warning").Render(
		"saved model " + sanitizeTerminal(gone) + " is no longer available (provider key removed?) — using the server default")
	return m
}

// renderModelsOverlay draws the picker centred over the conversation region via
// centerCard. All server-derived strings are terminal-sanitized.
func renderModelsOverlay(th theme.Theme, st modelsState, caps client.Capabilities, width, height int) string {
	if st.view != modelsPanel {
		return ""
	}
	return centerCard(th, renderModelsPanel(th, st, caps), width, height)
}

// modelsDisabledNote is the empty-state copy when model selection is NOT available
// on the connected server (caps.ModelSelection == false / zero providers), with
// the remedy — aligned with the zero-keys actionable copy.
const modelsDisabledNote = "Model selection is not available on this server.\n" +
	"Set OPENAI_API_KEY or OPENROUTER_API_KEY and reconnect."

// modelsEmptyCopy returns the empty-state line: the "not available" note (with
// remedy) when caps.ModelSelection is false, else the "enabled but empty" note.
func modelsEmptyCopy(caps client.Capabilities) string {
	if !caps.ModelSelection {
		return modelsDisabledNote
	}
	return "No selectable models advertised."
}

// renderModelsPanel renders the grouped-by-provider picker: a title, then each
// provider's models under a muted group header, the cursor row highlighted, and a
// ● marker on the ACTIVE (persisted) row. The list arrives (provider_id, id)-
// sorted, so grouping is a single linear pass (emit a header when provider_id
// changes). EVERY server-derived string is terminal-sanitized.
func renderModelsPanel(th theme.Theme, st modelsState, caps client.Capabilities) string {
	var b strings.Builder
	b.WriteString(th.Style("askTitle").Render("Models") + "\n\n")

	switch {
	case st.loading:
		b.WriteString(th.Style("muted").Render("loading…") + "\n")
	case st.err != nil:
		b.WriteString(th.Style("errorText").Render("✗ list models: "+sanitizeTerminal(st.err.Error())) + "\n")
	case len(st.models) == 0:
		b.WriteString(th.Style("muted").Render(modelsEmptyCopy(caps)) + "\n")
	default:
		lastProvider := ""
		for i, mi := range st.models {
			if mi.ProviderID != lastProvider {
				if i > 0 {
					b.WriteString("\n")
				}
				b.WriteString(th.Style("muted").Render(sanitizeTerminal(mi.ProviderID)) + "\n")
				lastProvider = mi.ProviderID
			}
			b.WriteString(renderRow(th, modelRowText(st.active, mi), i == st.cursor) + "\n")
		}
	}

	b.WriteString("\n" + th.Style("muted").Render("↑/↓ select · enter use · esc close · ● = current"))
	return b.String()
}

// modelRowText builds one model row's content: the active marker, the label, and
// the capability + context-window segments. The active marker is a fixed-width
// "● "/"  " prefix so a stripANSI'd row is layout-stable for goldens regardless of
// which row is active.
func modelRowText(active client.ModelSelection, mi client.ModelInfo) string {
	marker := "  "
	if active.Matches(mi) {
		marker = "● "
	}
	segs := modelCapSegments(mi)
	line := marker + sanitizeTerminal(modelLabel(mi))
	if len(segs) > 0 {
		line += "  " + strings.Join(segs, " ")
	}
	return line
}

// modelLabel is the human label for a model row: its display name, falling back to
// its id (the server already falls back, but be defensive against an empty one).
func modelLabel(mi client.ModelInfo) string {
	if mi.DisplayName != "" {
		return mi.DisplayName
	}
	return mi.ID
}

// modelCapSegments builds the fixed-token capability + context-window segments for
// a model row (ASCII-safe, layout-stable for goldens): "img" when Image, "reason"
// when Reasoning, and a humanized context window (reusing the footer's
// humanizeTokens — "200K"/"1.2M") OMITTED when unknown / 0 (never "0").
func modelCapSegments(mi client.ModelInfo) []string {
	var segs []string
	if mi.Image {
		segs = append(segs, "img")
	}
	if mi.Reasoning {
		segs = append(segs, "reason")
	}
	if mi.ContextLimit > 0 {
		segs = append(segs, humanizeTokens(mi.ContextLimit))
	}
	return segs
}
