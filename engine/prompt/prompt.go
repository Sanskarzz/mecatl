package prompt

import "strings"

// Layered is the two-layer system prompt. The StablePrefix is byte-stable across
// turns for a given configuration (role, tone, tool inventory, safety) so the LLM
// adapter can cache it; the VolatileSuffix holds per-turn material (e.g. the
// <env> block) that must not invalidate the cached prefix. The adapter places a
// prompt-cache breakpoint at the boundary between the two.
type Layered struct {
	// StablePrefix is the cache-stable portion of the system prompt.
	StablePrefix string
	// VolatileSuffix is the per-turn portion appended after the prefix.
	VolatileSuffix string
}

// Render concatenates the stable prefix and volatile suffix into the full system
// prompt string. The two are joined with a blank line when both are non-empty;
// an empty component contributes nothing (and no separator).
func (l Layered) Render() string {
	switch {
	case l.StablePrefix == "":
		return l.VolatileSuffix
	case l.VolatileSuffix == "":
		return l.StablePrefix
	default:
		var b strings.Builder
		b.WriteString(l.StablePrefix)
		b.WriteString("\n\n")
		b.WriteString(l.VolatileSuffix)
		return b.String()
	}
}
