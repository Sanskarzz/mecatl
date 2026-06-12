//go:build e2e

package e2e_test

import (
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/e2e/harness"
)

// skillSpecs covers scenarios 2 + 3: skill activation from the user-global lane
// (~/.claude/skills under the fake HOME, via --skills-conventional) and the
// workspace lane (<workspace>/.claude/skills, admitted because the harness
// passes --trust-project). The assertion is the EVENT: a Skill tool.call naming
// the fixture skill, resolved without error — never model prose.
//
// Deliberately NO ApproveTools backup here: the CLI-scope allow in
// permissions.yaml (--permission-config) is the wiring under test, and the
// specs assert that ZERO interactive approvals were needed. A Skill ask
// surfacing means the CLI-allow wiring regressed — fix the fixture, not the
// driver policy.
func skillSpecs() {
	ginkgo.Describe("skills", func() {
		activate := func(ctx ginkgo.SpecContext, scenario, skill, prompt string) {
			ginkgo.GinkgoHelper()
			res := runScenario(ctx, harness.RunOpts{
				Scenario: scenario,
				Timeout:  3 * time.Minute,
			}, prompt)

			// The CLI-scope allow must pre-approve Skill: no ask may surface.
			for _, a := range res.Asks {
				gomega.Expect(a.Tool).NotTo(gomega.Equal("Skill"),
					"Skill ASKED — the permissions.yaml CLI-scope allow is not wired\n"+failureReport())
			}
			gomega.Expect(res.Approved).To(gomega.BeEmpty(),
				"no interactive approvals should be needed for skill activation\n"+failureReport())

			calls := res.ToolCalls("Skill")
			gomega.Expect(calls).NotTo(gomega.BeEmpty(), "no Skill tool.call observed\n"+failureReport())
			activated := false
			for _, c := range calls {
				if !strings.Contains(c.Args, skill) {
					continue
				}
				if tr := res.ToolResult(c.ID); tr != nil && !tr.IsError {
					activated = true
				}
			}
			gomega.Expect(activated).To(gomega.BeTrue(),
				"no successful Skill activation of "+skill+" observed\n"+failureReport())
		}

		// PROMPT PHRASING NOTE: the upstream provider runs a deterministic
		// prompt-shield-style input filter on mecatl-shaped requests; phrases
		// like "follow its instructions" / "Do not use any tools" reliably
		// terminate the stream with `response incomplete: content_filter`.
		// Every prompt below was probe-verified clean (see e2e/README.md).
		ginkgo.It("activates a user-global skill (~/.claude/skills)",
			ginkgo.FlakeAttempts(2), ginkgo.SpecTimeout(210*time.Second),
			func(ctx ginkgo.SpecContext) {
				activate(ctx, "skill-global", "greet",
					`With the Skill tool, activate the skill named "greet", and then greet Ozz in the format the skill specifies. Call no other tool.`)
			})

		ginkgo.It("activates a workspace skill (<workspace>/.claude/skills, trust-gated)",
			ginkgo.FlakeAttempts(2), ginkgo.SpecTimeout(210*time.Second),
			func(ctx ginkgo.SpecContext) {
				activate(ctx, "skill-workspace", "repo-fact",
					`With the Skill tool, activate the skill named "repo-fact", and then state this repository's registered fact in the format the skill specifies. Call no other tool.`)
			})
	})
}
