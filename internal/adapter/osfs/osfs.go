// Package osfs implements tool.FileSystem over the real operating-system
// filesystem and a tool.Workspace that scopes every path under a single session
// root. All Workspace paths are interpreted relative to the root; paths that
// resolve outside the root (via "..", absolute paths, or symlink-style escapes)
// are rejected as a correctness invariant.
//
// The Workspace also carries the per-session Edit read-ledger
// (RecordRead/WasReadUnchanged) backed by a sha256 content fingerprint, the seam
// WP7's Edit tool uses to enforce read-before-edit-and-unchanged.
package osfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stacklok/mecatl/internal/tool"
)

// ErrPathEscape is returned when a session-relative path resolves outside the
// Workspace root.
var ErrPathEscape = errors.New("osfs: path escapes workspace root")

// defaultCommandTimeout bounds CommandRunner.Run when the caller's context has
// no deadline of its own.
const defaultCommandTimeout = 30 * time.Second

// maxCommandOutput caps each of stdout/stderr captured by CommandRunner.Run.
const maxCommandOutput = 1 << 20 // 1 MiB

// FileSystem implements tool.FileSystem over the real OS filesystem, rooted at a
// session workspace directory. All paths passed to its methods are treated as
// relative to root and are validated against escape.
//
// Every file operation goes through an *os.Root opened on the workspace root,
// which refuses both lexical ".." escapes and symlink traversal that would leave
// the root. This closes the gap a purely lexical cleanPath left open: the model
// can create a symlink inside the workspace (via Bash `ln -s /etc/passwd evil`),
// and os.Root refuses to follow it out of the root.
type FileSystem struct {
	root string
	r    *os.Root
}

// NewFileSystem returns a FileSystem rooted at the given directory. The root is
// resolved to an absolute, symlink-evaluated path, created if missing, then
// opened as an *os.Root so all subsequent operations are confined to it.
func NewFileSystem(root string) (*FileSystem, error) {
	abs, err := resolveRoot(root)
	if err != nil {
		return nil, err
	}
	// os.OpenRoot requires the directory to exist; create it so the constructor
	// preserves its previous lenient behavior (Write created missing dirs).
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &FileSystem{root: abs, r: r}, nil
}

// Root returns the absolute workspace root.
func (f *FileSystem) Root() string { return f.root }

// rootRelative converts a session-relative path into the slash-cleaned form
// os.Root expects, rejecting absolute paths up front. Symlink and ".." escapes
// that survive this lexical check are caught by os.Root at operation time and
// mapped to ErrPathEscape via mapEscape.
func rootRelative(path string) (string, error) {
	if path == "" {
		return ".", nil
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathEscape, path)
	}
	return filepath.Clean(filepath.FromSlash(path)), nil
}

// mapEscape rewrites os.Root's escape/insecure-path errors to the package's
// ErrPathEscape sentinel, leaving ordinary errors (e.g. not-exist) untouched.
func mapEscape(path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("%w: %q", ErrPathEscape, path)
	}
	// os.Root reports traversal that would leave the root via a *PathError whose
	// message mentions escaping the root; detect it without depending on an
	// unexported error type.
	if strings.Contains(err.Error(), "path escapes from parent") {
		return fmt.Errorf("%w: %q", ErrPathEscape, path)
	}
	return err
}

// Read returns the entire contents of the file at the session-relative path.
// maxReadBytes caps how much a single Read will pull into memory. It is far
// larger than any realistic source file but guards against an OOM DoS from a
// pathologically large file in the workspace (a tool reads the whole file before
// the model-facing output is truncated).
const maxReadBytes = 64 << 20 // 64 MiB

func (f *FileSystem) Read(_ context.Context, path string) ([]byte, error) {
	rel, err := rootRelative(path)
	if err != nil {
		return nil, err
	}
	if info, statErr := f.r.Stat(rel); statErr == nil && info.Size() > maxReadBytes {
		return nil, fmt.Errorf("osfs: file %q is %d bytes, exceeds the %d-byte read limit", path, info.Size(), int64(maxReadBytes))
	}
	data, err := f.r.ReadFile(rel)
	if err != nil {
		return nil, mapEscape(path, err)
	}
	return data, nil
}

// Write replaces the contents of the file at the session-relative path, creating
// it and any parent directories if needed.
func (f *FileSystem) Write(_ context.Context, path string, data []byte) error {
	rel, err := rootRelative(path)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(rel); dir != "." {
		// An "already exists" error here is benign (the dir, or a symlink in its
		// place, is present); let WriteFile make the final escape decision so a
		// symlinked parent component surfaces as ErrPathEscape rather than being
		// masked by MkdirAll's "file exists".
		if err := f.r.MkdirAll(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return mapEscape(path, err)
		}
	}
	if err := f.r.WriteFile(rel, data, 0o644); err != nil {
		return mapEscape(path, err)
	}
	return nil
}

// Stat returns metadata for the file at the session-relative path.
func (f *FileSystem) Stat(_ context.Context, path string) (tool.FileInfo, error) {
	rel, err := rootRelative(path)
	if err != nil {
		return tool.FileInfo{}, err
	}
	fi, err := f.r.Stat(rel)
	if err != nil {
		return tool.FileInfo{}, mapEscape(path, err)
	}
	return toFileInfo(fi), nil
}

// Glob returns session-relative paths matching the shell-style pattern. The
// pattern itself is interpreted relative to the root; matches that resolve
// outside the root are discarded.
func (f *FileSystem) Glob(_ context.Context, pattern string) ([]string, error) {
	// Validate the pattern's non-magic root does not escape.
	absPattern := filepath.Join(f.root, filepath.FromSlash(pattern))
	matches, err := filepath.Glob(absPattern)
	if err != nil {
		return nil, err
	}
	rels := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := f.toRel(m)
		if err != nil {
			continue
		}
		// Confine matches the same way every other operation is confined: Lstat
		// THROUGH the os.Root. This drops both (a) leaf symlinks — a symlink
		// inside the root can still target a file outside it, and Glob must not
		// be a channel for following links out of the workspace — and (b) matches
		// reachable only via a symlinked intermediate directory component that
		// leaves the root, which a raw os.Lstat(m) on the literal match path
		// would NOT catch (filepath.Glob does not resolve such components, so the
		// match looks like an in-root regular file). os.Root refuses to traverse
		// an escaping component, so the Lstat errors and the match is dropped —
		// closing an out-of-root filename-enumeration leak.
		fi, lerr := f.r.Lstat(rel)
		if lerr != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		rels = append(rels, rel)
	}
	return rels, nil
}

// toRel converts an absolute path under root into a slash-separated
// session-relative path, rejecting anything outside root.
func (f *FileSystem) toRel(abs string) (string, error) {
	rel, err := filepath.Rel(f.root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrPathEscape
	}
	return filepath.ToSlash(rel), nil
}

// toFileInfo maps an fs.FileInfo onto the domain tool.FileInfo.
func toFileInfo(fi fs.FileInfo) tool.FileInfo {
	return tool.FileInfo{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Mode:    fi.Mode(),
		ModTime: fi.ModTime(),
		IsDir:   fi.IsDir(),
	}
}

// resolveRoot makes root absolute and evaluates symlinks where possible so that
// later escape checks compare canonical paths.
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// Root may not exist yet; clean the absolute form.
	return filepath.Clean(abs), nil
}

// ResolveRoot exposes the EXACT path canonicalization the Workspace uses to confine
// Write/Edit (abs + EvalSymlinks, falling back to a cleaned abs path when the path
// does not yet exist). Callers that reason about whether a directory is inside or
// outside a workspace root (e.g. the SkillDraft quarantine trust-boundary check in
// cmd/mecated) MUST canonicalize through this so their comparison matches the
// enforcement layer — using filepath.Abs alone diverges on a symlinked workspace
// and would let a dir validation believes is "outside" actually resolve inside the
// os.Root.
func ResolveRoot(path string) (string, error) { return resolveRoot(path) }

// Workspace is the session-scoped seam over the real OS filesystem. It composes
// a FileSystem, performs an in-Go recursive Grep, and carries the Edit
// read-ledger. Command execution is NOT part of the Workspace: it lives behind
// the separate CommandRunner type (see NewCommandRunner) so the harness can run
// without any shell at all.
type Workspace struct {
	fs *FileSystem

	mu     sync.Mutex
	ledger map[string]string // session-relative path -> recorded fingerprint
}

// NewWorkspace returns a Workspace rooted at the given directory. The root is
// created if it does not already exist (NewFileSystem creates it before opening
// the os.Root).
func NewWorkspace(root string) (*Workspace, error) {
	fsys, err := NewFileSystem(root)
	if err != nil {
		return nil, err
	}
	return &Workspace{fs: fsys, ledger: make(map[string]string)}, nil
}

// Compile-time assertion that Workspace satisfies the frozen port.
var _ tool.Workspace = (*Workspace)(nil)

// Root returns the absolute session root all paths are scoped to.
func (w *Workspace) Root() string { return w.fs.Root() }

// Read returns the contents of the file at the session-relative path.
func (w *Workspace) Read(ctx context.Context, path string) ([]byte, error) {
	return w.fs.Read(ctx, path)
}

// Write replaces the contents of the file at the session-relative path, creating
// it and parent directories if needed.
func (w *Workspace) Write(ctx context.Context, path string, data []byte) error {
	return w.fs.Write(ctx, path, data)
}

// Stat returns metadata for the file at the session-relative path.
func (w *Workspace) Stat(ctx context.Context, path string) (tool.FileInfo, error) {
	return w.fs.Stat(ctx, path)
}

// Glob returns session-relative paths matching the shell-style pattern.
func (w *Workspace) Glob(ctx context.Context, pattern string) ([]string, error) {
	return w.fs.Glob(ctx, pattern)
}

// Grep returns the matches of a regular expression across files selected by an
// optional path glob (relative to root). When pathGlob is empty, the whole tree
// under root is searched. Binary-looking files (those containing a NUL byte) are
// skipped. The search honors ctx cancellation.
func (w *Workspace) Grep(ctx context.Context, pattern, pathGlob string) ([]tool.GrepMatch, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("osfs: invalid grep pattern: %w", err)
	}

	var files []string
	if pathGlob == "" {
		files, err = w.walkAll(ctx)
	} else {
		files, err = w.fs.Glob(ctx, pathGlob)
	}
	if err != nil {
		return nil, err
	}

	var matches []tool.GrepMatch
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := w.fs.Read(ctx, rel)
		if err != nil {
			continue // unreadable / vanished file: skip
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary file
		}
		lineNo := 0
		for _, line := range strings.Split(string(data), "\n") {
			lineNo++
			if re.MatchString(line) {
				matches = append(matches, tool.GrepMatch{
					Path: rel,
					Line: lineNo,
					Text: line,
				})
			}
		}
	}
	return matches, nil
}

// walkAll returns every regular file under root as a session-relative path.
func (w *Workspace) walkAll(ctx context.Context) ([]string, error) {
	var out []string
	err := filepath.WalkDir(w.fs.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			return nil
		}
		// Skip symlinks entirely: WalkDir does not descend into them, but a
		// symlinked FILE could still point outside the root. Excluding them here
		// keeps Grep from surfacing out-of-root content (and Read would refuse
		// it anyway via os.Root).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := w.fs.toRel(p)
		if rerr != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CommandRunner runs shell commands via /bin/sh -c with a fixed working
// directory (the session root). It is the local implementation of
// tool.CommandRunner; a Workspace no longer runs commands itself, so a
// shell-less deployment simply omits this runner.
type CommandRunner struct {
	root  string
	shell string
	// env, when set, is the COMPLETE process environment for every Run (it REPLACES
	// the inherited os.Environ(), it does not augment it). It is nil for an
	// unhardened runner (the main session, which inherits the operator's full
	// environment unchanged). The team-member sandboxed runner populates it via
	// WithCommandEnvList with a fully-scrubbed-and-neutralised environment computed
	// in composition (internal/app.buildSandboxedCommandRunner via gitenv.Scrub):
	// inherited git danger is REMOVED, not merely overridden. osfs stays free of
	// git-specific knowledge — it just sets whatever complete env it is handed.
	env []string
}

// CommandRunnerOption configures a CommandRunner at construction.
type CommandRunnerOption func(*CommandRunner)

// WithCommandEnvList sets the COMPLETE process environment ("KEY=VALUE" entries)
// used for every Run, REPLACING the inherited os.Environ() rather than augmenting
// it. This is how the composition root hardens the team-member shell against a
// shared `.git`: it computes a fully scrubbed-and-neutralised environment (via
// gitenv.Scrub — inherited GIT_* danger REMOVED, not just overridden) and hands
// the complete list here. Because the option REPLACES the environment, removing an
// inherited variable (e.g. GIT_EXTERNAL_DIFF) is possible — an append-only option
// could not. A nil/empty list leaves the runner unhardened (the main-session
// default, which inherits os.Environ() unchanged). osfs holds no git knowledge: it
// just runs with whatever complete environment it is given.
func WithCommandEnvList(env []string) CommandRunnerOption {
	return func(r *CommandRunner) {
		r.env = append([]string(nil), env...)
	}
}

// NewCommandRunner returns a local tool.CommandRunner that executes commands via
// /bin/sh -c, rooted at dir as the working directory. dir is resolved to an
// absolute, symlink-evaluated path so the runner's cwd matches the Workspace
// root. Use this from the composition root only when a shell is desired; omit it
// (and the Bash tool) to run shell-less.
func NewCommandRunner(dir string) (tool.CommandRunner, error) {
	return NewCommandRunnerShell(dir, "/bin/sh")
}

// NewCommandRunnerShell is like NewCommandRunner but lets the caller pick the
// shell binary (e.g. "/bin/bash"). An empty shell is rejected: a shell-less
// deployment must omit the runner (and the Bash tool) entirely rather than
// construct a runner with no shell.
func NewCommandRunnerShell(dir, shell string, opts ...CommandRunnerOption) (tool.CommandRunner, error) {
	if shell == "" {
		return nil, errors.New("osfs: command runner requires a non-empty shell")
	}
	abs, err := resolveRoot(dir)
	if err != nil {
		return nil, err
	}
	r := &CommandRunner{root: abs, shell: shell}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Compile-time assertion that CommandRunner satisfies the runner port.
var _ tool.CommandRunner = (*CommandRunner)(nil)

// Run runs command via /bin/sh -c, capturing (and truncating) stdout/stderr and
// the exit code. The working directory is workdir (the session/fork Workspace
// root the Bash tool executes against); an EMPTY workdir falls back to the
// runner's configured root, so the runner is usable standalone. workdir is used
// as-is and is intentionally NOT confined to the runner's configured root: a
// forked child lives under an isolated temp base OUTSIDE that root, and running
// its Bash there (not in the shared parent base) is exactly what fork isolation
// requires. Cancellation and timeout are governed by ctx; when ctx has no
// deadline a default timeout is applied. A non-zero exit is reported via the
// returned CommandResult.ExitCode, not as an error.
func (r *CommandRunner) Run(ctx context.Context, command, workdir string) (tool.CommandResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCommandTimeout)
		defer cancel()
	}

	var stdout, stderr cappedBuffer
	stdout.cap = maxCommandOutput
	stderr.cap = maxCommandOutput

	dir := workdir
	if dir == "" {
		dir = r.root
	}
	cmd := exec.CommandContext(ctx, r.shell, "-c", command)
	cmd.Dir = dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A hardened (team-member) runner carries a COMPLETE, pre-scrubbed environment
	// (computed in composition via gitenv.Scrub: inherited GIT_* danger removed,
	// neutralising config appended); use it verbatim so removal of an inherited
	// variable actually takes effect. An unhardened runner has r.env == nil, so
	// cmd.Env stays nil and exec inherits os.Environ() unchanged, as before.
	if r.env != nil {
		cmd.Env = r.env
	}

	err := cmd.Run()
	res := tool.CommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}

	if cerr := ctx.Err(); cerr != nil {
		// Context cancellation/timeout is a harness-level failure.
		return res, cerr
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// RecordRead stores the current on-disk fingerprint of path under the session
// ledger. The version argument is accepted for interface conformance but the
// adapter computes and stores its own authoritative fingerprint so that
// WasReadUnchanged can compare against the live file.
func (w *Workspace) RecordRead(path string, version string) {
	fp, err := w.fingerprint(path)
	if err != nil {
		// Record the caller-supplied token as a best-effort fallback so a later
		// unchanged-comparison can still be attempted.
		fp = version
	}
	w.mu.Lock()
	w.ledger[path] = fp
	w.mu.Unlock()
}

// WasReadUnchanged reports whether path was previously recorded via RecordRead
// and its current on-disk fingerprint still equals the recorded one. It returns
// false if path was never read or if the file changed (or vanished) since.
func (w *Workspace) WasReadUnchanged(_ context.Context, path string) (bool, error) {
	w.mu.Lock()
	recorded, ok := w.ledger[path]
	w.mu.Unlock()
	if !ok {
		return false, nil
	}
	current, err := w.fingerprint(path)
	if err != nil {
		// File unreadable/removed since the read: treat as changed, not an error.
		return false, nil
	}
	return current == recorded, nil
}

// fingerprint computes a sha256-based content fingerprint for a session-relative
// path.
func (w *Workspace) fingerprint(path string) (string, error) {
	// Route through the os.Root-backed FileSystem.Read so a symlink that escapes
	// the workspace cannot be fingerprinted (and thus cannot be followed out of
	// root for the read-before-edit ledger).
	data, err := w.fs.Read(context.Background(), path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// cappedBuffer is a bytes.Buffer-like writer that stops accepting bytes once cap
// is reached, so command output is bounded.
type cappedBuffer struct {
	buf bytes.Buffer
	cap int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.cap - c.buf.Len()
	if remaining <= 0 {
		return len(p), nil // discard, but report full consumption
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }
