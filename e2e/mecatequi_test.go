//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/stacklok/mecatl/internal/adapter/gitenv"
)

// mecatequi_test.go is the LIVE round-trip oracle for the standalone mecatequi
// binary (cmd/mecatequi). Unlike every other spec in this suite — which drives a
// spawned mecated SERVER over the gRPC Converse wire via the client harness —
// this spec BUILDS the mecatequi binary and EXECS it directly: mecatequi is a
// single-shot in-process runner with NO server, so there is no client/server
// harness to reuse.
//
// WHY THIS TEST EXISTS (the missing test class). mecatequi emits a working-tree
// patch (--out-diff) plus a machine summary (--out-summary). A real bug shipped
// where `git diff HEAD` omitted UNTRACKED (new) files: the summary honestly said
// non_empty_diff:true but the emitted patch was effectively empty for new files,
// so a downstream `git apply` silently LOST the agent's new files. Asserting the
// summary alone could never catch it — only a ROUND-TRIP (apply the emitted patch
// to a clean checkout of HEAD and verify the work reproduces) closes the gap.
//
// THE ORACLE. The prompt instructs the model to (a) CREATE a new file (the
// untracked-file path — the bug) and (b) MODIFY the committed README (the tracked
// path). After the run we clone the repo's HEAD into a SEPARATE clean dir, apply
// the emitted patch there, and assert BOTH changes reproduce. That apply is the
// assertion the shipped bug would have failed.
//
// LANE. It hard-pins haikuLane (the tool-calling lane; the OpenAI-family lane
// content-filters tool-bearing requests — finding F2, e2e/README.md), independent
// of MECATL_E2E_MODEL, because the run REQUIRES real Write/Edit tool calls.

// mecatequiSpecs registers the live round-trip spec. Called from the suite's one
// Ordered container (suite_test.go) so it shares the canary gate.
func mecatequiSpecs() {
	ginkgo.Describe("mecatequi binary (round-trip patch oracle)", func() {
		// One model call. SpecTimeout = the binary's --timeout (3m) + 60s slack for
		// the build + git plumbing.
		ginkgo.It("emits a patch that reproduces BOTH a new file and a README edit",
			ginkgo.SpecTimeout(4*time.Minute), func(ctx ginkgo.SpecContext) {
				key := os.Getenv("OPENROUTER_API_KEY")
				if key == "" {
					// The suite-wide BeforeSuite already aborts when neither
					// OPENROUTER_API_KEY nor a target is set; a MECATL_E2E_TARGET-only
					// run (remote server, no key locally) cannot exec the local binary
					// against OpenRouter, so skip honestly rather than fail.
					ginkgo.Skip("OPENROUTER_API_KEY is not set in this process; mecatequi execs the binary against OpenRouter directly and needs the key in its own environment")
				}

				bin := buildMecatequi(ctx)
				repo := initMecatequiRepo(ctx)

				diffPath := filepath.Join(ginkgo.GinkgoT().TempDir(), "out.diff")
				summaryPath := filepath.Join(ginkgo.GinkgoT().TempDir(), "out.summary.json")

				const (
					newFileName    = "GREETING.txt"
					newFileMarker  = "MECATEQUI_ROUNDTRIP_MARKER"
					readmeMarker   = "MECATEQUI_README_EDIT"
					readmeBaseline = "# mecatequi round-trip fixture\n\nBaseline README, committed at HEAD.\n"
				)

				prompt := "You are working in a git repository. Perform EXACTLY these two file changes and then stop:\n" +
					"1. CREATE a brand-new file named " + newFileName + " whose contents include the exact line: " + newFileMarker + "\n" +
					"2. MODIFY the existing README.md so that it contains the exact line: " + readmeMarker + " (append it; keep the existing text).\n" +
					"Use the Write and Edit tools. Do NOT run git commit. Do NOT create any other files. When both changes are done, report that you are finished."

				// Drive the binary. --posture auto so the headless main engine never
				// asks (a strict run would cancel-on-ask, exit 1). --untrusted-prompt
				// exercises the fence path. The key rides the ENV, never the args.
				args := []string{
					"--workspace", repo,
					"--posture", "auto",
					"--headless",
					"--untrusted-prompt",
					"--default-provider", "openrouter",
					"--default-model", haikuLane,
					"--max-run-tokens", "90000",
					"--timeout", "3m",
					"--out-diff", diffPath,
					"--out-summary", summaryPath,
					"--prompt", prompt,
				}
				cmd := exec.CommandContext(ctx, bin, args...)
				cmd.Dir = repo
				// Scrub inherited GIT_* danger but inject only OPENROUTER_API_KEY (no
				// OPENAI/ANTHROPIC key) so the binary resolves the openrouter provider.
				cmd.Env = append(gitenv.Scrub(os.Environ()), "OPENROUTER_API_KEY="+key)

				var stderr strings.Builder
				cmd.Stderr = &stderr
				stdout, runErr := cmd.Output()

				logTail := func() string {
					return "\n--- mecatequi stdout ---\n" + string(stdout) +
						"\n--- mecatequi stderr tail ---\n" + tail(stderr.String(), 4096)
				}

				gomega.Expect(runErr).NotTo(gomega.HaveOccurred(),
					"mecatequi exited non-zero (a clean end_turn run must exit 0)"+logTail())

				// --- (4) Summary assertions: exit 0 (above) + the honest terminal. ---
				summaryBytes, err := os.ReadFile(summaryPath)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "reading the summary file"+logTail())

				var summary struct {
					StopReason   string `json:"stop_reason"`
					NonEmptyDiff bool   `json:"non_empty_diff"`
					DiffBytes    int    `json:"diff_bytes"`
					Usage        struct {
						TotalTokens int `json:"total_tokens"`
					} `json:"usage"`
				}
				gomega.Expect(json.Unmarshal(summaryBytes, &summary)).To(gomega.Succeed(),
					"the summary is not valid JSON"+logTail())

				gomega.Expect(summary.StopReason).To(gomega.Equal("end_turn"),
					"the run did not finish cleanly (read stop_reason)"+logTail())
				gomega.Expect(summary.NonEmptyDiff).To(gomega.BeTrue(),
					"the summary reports an empty diff — the model did no file work"+logTail())
				gomega.Expect(summary.DiffBytes).To(gomega.BeNumerically(">", 0),
					"diff_bytes is zero"+logTail())
				gomega.Expect(summary.Usage.TotalTokens).To(gomega.BeNumerically(">", 0),
					"usage.total_tokens is zero — no provider call accounted"+logTail())

				patch, err := os.ReadFile(diffPath)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "reading the diff file"+logTail())
				gomega.Expect(len(patch)).To(gomega.BeNumerically(">", 0),
					"the emitted patch file is empty"+logTail())

				// --- (5) THE KEY ASSERTION: round-trip the patch onto a CLEAN HEAD. ---
				// `git clone` of a local repo materialises only the committed HEAD, so
				// the clone has neither the new file nor the README edit until we apply
				// the patch. Both reproducing proves the patch is faithful.
				clean := filepath.Join(ginkgo.GinkgoT().TempDir(), "clean")
				gitRun(ctx, "", "clone", repo, clean)

				gitRun(ctx, clean, "apply", diffPath)

				// (5a) the NEW file exists with its marker (the shipped-bug path).
				newFileBytes, err := os.ReadFile(filepath.Join(clean, newFileName))
				gomega.Expect(err).NotTo(gomega.HaveOccurred(),
					"the new file did not reproduce after applying the patch to a clean HEAD — THIS is the omitted-untracked-files bug"+logTail())
				gomega.Expect(string(newFileBytes)).To(gomega.ContainSubstring(newFileMarker),
					"the reproduced new file is missing its marker line"+logTail())

				// (5b) the README edit reproduces (the tracked-modification path).
				readmeBytes, err := os.ReadFile(filepath.Join(clean, "README.md"))
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "reading the reproduced README"+logTail())
				gomega.Expect(string(readmeBytes)).To(gomega.ContainSubstring(readmeMarker),
					"the README edit did not reproduce after applying the patch"+logTail())

				// --- (6) NO secret leak: the key value appears nowhere observable. ---
				gomega.Expect(string(patch)).NotTo(gomega.ContainSubstring(key),
					"the OPENROUTER_API_KEY value leaked into the emitted patch")
				gomega.Expect(string(summaryBytes)).NotTo(gomega.ContainSubstring(key),
					"the OPENROUTER_API_KEY value leaked into the summary")
				gomega.Expect(stderr.String()).NotTo(gomega.ContainSubstring(key),
					"the OPENROUTER_API_KEY value leaked into stderr")
				_ = readmeBaseline // documented baseline; written by initMecatequiRepo
			})
	})
}

// buildMecatequi compiles ./cmd/mecatequi into a temp binary and returns its path.
// It builds from the repo root (resolved from this test file's package dir).
func buildMecatequi(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	bin := filepath.Join(ginkgo.GinkgoT().TempDir(), "mecatequi")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/mecatequi")
	cmd.Dir = repoRoot()
	out, err := cmd.CombinedOutput()
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "building mecatequi failed:\n"+string(out))
	return bin
}

// initMecatequiRepo creates a hermetic git repo in a temp dir: git init, a scrubbed
// + injected git identity (via gitenv, so no global config leaks in), and a single
// committed README at HEAD. It returns the repo root. The recipe mirrors the
// determinism of the harness's git plumbing (gitenv.Scrub + NeutralizingVars).
func initMecatequiRepo(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	repo := ginkgo.GinkgoT().TempDir()
	gitRun(ctx, repo, "init", "-b", "main")
	readme := filepath.Join(repo, "README.md")
	gomega.Expect(os.WriteFile(readme, []byte("# mecatequi round-trip fixture\n\nBaseline README, committed at HEAD.\n"), 0o644)).
		To(gomega.Succeed())
	gitRun(ctx, repo, "add", "README.md")
	gitRun(ctx, repo, "commit", "-m", "baseline")
	return repo
}

// gitRun runs a git command in dir (empty = inherit cwd) with a SCRUBBED +
// neutralised environment so global/system git config cannot make the test
// non-deterministic — the same hardening mecatequi's own gitDiffPatch applies.
// GIT_CONFIG_GLOBAL=/dev/null + GIT_CONFIG_NOSYSTEM=1 (from gitenv.NeutralizingVars)
// mean the host's global identity is unavailable, so we inject a deterministic
// author/committer identity via the GIT_AUTHOR_*/GIT_COMMITTER_* env vars (git
// honours these without any config file), letting `git commit` work hermetically.
func gitRun(ctx context.Context, dir string, args ...string) {
	ginkgo.GinkgoHelper()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(gitenv.Scrub(os.Environ()), gitenv.NeutralizingVars()...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=mecatequi e2e",
		"GIT_AUTHOR_EMAIL=e2e@mecatl.test",
		"GIT_COMMITTER_NAME=mecatequi e2e",
		"GIT_COMMITTER_EMAIL=e2e@mecatl.test",
	)
	out, err := cmd.CombinedOutput()
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "git "+strings.Join(args, " ")+" failed:\n"+string(out))
}

// repoRoot resolves the repository root by walking up from the test's cwd to the
// directory holding go.mod. `go test` runs with cwd = the package dir (e2e/), so
// the parent is the root; the walk is defensive.
func repoRoot() string {
	ginkgo.GinkgoHelper()
	dir, err := os.Getwd()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		gomega.Expect(parent).NotTo(gomega.Equal(dir), "walked to filesystem root without finding go.mod")
		dir = parent
	}
}

// tail returns the last n bytes of s (the stderr tail for failure reports).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
