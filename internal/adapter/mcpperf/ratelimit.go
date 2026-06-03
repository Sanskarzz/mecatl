package mcpperf

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// defaultCPUCooldown is the minimum spacing between two CPU-profiling captures.
// CPU profiling perturbs the whole process (it installs a SIGPROF timer), so the
// gate enforces at most one capture per window across BOTH capture_cpu_profile
// and top_cpu_functions. Chosen to keep an agent from pinning the profiler in a
// tight loop while still allowing a follow-up capture within a debugging session.
const defaultCPUCooldown = 30 * time.Second

// maxCPUProfileSeconds / minCPUProfileSeconds clamp the client-requested CPU
// profile duration server-side, so a request can neither be a no-op (0s) nor
// stall the process for minutes.
const (
	minCPUProfileSeconds = 1
	maxCPUProfileSeconds = 30
)

// cpuGate serializes and rate-limits the CPU-profiling tools. It combines:
//
//   - inFlight (atomic.Bool): at most ONE CPU profile may run at a time. A
//     concurrent attempt is rejected immediately, before the cooldown check, so a
//     second caller never blocks on the mutex for the whole profile duration.
//   - a cooldown (mutex-guarded lastCapture time): after a capture STARTS, the
//     next is refused until the cooldown elapses.
//
// The gate is shared by capture_cpu_profile and top_cpu_functions so the two
// cannot be alternated to bypass the limit. Read tools (resources, query_metric,
// list_slow_turns) never touch the gate — reads are unlimited.
type cpuGate struct {
	now      func() time.Time
	cooldown time.Duration

	inFlight atomic.Bool

	mu          sync.Mutex
	lastCapture time.Time
	hasCaptured bool
}

// newCPUGate constructs a gate with the default cooldown. now defaults to
// time.Now when nil.
func newCPUGate(now func() time.Time) *cpuGate {
	if now == nil {
		now = time.Now
	}
	return &cpuGate{now: now, cooldown: defaultCPUCooldown}
}

// acquireResult reports the outcome of an acquire attempt. When ok is false,
// reason is a model-facing, natural-language recovery sentence to surface as a
// tool error (isError:true), never a protocol error.
type acquireResult struct {
	ok      bool
	reason  string
	release func()
}

// acquire attempts to reserve the CPU profiler. On success it returns ok=true and
// a release func the caller MUST defer to clear the in-flight flag (the cooldown
// clock starts at acquire time, so back-to-back captures are spaced by the
// cooldown regardless of how long each profile runs). On failure it returns
// ok=false with a recovery sentence and a no-op release.
//
// Order matters: the in-flight check comes FIRST so a concurrent caller is
// rejected with the "already in progress" message rather than the cooldown
// message, and so it does not have to wait on the mutex.
func (g *cpuGate) acquire() acquireResult {
	if !g.inFlight.CompareAndSwap(false, true) {
		return acquireResult{
			ok:      false,
			reason:  "a CPU profile is already in progress; wait for it to finish before requesting another",
			release: func() {},
		}
	}

	g.mu.Lock()
	if g.hasCaptured {
		if elapsed := g.now().Sub(g.lastCapture); elapsed < g.cooldown {
			retryIn := g.cooldown - elapsed
			g.mu.Unlock()
			g.inFlight.Store(false)
			return acquireResult{
				ok: false,
				reason: fmt.Sprintf(
					"rate limit: at most one CPU profile per %s; retry in %s",
					g.cooldown.Round(time.Second), retryIn.Round(time.Second),
				),
				release: func() {},
			}
		}
	}
	// Start the cooldown clock at acquire time.
	g.lastCapture = g.now()
	g.hasCaptured = true
	g.mu.Unlock()

	var once sync.Once
	return acquireResult{
		ok:     true,
		reason: "",
		release: func() {
			once.Do(func() { g.inFlight.Store(false) })
		},
	}
}

// clampCPUSeconds clamps a client-requested CPU profile duration (in seconds) to
// [minCPUProfileSeconds, maxCPUProfileSeconds], applying the default of 5s when
// the request is zero/unset. It is the single server-side enforcement point for
// the duration bound.
func clampCPUSeconds(requested int) int {
	if requested <= 0 {
		return 5
	}
	if requested < minCPUProfileSeconds {
		return minCPUProfileSeconds
	}
	if requested > maxCPUProfileSeconds {
		return maxCPUProfileSeconds
	}
	return requested
}
