package llmresilience

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs a goroutine-leak gate over the llmresilience package. The
// post-first-chunk idle watchdog (restSeq) runs each inner stream.Next() on a
// helper goroutine and, on a timeout, cancels the per-attempt context and DRAINS
// the pending helper so it exits. This gate proves that drain happens on every
// path — a watchdog goroutine that lingered after a stall (or a normal
// completion) would fail the suite.
//
// There is NO ignore list: the suite runs fully offline (fake providers, no real
// HTTP/network), so every goroutine the tests spawn is expected to unwind.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
