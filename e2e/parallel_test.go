//go:build e2e

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/e2e/harness"
)

// parallelSpecs is scenario 5: one Parallel call with two branches, asserted
// purely from the parallel.* event family — parallel.start with BranchCount=2,
// per-branch events, and parallel.end.
func parallelSpecs() {
	ginkgo.Describe("parallel", func() {
		ginkgo.It("runs a two-branch Parallel fan-out to completion", ginkgo.SpecTimeout(390*time.Second), func(ctx ginkgo.SpecContext) {
			res := runScenario(ctx, harness.RunOpts{
				Scenario:     "parallel-construct",
				ApproveTools: []string{"Parallel"}, // backup; the CLI permission config already allows Parallel
				Timeout:      6 * time.Minute,
			}, `Use the tool named "Parallel" — not the Subagent tool — exactly once, with these arguments: tasks = ["Reply with the single word RED. Call no tool.", "Reply with the single word BLUE. Call no tool."] and join = "all". Never call Subagent and call no other tool. When the Parallel tool returns, reply with the single word done.`)

			starts := res.ParallelMsgs(client.ParallelStart)
			gomega.Expect(starts).NotTo(gomega.BeEmpty(), "no parallel.start observed\n"+failureReport())
			gomega.Expect(starts[0].BranchCount).To(gomega.Equal(2),
				"expected a 2-branch fan-out\n"+failureReport())

			branchStarts := res.ParallelMsgs(client.ParallelBranchStart)
			branches := map[int]bool{}
			for _, b := range branchStarts {
				branches[b.BranchIndex] = true
			}
			gomega.Expect(len(branches)).To(gomega.BeNumerically(">=", 2),
				"expected >=2 distinct branch starts\n"+failureReport())

			ends := res.ParallelMsgs(client.ParallelEnd)
			gomega.Expect(ends).NotTo(gomega.BeEmpty(), "no parallel.end observed\n"+failureReport())

			calls := res.ToolCalls("Parallel")
			gomega.Expect(calls).NotTo(gomega.BeEmpty(), failureReport())
			tr := res.ToolResult(calls[0].ID)
			gomega.Expect(tr).NotTo(gomega.BeNil(), failureReport())
			gomega.Expect(tr.IsError).To(gomega.BeFalse(), "Parallel tool result errored\n"+failureReport())
		})
	})
}
