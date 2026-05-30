package agents

import (
	"context"
	"fmt"
	"sort"
)

// AgentSource is the pluggable EXTENSIBILITY POINT for where agent definitions
// come from. A Source produces a set of AgentDef value objects together with
// non-fatal diagnostics (SkipError), and a fatal error only for a genuine
// infrastructure fault that prevented the source from being consulted at all.
//
// It is the verbatim shape of skills.Source: the local-OS-filesystem layout
// (DirSource) is just ONE implementation; an embedded default set or a remote
// registry would satisfy the same interface and slot in via MultiSource without
// touching the consumer or the AgentDef value object.
//
// Agents(ctx) returns:
//   - the discovered defs (deterministically ordered by the implementation),
//   - the non-fatal diagnostics (skipped/duplicate/truncated/shadowed entries),
//   - a non-nil error ONLY for a hard fault. An absent source (e.g. a missing
//     directory) is "no defs", not an error — agent defs are opt-in.
type AgentSource interface {
	Agents(ctx context.Context) ([]AgentDef, []SkipError, error)
}

// SkipError records one diagnostic from discovery: a def that could not be
// loaded (a fatal SKIP — the def is excluded), a non-fatal WARNING about a def
// that WAS kept (e.g. its description was truncated), or a def dropped because it
// was SHADOWED by a higher-precedence source. Discovery keeps scanning rather
// than aborting and returns the collected diagnostics so the composition root can
// surface them. It is never returned as a Source's fatal error.
type SkipError struct {
	// Path is the <name>.md (or directory) the problem was found at.
	Path string
	// Reason is a short, human-readable description of the problem.
	Reason string
}

func (e SkipError) Error() string {
	return fmt.Sprintf("skipped agent def at %q: %s", e.Path, e.Reason)
}

// MultiSource composes an ORDERED list of Sources into one, with a defined
// precedence on name collisions and aggregated diagnostics.
//
// PRECEDENCE (collision rule): EARLIER sources win. When two sources both produce
// a def with the same effective name, the one from the earlier source is kept and
// the later one is SHADOWED — dropped, with a SkipError notice. Callers order the
// slice highest-precedence-first; ResolveSources builds it as
// explicit > project > user.
//
// A fatal error from ANY source is returned immediately (with the diagnostics
// gathered so far). Output is sorted by name for deterministic, cache-stable
// ordering.
type MultiSource struct {
	sources []AgentSource
}

// NewMultiSource builds a MultiSource over the given ordered sources
// (highest-precedence first). nil entries are ignored so callers can assemble the
// slice conditionally without sprinkling nil checks.
func NewMultiSource(sources ...AgentSource) MultiSource {
	filtered := make([]AgentSource, 0, len(sources))
	for _, s := range sources {
		if s != nil {
			filtered = append(filtered, s)
		}
	}
	return MultiSource{sources: filtered}
}

// Agents aggregates every composed source, applies the earlier-wins precedence on
// name collisions, and returns the merged defs sorted by name. Shadowed
// lower-precedence defs are dropped and reported.
func (m MultiSource) Agents(ctx context.Context) ([]AgentDef, []SkipError, error) {
	var (
		out    []AgentDef
		skips  []SkipError
		winner = map[string]AgentDef{} // effective name -> the kept (higher-precedence) def
	)
	for _, src := range m.sources {
		got, srcSkips, err := src.Agents(ctx)
		skips = append(skips, srcSkips...)
		if err != nil {
			return nil, skips, err
		}
		for _, def := range got {
			if prev, taken := winner[def.Name]; taken {
				skips = append(skips, SkipError{
					Path: def.Path,
					Reason: fmt.Sprintf(
						"agent def %q shadowed by a higher-precedence source (kept %q)",
						def.Name, prev.Path),
				})
				continue
			}
			winner[def.Name] = def
			out = append(out, def)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, skips, nil
}
