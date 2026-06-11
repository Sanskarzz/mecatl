package agent

import (
	"strings"
	"testing"
)

// TestBuildJudgePromptCarriesRubricAndOutputContract pins the judge prompt's content
// (the behavior tests in forkjudge_test.go drive Judge() with a scripted mock, so they
// would pass even if the prompt text regressed): the summaries-only disclosure, the
// three evaluation axes, and the strict single-line-JSON output contract must all be
// present, alongside the unchanged header/branch framing.
func TestBuildJudgePromptCarriesRubricAndOutputContract(t *testing.T) {
	got := buildJudgePrompt([]BranchSummary{
		{Label: "alpha", Summary: "did the thing"},
		{Label: "beta", Summary: "did it differently"},
	}, "")

	for _, want := range []string{
		"You are selecting the best result among 2 candidate branches.",
		"Branch 1 (alpha): did the thing",
		"Branch 2 (beta): did it differently",
		"summaries alone",
		"correctness",
		"completeness",
		"clarity",
		"single line of JSON",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("judge prompt missing %q:\n%s", want, got)
		}
	}
}
