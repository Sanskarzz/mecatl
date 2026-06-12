//go:build e2e

package e2e_test

import (
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/e2e/harness"
)

// soulSpecs is scenario 10. The DETERMINISTIC assertion is the composition
// fact mecated logs at build time when a soul is applied (internal/app
// soulselect.go: "soul ENABLED (user provenance, ...)"); the BEHAVIOURAL
// marker ("prefix replies with SOUL-OK:") is model-dependent, so it lives in a
// QUARANTINE-labelled spec that records its outcome but never fails the suite.
func soulSpecs() {
	ginkgo.Describe("soul", func() {
		ginkgo.It("logs the soul-applied composition fact at build", func() {
			if !target.IsLocal() {
				ginkgo.Skip("remote target: cannot read the server's log")
			}
			// The fact is logged once at app.Build (startup), so the captured
			// log already holds it — no run needed.
			tail := target.LogTail(64 * 1024)
			gomega.Expect(tail).To(gomega.ContainSubstring("soul ENABLED (user provenance"),
				"the soul composition diagnostic was not logged at startup; --soul-file wiring or the fixture is broken")
		})

		ginkgo.It("behavioural marker: replies carry the soul's SOUL-OK prefix",
			ginkgo.Label("quarantine"), ginkgo.SpecTimeout(150*time.Second),
			func(ctx ginkgo.SpecContext) {
				// QUARANTINE: records the outcome, NEVER fails the suite — model
				// adherence to a stylistic persona is not a harness contract.
				res, err := driver.Run(ctx, harness.RunOpts{Scenario: "soul-behavioural", Timeout: 2 * time.Minute},
					"Reply with the single word hello.")
				switch {
				case err != nil:
					ginkgo.AddReportEntry("soul behavioural marker (quarantine)",
						"run failed (not counted): "+err.Error())
				case res.Result == nil:
					ginkgo.AddReportEntry("soul behavioural marker (quarantine)",
						"no terminal result (not counted)")
				default:
					trackUsage(res)
					text := strings.TrimSpace(res.AssistantText())
					if strings.HasPrefix(text, "SOUL-OK:") {
						ginkgo.AddReportEntry("soul behavioural marker (quarantine)",
							"PRESENT: the reply carried the SOUL-OK: prefix")
					} else {
						ginkgo.AddReportEntry("soul behavioural marker (quarantine)",
							"ABSENT: the reply did not carry the SOUL-OK: prefix (model adherence, not a harness failure). transcript: "+res.TranscriptPath)
					}
				}
			})
	})
}
