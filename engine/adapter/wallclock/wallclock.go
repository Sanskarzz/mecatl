// Package wallclock is the production port.Clock: a stateless adapter over
// time.Now. Composition injects it into agent.Deps so the loop's latency
// instrumentation (turn duration, TTFT, inter-token gaps, tool queued/took)
// observes real wall time; engine tests inject fake clocks instead.
package wallclock

import (
	"time"

	"github.com/stacklok/mecatl/engine/port"
)

// Clock reads the real wall clock. The zero value is ready to use — it carries
// no state, so there is no constructor.
type Clock struct{}

var _ port.Clock = Clock{}

// Now returns the current wall time.
func (Clock) Now() time.Time { return time.Now() }
