package agent

import (
	"testing"

	"github.com/stacklok/mecatl/internal/session"
)

// TestProjectTeamEventTurnEndContextMeter asserts the turn.end projection carries
// the per-member context-meter fields: ContextUsed is THIS turn's input-token
// count (current occupancy, the meter numerator — an assignment, not a sum) and
// ContextWindow is the producing member engine's window (forwarded off the
// TeamEvent, the meter denominator). It also confirms the existing per-turn Usage
// is still forwarded unchanged.
func TestProjectTeamEventTurnEndContextMeter(t *testing.T) {
	const window = 200000
	te := TeamEvent{
		Member:        "scout",
		ContextWindow: window,
		Event: session.Event{
			Type: session.EvTurnEnd,
			TurnEnd: &session.TurnEndPayload{
				Usage: session.Usage{InputTokens: 40000, OutputTokens: 80},
			},
		},
	}

	ev, ok := projectTeamEvent("p1", "team-p1", te)
	if !ok {
		t.Fatal("turn.end must project to a team.member event")
	}
	if ev.Type != session.EvTeamMember || ev.Team == nil {
		t.Fatalf("projected event mis-shaped: %+v", ev)
	}
	p := ev.Team
	if p.ContextUsed != 40000 {
		t.Errorf("ContextUsed = %d, want 40000 (this turn's input tokens)", p.ContextUsed)
	}
	if p.ContextWindow != window {
		t.Errorf("ContextWindow = %d, want %d (the member engine's window)", p.ContextWindow, window)
	}
	// The pre-existing per-turn usage projection must be unchanged.
	if p.Usage.InputTokens != 40000 || p.Usage.OutputTokens != 80 {
		t.Errorf("Usage = %+v, want the forwarded per-turn usage", p.Usage)
	}
}

// TestProjectTeamEventUnknownWindow asserts a turn.end whose TeamEvent carries no
// window (a member engine with ContextWindowTokens == 0) still projects ContextUsed
// but a zero ContextWindow — the client then suppresses the meter (no denominator).
func TestProjectTeamEventUnknownWindow(t *testing.T) {
	te := TeamEvent{
		Member: "scout",
		Event: session.Event{
			Type:    session.EvTurnEnd,
			TurnEnd: &session.TurnEndPayload{Usage: session.Usage{InputTokens: 1200}},
		},
	}
	ev, ok := projectTeamEvent("p1", "team-p1", te)
	if !ok || ev.Team == nil {
		t.Fatal("turn.end must still project")
	}
	if ev.Team.ContextUsed != 1200 {
		t.Errorf("ContextUsed = %d, want 1200", ev.Team.ContextUsed)
	}
	if ev.Team.ContextWindow != 0 {
		t.Errorf("ContextWindow = %d, want 0 (unknown window)", ev.Team.ContextWindow)
	}
}
