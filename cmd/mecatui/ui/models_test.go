package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/theme"
)

// Tests for the /models picker: the first SELECTING overlay (cursor + enter-to-
// select + persist), mirroring the mcp.go resource picker. It is idle-only and
// renders purely from client.ModelInfo (no proto in ui).

// newModelsModel builds an idle, sized Model wired to the given fakeModels +
// store + caps, with an optional pre-seeded active selection (the persisted
// last-used). It uses applyAll with SessionReadyMsg (skipping Init's connect
// sequencing) so the picker can be opened directly while idle.
func newModelsModel(t *testing.T, fm *fakeModels, store SelectionStore, caps client.Capabilities, initial client.ModelSelection) Model {
	t.Helper()
	recv := &fakeRecver{gate: make(chan struct{})}
	send := &fakeSender{}
	conv := &fakeConv{recv: recv, send: send, caps: caps}
	m := New(Deps{
		Session:        conv,
		Conv:           conv,
		Models:         fm,
		SelectionStore: store,
		InitialModel:   initial,
		Theme:          theme.New("aztec", theme.AztecPalette()),
		Server:         "127.0.0.1:8080",
		Workspace:      "/workspace",
		Mode:           "default",
		Model:          "mock-model",
		Ctx:            context.Background(),
		NoAltScreen:    true,
	})
	m = applyAll(m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		client.SessionReadyMsg{SessionID: "sess-test-0001", Capabilities: caps},
	)
	return m
}

// sampleModels is a 2-provider list exercising the glyph matrix: both caps, reason-
// only, neither, and a context-limit-absent row.
func sampleModels() *fakeModels {
	return &fakeModels{models: []client.ModelInfo{
		{ID: "gpt-5", ProviderID: "openai", DisplayName: "GPT-5", Image: true, Reasoning: true, ContextLimit: 200000},
		{ID: "gpt-5-mini", ProviderID: "openai", DisplayName: "GPT-5 mini", Reasoning: true, ContextLimit: 128000},
		{ID: "text-embed", ProviderID: "openai", DisplayName: "text-embed"}, // neither cap, no ctx
		{ID: "anthropic/claude", ProviderID: "openrouter", DisplayName: "Claude", Image: true, Reasoning: true, ContextLimit: 1000000},
	}}
}

func modelsCaps() client.Capabilities { return client.Capabilities{ModelSelection: true} }

// TestRunModelsOpensPicker asserts runModels opens the picker, blurs the input,
// fires ListModels, and renders the rows once the result lands.
func TestRunModelsOpensPicker(t *testing.T) {
	fm := sampleModels()
	m := newModelsModel(t, fm, &fakeStore{}, modelsCaps(), client.ModelSelection{})

	mm, cmd := m.runModels()
	m = mm.(Model)
	if m.models.view != modelsPanel {
		t.Fatalf("view = %v, want modelsPanel", m.models.view)
	}
	if !m.models.loading {
		t.Error("picker should be loading until ListModels lands")
	}
	if m.ta.Focused() {
		t.Error("opening the picker should blur the textarea")
	}
	if cmd == nil {
		t.Fatal("runModels should fire the ListModels RPC command")
	}
	m = feedCmd(t, m, cmd)
	if fm.calls != 1 {
		t.Errorf("ListModels calls = %d, want 1", fm.calls)
	}
	if !strings.Contains(m.View().Content, "GPT-5") {
		t.Errorf("picker missing a model label:\n%s", m.View().Content)
	}
}

// TestRunModelsNilGuard asserts the picker won't open without a model lister wired.
func TestRunModelsNilGuard(t *testing.T) {
	recv := &fakeRecver{gate: make(chan struct{})}
	conv := &fakeConv{recv: recv, send: &fakeSender{}, caps: modelsCaps()}
	m := New(Deps{Session: conv, Conv: conv, Theme: theme.New("aztec", theme.AztecPalette()), Ctx: context.Background()})
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30}, client.SessionReadyMsg{Capabilities: modelsCaps()})
	mm, cmd := m.runModels()
	if mm.(Model).models.view != modelsNone || cmd != nil {
		t.Error("runModels with no lister wired must be a no-op")
	}
}

// TestModelsCursorNav asserts up/down move across the flattened list (skipping no
// rows — every model is a row) and clamp at the ends.
func TestModelsCursorNav(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), client.ModelSelection{})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	if m.models.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.models.cursor)
	}
	// Up at the top clamps.
	m = pressModelsKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.models.cursor != 0 {
		t.Errorf("cursor after up at top = %d, want 0 (clamped)", m.models.cursor)
	}
	// Down moves through all 4 rows then clamps at the last.
	for i := 0; i < 6; i++ {
		m = pressModelsKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.models.cursor != 3 {
		t.Errorf("cursor after many downs = %d, want 3 (clamped at last)", m.models.cursor)
	}
}

// TestModelsChooseSetsActiveAndPersists asserts enter on the cursor row sets the
// active selection, fires the Save, and emits a status notice.
func TestModelsChooseSetsActiveAndPersists(t *testing.T) {
	store := &fakeStore{}
	m := newModelsModel(t, sampleModels(), store, modelsCaps(), client.ModelSelection{})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)

	// Move to the 4th row (openrouter/claude) and select it.
	for i := 0; i < 3; i++ {
		m = pressModelsKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	mm, cmd = m.onModelsKeyTuple(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	want := client.ModelSelection{ProviderID: "openrouter", ModelID: "anthropic/claude"}
	if m.activeModel != want {
		t.Fatalf("activeModel = %+v, want %+v", m.activeModel, want)
	}
	if m.models.active != want {
		t.Fatalf("models.active = %+v, want %+v", m.models.active, want)
	}
	if !strings.Contains(stripANSIstr(m.statusMsg), "model set") {
		t.Errorf("status should read 'model set', got %q", stripANSIstr(m.statusMsg))
	}
	// Run the Save cmd and assert the store recorded the pick for the workspace.
	m = feedCmd(t, m, cmd)
	if store.saves != 1 || store.lastSel != want || store.lastWS != "/workspace" {
		t.Fatalf("store: saves=%d lastSel=%+v lastWS=%q, want 1 / %+v / /workspace",
			store.saves, store.lastSel, store.lastWS, want)
	}
}

// TestModelsEscClosesNoChange asserts esc closes the picker without changing the
// active selection.
func TestModelsEscClosesNoChange(t *testing.T) {
	seed := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(), seed)
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	// Move the cursor, then esc — active must be unchanged.
	m = pressModelsKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	mm, _, handled := m.onModelsKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !handled {
		t.Fatal("esc should be handled by the open picker")
	}
	m = mm.(Model)
	if m.models.view != modelsNone {
		t.Error("esc should close the picker")
	}
	if m.activeModel != seed {
		t.Errorf("esc must not change activeModel: got %+v, want %+v", m.activeModel, seed)
	}
}

// TestModelsErrorRenders asserts a ListModels error is recorded + surfaced.
func TestModelsErrorRenders(t *testing.T) {
	fm := &fakeModels{err: errors.New("boom")}
	m := newModelsModel(t, fm, &fakeStore{}, modelsCaps(), client.ModelSelection{})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	if m.models.err == nil {
		t.Fatal("a ListModels error should be recorded")
	}
	if !strings.Contains(m.View().Content, "boom") {
		t.Errorf("error not surfaced in the picker:\n%s", m.View().Content)
	}
}

// TestModelsKeyRemovedFallback is the §4 reconcile: when the loaded list does NOT
// contain the persisted active selection, active is cleared to the server default
// and a loud notice fires — but the store is NOT rewritten.
func TestModelsKeyRemovedFallback(t *testing.T) {
	// Persisted selection names openrouter, but the list only has openai now (key
	// removed). The reconcile must clear active to zero with a notice.
	fm := &fakeModels{models: []client.ModelInfo{
		{ID: "gpt-5", ProviderID: "openai", DisplayName: "GPT-5", ContextLimit: 200000},
	}}
	store := &fakeStore{}
	gone := client.ModelSelection{ProviderID: "openrouter", ModelID: "anthropic/claude"}
	m := newModelsModel(t, fm, store, modelsCaps(), gone)
	if m.activeModel != gone {
		t.Fatalf("precondition: activeModel seeded = %+v, want %+v", m.activeModel, gone)
	}

	// Drive ListModels through updateModelsMsg directly (idle phase ⇒ no create).
	mm, cmd, handled := m.updateModelsMsg(client.ModelsMsg{Models: fm.models})
	m = mm.(Model)
	if !handled {
		t.Fatal("ModelsMsg should be handled")
	}
	if cmd != nil {
		t.Error("at idle, ModelsMsg must NOT fire a create command")
	}
	if !m.activeModel.IsZero() {
		t.Errorf("active should be cleared to the server default, got %+v", m.activeModel)
	}
	if !strings.Contains(stripANSIstr(m.statusMsg), "no longer available") {
		t.Errorf("a loud key-removed notice should fire, got %q", stripANSIstr(m.statusMsg))
	}
	if store.saves != 0 {
		t.Errorf("the state file must NOT be rewritten on fallback, got %d saves", store.saves)
	}
}

// TestModelsReconcileKeepsAvailable asserts an active selection still present in
// the list is preserved (no clear, no notice).
func TestModelsReconcileKeepsAvailable(t *testing.T) {
	fm := sampleModels()
	keep := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	m := newModelsModel(t, fm, &fakeStore{}, modelsCaps(), keep)
	mm, _, _ := m.updateModelsMsg(client.ModelsMsg{Models: fm.models})
	m = mm.(Model)
	if m.activeModel != keep {
		t.Errorf("an available selection must be preserved, got %+v want %+v", m.activeModel, keep)
	}
}

// TestModelsSaveFailureFailSoft asserts a Save error surfaces as a notice but the
// active selection still holds for the run.
func TestModelsSaveFailureFailSoft(t *testing.T) {
	store := &fakeStore{err: errors.New("disk full")}
	m := newModelsModel(t, sampleModels(), store, modelsCaps(), client.ModelSelection{})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	mm, cmd = m.onModelsKeyTuple(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	want := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	if m.activeModel != want {
		t.Fatalf("active should hold for the run despite the save failure, got %+v", m.activeModel)
	}
	m = feedCmd(t, m, cmd) // runs the failing Save + the selectionSavedMsg reduction
	if !strings.Contains(stripANSIstr(m.statusMsg), "could not persist") {
		t.Errorf("a persist failure should surface as a notice, got %q", stripANSIstr(m.statusMsg))
	}
}

// TestModelsConnectErrorDegradesToCreate guards the connecting-phase ListModels
// error branch (models.go ~:157): a ListModels error at connect must NOT strand the
// UI at "connecting…" — it proceeds to CreateSession with the (unreconciled) seeded
// selection so connect completes. (TestModelsErrorRenders covers the idle path; this
// covers the connect path.)
func TestModelsConnectErrorDegradesToCreate(t *testing.T) {
	seed := client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"}
	recv := &fakeRecver{gate: make(chan struct{})}
	conv := &fakeConv{recv: recv, send: &fakeSender{}, caps: modelsCaps()}
	m := New(Deps{
		Session:      conv,
		Conv:         conv,
		Models:       &fakeModels{err: errors.New("list boom")},
		Theme:        theme.New("aztec", theme.AztecPalette()),
		Ctx:          context.Background(),
		InitialModel: seed,
	})
	if m.phase != phaseConnecting {
		t.Fatalf("precondition: phase = %v, want phaseConnecting", m.phase)
	}

	// Drive the connect-time ListModels error through the reducer.
	mm, cmd, handled := m.updateModelsMsg(client.ModelsMsg{Err: errors.New("list boom")})
	m = mm.(Model)
	if !handled {
		t.Fatal("connect-time ModelsMsg error should be handled")
	}
	if cmd == nil {
		t.Fatal("a connect-time ListModels error must still fire CreateSession (not strand at connecting)")
	}
	// Running that create cmd must reach SessionReadyMsg carrying the SEEDED
	// (unreconciled) selection — connect completes.
	m = feedCmd(t, applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30}), cmd)
	if m.phase != phaseIdle {
		t.Errorf("phase after the degraded create = %v, want phaseIdle (connected)", m.phase)
	}
	if conv.createdSel != seed {
		t.Errorf("CreateSession carried %+v, want the seeded (unreconciled) %+v", conv.createdSel, seed)
	}
}

// TestInitNoListerFiresCreateDirectly guards the no-lister Init branch
// (model.go ~:383): with deps.Models == nil (old server / persistence off) Init
// must fire CreateSession directly with an empty selection — NOT wait on a
// ListModels that never comes.
func TestInitNoListerFiresCreateDirectly(t *testing.T) {
	recv := &fakeRecver{gate: make(chan struct{})}
	conv := &fakeConv{recv: recv, send: &fakeSender{}, caps: client.Capabilities{}}
	m := New(Deps{
		Session: conv,
		Conv:    conv,
		// Models intentionally nil.
		Theme: theme.New("aztec", theme.AztecPalette()),
		Ctx:   context.Background(),
	})

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init should return a command")
	}
	// Init returns tea.Batch(sp.Tick, <connect>). Run the leaves and collect their
	// msgs WITHOUT feeding them recursively — the spinner tick re-arms itself, so
	// feedCmd would recurse forever (the bug this test guards must not also trip it).
	// The no-lister branch's connect leaf is createSessionCmd ⇒ a SessionReadyMsg;
	// there must be NO ListModelsMsg (it did not take the lister path).
	msgs := initLeafMsgs(cmd)
	var ready *client.SessionReadyMsg
	for _, msg := range msgs {
		switch v := msg.(type) {
		case client.SessionReadyMsg:
			ready = &v
		case client.ModelsMsg:
			t.Fatal("no-lister Init must NOT fire ListModels (it has no lister wired)")
		}
	}
	if ready == nil {
		t.Fatal("no-lister Init must fire CreateSession directly (a SessionReadyMsg), not stall awaiting ListModels")
	}
	m = applyAll(m, tea.WindowSizeMsg{Width: 100, Height: 30}, *ready)
	if m.phase != phaseIdle {
		t.Errorf("phase after no-lister Init = %v, want phaseIdle (connected directly)", m.phase)
	}
	if !conv.createdSel.IsZero() {
		t.Errorf("no-lister create carried %+v, want the empty selection", conv.createdSel)
	}
}

// --- goldens ---------------------------------------------------------------

// TestModelsPickerGolden locks the populated, grouped picker with an active marker
// on the persisted row and the glyph matrix.
func TestModelsPickerGolden(t *testing.T) {
	m := newModelsModel(t, sampleModels(), &fakeStore{}, modelsCaps(),
		client.ModelSelection{ProviderID: "openai", ModelID: "gpt-5"})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	if m.models.view != modelsPanel {
		t.Fatalf("view = %v, want modelsPanel", m.models.view)
	}
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "models.golden", got)
}

// TestModelsPickerDisabledGolden locks the "model selection not available" empty
// state (caps.ModelSelection false, empty list).
func TestModelsPickerDisabledGolden(t *testing.T) {
	m := newModelsModel(t, &fakeModels{}, &fakeStore{}, client.Capabilities{ModelSelection: false}, client.ModelSelection{})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "models_disabled.golden", got)
}

// TestModelsPickerEmptyGolden locks the "enabled but empty" state.
func TestModelsPickerEmptyGolden(t *testing.T) {
	m := newModelsModel(t, &fakeModels{}, &fakeStore{}, modelsCaps(), client.ModelSelection{})
	mm, cmd := m.runModels()
	m = feedCmd(t, mm.(Model), cmd)
	got := stripANSI([]byte(m.View().Content))
	compareGolden(t, "models_empty.golden", got)
}

// pressModelsKey routes a key through onModelsKey, asserting it was handled.
func pressModelsKey(t *testing.T, m Model, msg tea.KeyPressMsg) Model {
	t.Helper()
	mm, _, handled := m.onModelsKey(msg)
	if !handled {
		t.Fatalf("key %v should be handled by the open picker", msg)
	}
	return mm.(Model)
}

// onModelsKeyTuple is a thin helper that drops the handled bool for the cases that
// only need the model + cmd.
func (m Model) onModelsKeyTuple(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	mm, cmd, _ := m.onModelsKey(msg)
	return mm, cmd
}

// initLeafMsgs runs cmd's batch leaves ONCE and collects their result msgs WITHOUT
// re-feeding them (unlike feedCmd). This is the safe way to inspect what Init()
// dispatched: Init batches the spinner tick, whose handler re-arms itself, so a
// recursive feed would loop forever — initLeafMsgs runs each leaf exactly once and
// returns the raw msgs for the test to assert on. Nested batches are flattened.
func initLeafMsgs(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	var walk func(c tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, leaf := range batch {
				walk(leaf)
			}
			return
		}
		out = append(out, msg)
	}
	walk(cmd)
	return out
}
