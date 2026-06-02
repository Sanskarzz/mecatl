package prompt

import (
	"context"

	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// InstructionAssembler resolves the ordered set of project-instruction messages
// for a workspace, returned as user-role messages to be recorded once at the
// start of a run.
//
// It is the seam that makes scoped context assembly (pattern 2) pluggable: the
// loop consumes this interface instead of calling a discovery function directly,
// so a richer adapter (parent-directory walk, user/managed scopes, @imports) can
// be wired at the composition root without touching the loop. The default
// implementation, RootAssembler, reproduces the v1 root-only behaviour exactly.
type InstructionAssembler interface {
	// Assemble returns the instruction messages for ws, in the order they should
	// be recorded. A nil/empty slice means "no project instructions"; an error is
	// returned only for a genuine read fault (not a missing file).
	Assemble(ctx context.Context, ws tool.Workspace) ([]session.Message, error)
}

// RootAssembler is the default InstructionAssembler. It resolves project
// instructions from the workspace root only (AGENTS.md winning, CLAUDE.md as
// fallback) by delegating to DiscoverInstructions, so it is byte-for-byte
// identical to the v1 behaviour. The zero value is ready to use.
type RootAssembler struct{}

// Assemble implements InstructionAssembler over the workspace root, delegating to
// DiscoverInstructions.
func (RootAssembler) Assemble(ctx context.Context, ws tool.Workspace) ([]session.Message, error) {
	return DiscoverInstructions(ctx, ws)
}

// Compile-time assertion that RootAssembler satisfies the interface.
var _ InstructionAssembler = RootAssembler{}

// MultiAssembler composes several InstructionAssemblers, concatenating their
// messages in order. It lets the composition root layer turn-0 context — e.g.
// RootAssembler (AGENTS.md/CLAUDE.md) THEN MemoryIndexAssembler (the tier-0 memory
// index) — so the conversation opens with project instructions followed by the
// memory index, all as user-role messages recorded once at turn 0 (and so, by
// construction, AFTER the cache-stable system prefix — never in StablePrefix).
//
// A nil child is skipped. The first child to return an error aborts (so a genuine
// read fault still surfaces); children that fail soft (return nil, nil) simply
// contribute nothing.
type MultiAssembler struct {
	Assemblers []InstructionAssembler
}

// NewMultiAssembler builds a MultiAssembler from the given children (nil children
// are tolerated and skipped at Assemble time).
func NewMultiAssembler(assemblers ...InstructionAssembler) MultiAssembler {
	return MultiAssembler{Assemblers: assemblers}
}

// Assemble runs each child in order and concatenates their messages.
func (m MultiAssembler) Assemble(ctx context.Context, ws tool.Workspace) ([]session.Message, error) {
	var out []session.Message
	for _, a := range m.Assemblers {
		if a == nil {
			continue
		}
		msgs, err := a.Assemble(ctx, ws)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// Compile-time assertion that MultiAssembler satisfies the interface.
var _ InstructionAssembler = MultiAssembler{}
