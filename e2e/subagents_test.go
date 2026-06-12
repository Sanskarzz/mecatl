//go:build e2e

package e2e_test

import (
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/e2e/harness"
)

// subagentSpecs is scenario 4: two Subagent calls issued in one assistant
// message (the dispatcher runs read-only tools in parallel). The assertion is
// N DISTINCT child runs — at least two subagent.start events with distinct
// child ids and two Subagent tool results carrying the agentId trailer — NOT
// temporal overlap (timing is not observable from the event stream and would
// flake).
func subagentSpecs() {
	ginkgo.Describe("subagents", func() {
		ginkgo.It("fans out two parallel Subagent calls with distinct child runs", ginkgo.SpecTimeout(390*time.Second), func(ctx ginkgo.SpecContext) {
			// The child goals make each child call Read (a floor-allowed,
			// read-only tool): that is what gives the metrics scenario its
			// role="subagent" tool-call series, and it keeps the goal phrasing
			// clean of the upstream prompt-filter triggers ("Do not use any
			// tools" deterministically content_filters — see e2e/README.md).
			res := runScenario(ctx, harness.RunOpts{
				Scenario: "subagents-parallel",
				Timeout:  6 * time.Minute,
			}, `Call the Subagent tool exactly twice, and place BOTH calls in the same single assistant message so they run in parallel. First call: goal "Read the file FRUIT.txt with the Read tool and reply with the fruit it names." Second call: goal "Read the file README.md with the Read tool and reply with its first heading." Call no other tool. After both results return, reply with the single word done.`)

			starts := res.SubagentMsgs(client.SubagentStart)
			ids := map[string]bool{}
			for _, s := range starts {
				if s.ChildID != "" {
					ids[s.ChildID] = true
				}
			}
			gomega.Expect(len(ids)).To(gomega.BeNumerically(">=", 2),
				"expected >=2 distinct subagent child ids\n"+failureReport())

			calls := res.ToolCalls("Subagent")
			gomega.Expect(len(calls)).To(gomega.BeNumerically(">=", 2),
				"expected >=2 Subagent tool calls\n"+failureReport())
			trailers := 0
			for _, c := range calls {
				if tr := res.ToolResult(c.ID); tr != nil && strings.Contains(tr.Content, "agentId:") {
					trailers++
				}
			}
			gomega.Expect(trailers).To(gomega.BeNumerically(">=", 2),
				"expected >=2 Subagent results carrying the agentId trailer\n"+failureReport())

			// The children must have ENDED CLEANLY — the agentId trailer rides
			// every terminal incl. error, so without this a silently-erroring
			// child would still pass the fan-out assertions above.
			clean := 0
			for _, e := range res.SubagentMsgs(client.SubagentEnd) {
				if e.Stop == "end_turn" {
					clean++
				}
			}
			gomega.Expect(clean).To(gomega.BeNumerically(">=", 2),
				"expected >=2 subagent children ending stop=end_turn\n"+failureReport())

			// All assertions passed → real child activity happened. The metrics
			// spec gates its role="subagent" assertion on this flag, so a
			// model-behaviour failure here is reported once, not twice.
			subagentChildActivity = true
		})
	})
}
