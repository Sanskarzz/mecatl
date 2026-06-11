//go:build goleakcheck

package agent_test

import (
	"testing"
	"time"
)

// TestLeakGateCatchesARealLeak is a PERMANENT, build-tagged liveness proof for
// the package's goleak gate (leakmain_test.go's TestMain). It deliberately
// spawns a goroutine that blocks in time.Sleep and is NEVER joined, so the
// goroutine is still alive when the test returns. If the goleak gate is intact,
// goleak.VerifyTestMain observes that lingering goroutine after all tests finish
// and FAILS the package (non-zero exit) — which is the PASS condition for this
// liveness check.
//
// This guards against silent gate degradation: a goleak version bump that
// changes behaviour, or someone adding a broad ignore that swallows real leaks,
// would make this stop failing and thereby surface the regression.
//
// It is kept OFF the normal build by the `goleakcheck` tag so the standard
// `task test` suite stays green. To run the liveness check (it is EXPECTED to
// fail with a goleak report; that failure is the proof the gate works):
//
//	go test -tags goleakcheck ./engine/agent/
//
// A passing (exit 0) run of that command means the gate has degraded and is no
// longer catching leaks.
func TestLeakGateCatchesARealLeak(t *testing.T) {
	go func() {
		// Blocks well past the suite's lifetime; never joined, so it is still
		// running when goleak inspects the goroutine set at TestMain teardown.
		time.Sleep(time.Hour)
	}()
}
