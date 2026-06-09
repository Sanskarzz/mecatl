// Package forker implements the default tool.WorkspaceForker used by fork-join
// parallelism (harness pattern 8). It isolates a forked child agent loop from
// the shared base working tree so parallel children cannot race on, or mutate,
// the base.
//
// Isolation strategy (chosen per base at Fork time):
//
//   - Git worktree — when the base workspace root is inside a git repository,
//     Fork runs `git worktree add --detach <child> HEAD` so the child gets a
//     real, independent checkout of HEAD that shares the object store but has its
//     own index and working tree. cleanup runs `git worktree remove --force` and
//     deletes the directory. This is the cheapest correct isolation for a repo:
//     it copies no file contents.
//
//   - Recursive copy — when the base root is NOT a git repo (no `.git`), OR when
//     the Forker was constructed WithForceCopy, Fork recursively copies the whole
//     base tree into a fresh temp directory. cleanup removes that directory. This
//     is a full, independent copy: writes in the child never touch the base. When
//     the base IS a git repo, the copy INCLUDES its `.git` directory, so the fork
//     is an independent repository with its OWN object database and refs.
//
// Isolation guarantees: a child Workspace returned by Fork is rooted at an
// isolated directory; Write/Edit/Bash through the child affect ONLY that
// directory. The base tree is never written. cleanup is idempotent-friendly (it
// tolerates an already-removed child) and must be called when the child is done.
//
// Worktree vs. full-copy isolation — the tradeoff:
//
// The git-worktree path isolates the WORKING TREE and INDEX but SHARES the object
// database and refs. Forked children CAN mutate their working tree: Edit/Write land
// in the fork, and Bash is workspace-aware (its CommandRunner runs with the forked
// child's Workspace.Root() as the working directory; see app.buildParallelChildEngine /
// buildMemberEngine and internal/adapter/tools/bash.go), so a child's Bash — and any
// git it runs — defaults to the fork's working tree, not the parent base. But in a
// worktree, a child that runs `git commit` / `git push` / `git update-ref` via Bash
// writes objects and refs into the SHARED `.git`, escaping isolation. That is the
// inherent git-worktree model.
//
// To close that gap for MUTATING forks, construct the Forker WithForceCopy: it
// forces the recursive-copy path even for a git repo and copies the `.git`
// directory along with the tree, so the fork is a SELF-CONTAINED repository. A
// branch's git/Bash writes (commits, refs, objects) then stay inside the fork and
// CANNOT reach the base repo. The composition root wires WithForceCopy for the Fork
// tool's branches and for mutating team members (see internal/app/build.go).
//
// The DEFAULT (no option) — the cheap auto worktree-vs-copy behaviour — is now used
// DELIBERATELY for READ-ONLY callers that nonetheless need a shell, specifically
// read-only team members (see internal/app.buildTeamWiring): a worktree SHARES the
// base repo's `.git`, so the member gets the full commit history for `git log`/`git
// show` inspection at near-zero cost, while its own working tree + index keep its
// (non-mutating) Bash from disturbing the base working tree. Because a worktree shares
// `.git/config` + `.git/hooks`, the composition root runs such a member's Bash through
// a SANDBOXED command runner that neutralises git config-driven code execution
// (core.pager / core.hooksPath / core.fsmonitor / external diff); see
// internal/app.buildSandboxedCommandRunner.
//
// The forker's OWN git invocations are hardened the same way. `git worktree add`
// fires the base repo's post-checkout hook, and even `git rev-parse` honours
// core.pager / external diff — all at FORK time, BEFORE the sandboxed member runner
// exists. So runGit/gitRepoRoot set cmd.Env = gitenv.Scrub(os.Environ()), the SAME
// neutralizing environment the member runner uses (the single shared source in
// internal/adapter/gitenv keeps the two from drifting): inherited GIT_* danger is
// dropped and hooks/pager/fsmonitor/external-diff are force-neutralised.
//
// Cost: a full copy (including `.git`, which for an established repo is often the
// bulk of the bytes) is HEAVIER than a worktree, which copies no file contents.
// That is exactly why worktree is the default. For mutating branches the stronger
// isolation is worth the extra copy.
//
// The copy path is bounded only by available disk and the size of the base tree; it
// copies regular files and directories and SKIPS symlinks (so a symlink cannot
// smuggle the copy outside the base). Neither path auto-merges results back — see
// the ParallelTool docs (no-auto-merge boundary).
package forker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/stacklok/mecatl/internal/adapter/gitenv"
	"github.com/stacklok/mecatl/internal/tool"
)

// childRoot is the constructor the forker uses to build a child tool.Workspace
// over an isolated directory. It is injected so this adapter does not import the
// osfs adapter directly (avoiding an adapter→adapter dependency) and so tests can
// substitute a workspace constructor. The composition root passes osfs.NewWorkspace.
type childRoot func(root string) (tool.Workspace, error)

// Forker is the default tool.WorkspaceForker. It picks git-worktree isolation
// when the base root is a git repo and falls back to a recursive directory copy
// otherwise. It is safe for concurrent use: Fork holds no per-call state on the
// Forker, and each call derives a uniquely-named child directory.
type Forker struct {
	// newWorkspace builds a child tool.Workspace over an isolated directory.
	newWorkspace childRoot
	// tmpBase is the parent directory under which child directories are created.
	// Empty means os.MkdirTemp's default (os.TempDir()).
	tmpBase string
	// runGit executes a git subcommand in dir; injected so tests can avoid git.
	runGit func(ctx context.Context, dir string, args ...string) error
	// forceCopy, when true, makes Fork always take the recursive-copy path (copying
	// .git too) instead of the git-worktree path, even for a git repo — giving the
	// fork its OWN object DB/refs so a child's git/Bash writes stay inside the fork.
	forceCopy bool
	// seq disambiguates concurrently-created child directories for the same label.
	seq atomic.Uint64
}

// Option configures a Forker.
type Option func(*Forker)

// WithTempBase sets the parent directory under which isolated child directories
// are created (default: the OS temp dir). Useful to keep forks on the same
// filesystem as the base for cheaper copies, or to scope them to a test dir.
func WithTempBase(dir string) Option {
	return func(f *Forker) { f.tmpBase = dir }
}

// WithForceCopy forces FULL isolation: Fork always takes the recursive-copy path
// (copying the base tree INCLUDING its `.git` when present) instead of a git
// worktree, even when the base is a git repo. The fork is then a self-contained
// repository with its own object database and refs, so a child branch's git/Bash
// writes (commits, refs, objects, working-tree edits) CANNOT reach the base repo.
//
// This is the mode for MUTATING forks (the Parallel tool's branches and mutating team
// members), where isolation matters more than speed: a full copy — `.git` and all
// — is heavier than a worktree (which copies no file contents), which is why the
// default leaves the cheaper auto worktree-vs-copy behaviour in place. Symlinks are
// still skipped, so the copy cannot be smuggled outside the base.
func WithForceCopy() Option {
	return func(f *Forker) { f.forceCopy = true }
}

// New constructs the default Forker. newWorkspace builds a child tool.Workspace
// over an isolated directory (the composition root passes osfs.NewWorkspace);
// it must be non-nil.
func New(newWorkspace func(root string) (tool.Workspace, error), opts ...Option) *Forker {
	if newWorkspace == nil {
		panic("forker: New requires a non-nil newWorkspace constructor")
	}
	f := &Forker{
		newWorkspace: newWorkspace,
		runGit:       runGit,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Compile-time assertion that Forker satisfies the seam.
var _ tool.WorkspaceForker = (*Forker)(nil)

// Fork creates an isolated child workspace derived from base. It uses a git
// worktree when base's root is a git repo, else a recursive copy. The returned
// cleanup removes the child's backing storage (worktree or copy).
func (f *Forker) Fork(ctx context.Context, base tool.Workspace, label string) (tool.Workspace, func() error, error) {
	if base == nil {
		return nil, nil, errors.New("forker: Fork requires a non-nil base workspace")
	}
	baseRoot := base.Root()
	if baseRoot == "" {
		return nil, nil, errors.New("forker: base workspace has no root")
	}

	childDir, err := f.childDir(label)
	if err != nil {
		return nil, nil, err
	}

	// Force-copy mode: always take the full recursive copy (including .git), giving
	// the fork its own object DB/refs. This is the MUTATING-fork isolation mode — a
	// child's git/Bash writes can never reach the base repo.
	if f.forceCopy {
		return f.forkCopyInto(baseRoot, childDir)
	}

	repoRoot, isRepo := gitRepoRoot(ctx, baseRoot)
	if isRepo {
		ws, cleanup, ferr := f.forkWorktree(ctx, repoRoot, childDir)
		if ferr != nil {
			// Worktree creation failed (e.g. dirty/odd repo state): fall back to a
			// copy so a fork never hard-fails just because git refused.
			_ = os.RemoveAll(childDir)
			return f.forkCopy(baseRoot, label)
		}
		return ws, cleanup, nil
	}
	return f.forkCopyInto(baseRoot, childDir)
}

// childDir reserves (creates) a uniquely-named, empty directory for a child fork,
// folding a sanitized label in for observability.
func (f *Forker) childDir(label string) (string, error) {
	n := f.seq.Add(1)
	pattern := fmt.Sprintf("mecatlfork-%s-%d-*", sanitizeLabel(label), n)
	dir, err := os.MkdirTemp(f.tmpBase, pattern)
	if err != nil {
		return "", fmt.Errorf("forker: create child dir: %w", err)
	}
	return dir, nil
}

// forkWorktree adds a detached git worktree at childDir pointing at repoRoot's
// HEAD. git refuses to create a worktree at an existing non-empty directory, so
// childDir (created empty by childDir) is removed first and recreated by git.
func (f *Forker) forkWorktree(ctx context.Context, repoRoot, childDir string) (tool.Workspace, func() error, error) {
	// git worktree add wants to create the directory itself.
	if err := os.RemoveAll(childDir); err != nil {
		return nil, nil, fmt.Errorf("forker: prepare worktree dir: %w", err)
	}
	if err := f.runGit(ctx, repoRoot, "worktree", "add", "--detach", childDir, "HEAD"); err != nil {
		return nil, nil, fmt.Errorf("forker: git worktree add: %w", err)
	}
	ws, err := f.newWorkspace(childDir)
	if err != nil {
		_ = f.runGit(ctx, repoRoot, "worktree", "remove", "--force", childDir)
		return nil, nil, fmt.Errorf("forker: open child workspace: %w", err)
	}
	cleanup := func() error {
		// Detached context: cleanup must run even if the fork's ctx was cancelled.
		rmErr := f.runGit(context.Background(), repoRoot, "worktree", "remove", "--force", childDir)
		// Best-effort directory removal in case git left anything (or already gone).
		_ = os.RemoveAll(childDir)
		// Prune any dangling administrative entry.
		_ = f.runGit(context.Background(), repoRoot, "worktree", "prune")
		return rmErr
	}
	return ws, cleanup, nil
}

// forkCopy creates a fresh child directory and recursively copies base into it.
func (f *Forker) forkCopy(baseRoot, label string) (tool.Workspace, func() error, error) {
	childDir, err := f.childDir(label)
	if err != nil {
		return nil, nil, err
	}
	return f.forkCopyInto(baseRoot, childDir)
}

// forkCopyInto recursively copies baseRoot's tree into the (already-created,
// empty) childDir and opens a child workspace there.
func (f *Forker) forkCopyInto(baseRoot, childDir string) (tool.Workspace, func() error, error) {
	if err := copyTree(baseRoot, childDir); err != nil {
		_ = os.RemoveAll(childDir)
		return nil, nil, fmt.Errorf("forker: copy base tree: %w", err)
	}
	ws, err := f.newWorkspace(childDir)
	if err != nil {
		_ = os.RemoveAll(childDir)
		return nil, nil, fmt.Errorf("forker: open child workspace: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(childDir) }
	return ws, cleanup, nil
}

// gitRepoRoot reports whether dir is inside a git work tree and, if so, the
// absolute top-level directory. A git failure (git absent, not a repo) reports
// (",", false) and the caller falls back to a copy.
func gitRepoRoot(ctx context.Context, dir string) (string, bool) {
	// Use a capturing exec directly (the injected runner is fire-and-forget); a
	// failure simply means "not a repo / no git" and triggers the copy fallback.
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	// Even this read-only probe runs git config-aware; scrub the env so a shared
	// `.git/config` (core.pager/external diff/etc.) can never drive code here.
	cmd.Env = gitenv.Scrub(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", false
	}
	return root, true
}

// runGit executes a git subcommand in dir, returning a descriptive error on a
// non-zero exit (folding in stderr).
func runGit(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	// CRITICAL: scrub the env BEFORE any git invocation. `git worktree add` fires
	// the base repo's post-checkout hook, and other subcommands honour core.pager /
	// external diff — all at FORK time, before the sandboxed member runner exists.
	// gitenv.Scrub (shared with buildSandboxedCommandRunner) neutralises hooks,
	// pager, fsmonitor and external diff and drops inherited GIT_* danger.
	cmd.Env = gitenv.Scrub(os.Environ())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// copyTree recursively copies the directory tree rooted at src into dst (which
// must already exist). It copies regular files and directories, preserves
// permission bits, and deliberately SKIPS symlinks. The repo's `.git` admin dir is
// copied like any other directory: on the auto path it is only reached for a
// non-repo (where there is no `.git`), but on the WithForceCopy path the base IS a
// repo and copying `.git` is the POINT — it makes the fork a self-contained repo
// with its own object DB/refs, so a child's git writes stay inside the fork.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			// Skip symlinks: they could point outside the base, and copying their
			// target would break isolation.
			return nil
		case d.IsDir():
			if rel == "." {
				return nil // dst already exists
			}
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type().IsRegular():
			return copyFile(p, target)
		default:
			// Skip irregular files (devices, sockets, pipes).
			return nil
		}
	})
}

// copyFile copies a single regular file from src to dst, preserving its mode.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // src is a tree entry under the base root
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(dst), 0o700); mkErr != nil {
		return mkErr
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // dst is under the freshly-created child dir
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// sanitizeLabel reduces an arbitrary label to a short, filesystem-safe token for
// use in a child directory name. Empty or all-stripped labels become "fork".
func sanitizeLabel(label string) string {
	const maxLen = 24
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= maxLen {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "fork"
	}
	return s
}
