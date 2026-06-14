//go:build e2e

package e2e_test

import (
	"time"

	"github.com/onsi/ginkgo/v2"
)

// approveAfterKillSpecs is the cloud-native Phase 2 LIVE scenario: raise a
// permission ask, SIGKILL the mecated process, restart a SECOND mecated over the
// SAME --store-dir, POST approve, and assert the pending tool ran EXACTLY ONCE and
// the run completed.
//
// HARNESS GAP (honest note, per the Phase 2 plan): the current live harness cannot
// express this scenario without surgery, and it is deliberately NOT faked here:
//
//   - The shared suite target (harness.NewLocal, e2e/suite_test.go BeforeSuite)
//     spawns ONE mecated with a per-spawn EPHEMERAL store dir under its scratch
//     root (e2e/harness/local.go: --store-dir l.dir("store")). There is no seam to
//     spawn a SECOND mecated over the SAME store dir, which the restart leg
//     requires.
//   - The driver speaks only the gRPC Converse bidi stream and resolves asks
//     IN-STREAM via policy (e2e/harness/driver.go); it exposes no standalone
//     POST /v1/sessions/{id}/approve client, and mecated is spawned with --grpc-addr
//     only (no --http-addr), so the reconnect-and-relay approve path has no live
//     surface to drive.
//   - There is no exposed SIGKILL-without-cleanup primitive on the target (Close
//     SIGTERMs then SIGKILLs, but tears the scratch tree down too).
//
// The REQUIRED CI-green proof is the offline two-Build gate
// TestApproveAfterRestartE2E (internal/app/), which exercises the IDENTICAL path
// (app.Build #1 → ask + Persist → Close = process death → app.Build #2 over the
// same store → Service.ApproveRun → ResumeApproval → exactly-once Write +
// StopEndTurn), plus the server-layer TestApproveAfterRestartResumesAwaiting and
// the engine-layer ResumeApproval suite. This spec is the live counterpart whose
// harness support is a follow-on; it Skips with this rationale rather than asserting
// nothing.
//
// CAVEAT (what only a real SIGKILL would catch, and the two-Build does NOT): a torn
// final append racing the kill (jsonlstore appendLine is not an atomic rename) and
// OS-crash durability (no fsync) — both narrow and out of scope for "disposable
// process" (process restart, not host crash); see docs/design/CLOUD-NATIVE.md.
func approveAfterKillSpecs() {
	ginkgo.Describe("approve-after-kill (cloud-native Phase 2)", func() {
		ginkgo.It("resumes an awaiting session across a SIGKILL+restart and runs the tool exactly once",
			ginkgo.SpecTimeout(60*time.Second), func(_ ginkgo.SpecContext) {
				ginkgo.Skip("live SIGKILL+restart+HTTP-approve needs a shared-store second-spawn and an HTTP approve client " +
					"the current harness does not expose; the offline two-Build gate TestApproveAfterRestartE2E " +
					"(internal/app/) is the CI-green proof of the identical resume-from-awaiting path.")
			})
	})
}
