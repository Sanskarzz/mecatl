package port

import (
	"context"

	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/session"
)

// PermissionPolicy evaluates a tool call under a permission mode, resolving
// across merged scopes with deny → ask → allow precedence. Implementations live
// in the governance context (WP4).
type PermissionPolicy interface {
	// Evaluate returns the permission decision for tool call c under mode.
	Evaluate(ctx context.Context, mode session.PermissionMode, c session.ToolCall) governance.PermissionDecision
}
