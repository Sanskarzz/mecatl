//go:build e2e

package e2e_test

import (
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/e2e/harness"
)

// metricFamilyToolCalls is the Prometheus rendering of the OTel instrument
// "mecatl.tool.calls" (a counter → the exporter appends _total and rewrites
// dots; verified live against the /metrics scrape).
const metricFamilyToolCalls = "mecatl_tool_calls_total"

// metricsSpecs is scenario 7. It is registered AFTER the delegation scenarios
// in the ordered root container, so by the time it scrapes, role="main" tool
// calls (skills/subagent/parallel/team scenarios) and role="subagent" child
// activity (the subagents scenario) have been recorded.
//
// LOCAL-ONLY: a remote target's counters carry whatever traffic the server has
// already served, so ">0 after the scenarios" would be vacuously true — the
// spec Skips on remote targets. The role="subagent" assertion is additionally
// gated on subagentChildActivity (set by the subagents spec), so a
// model-behaviour failure there is not double-reported as a missing series.
func metricsSpecs() {
	ginkgo.Describe("metrics", func() {
		ginkgo.It("exports role-labelled tool-call counters", func() {
			if !target.IsLocal() {
				ginkgo.Skip("remote target: pre-existing counters make the >0 assertions vacuous")
			}
			url := target.MetricsURL()
			if url == "" {
				ginkgo.Skip("no metrics URL for this target")
			}

			dump, err := harness.ScrapeMetrics(url)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Keep the raw scrape as an artifact — the assertion below names the
			// exact family, and the scrape is the proof either way.
			if dir := target.StateDir(harness.StateArtifacts); dir != "" {
				_ = os.WriteFile(filepath.Join(dir, "metrics-scrape.txt"), []byte(dump.Raw), 0o644)
			}

			mainCalls := dump.Sum(metricFamilyToolCalls, map[string]string{"role": "main"})
			gomega.Expect(mainCalls).To(gomega.BeNumerically(">", 0),
				"expected mecatl_tool_calls_total{role=\"main\"} > 0 after the tool scenarios")

			if !subagentChildActivity {
				ginkgo.AddReportEntry("metrics: subagent series check skipped",
					"the subagents spec did not observe clean child activity — its own failure/skip is the report; not double-asserting the role=\"subagent\" series here")
				return
			}
			gomega.Expect(dump.Has(metricFamilyToolCalls, map[string]string{"role": "subagent"})).To(gomega.BeTrue(),
				"expected a mecatl_tool_calls_total{role=\"subagent\"} series after the subagent scenario")
		})
	})
}
