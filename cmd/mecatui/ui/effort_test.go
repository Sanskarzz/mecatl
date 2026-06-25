package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// Tests for the /effort picker (ADR 0055): a tiny SELECTING enum overlay that
// RESTARTS the session on the SAME provider/model with the chosen reasoning-effort
// tier, mirroring the /models restart-now handoff. Renders purely from client state
// (no proto in ui).

// pressEffortKey routes a key through onEffortKey, asserting it was handled.
func pressEffortKey(t *testing.T, m Model, msg tea.KeyPressMsg) Model {
	t.Helper()
	mm, _, handled := m.onEffortKey(msg)
	if !handled {
		t.Fatalf("key %v should be handled by the open picker", msg)
	}
	return mm.(Model)
}

// TestRunEffortOpensPicker asserts runEffort opens the picker, blurs the input, and
// renders the enum rows (no RPC — the enum is fixed and client-owned).
func TestRunEffortOpensPicker(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	mm, cmd := m.runEffort()
	m = mm.(Model)
	if m.effort.view != effortPanel {
		t.Fatalf("view = %v, want effortPanel", m.effort.view)
	}
	if m.ta.Focused() {
		t.Error("opening the picker should blur the textarea")
	}
	_ = cmd
	out := stripANSIstr(m.View().Content)
	for _, tier := range effortTiers {
		if !strings.Contains(out, tier) {
			t.Errorf("picker missing the %q tier row:\n%s", tier, out)
		}
	}
	if !strings.Contains(out, "Reasoning effort") {
		t.Errorf("picker missing the title:\n%s", out)
	}
}

// TestEffortPickerWarnsOnNoReasoningModel (ADR 0055 UX): when the CURRENT effective
// model is KNOWN (in the loaded inventory) to lack reasoning support, the picker shows
// a degrade warning — so a user who restarts for an unattainable tier is acknowledged
// in the UI, not only the server log. A reasoning-capable model shows NO warning, and
// an UNKNOWN model (not in the inventory) is silent (fail-open).
func TestEffortPickerWarnsOnNoReasoningModel(t *testing.T) {
	const warn = "no reasoning support"

	// (1) Known no-reasoning model → warning.
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	m.models.models = []client.ModelInfo{
		{ID: "no-reason", ProviderID: "openai", DisplayName: "No Reason", Reasoning: false},
	}
	m.effectiveModel = client.ResolvedModel{ProviderID: "openai", ModelID: "no-reason"}
	mm, _ := m.runEffort()
	m = mm.(Model)
	out := stripANSIstr(m.View().Content)
	if !strings.Contains(out, warn) {
		t.Errorf("a known no-reasoning model must warn in the picker:\n%s", out)
	}

	// (2) Reasoning-capable model → no warning.
	m2 := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	m2.models.models = []client.ModelInfo{
		{ID: "gpt-5", ProviderID: "openai", DisplayName: "GPT-5", Reasoning: true},
	}
	m2.effectiveModel = client.ResolvedModel{ProviderID: "openai", ModelID: "gpt-5"}
	mm2, _ := m2.runEffort()
	if got := stripANSIstr(mm2.(Model).View().Content); strings.Contains(got, warn) {
		t.Errorf("a reasoning-capable model must NOT warn:\n%s", got)
	}

	// (3) Unknown model (not in inventory) → silent (fail-open).
	m3 := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	m3.models.models = nil
	m3.effectiveModel = client.ResolvedModel{ProviderID: "openai", ModelID: "mystery"}
	mm3, _ := m3.runEffort()
	if got := stripANSIstr(mm3.(Model).View().Content); strings.Contains(got, warn) {
		t.Errorf("an unknown model must be silent (fail-open):\n%s", got)
	}
}

// TestRunEffortGatedWithoutModelSelection asserts the picker is a no-op when model
// selection is unavailable (the effort only matters with a selectable model).
func TestRunEffortGatedWithoutModelSelection(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, client.Capabilities{ModelSelection: false}, client.ModelSelection{})
	mm, _ := m.runEffort()
	m = mm.(Model)
	if m.effort.view != effortNone {
		t.Fatalf("without ModelSelection the picker must stay closed, view = %v", m.effort.view)
	}
}

// TestEffortCursorStartsOnCurrent asserts the cursor opens on the CURRENT effective
// effort row (so the active tier is pre-selected), not always at the top.
func TestEffortCursorStartsOnCurrent(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	m.effectiveModel.ReasoningEffort = "high"
	mm, _ := m.runEffort()
	m = mm.(Model)
	// effortTiers = [auto low medium high xhigh max] → "high" is index 3.
	if m.effort.cursor != 3 {
		t.Fatalf("cursor = %d, want 3 (the current 'high' row)", m.effort.cursor)
	}
}

// TestEffortNavBounds asserts arrow nav is clamped to [0, len-1].
func TestEffortNavBounds(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	mm, _ := m.runEffort()
	m = mm.(Model)
	// Up at the top stays at 0.
	m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.effort.cursor != 0 {
		t.Fatalf("up at top = %d, want 0", m.effort.cursor)
	}
	// Down past the bottom clamps at len-1.
	for i := 0; i < len(effortTiers)+3; i++ {
		m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.effort.cursor != len(effortTiers)-1 {
		t.Fatalf("down past bottom = %d, want %d", m.effort.cursor, len(effortTiers)-1)
	}
}

// TestEffortEscCloses asserts esc closes the picker.
func TestEffortEscCloses(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	mm, _ := m.runEffort()
	m = mm.(Model)
	mm, _, handled := m.onEffortKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !handled {
		t.Fatal("esc should be handled by the open picker")
	}
	if mm.(Model).effort.view != effortNone {
		t.Error("esc should close the picker")
	}
}

// TestEffortPickRestartsWithPreservedModel is the load-bearing behavioural test: it
// opens the picker, moves to a real tier, presses enter, and asserts the picker fired
// the restart handoff carrying the CURRENT provider/model (PRESERVED) plus the new
// effort — and that the re-create CreateSession recorded exactly that selector.
func TestEffortPickRestartsWithPreservedModel(t *testing.T) {
	store := &fakeStore{}
	m := newModelsModel(t, sampleModels(), store, modelsCaps(), client.ModelSelection{})
	// newModelsModel delivered openai/gpt-5 as the effective model — that is what the
	// effort change must PRESERVE.
	conv := m.deps.Session.(*fakeConv)

	mm, _ := m.runEffort()
	m = mm.(Model)
	// Move to "high" (index 3) and press enter to restart.
	m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	mm, cmd, handled := m.onEffortKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if !handled {
		t.Fatal("enter should be handled by the picker")
	}
	// The restart applied the selection synchronously: the model is PRESERVED, the new
	// effort is set, and it is recorded as the explicit this-session pick.
	want := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5", ReasoningEffort: "high"}
	if m.activeModel != want {
		t.Fatalf("activeModel = %+v, want %+v (model preserved, effort changed)", m.activeModel, want)
	}
	if m.pickedThisSession != want {
		t.Fatalf("pickedThisSession = %+v, want %+v", m.pickedThisSession, want)
	}
	// The picker overlay is dismissed and the session is mid-restart.
	if m.effort.view != effortNone {
		t.Errorf("the restart should dismiss the effort overlay, view = %v", m.effort.view)
	}
	if m.phase != phaseConnecting {
		t.Errorf("the restart should drive phaseConnecting, got %v", m.phase)
	}
	// Run the restart cmd batch: the re-create CreateSession must carry the preserved
	// model + the new effort.
	m = feedCmd(t, m, cmd)
	if conv.createdSel != want {
		t.Fatalf("re-create CreateSession carried %+v, want %+v", conv.createdSel, want)
	}
	// The pick is persisted per-workspace (the effort rides the selection).
	if store.saves < 1 || store.lastSel != want {
		t.Fatalf("store: saves=%d lastSel=%+v, want >=1 / %+v", store.saves, store.lastSel, want)
	}
}

// TestEffortPickAutoSendsEmpty asserts the auto sentinel maps to the EMPTY effort on
// the wire (and in the persisted selection) — the clean-state convention.
func TestEffortPickAutoSendsEmpty(t *testing.T) {
	store := &fakeStore{}
	m := newModelsModel(t, sampleModels(), store, modelsCaps(), client.ModelSelection{})
	m.effectiveModel.ReasoningEffort = "high" // start from a non-auto state
	conv := m.deps.Session.(*fakeConv)

	mm, _ := m.runEffort()
	m = mm.(Model)
	// The cursor opens on "high" (index 3); move up to "auto" (index 0).
	for i := 0; i < 5; i++ {
		m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if m.effort.cursor != 0 {
		t.Fatalf("cursor = %d, want 0 (auto)", m.effort.cursor)
	}
	mm, cmd, _ := m.onEffortKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	want := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5", ReasoningEffort: ""}
	if m.activeModel != want {
		t.Fatalf("activeModel = %+v, want %+v (auto ⇒ empty effort)", m.activeModel, want)
	}
	m = feedCmd(t, m, cmd)
	if conv.createdSel.ReasoningEffort != "" {
		t.Fatalf("auto pick carried effort %q, want empty", conv.createdSel.ReasoningEffort)
	}
}

// TestEffortValueLabelRoundTrip locks the auto↔"" sentinel mapping in both
// directions, and that real tiers are pass-through.
func TestEffortValueLabelRoundTrip(t *testing.T) {
	if effortValue("auto") != "" {
		t.Errorf("effortValue(auto) = %q, want empty", effortValue("auto"))
	}
	if effortValue("high") != "high" {
		t.Errorf("effortValue(high) = %q, want high", effortValue("high"))
	}
	if effortLabel("") != "auto" {
		t.Errorf("effortLabel(\"\") = %q, want auto", effortLabel(""))
	}
	if effortLabel("medium") != "medium" {
		t.Errorf("effortLabel(medium) = %q, want medium", effortLabel("medium"))
	}
}

// TestEffortHeaderSuffix asserts the header effort suffix renders the resolved effort
// and is ABSENT for the unset/auto states.
func TestEffortHeaderSuffix(t *testing.T) {
	if got := effortHeaderSuffix("high"); got != "high" {
		t.Errorf("effortHeaderSuffix(high) = %q, want high", got)
	}
	if got := effortHeaderSuffix(""); got != "" {
		t.Errorf("effortHeaderSuffix(\"\") = %q, want empty", got)
	}
	if got := effortHeaderSuffix("auto"); got != "" {
		t.Errorf("effortHeaderSuffix(auto) = %q, want empty (defensive)", got)
	}
}

// TestEffortPickerGolden locks the populated enum picker: the ● marks the current
// (effective) tier and the cursor highlights a different row, so both affordances
// render together.
func TestEffortPickerGolden(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	m.effectiveModel.ReasoningEffort = "medium" // the ● row
	mm, _ := m.runEffort()
	m = mm.(Model)
	// Move the cursor OFF the current row so the ● and the highlight are distinct.
	m = pressEffortKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.effort.view != effortPanel {
		t.Fatalf("view = %v, want effortPanel", m.effort.view)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "effort_picker.golden", got)
}

// TestEffortRendersInHeader asserts the resolved effort appears beside the model in
// the header when set, and is absent when unset (the footer/header live display).
func TestEffortRendersInHeader(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	// Unset effort: no suffix beside the model. (The inventory is not loaded here, so
	// the header shows the raw effective model id "gpt-5", not the display name.)
	header := stripANSIstr(m.renderHeader())
	if !strings.Contains(header, "gpt-5") {
		t.Fatalf("header should show the model:\n%s", header)
	}
	if strings.Contains(header, "· high") {
		t.Fatalf("unset effort must not render a suffix:\n%s", header)
	}
	// Set effort: the suffix renders beside the model.
	m.effectiveModel.ReasoningEffort = "high"
	m.refreshView()
	header = stripANSIstr(m.renderHeader())
	if !strings.Contains(header, "· high") {
		t.Fatalf("a set effort should render a ` · high` suffix beside the model:\n%s", header)
	}
}
