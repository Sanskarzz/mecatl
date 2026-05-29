package main

import (
	"context"
	"testing"

	"github.com/stacklok/ozzharness/internal/session"
)

// TestRunScenarioOffline runs the demo's offline scenario against mockllm and
// asserts the emitted event sequence proves the whole shape of the loop: a turn
// boundary, a tool call, a tool result, a permission ask (then auto-approved),
// and a successful terminal result.
func TestRunScenarioOffline(t *testing.T) {
	events, err := RunScenario(context.Background(), mockProvider())
	if err != nil {
		t.Fatalf("RunScenario: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}

	seen := make(map[session.EventType]bool)
	for _, ev := range events {
		seen[ev.Type] = true
	}

	for _, want := range []session.EventType{
		session.EvTurnStart,
		session.EvToolCall,
		session.EvToolResult,
		session.EvPermissionAsk,
		session.EvResult,
	} {
		if !seen[want] {
			t.Errorf("event sequence missing %q", want)
		}
	}

	// The terminal result must be a successful end_turn, proving the loop resumed
	// after the approval and the model finished normally.
	last := events[len(events)-1]
	if last.Type != session.EvResult {
		t.Fatalf("last event = %q, want %q", last.Type, session.EvResult)
	}
	if last.Result == nil {
		t.Fatal("terminal result has no payload")
	}
	if last.Result.Stop != session.StopEndTurn {
		t.Errorf("terminal stop reason = %q, want %q", last.Result.Stop, session.StopEndTurn)
	}
	if last.Result.Text == "" {
		t.Error("terminal result has empty final text")
	}

	// The permission ask must precede the approved tool's result, and a Write
	// result (the approved tool) must appear and be non-error.
	assertApprovalResumed(t, events)
}

// assertApprovalResumed verifies a permission.ask was emitted and the loop then
// produced a non-error tool.result for the approved tool (Write) — i.e. the
// approval round-trip resumed the loop rather than denying the call.
func assertApprovalResumed(t *testing.T, events []session.Event) {
	t.Helper()
	var askSeen bool
	var writeResultOK bool
	for _, ev := range events {
		switch ev.Type {
		case session.EvPermissionAsk:
			if ev.Ask == nil || ev.Ask.AskID == "" {
				t.Error("permission.ask without an AskID")
			}
			askSeen = true
		case session.EvToolResult:
			// After approval, the Write tool result should be present and succeed.
			if askSeen && ev.ToolResult != nil && !ev.ToolResult.IsError {
				writeResultOK = true
			}
		}
	}
	if !askSeen {
		t.Error("no permission.ask was emitted")
	}
	if !writeResultOK {
		t.Error("no successful tool result after approval (loop did not resume)")
	}
}
