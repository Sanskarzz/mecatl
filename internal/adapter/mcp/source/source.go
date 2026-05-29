// Package source is the pluggable EXTENSIBILITY POINT for WHERE the harness's
// MCP server configs come from. A Source yields a set of mcp.ServerConfig values
// together with non-fatal per-server diagnostics (SkipError), and a fatal error
// only for a genuine infrastructure fault that prevented the source from being
// consulted at all.
//
// LAYERING: this seam lives in this ADAPTER package, not the domain. Nothing in
// the domain or the agent loop consumes MCP server configs — they are connected
// and packaged into tools at composition time (cmd/mecated) — so a domain port
// would be the wrong home. The seam is scoped to where it is consumed (the
// composition root), mirroring skills.Source. The static --mcp-server list is one
// implementation (StaticSource); a live ToolHive workload inventory is another
// (ToolHiveSource); both satisfy this same interface and slot in without touching
// the consumer or the mcp.ServerConfig value object.
//
// THIS PACKAGE IS SDK-FREE except for toolhive.go, which is the ONLY file allowed
// to import the ToolHive Go library. Everything else here — the seam, the static
// source, the resolver, the inspection types — is pure mecatl + stdlib so the
// layering audit stays simple.
package source

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/stacklok/mecatl/internal/adapter/mcp"
)

// Source produces MCP server configs for the composition root to connect.
//
// Servers(ctx) returns:
//   - the server configs this source knows about (each a candidate connection),
//   - non-fatal per-server diagnostics (SkipError) for servers the source saw but
//     deliberately excluded (wrong transport, bad name, …),
//   - a non-nil error ONLY for a hard INFRA fault that prevented consultation
//     entirely. An ABSENT source (e.g. no container runtime, no configured
//     servers) is "zero servers", NOT an error — MCP is opt-in and fail-soft.
type Source interface {
	Servers(ctx context.Context) ([]mcp.ServerConfig, []SkipError, error)
	// Name identifies the source for diagnostics, e.g. "static",
	// "toolhive(default)", "toolhive(<group>)".
	Name() string
}

// SkipError records one non-fatal diagnostic: a server a Source saw but excluded,
// or a server dropped because a higher-precedence source already claimed its name
// (a SHADOW notice). It is collected and surfaced by the composition root; it is
// NEVER returned as a Source's fatal error.
type SkipError struct {
	// Server is the server name (or source label) the problem concerns.
	Server string
	// Reason is a short, human-readable description of the problem.
	Reason string
}

func (e SkipError) Error() string {
	return fmt.Sprintf("skipped MCP server %q: %s", e.Server, e.Reason)
}

// MultiSource composes an ORDERED list of Sources into one, with a defined
// precedence on name collisions and aggregated diagnostics. It is what makes
// "keep adding sources" easy: future sources implement Source and slot into the
// ordered list — the consumer (registerMCP) is unchanged.
//
// PRECEDENCE (collision rule): EARLIER sources win. When two sources both produce
// a server with the same ServerConfig.Name, the one from the earlier source is
// kept and the later one is SHADOWED — dropped, with a SkipError "shadowed by a
// higher-precedence source" notice so the operator can see it happened. Callers
// order the slice highest-precedence-first; the conventional resolver
// (ResolveSources) builds it as static (explicit --mcp-server) > ToolHive.
//
// Diagnostics from every source are concatenated in source order, with each
// shadow notice appended at the point the collision is detected. A fatal error
// from ANY source is returned immediately (with the diagnostics gathered so far)
// — a source that genuinely could not be consulted is a fault worth surfacing,
// unlike a merely-absent one which its own implementation reports as "no servers".
type MultiSource struct {
	sources []Source
}

// NewMultiSource builds a MultiSource over the given ordered sources
// (highest-precedence first). nil entries are ignored so callers can assemble the
// slice conditionally without sprinkling nil checks.
func NewMultiSource(sources ...Source) MultiSource {
	filtered := make([]Source, 0, len(sources))
	for _, s := range sources {
		if s != nil {
			filtered = append(filtered, s)
		}
	}
	return MultiSource{sources: filtered}
}

// Name reports the composite source label.
func (MultiSource) Name() string { return "multi" }

// Servers aggregates every composed source, applies the earlier-wins precedence
// on name collisions, and returns the merged configs sorted by name for
// deterministic output. Shadowed lower-precedence configs are dropped and
// reported.
func (m MultiSource) Servers(ctx context.Context) ([]mcp.ServerConfig, []SkipError, error) {
	var (
		out    []mcp.ServerConfig
		skips  []SkipError
		winner = map[string]string{} // server name -> source Name() that claimed it
	)
	for _, src := range m.sources {
		got, srcSkips, err := src.Servers(ctx)
		// Preserve each source's own diagnostics, in source order.
		skips = append(skips, srcSkips...)
		if err != nil {
			return nil, skips, err
		}
		for _, cfg := range got {
			if prevSrc, taken := winner[cfg.Name]; taken {
				// An earlier (higher-precedence) source already claimed this name. The
				// current, lower-precedence config is shadowed: drop it and note why.
				skips = append(skips, SkipError{
					Server: cfg.Name,
					Reason: fmt.Sprintf(
						"shadowed by a higher-precedence source (kept the one from %q, dropped the one from %q)",
						prevSrc, src.Name()),
				})
				continue
			}
			winner[cfg.Name] = src.Name()
			out = append(out, cfg)
		}
	}
	slices.SortFunc(out, func(a, b mcp.ServerConfig) int { return cmp.Compare(a.Name, b.Name) })
	return out, skips, nil
}
