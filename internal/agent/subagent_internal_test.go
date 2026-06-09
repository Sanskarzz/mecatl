package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// TestTightenLimit is the unit table for the tighten-only clamp, including the
// inherited==0 (unlimited) branch: a positive override against an unlimited (0) bound
// TIGHTENS to the override; a nil/zero override is a no-op; a higher override never
// loosens. It relies on session.Limits treating 0 as unlimited (see tightenLimit's
// doc) — this table is the regression guard if that zero-semantics ever changes.
func TestTightenLimit(t *testing.T) {
	ptr := func(n int) *int { return &n }
	tests := []struct {
		name      string
		inherited int
		override  *int
		want      int
	}{
		{"unlimited inherited tightens to override", 0, ptr(5), 5},
		{"override lower wins", 5, ptr(3), 3},
		{"override higher does not loosen", 5, ptr(10), 5},
		{"nil override is a no-op", 5, nil, 5},
		{"zero override is a no-op", 5, ptr(0), 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tightenLimit(tc.inherited, tc.override); got != tc.want {
				t.Fatalf("tightenLimit(%d, %v) = %d, want %d", tc.inherited, tc.override, got, tc.want)
			}
		})
	}
}

// TestDriveChildStructuredPlainTextExhaustsToCleanTerminal is the ADVERSARIAL exhaustion
// case the e2e tests do NOT cover (QA MUST #1 + #2): an output_schema IS set but the
// child returns PLAIN TEXT on every attempt and NEVER calls SubmitResult, so
// submit.valid() stays false across the bounded correction re-drives. It drives the child
// DIRECTLY (this is package agent) so it can assert the STRONG terminal guarantee (QA #2):
// driveChild returns StopStructuredOutput, AND the underlying child session ends COMPLETED
// (a CLEAN terminal — each plain-text drive ends StopEndTurn → completed) and is
// Reopen-recoverable. A regression to StopError / a failed session fails this test.
func TestDriveChildStructuredPlainTextExhaustsToCleanTerminal(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	// The child emits ONLY plain text — never a SubmitResult call — across every attempt
	// (script more turns than the 1+defaultStructuredOutputRetries attempts can consume).
	var turns []mockllm.Turn
	for i := 0; i < 1+defaultStructuredOutputRetries+2; i++ {
		turns = append(turns, mockllm.TextTurn("here is a plain text answer, ignoring SubmitResult"))
	}
	engine := NewEngine(Deps{
		LLM:     mockllm.New(turns...),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "child-model",
	})

	ws := memfs.NewWorkspace("/ws")
	childID := session.SessionID("subagent-c1")
	child := session.New(childID, session.ModeDefault, ws.Root(), session.Limits{}, time.Now())
	submit := newSubmitResultTool(schema)
	call := session.NewToolCall("c1", subagentToolName, nil)

	_, stop, _, _ := driveChild(context.Background(), engine, child, ws,
		"profile someone", RunOptions{ExtraTools: []tool.Tool{submit}},
		nil, call, childID, childPosture{}, submit, schema)

	if stop != session.StopStructuredOutput {
		t.Fatalf("driveChild stop = %q, want %q (plain-text-only exhaustion)", stop, session.StopStructuredOutput)
	}
	if submit.valid() {
		t.Fatal("submit.valid() must stay false (the child never called SubmitResult)")
	}
	// The child must have been re-driven the full bounded number of times (a plain-text
	// turn ends each attempt, then a Reopen+correction re-drives — never an infinite loop).
	if got := engine.deps.LLM.(*mockllm.Provider).Calls(); got != 1+defaultStructuredOutputRetries {
		t.Fatalf("child made %d model calls, want %d (initial + bounded corrections)", got, 1+defaultStructuredOutputRetries)
	}
	// STRONG terminal guarantee: the child session is a CLEAN COMPLETED terminal (never
	// failed), so it is Reopen-recoverable — exactly like StopBudget. A regression to
	// StopError/failed would make the session non-recoverable and fail here.
	if child.State != session.StateCompleted {
		t.Fatalf("child session state = %q, want completed (clean terminal, not failed)", child.State)
	}
	if err := child.Reopen(); err != nil {
		t.Fatalf("Reopen after structured-output exhaustion: %v (the terminal must stay recoverable)", err)
	}
}

// TestSubmitResultOverlayWinsAndIsAdvertised is the focused RunOptions overlay test (QA
// SHOULD #6): a run-scoped ExtraTool whose name COLLIDES with a catalog tool of the same
// name must (a) win via lookupTool (overlay-first resolution) and (b) be advertised by
// buildRequest with the OVERLAY's spec, so the advertised set and dispatch resolution
// never disagree. It exercises both seams directly (package agent).
func TestSubmitResultOverlayWinsAndIsAdvertised(t *testing.T) {
	// A catalog tool named SubmitResult with a DISTINCT (decoy) schema.
	decoy := &fakeOverlayTool{name: submitResultToolName, schema: json.RawMessage(`{"type":"object","title":"DECOY"}`)}
	cat := tool.NewCatalog()
	cat.MustRegister(decoy)
	engine := NewEngine(Deps{
		LLM:     mockllm.New(),
		Catalog: cat,
		Policy:  permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil),
		Model:   "m",
	})

	overlaySchema := json.RawMessage(`{"type":"object","title":"OVERLAY"}`)
	submit := newSubmitResultTool(overlaySchema)
	r := &Run{opts: RunOptions{ExtraTools: []tool.Tool{submit}}}

	// (a) lookupTool resolves the OVERLAY, not the catalog decoy.
	got, ok := engine.lookupTool(r, submitResultToolName)
	if !ok {
		t.Fatal("lookupTool must resolve the overlay tool")
	}
	if _, isSubmit := got.(*submitResultTool); !isSubmit {
		t.Fatalf("lookupTool returned %T, want the overlay *submitResultTool (overlay-first)", got)
	}

	// (b) buildRequest advertises the OVERLAY's spec for the colliding name, exactly once.
	sess := session.New("s1", session.ModeDefault, "/ws", session.Limits{}, time.Now())
	req := engine.buildRequest(r, sess)
	count, sawOverlay := 0, false
	for _, spec := range req.Tools {
		if spec.Name == submitResultToolName {
			count++
			if strings.Contains(string(spec.Schema), "OVERLAY") {
				sawOverlay = true
			}
		}
	}
	if count != 1 {
		t.Fatalf("SubmitResult advertised %d times, want exactly 1 (overlay replaces the catalog spec)", count)
	}
	if !sawOverlay {
		t.Fatal("buildRequest must advertise the OVERLAY schema, not the catalog decoy")
	}
}

// fakeOverlayTool is a trivial read-only tool with a controllable name + schema, used to
// stand in as a catalog tool whose name collides with a run-scoped ExtraTool.
type fakeOverlayTool struct {
	name   string
	schema json.RawMessage
}

func (f *fakeOverlayTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{Name: f.name, Description: "decoy", Schema: f.schema}
}
func (*fakeOverlayTool) ReadOnly() bool { return true }
func (*fakeOverlayTool) Execute(_ context.Context, c session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	return session.NewToolResult(c.ID, "decoy"), nil
}
