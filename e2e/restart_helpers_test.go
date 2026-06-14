//go:build e2e

package e2e_test

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
)

// restart_helpers_test.go holds the small, genuinely-shared building blocks of
// the THREE cloud-native restart specs (approve-after-kill, snapshot-fidelity,
// verdict-replay). The restart DANCE itself (NewLocal → drive → Kill →
// NewLocalSharingStore) is NOT abstracted: each spec's spawn/kill/respawn
// sequence is short, and what varies between them (what is driven, what is
// approved, what is asserted) is the WHOLE point of the spec — folding it behind
// one helper would hide the distinct assertions, not clarify them (the wrong
// abstraction is worse than the duplication; Rule of Three applies to the
// stream-drain loops below, which all three specs genuinely share, NOT to the
// process lifecycle).
//
// What IS shared and extracted here:
//   - haikuLane: the hard-pinned tool-calling lane every restart spec uses.
//   - driveToWriteAsk: drain a Converse stream until the first Write permission
//     ask, returning the ask id AND the Write tool.call id (card-before-the-gate).
//   - driveToResult: drain a Converse stream until the terminal ResultMsg.

// haikuLane is the model every cloud-native restart spec HARD-PINS, independent
// of the MECATL_E2E_MODEL env override. These scenarios REQUIRE a real tool call
// (Write) and/or deterministic token accounting; the OpenAI-family lane
// content-filters tool-bearing mecatl-shaped requests (finding F2, full trail in
// e2e/README.md), so a Write ask would never fire and the budget turns would not
// run. harness.DefaultModel() honours the env override, so it is deliberately NOT
// used by these specs. (Same value + rationale as approveAfterKillModel, shared
// here so the three restart specs cannot drift on the lane.)
const haikuLane = "anthropic/claude-3.5-haiku"

// driveToWriteAsk drains the stream msgs channel until the FIRST Write permission
// ask, returning that ask's id AND the Write tool.call id. The tool.call id
// arrives BEFORE the ask (card-before-the-gate: the EvToolCall is emitted before
// authorize), so a single drain captures both. It does NOT answer the ask — the
// caller decides the verdict (approve-after-kill leaves it parked; verdict-replay
// approves allow-always). On the deadline it returns whatever it captured (the
// caller asserts non-empty with a log tail).
//
// Shared by approve-after-kill (Phase 2) and verdict-replay (Phase 3): both must
// drive a real model to a real Write ask before they diverge on the verdict.
func driveToWriteAsk(ctx ginkgo.SpecContext, stream *client.Stream, deadline time.Duration) (askID, writeCallID string) {
	ginkgo.GinkgoHelper()
	msgs := make(chan tea.Msg, 256)
	go stream.ReadLoop(ctx, msgs)

	timeout := time.After(deadline)
	for {
		select {
		case <-timeout:
			return askID, writeCallID
		case m, ok := <-msgs:
			if !ok {
				return askID, writeCallID
			}
			switch v := m.(type) {
			case client.ToolCallMsg:
				if v.Name == "Write" && writeCallID == "" {
					writeCallID = v.ID
				}
			case client.PermissionAskMsg:
				if v.Tool == "Write" {
					return v.AskID, writeCallID
				}
			}
		}
	}
}

// driveToResult drains the stream msgs channel until the terminal ResultMsg,
// returning it (and false on a deadline / clean close with no result). The
// ResultMsg carries the run's stop reason and per-run usage — the event-layer
// oracle the snapshot-fidelity spec asserts the budget terminal on.
//
// Shared by both turns of the snapshot-fidelity spec (a budget-PASSING turn on
// #1 and a budget-TRIPPING turn on #2); kept here next to driveToWriteAsk so the
// two stream-drain idioms live together.
func driveToResult(ctx ginkgo.SpecContext, stream *client.Stream, deadline time.Duration) (client.ResultMsg, bool) {
	ginkgo.GinkgoHelper()
	msgs := make(chan tea.Msg, 256)
	go stream.ReadLoop(ctx, msgs)

	timeout := time.After(deadline)
	for {
		select {
		case <-timeout:
			return client.ResultMsg{}, false
		case m, ok := <-msgs:
			if !ok {
				return client.ResultMsg{}, false
			}
			if r, isResult := m.(client.ResultMsg); isResult {
				return r, true
			}
		}
	}
}

// expectNonEmpty is a tiny shared assertion guard: a captured id must be present,
// or the spec failed at the drive step (with the server log tail for diagnosis).
func expectNonEmpty(got, what string, logTail string) {
	ginkgo.GinkgoHelper()
	gomega.Expect(got).NotTo(gomega.BeEmpty(),
		"never observed "+what+" within the deadline\n--- mecated log tail ---\n"+logTail)
}
