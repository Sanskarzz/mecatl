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
