//go:build e2e

package e2e_test

import (
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/e2e/harness"
)

// webSearchSpecs covers issue #26: WebSearch is an always-present core tool, ON by
// default via the Exa anonymous tier (no key, no config). Two specs:
//
//	(a) live Exa default — the shared suite target runs with NO --websearch flag, so
//	    web search is the Exa anonymous default. The model must invoke WebSearch and
//	    get a NON-error result that either carries a real hit (a URL line) OR the
//	    backend-down degradation message (Exa unreachable/rate-limited). It must
//	    NEVER be the disabled message — that would mean the default regressed.
//	    Network-dependent → FlakeAttempts(2).
//	(b) kill switch — a SEPARATE mecated spawned with --websearch=off. The model
//	    invokes WebSearch and gets the deterministic, network-INDEPENDENT disabled
//	    message. No real backend is contacted.
//
// WebSearch is a floor-scoped Allow (defaultRules in internal/app/build.go, same
// posture as WebFetch), so there is NO ask round-trip — mirroring the memory specs.
func webSearchSpecs() {
	ginkgo.Describe("websearch", func() {
		// (a) The Exa anonymous default — web search is ON out of the box.
		ginkgo.It("invokes WebSearch and gets a live result or the backend-down message (Exa default)",
			ginkgo.FlakeAttempts(2), ginkgo.SpecTimeout(150*time.Second),
			func(ctx ginkgo.SpecContext) {
				res := runScenario(ctx, harness.RunOpts{Scenario: "websearch-default", Timeout: 2 * time.Minute},
					`Use the WebSearch tool to search for "Go 1.26 release notes". Call no other tool. After the WebSearch result returns, reply with the single word done.`)

				calls := res.ToolCalls("WebSearch")
				gomega.Expect(calls).NotTo(gomega.BeEmpty(),
					"no WebSearch tool.call observed (the tool is not wired or the model could not invoke it)\n"+failureReport())

				// The default backend is Exa: the result is a NON-error tool result that
				// is EITHER a live hit (a "URL:" line) OR the backend-down degradation
				// message. It must NEVER be the disabled message (that would mean the
				// Exa default regressed) and never a permission denial.
				const backendDown = "temporarily unavailable"
				const disabled = "disabled on this deployment"
				ok := false
				for _, c := range calls {
					tr := res.ToolResult(c.ID)
					if tr == nil || tr.IsError {
						continue
					}
					if strings.Contains(tr.Content, disabled) {
						ginkgo.Fail("WebSearch returned the DISABLED message but no --websearch=off was set — the Exa default regressed\n" + failureReport())
					}
					if strings.Contains(tr.Content, "URL:") || strings.Contains(tr.Content, backendDown) {
						ok = true
					}
				}
				gomega.Expect(ok).To(gomega.BeTrue(),
					"WebSearch (Exa default) returned neither a live result (URL: line) nor the backend-down message\n"+failureReport())

				gomega.Expect(res.Denied).NotTo(gomega.ContainElement("WebSearch"),
					"WebSearch was auto-denied — the floor-Allow posture regressed\n"+failureReport())
			})

		// (b) The kill switch — deterministic, network-independent.
		ginkgo.It("returns the disabled message under --websearch=off",
			ginkgo.SpecTimeout(150*time.Second),
			func(ctx ginkgo.SpecContext) {
				// A dedicated mecated with the kill switch flipped. It owns its own
				// scratch tree and is torn down at the end of the spec.
				off, err := harness.NewLocalWith("--websearch=off")
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "spawning a --websearch=off mecated failed")
				defer func() { _ = off.Close() }()

				driverOff := harness.NewDriver(off)
				res, err := driverOff.Run(ctx, harness.RunOpts{Scenario: "websearch-off", Timeout: 2 * time.Minute},
					`Use the WebSearch tool to search for "Go 1.26 release notes". Call no other tool. After the WebSearch result returns, reply with the single word done.`)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), harness.Summary(res, err, off.LogTail(4096)))
				gomega.Expect(res).NotTo(gomega.BeNil())

				calls := res.ToolCalls("WebSearch")
				gomega.Expect(calls).NotTo(gomega.BeEmpty(),
					"no WebSearch tool.call observed under the kill switch\n"+harness.Summary(res, err, off.LogTail(4096)))

				const disabled = "disabled on this deployment"
				ok := false
				for _, c := range calls {
					tr := res.ToolResult(c.ID)
					if tr == nil {
						continue
					}
					if !tr.IsError && strings.Contains(tr.Content, disabled) {
						ok = true
					}
				}
				gomega.Expect(ok).To(gomega.BeTrue(),
					"WebSearch did not return the disabled message under --websearch=off\n"+harness.Summary(res, err, off.LogTail(4096)))
			})
	})
}
