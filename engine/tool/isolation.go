package tool

import "context"

// WorkspaceForker is the workspace-isolation seam for fork-join parallelism
// (harness pattern 8). It produces an isolated CHILD Workspace derived from a
// base Workspace so a forked agent loop can read — and, when its catalog allows
// it, WRITE — without racing on, or mutating, the shared base tree.
//
// It lives here in engine/tool, next to Workspace, for the same layering
// reason Workspace does: port already imports tool, so a separate package would
// risk the port↔tool import cycle. The interface is additive — no frozen domain
// type changes.
//
// Implementations may isolate via a git worktree, a recursive directory copy, or
// an overlay; the agent never knows (or cares) which. The contract is only:
//
//   - the returned child is a fully usable Workspace rooted at an isolated path;
//   - writes through the child do NOT affect the base tree;
//   - cleanup tears the child down (removes the worktree/copy) and is safe to
//     call exactly once after the child is no longer in use.
type WorkspaceForker interface {
	// Fork creates an isolated child workspace derived from base, returning the
	// child workspace and a cleanup func. label is a short, human-meaningful tag
	// (e.g. the branch idea or task name) implementations MAY fold into the child
	// path or worktree branch for observability; it need not be unique and is not
	// load-bearing. Implementations may use git worktrees, a copy, or an overlay;
	// the agent never knows which. The returned cleanup is always non-nil when err
	// is nil and removes the child's backing storage.
	Fork(ctx context.Context, base Workspace, label string) (child Workspace, cleanup func() error, err error)
}
