//go:build e2e

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/e2e/harness"
)

// providerSpecs is scenario 1: the provider smoke — one cheap single-turn run
// per lane, asserting the clean wire-level terminal (stop=end_turn, no error,
// real usage). The default-lane spec is THE canary: its outcome gates every
// later scenario (see the BeforeEach in suite_test.go).
func providerSpecs() {
	ginkgo.Describe("provider smoke", func() {
		ginkgo.It("completes a one-word turn on the default lane",
			ginkgo.Label("canary"), ginkgo.FlakeAttempts(2), ginkgo.SpecTimeout(150*time.Second),
			func(ctx ginkgo.SpecContext) {
				canaryDone, canaryOK = true, false
				res := runScenario(ctx, harness.RunOpts{Scenario: "provider-smoke", Timeout: 2 * time.Minute},
					"Reply with exactly the single word: ok. Do not call any tools.")

				gomega.Expect(res.Result.Error).To(gomega.BeEmpty(), failureReport())
				gomega.Expect(res.Stop()).To(gomega.Equal("end_turn"), failureReport())
				gomega.Expect(res.Usage().InputTokens).To(gomega.BeNumerically(">", 0), failureReport())
				gomega.Expect(res.Usage().OutputTokens).To(gomega.BeNumerically(">", 0), failureReport())
				canaryOK = true
			})

		ginkgo.It("completes a one-word turn on the secondary (OpenAI-family) lane",
			ginkgo.FlakeAttempts(2), ginkgo.SpecTimeout(150*time.Second),
			func(ctx ginkgo.SpecContext) {
				model := harness.SecondaryModel()
				if model == "" {
					ginkgo.Skip("secondary lane disabled (MECATL_E2E_MODEL_SECONDARY=skip)")
				}
				res := runScenario(ctx, harness.RunOpts{Scenario: "provider-smoke-secondary", Model: model, Timeout: 2 * time.Minute},
					"Reply with exactly the single word: ok. Do not call any tools.")

				gomega.Expect(res.Result.Error).To(gomega.BeEmpty(), failureReport())
				gomega.Expect(res.Stop()).To(gomega.Equal("end_turn"), failureReport())
				gomega.Expect(res.Usage().OutputTokens).To(gomega.BeNumerically(">", 0), failureReport())
			})
	})
}
