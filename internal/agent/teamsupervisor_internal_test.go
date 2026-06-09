package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/team"
	"github.com/stacklok/mecatl/internal/tool"
)

// recordedWarn captures one Diagnostics record for the WI-9 helper test.
type recordedWarn struct {
	level port.Level
	msg   string
	args  []any
}

// capturingDiag is a minimal in-package recording Diagnostics stub (the external
// recordingDiag lives in the agent_test package and is unreachable from this internal
// test file).
type capturingDiag struct {
	lines []recordedWarn
}

func (d *capturingDiag) Log(_ context.Context, level port.Level, msg string, args ...any) {
	d.lines = append(d.lines, recordedWarn{level: level, msg: msg, args: args})
}

func (d *capturingDiag) With(...any) port.Diagnostics { return d }

func warnArgValue(line recordedWarn, key string) any {
	for i := 0; i+1 < len(line.args); i += 2 {
		if k, ok := line.args[i].(string); ok && k == key {
			return line.args[i+1]
		}
	}
	return nil
}

// TestWarnUnexpectedReopen exercises the WI-9 helper directly: it emits exactly one WARN
// only when a member's Reopen failed for a reason OTHER than cancellation, rides the
// supplied diag, and is a no-op (no panic) for the nil-diag/expected cases.
func TestWarnUnexpectedReopen(t *testing.T) {
	t.Run("nil reopen error → no line", func(t *testing.T) {
		d := &capturingDiag{}
		warnUnexpectedReopen(context.Background(), d, "worker", session.StopMaxTurns, nil)
		if len(d.lines) != 0 {
			t.Fatalf("expected no diagnostic for a nil reopen error; got %+v", d.lines)
		}
	})
	t.Run("cancelled stop with error → no line", func(t *testing.T) {
		d := &capturingDiag{}
		warnUnexpectedReopen(context.Background(), d, "worker", session.StopCancelled, errors.New("reopen: not completed"))
		if len(d.lines) != 0 {
			t.Fatalf("a cancelled member's expected reopen failure must not warn; got %+v", d.lines)
		}
	})
	t.Run("unexpected reopen failure → one warn", func(t *testing.T) {
		d := &capturingDiag{}
		warnUnexpectedReopen(context.Background(), d, "worker", session.StopMaxTurns, errors.New("boom"))
		if len(d.lines) != 1 {
			t.Fatalf("expected exactly one WARN line; got %+v", d.lines)
		}
		line := d.lines[0]
		if line.level != port.LevelWarn {
			t.Fatalf("level = %v, want LevelWarn", line.level)
		}
		if got := warnArgValue(line, "member"); got != "worker" {
			t.Fatalf("member attr = %v, want %q", got, "worker")
		}
		if got := warnArgValue(line, "error"); got != "boom" {
			t.Fatalf("error attr = %v, want %q", got, "boom")
		}
	})
	t.Run("nil diag → no panic", func(_ *testing.T) {
		warnUnexpectedReopen(context.Background(), nil, "worker", session.StopMaxTurns, errors.New("boom"))
	})
}

// TestRunTurnCancelledMemberCapturesTurnsUsed replicates the cancelled-member scenario
// (a request observer cancels the ctx as the worker's turn reaches the provider, so the
// member ends StopCancelled) and asserts the internal memberRT state: the cancelled
// member's turn spend is captured and counted (turnsUsed == 1) despite its failed
// Reopen (Reopen is completed-only, so it always fails for a cancelled member), and
// the member is classified StopReasonCancelled.
func TestRunTurnCancelledMemberCapturesTurnsUsed(t *testing.T) {
	tm := team.New("t")
	ctx, cancel := context.WithCancel(context.Background())
	allow := permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
	factory := func(spec MemberSpec) MemberBuild {
		cat := tool.NewCatalog()
		for _, tl := range MemberTools(tm, spec.Name, nil) {
			cat.MustRegister(tl)
		}
		prov := mockllm.NewWith(
			[]mockllm.Option{mockllm.WithRequestObserver(func(port.LLMRequest) { cancel() })},
			mockllm.TextTurn("never reached cleanly"),
		)
		return MemberBuild{Engine: NewEngine(Deps{
			LLM: prov, Catalog: cat, Policy: allow, Hooks: hookexec.New(nil), Model: "mock",
		})}
	}
	sup := NewSupervisor(tm, memfs.NewWorkspace("/ws"), factory, WithMaxRounds(5))
	if err := sup.AddMember(context.Background(), MemberSpec{Name: "worker", InitialPrompt: "go"}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	out := sup.Run(ctx, nil)

	// The outcome must report the single member as stopped/cancelled (the scenario
	// actually fired), so the internal-state assertions below mean something.
	if len(out.Members) != 1 {
		t.Fatalf("outcome members = %d, want 1: %+v", len(out.Members), out.Members)
	}
	if mo := out.Members[0]; mo.Disposition != DispositionStopped || mo.Reason != StopReasonCancelled {
		t.Fatalf("outcome disposition = %q/%q, want stopped/cancelled", mo.Disposition, mo.Reason)
	}

	m, ok := sup.members["worker"]
	if !ok {
		t.Fatalf("supervisor has no member %q; members=%v", "worker", sup.members)
	}
	if m.turnsUsed != 1 {
		t.Fatalf("turnsUsed = %d, want 1 (the cancelled member's turn spend must be captured and counted)", m.turnsUsed)
	}
	if m.stopReason != StopReasonCancelled {
		t.Fatalf("stopReason = %q, want %q", m.stopReason, StopReasonCancelled)
	}
}
