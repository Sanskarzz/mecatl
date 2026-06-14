//go:build e2e

package e2e_test

import (
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/e2e/harness"
)

// webSearchSpecs covers issue #26: WebSearch is an always-present core tool. The
// e2e harness sets NO --websearch-url (see harness/local.go), so the tool runs
// with a nil provider and returns its honest, model-facing "not configured"
// message — NOT a crash, a permission denial, or a missing-tool error. This spec
// proves the tool is wired end-to-end LIVE (present + permitted + invocable by a
// real model) without needing a real search backend.
//
// WebSearch is a floor-scoped Allow (defaultRules in internal/app/build.go, same
// posture as WebFetch), so there is NO ask round-trip and no ApproveTools entry
// is needed — mirroring the memory specs' floor-allowed tools.
func webSearchSpecs() {
	ginkgo.Describe("websearch", func() {
		ginkgo.It("invokes WebSearch and returns the honest not-configured message",
			ginkgo.FlakeAttempts(2), ginkgo.SpecTimeout(150*time.Second),
			func(ctx ginkgo.SpecContext) {
				res := runScenario(ctx, harness.RunOpts{Scenario: "websearch", Timeout: 2 * time.Minute},
					`Use the WebSearch tool to search for "Go 1.26 release notes". Call no other tool. After the WebSearch result returns, reply with the single word done.`)

				// The tool is present + permitted + the model could invoke it live.
				calls := res.ToolCalls("WebSearch")
				gomega.Expect(calls).NotTo(gomega.BeEmpty(),
					"no WebSearch tool.call observed (the tool is not wired or the model could not invoke it)\n"+failureReport())

				// The result is the honest not-configured message: a NON-error tool
				// result (the tool exists and ran; the backend is just absent), with
				// the message substring. A missing/oversized substring, an IsError
				// result, or a permission denial all fail here. The substring is a
				// stable phrase from webSearchNotConfiguredMsg in
				// internal/adapter/tools/websearch.go.
				const notConfigured = "Web search is not enabled on this deployment"
				ok := false
				for _, c := range calls {
					tr := res.ToolResult(c.ID)
					if tr == nil {
						continue
					}
					if !tr.IsError && strings.Contains(tr.Content, notConfigured) {
						ok = true
					}
				}
				gomega.Expect(ok).To(gomega.BeTrue(),
					"WebSearch did not return the honest not-configured message (expected a non-error result containing "+notConfigured+")\n"+failureReport())

				// Belt-and-braces: WebSearch must never have been auto-denied (it is
				// a floor-Allow — a denial means the floor posture regressed).
				gomega.Expect(res.Denied).NotTo(gomega.ContainElement("WebSearch"),
					"WebSearch was auto-denied — the floor-Allow posture regressed\n"+failureReport())
			})
	})
}
