package port

import (
	"context"

	"github.com/stacklok/ozzharness/internal/governance"
)

// HookRunner executes a lifecycle hook for a HookEvent and returns its outcome.
// The shell-exec adapter maps process exit code 0 to allow and exit code 2 to a
// blocking outcome.
type HookRunner interface {
	// Run executes the hook(s) registered for ev.Phase and returns the outcome.
	Run(ctx context.Context, ev governance.HookEvent) (governance.HookOutcome, error)
}
