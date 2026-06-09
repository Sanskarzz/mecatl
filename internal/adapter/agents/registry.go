package agents

import (
	"context"
	"sort"
)

// Registry is an immutable, name-indexed view of the discovered agent
// definitions: the ONE registry both consumers (the Subagent tool now; the team
// member factory in a later slice) share. It is built once at composition time
// from a resolved Source and never mutated thereafter, so it is safe to read
// concurrently.
type Registry struct {
	byName map[string]AgentDef
	order  []string // def names, sorted, for deterministic iteration
}

// NewRegistry builds a Registry from the given defs (typically the output of a
// MultiSource). On a duplicate name the FIRST occurrence wins (callers pass
// already-deduped, precedence-ordered defs from MultiSource, so this is only a
// defensive backstop). Iteration order is by name for determinism.
func NewRegistry(defs []AgentDef) *Registry {
	r := &Registry{byName: make(map[string]AgentDef, len(defs))}
	for _, d := range defs {
		if _, exists := r.byName[d.Name]; exists {
			continue
		}
		r.byName[d.Name] = d
		r.order = append(r.order, d.Name)
	}
	sort.Strings(r.order)
	return r
}

// ResolveRegistry runs the source to completion and builds a Registry from the
// resulting defs. It is the convenience the composition root uses: resolve the
// MultiSource, surface diagnostics, build the registry in one step. The returned
// SkipErrors are the source's diagnostics (shadowed/truncated/skipped); a non-nil
// error is a hard fault that prevented discovery.
func ResolveRegistry(ctx context.Context, src AgentSource) (*Registry, []SkipError, error) {
	if src == nil {
		return NewRegistry(nil), nil, nil
	}
	defs, skips, err := src.Agents(ctx)
	if err != nil {
		return NewRegistry(nil), skips, err
	}
	return NewRegistry(defs), skips, nil
}

// Get returns the def registered under name and true, or a zero def and false.
func (r *Registry) Get(name string) (AgentDef, bool) {
	d, ok := r.byName[name]
	return d, ok
}

// List returns every def, ordered by name for determinism. The returned slice is
// a fresh copy; mutating it does not affect the Registry.
func (r *Registry) List() []AgentDef {
	out := make([]AgentDef, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Len reports how many defs the Registry holds.
func (r *Registry) Len() int { return len(r.order) }
