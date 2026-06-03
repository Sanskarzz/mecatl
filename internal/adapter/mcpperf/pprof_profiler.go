package mcpperf

import (
	"bytes"
	"errors"
	"fmt"
	"runtime/pprof"
	"time"
)

// defaultProfiler is the production Profiler: it wraps runtime/pprof. Named
// profiles are written via pprof.Lookup(name).WriteTo(buf, 0) (gzipped protobuf,
// parseable by google/pprof); the CPU profile uses StartCPUProfile/StopCPUProfile
// over a buffer. It holds no state — runtime/pprof owns the global profile state,
// and the cpuGate (not this type) enforces the one-CPU-profile-at-a-time policy.
type defaultProfiler struct{}

// NewProfiler returns the production Profiler backed by runtime/pprof. Inject it
// into Deps.Profiler in the composition root; tests use a fake instead.
func NewProfiler() Profiler { return defaultProfiler{} }

// knownNamedProfiles is the allowlist of runtime/pprof named profiles this server
// will look up. It bounds the pprof://pprof/{profile} resource template and the
// allocation tool to profiles that are safe and meaningful to summarize, and
// keeps an arbitrary client-supplied name from reaching pprof.Lookup.
var knownNamedProfiles = map[string]struct{}{
	"heap":      {},
	"goroutine": {},
	"allocs":    {},
	"mutex":     {},
	"block":     {},
}

// isKnownNamedProfile reports whether name is one of the allowlisted named
// profiles. CPU is deliberately excluded — it is not a Lookup profile.
func isKnownNamedProfile(name string) bool {
	_, ok := knownNamedProfiles[name]
	return ok
}

// Lookup writes the named runtime/pprof profile to a buffer and returns its
// bytes. It rejects any name not on the allowlist before touching pprof.Lookup.
func (defaultProfiler) Lookup(name string) ([]byte, error) {
	if !isKnownNamedProfile(name) {
		return nil, fmt.Errorf("mcpperf: unknown profile %q", name)
	}
	p := pprof.Lookup(name)
	if p == nil {
		return nil, fmt.Errorf("mcpperf: profile %q not available on this runtime", name)
	}
	var buf bytes.Buffer
	if err := p.WriteTo(&buf, 0); err != nil {
		return nil, fmt.Errorf("mcpperf: write profile %q: %w", name, err)
	}
	return buf.Bytes(), nil
}

// CPUProfile runs a CPU profile for d and returns its pprof bytes. It blocks for
// the duration. The caller (cpuGate) guarantees no concurrent CPU profile, but
// StartCPUProfile is still checked: if the runtime reports one already active
// (e.g. an external /debug/pprof/profile in flight), it returns an error rather
// than silently producing an empty profile.
func (defaultProfiler) CPUProfile(d time.Duration) ([]byte, error) {
	if d <= 0 {
		return nil, errors.New("mcpperf: cpu profile duration must be positive")
	}
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		return nil, fmt.Errorf("mcpperf: start cpu profile: %w", err)
	}
	time.Sleep(d)
	pprof.StopCPUProfile()
	return buf.Bytes(), nil
}
