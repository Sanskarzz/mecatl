package ui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
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
	modelsPanel                   // the flat, type-to-filter picker
)

// modelsChrome is the number of non-row lines renderModelsPanel writes around the
// windowed row block (so the budget is one obvious expression, not magic numbers
// sprinkled in the loop). It accounts for the in-card lines — title (1), the filter
// input row (1), the blank after it (1), the footer's leading blank (1) + hint (1)
// = 5 — plus the askCard border/padding the centred card adds (2). Mirrors
// team.go's roster chrome accounting; any small over-count just shrinks the window
// by a row, never overflows.
const modelsChrome = 7

// modelsMinRows is the floor on visible rows so even a very short terminal still
// shows a usable window (mirrors team.go's teamMinRosterRows). The window still
// follows the cursor within those rows.
const modelsMinRows = 3

// modelsState holds the /models overlay state on the Model. Value-embedded so the
// Model stays a plain struct Update copies; the slices are replaced wholesale on
// each RPC result / filter recompute (never mutated in place).
type modelsState struct {
	view     modelsView
	loading  bool                  // the ListModels RPC is in flight
	err      error                 // last ListModels error, rendered distinctly
	models   []client.ModelInfo    // full list as relayed (provider_id,id-sorted)
	filtered []client.ModelInfo    // subset matching filter.Value(); recomputed on each key (mirror palette.filtered)
	filter   textinput.Model       // the type-to-filter input; focused while the picker is open
	cursor   int                   // index into FILTERED (clamped to its bounds)
	active   client.ModelSelection // the persisted/active selection (drives the ● marker)
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
	// Open with the filter FOCUSED so the user can type to narrow immediately (the
	// headline use case at 300+ models). The filtered slice is (re)derived when the
	// list lands in updateModelsMsg; an empty query ⇒ filtered == models.
	ti := textinput.New()
	ti.Placeholder = "filter models…"
	// A fixed width so the placeholder + a typed query render in full (the bubbles
	// default width is 0, which truncates the view to a single glyph). Kept narrow
	// enough to sit inside the centred card at the test/default widths.
	ti.SetWidth(40)
	ti.Focus()
	m.models.filter = ti
	m.models.filtered = nil
	return m, tea.Batch(client.ListModelsCmd(m.deps.Ctx, m.deps.Models), textinput.Blink)
}

// closeModels dismisses the overlay and returns focus to the prompt input. The
// active selection + the loaded list survive (the picker is reopened cheaply); the
// filter is reset so a reopen starts clean (openModels re-News it regardless).
func (m Model) closeModels() (tea.Model, tea.Cmd) {
	m.models.view = modelsNone
	m.models.filter = textinput.Model{}
	m.models.filtered = nil
	cmd := m.ta.Focus()
	return m, cmd
}

// onModelsKey routes key presses while the picker is open. The filter input is
// FOCUSED, so the routing mirrors mcp.go's onPromptArgsKey: nav/action keys are
// intercepted first, everything else feeds the input.
//
// The single sharp edge (mirroring mcp.go:283-288): keys.Up/keys.Down also bind
// "k"/"j" (keys.go), so matching them with key.Matches would hijack a typed model
// name like "kimi"/"jamba". So list nav matches the ARROW keys by msg.String()
// ONLY; the other nav keys (pgup/pgdown/home/end) and the action keys
// (enter/esc) are all non-printable, so key.Matches against ScrollU/ScrollD/
// ScrollTop/ScrollBottom/Choose/Close is safe — none collide with typed text.
//
// esc is TWO-STAGE: a non-empty filter is cleared first (the picker stays open so
// a mistyped query can be undone without losing the open picker); an empty filter
// closes the picker. enter selects filtered[cursor]. Every key is handled=true
// (the open picker swallows keys), unchanged. After any input-feeding key the
// filter is re-synced (recompute + cursor clamp) and the input's cmd returned for
// the cursor blink.
func (m Model) onModelsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if m.models.view == modelsNone {
		return m, nil, false
	}
	budget := m.modelsRowBudget()
	switch {
	case key.Matches(msg, m.keys.Close):
		if m.models.filter.Value() != "" {
			m.models.filter.SetValue("")
			m = m.syncModelsFilter()
			return m, nil, true
		}
		mm, cmd := m.closeModels()
		return mm, cmd, true
	case msg.String() == "up":
		if m.models.cursor > 0 {
			m.models.cursor--
		}
		return m, nil, true
	case msg.String() == "down":
		if m.models.cursor < len(m.models.filtered)-1 {
			m.models.cursor++
		}
		return m, nil, true
	case key.Matches(msg, m.keys.ScrollU):
		m.models.cursor = clampModelsCursor(m.models.cursor-budget, len(m.models.filtered))
		return m, nil, true
	case key.Matches(msg, m.keys.ScrollD):
		m.models.cursor = clampModelsCursor(m.models.cursor+budget, len(m.models.filtered))
		return m, nil, true
	case key.Matches(msg, m.keys.ScrollTop):
		m.models.cursor = 0
		return m, nil, true
	case key.Matches(msg, m.keys.ScrollBottom):
		m.models.cursor = clampModelsCursor(len(m.models.filtered)-1, len(m.models.filtered))
		return m, nil, true
	case key.Matches(msg, m.keys.Choose):
		mm, cmd := m.chooseModel()
		return mm, cmd, true
	}
	// Everything else feeds the focused filter input (printable runes, backspace,
	// ←/→, …); recompute the filtered slice + clamp the cursor afterwards.
	var cmd tea.Cmd
	m.models.filter, cmd = m.models.filter.Update(msg)
	m = m.syncModelsFilter()
	return m, cmd, true
}

// clampModelsCursor clamps c to [0, n-1], or 0 when the list is empty.
func clampModelsCursor(c, n int) int {
	if n <= 0 {
		return 0
	}
	if c < 0 {
		return 0
	}
	if c >= n {
		return n - 1
	}
	return c
}

// filterModels returns the models whose provider_id, id, or display_name CONTAIN q
// (case-insensitive), preserving input order (already provider_id,id-sorted). An
// empty q returns the full list. Mirrors palette.filterCommands.
func filterModels(models []client.ModelInfo, q string) []client.ModelInfo {
	if q == "" {
		return models
	}
	lq := strings.ToLower(q)
	out := make([]client.ModelInfo, 0, len(models))
	for _, mi := range models {
		if strings.Contains(strings.ToLower(mi.ProviderID), lq) ||
			strings.Contains(strings.ToLower(mi.ID), lq) ||
			strings.Contains(strings.ToLower(mi.DisplayName), lq) {
			out = append(out, mi)
		}
	}
	return out
}

// syncModelsFilter recomputes the filtered slice from the current filter value and
// clamps the cursor into its bounds. Mirrors palette.syncPalette's recompute +
// clamp. Called on every input-feeding key and once when the list lands.
func (m Model) syncModelsFilter() Model {
	m.models.filtered = filterModels(m.models.models, m.models.filter.Value())
	if m.models.cursor >= len(m.models.filtered) {
		m.models.cursor = 0
	}
	return m
}

// chooseModel selects the cursor model: it sets the active selection, fires the
// persist Cmd (off the update goroutine, like every store write), and shows a
// success notice. It does NOT recreate the session (apply-on-next-create) — the
// notice says as much. A cursor past the list end is a no-op (defensive).
func (m Model) chooseModel() (tea.Model, tea.Cmd) {
	if m.models.cursor < 0 || m.models.cursor >= len(m.models.filtered) {
		return m, nil
	}
	chosen := m.models.filtered[m.models.cursor]
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
		// Derive the filtered slice (+ clamp the cursor) from the current filter
		// value; on a fresh open the filter is empty, so filtered == models.
		m = m.syncModelsFilter()
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
	return centerCard(th, renderModelsPanel(th, st, caps, modelsRowBudgetFor(height)), width, height)
}

// modelsRowBudgetFor converts an available card height into the number of model
// rows the windowed list may show, floored at modelsMinRows so a short terminal
// still shows a usable window. The chrome (title/filter/footer/card border) is
// subtracted via the single modelsChrome const. A height of 0 (unsized) yields the
// floor.
func modelsRowBudgetFor(height int) int {
	b := height - modelsChrome
	if b < modelsMinRows {
		return modelsMinRows
	}
	return b
}

// modelsRowBudget is the row budget derived from the live terminal height (the
// conversation viewport height, the same value view.go passes into the overlay).
// Used by the pgup/pgdown paging keys so a page equals one window.
func (m Model) modelsRowBudget() int {
	return modelsRowBudgetFor(m.vp.Height())
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

// renderModelsPanel renders the flat type-to-filter picker: a title, the filter
// input row, then a WINDOWED slice of the filtered rows (the window follows the
// cursor via the shared scrollWindow helper so the selected row stays visible past
// the top/bottom edge), the cursor row highlighted, and a ● marker on the ACTIVE
// (persisted) row. Group headers are dropped; provider_id is folded into each row
// (so it is still visible AND a filter target). EVERY server-derived string is
// terminal-sanitized. rowBudget clips the list to the card height (the overflow
// fix — centerCard centres but does not clip).
func renderModelsPanel(th theme.Theme, st modelsState, caps client.Capabilities, rowBudget int) string {
	var b strings.Builder

	// The title carries a scroll-position indicator in the default (row-list) case so
	// the user knows the list is windowed (at 300+ models a 3-row window otherwise
	// hides that there are more rows off-screen). Computed once here so the window is
	// rendered with the SAME [start,end) the title reports.
	title := "Models"
	if !st.loading && st.err == nil && len(st.filtered) > 0 {
		start, end := scrollWindow(st.cursor, len(st.filtered), rowBudget)
		title += "  " + modelsPositionLabel(start, end, len(st.filtered))
	}
	b.WriteString(th.Style("askTitle").Render(title) + "\n")
	b.WriteString(st.filter.View() + "\n\n")

	switch {
	case st.loading:
		b.WriteString(th.Style("muted").Render("loading…") + "\n")
	case st.err != nil:
		b.WriteString(th.Style("errorText").Render("✗ list models: "+sanitizeTerminal(st.err.Error())) + "\n")
	case len(st.models) == 0:
		// Server-side empty/disabled (no inventory at all) — distinct from a filter
		// that matched nothing.
		b.WriteString(th.Style("muted").Render(modelsEmptyCopy(caps)) + "\n")
	case len(st.filtered) == 0:
		// The filter matched nothing (the inventory is non-empty). A clear, distinct
		// note with a recovery hint; the cursor is safe (clamped to 0) and enter is a
		// no-op.
		b.WriteString(th.Style("muted").Render("no models match "+strconv.Quote(st.filter.Value())+" — esc to clear") + "\n")
	default:
		start, end := scrollWindow(st.cursor, len(st.filtered), rowBudget)
		for i := start; i < end; i++ {
			mi := st.filtered[i]
			b.WriteString(renderRow(th, modelRowText(st.active, mi), i == st.cursor) + "\n")
		}
	}

	b.WriteString("\n" + th.Style("muted").Render(
		"type to filter · ↑/↓/pgup move · enter use · esc clear filter / close · ● = current"))
	return b.String()
}

// modelsPositionLabel formats the scroll-position indicator for the title from a
// window's [start,end) bounds (0-based, end-exclusive) over total rows. It renders
// 1-based, end-INCLUSIVE: a partial window reads "(27–29 of 344)"; a window that
// holds the whole list reads "(4)" (no range when nothing is off-screen). total is
// the FILTERED count, so it tracks the filter. It is ASCII-safe + derived from
// internal ints (no server string), so it needs no sanitization.
func modelsPositionLabel(start, end, total int) string {
	if end-start >= total {
		return "(" + strconv.Itoa(total) + ")"
	}
	return "(" + strconv.Itoa(start+1) + "–" + strconv.Itoa(end) + " of " + strconv.Itoa(total) + ")"
}

// modelRowText builds one model row's content: the active marker, the
// provider_id segment, the label, and the capability + context-window segments.
// The active marker is a fixed-width "● "/"  " prefix so a stripANSI'd row is
// layout-stable for goldens regardless of which row is active.
func modelRowText(active client.ModelSelection, mi client.ModelInfo) string {
	marker := "  "
	if active.Matches(mi) {
		marker = "● "
	}
	segs := modelCapSegments(mi)
	line := marker + sanitizeTerminal(mi.ProviderID) + " · " + sanitizeTerminal(modelLabel(mi))
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
