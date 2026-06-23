package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/osfs"
)

// fsCallTimeout bounds a single outbound fs/read_text_file / fs/write_text_file
// round-trip to the editor. A wedged or hung editor must not block the turn
// indefinitely (CWE-400): without a per-call deadline a Read/Write would hang
// until session/cancel or disconnect. 30s matches the operator-path MCP connect
// budget (the generous end) — a real fs/* round-trip is sub-millisecond, so this
// only fires on a genuinely unresponsive client. The caller's own ctx still
// applies; this is an UPPER bound layered on top of it.
const fsCallTimeout = 30 * time.Second

// fsWorkspace is the per-session tool.Workspace that routes file Read/Write
// through the ACP client's editor buffers (fs/read_text_file /
// fs/write_text_file) instead of touching disk directly. It is the heart of the
// fs/* delegation: an edit issued by the model lands in the editor's in-memory
// buffer (including unsaved changes) rather than overwriting the file on disk,
// so the agent's view of a file and the editor's view never diverge on the
// MUTATION path.
//
// It is a HYBRID, not a full reimplementation:
//
//   - Read / Write — DELEGATED through fs/* over the ACP connection.
//   - RecordRead / WasReadUnchanged — the Edit read-ledger, SYNTHESIZED locally
//     over the delegated reads: the fingerprint is the sha256 of the fs/read
//     content, so Edit's read-before-edit-and-unchanged invariant tracks the
//     editor's BUFFER, not disk (strictly better than osfs for an editor session).
//   - Root / Glob / Grep — COMPOSED from an osfs.Workspace rooted at the SAME
//     session cwd. ACP has no fs/list or fs/grep, so these read the local on-disk
//     tree. The residual: Grep/Glob see disk, not unsaved buffers. This is
//     acceptable — the Edit invariant forces a re-read-through-fs/* before any
//     edit, so the divergence is confined to search/discovery and never reaches
//     the mutation path. (Documented in docs/adr/0001-acp-adapter.md.)
//   - Stat — disk-primary BUT buffer-aware for EXISTENCE: when disk reports
//     not-exist it probes the editor via fs/read_text_file, so a file that exists
//     only as an unsaved buffer is reported as existing. This is load-bearing for
//     write integrity (see Stat) — otherwise the Write tool would treat an unsaved
//     buffer as a new file and clobber it with no read-before-overwrite check.
//
// It is registered per-session on the shared *server.Service via
// SetSessionWorkspace and evicted on editor disconnect; concurrent read-only
// dispatch may fire several Read (hence fs/read_text_file) calls at once, which
// the ACP Conn handles safely, and the local ledger has its OWN mutex.
type fsWorkspace struct {
	conn      *Conn
	sessionID string

	// local is an osfs.Workspace rooted at the same session cwd. It supplies
	// Root/Stat/Glob/Grep and the canonical root path; its OWN ledger is unused
	// (fsWorkspace carries a buffer-keyed ledger instead).
	local *osfs.Workspace

	// callTimeout bounds a single fs/* round-trip. It defaults to fsCallTimeout;
	// tests may shrink it to assert the bound fires against a non-responsive peer.
	callTimeout time.Duration

	mu     sync.Mutex
	ledger map[string]string // ledgerKey(path) -> sha256 of last fs/read content
}

// Compile-time assertion that fsWorkspace satisfies the tool.Workspace port.
var _ tool.Workspace = (*fsWorkspace)(nil)

// newFSWorkspace builds an fsWorkspace over conn for sessionID, composing an
// osfs.Workspace rooted at root for the local (Stat/Glob/Grep) view. It returns
// an error only if the osfs root cannot be opened (so a bad cwd fails loudly at
// session/new rather than silently falling back to disk).
func newFSWorkspace(conn *Conn, sessionID, root string) (*fsWorkspace, error) {
	local, err := osfs.NewWorkspace(root)
	if err != nil {
		return nil, fmt.Errorf("acp: fs workspace: %w", err)
	}
	return &fsWorkspace{
		conn:        conn,
		sessionID:   sessionID,
		local:       local,
		callTimeout: fsCallTimeout,
		ledger:      make(map[string]string),
	}, nil
}

// Root returns the absolute session root all paths are scoped to — the SAME root
// the composed osfs view uses, so local (Glob/Grep/Stat) and delegated
// (Read/Write) operations address the same files.
func (w *fsWorkspace) Root() string { return w.local.Root() }

// absPath confines a session-relative (or absolute in-root) path under the root
// and returns the ABSOLUTE path the ACP fs/* contract requires. The model is
// UNTRUSTED even though the editor is trusted, so escapes are rejected here,
// BEFORE the path is handed to the editor — never delegate an unvalidated
// "../../etc/passwd".
//
// Confinement guarantee (stated honestly — this is NOT os.Root-grade):
//  1. LEXICAL: reject any ".." that climbs out of the root after Clean. This is
//     the same lexical check osfs.resolvePath applies to relative paths.
//  2. SYMLINK (best-effort, on the on-disk tree): EvalSymlinks the deepest
//     EXISTING ancestor of the joined target and re-verify the resolved real path
//     is still within the EvalSymlinks-resolved Root(); reject if it escapes. This
//     defends against a model creating an in-workspace symlink (e.g. `ln -s
//     /etc/passwd evil` via Bash) and then reading/writing it — without this the
//     editor would receive "<root>/evil" and might follow it out of root.
//
// An ABSOLUTE path is accepted iff confineSymlinks confirms it resolves inside
// Root() (mirroring osfs.resolveInRoot, so the ACP and osfs workspaces treat
// absolute in-root paths identically). A relative path takes the lexical + symlink
// check. Unlike osfs (every op flows through *os.Root, which refuses symlink
// traversal at the kernel level), this is a best-effort filesystem-side
// re-confinement: it resolves the existing parent for a buffer-only/non-existent
// leaf, so a not-yet-created path is still checked against its real parent. The
// editor is a trusted-local process and owns final filesystem policy; this layer
// rejects the obviously-escaping shapes the untrusted model can construct.
func (w *fsWorkspace) absPath(path string) (string, error) {
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		// An absolute path is accepted iff it canonicalizes inside the workspace
		// root (mirroring osfs.resolveInRoot); an out-of-root absolute path
		// escapes and is rejected by confineSymlinks. No relative join happens.
		abs := filepath.Clean(path)
		if err := w.confineSymlinks(abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("acp: fs workspace: %q escapes the workspace root", path)
	}
	abs := filepath.Join(w.Root(), clean)
	if err := w.confineSymlinks(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// confineSymlinks re-verifies that abs, after resolving symlinks on the deepest
// existing ancestor, still lives within the EvalSymlinks-resolved Root(). For a
// not-yet-existing leaf (a buffer-only or brand-new file) it resolves the closest
// existing parent and re-appends the unresolved tail, so a symlinked PARENT
// component that escapes is still caught. A resolution fault on the parent (other
// than not-exist) fails safe (rejects).
func (w *fsWorkspace) confineSymlinks(abs string) error {
	root := w.Root() // already EvalSymlinks-resolved by osfs.NewWorkspace.
	// Find the deepest existing ancestor and resolve it.
	existing := abs
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			// An ambiguous stat error on an ancestor: fail safe (reject) rather than
			// delegate a path we cannot vet.
			return fmt.Errorf("acp: fs workspace: cannot verify %q: %w", abs, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			// Reached the filesystem root without finding an existing ancestor; this
			// should be impossible since Root() itself exists, but fail safe.
			return fmt.Errorf("acp: fs workspace: %q has no resolvable ancestor", abs)
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return fmt.Errorf("acp: fs workspace: resolving %q: %w", existing, err)
	}
	realPath := resolved
	if len(tail) > 0 {
		realPath = filepath.Join(append([]string{resolved}, tail...)...)
	}
	if realPath != root && !strings.HasPrefix(realPath, root+string(filepath.Separator)) {
		return fmt.Errorf("acp: fs workspace: %q resolves to %q, which escapes the workspace root", abs, realPath)
	}
	return nil
}

// Read returns the file's content by delegating to fs/read_text_file (the
// editor's buffer view). line/limit are omitted (whole-file); the Read tool does
// its own line slicing.
func (w *fsWorkspace) Read(ctx context.Context, path string) ([]byte, error) {
	abs, err := w.absPath(path)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, w.callTimeout)
	defer cancel()
	var resp fsReadTextFileResponse
	if err := w.conn.Call(callCtx, methodFSReadTextFile, fsReadTextFileRequest{
		SessionID: w.sessionID,
		Path:      abs,
	}, &resp); err != nil {
		return nil, fmt.Errorf("acp: fs/read_text_file %q: %w", path, err)
	}
	return []byte(resp.Content), nil
}

// Write replaces the file's content by delegating to fs/write_text_file, so the
// write lands in the editor's buffer. The response is empty.
func (w *fsWorkspace) Write(ctx context.Context, path string, data []byte) error {
	abs, err := w.absPath(path)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, w.callTimeout)
	defer cancel()
	if err := w.conn.Call(callCtx, methodFSWriteTextFile, fsWriteTextFileRequest{
		SessionID: w.sessionID,
		Path:      abs,
		Content:   string(data),
	}, nil); err != nil {
		return fmt.Errorf("acp: fs/write_text_file %q: %w", path, err)
	}
	return nil
}

// Stat returns metadata for path. Disk is PRIMARY (ACP has no fs/stat): a file
// present on disk is reported with full local metadata. But existence is ALSO
// buffer-aware — when disk reports not-exist, Stat probes the editor via
// fs/read_text_file, because a file may exist ONLY as an unsaved editor buffer
// (never yet written to disk). This is load-bearing for write-integrity: the
// Write tool uses a not-exist Stat to mean "new file, no read-before-overwrite
// required". Without the buffer probe, an unsaved buffer would look new and Write
// would clobber it through fs/write_text_file with NO unchanged-since check — the
// exact divergence this feature exists to prevent.
//
// Classification when disk says not-exist:
//   - fs/read succeeds            -> EXISTS (synthesized FileInfo) so Write's
//     existing-file gate (read-before-overwrite) engages.
//   - fs/read is a clean not-found -> ErrNotExist (genuinely new; Write allowed).
//   - fs/read fails ambiguously   -> FAIL SAFE: report EXISTS, forcing the
//     read-before-overwrite gate rather than allowing an unguarded write.
func (w *fsWorkspace) Stat(ctx context.Context, path string) (tool.FileInfo, error) {
	info, err := w.local.Stat(ctx, path)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		// A real stat fault (e.g. an escape rejection): surface it unchanged.
		return tool.FileInfo{}, err
	}
	// Disk says not-exist; consult the editor buffer.
	abs, aerr := w.absPath(path)
	if aerr != nil {
		// An escaping path: keep the original not-exist semantics (it was never a
		// delegatable path anyway). Return the disk error.
		return tool.FileInfo{}, err
	}
	content, readErr := w.bufferRead(ctx, abs, path)
	switch {
	case readErr == nil:
		// Buffer exists -> synthesize an EXISTS FileInfo. Name is the leaf; Size is
		// the buffer length; a regular-file mode and a zero modtime are sane stand-ins
		// (the Write tool only consults existence, not the metadata).
		return tool.FileInfo{
			Name:    filepath.Base(path),
			Size:    int64(len(content)),
			Mode:    0o644,
			ModTime: time.Time{},
			IsDir:   false,
		}, nil
	case isFSNotFound(readErr):
		// Editor confirms the file does not exist anywhere -> genuinely new.
		return tool.FileInfo{}, err
	default:
		// Ambiguous fs/read fault -> fail safe: report EXISTS so Write's
		// read-before-overwrite gate engages rather than permitting an unguarded write.
		return tool.FileInfo{
			Name:    filepath.Base(path),
			Mode:    0o644,
			ModTime: time.Time{},
			IsDir:   false,
		}, nil
	}
}

// bufferRead issues a single fs/read_text_file for an already-confined absolute
// path, returning the content or the raw call error (so the caller can classify
// it). It is the existence-probe used by Stat; it does NOT go through Read (which
// re-confines), so the caller MUST pass an abs that absPath already produced.
func (w *fsWorkspace) bufferRead(ctx context.Context, abs, path string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, w.callTimeout)
	defer cancel()
	var resp fsReadTextFileResponse
	if err := w.conn.Call(callCtx, methodFSReadTextFile, fsReadTextFileRequest{
		SessionID: w.sessionID,
		Path:      abs,
	}, &resp); err != nil {
		return "", fmt.Errorf("acp: fs/read_text_file %q: %w", path, err)
	}
	return resp.Content, nil
}

// isFSNotFound reports whether a fs/read_text_file error is a CLEAN "file does not
// exist" from the editor, as opposed to a transport/timeout/other fault. ACP does
// not define a canonical not-found JSON-RPC code for fs/read, and editors differ,
// so this is conservative: it matches only on unambiguous not-found markers in the
// error text. Anything it does not recognize is treated as ambiguous (NOT
// not-found) so the caller can fail safe to "exists". A context deadline/cancel is
// explicitly NOT not-found.
func isFSNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// The JSON-RPC error surfaces as *rpcError; fall back to the message text.
	msg := strings.ToLower(err.Error())
	var re *rpcError
	if errors.As(err, &re) {
		// LSP/ACP convention sometimes uses -32602 (invalid params) for a bad path,
		// but that is not reliably "not found", so we still gate on the message.
		msg = strings.ToLower(re.Message)
	}
	for _, marker := range []string{
		"no such file",
		"not found",
		"does not exist",
		"enoent",
		"cannot find",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// Glob returns local on-disk matches (ACP has no fs/list/glob). Residual: it does
// not see files that exist only as unsaved editor buffers.
func (w *fsWorkspace) Glob(ctx context.Context, pattern string) ([]string, error) {
	return w.local.Glob(ctx, pattern)
}

// Grep searches the local on-disk tree (ACP has no fs/grep). Residual: it sees
// disk content, not unsaved buffer content. Acceptable because the Edit invariant
// forces a re-read-through-fs/* before any mutation, so a stale grep hit can
// never become a stale EDIT.
func (w *fsWorkspace) Grep(ctx context.Context, pattern, pathGlob string) ([]tool.GrepMatch, error) {
	return w.local.Grep(ctx, pattern, pathGlob)
}

// ledgerKey normalizes a ledger path to its canonical absolute form so a file
// read by absolute path and then edited by relative path (or vice versa) matches
// in the ledger. It resolves through absPath; an in-root absolute path is left as
// its canonical absolute form (absPath canonicalizes BOTH relative and absolute
// in-root inputs to the SAME <root>/<rel> absolute form — the editor buffer
// address), a relative path is joined under Root() to that same absolute form,
// and an escaping path absPath rejects falls back to filepath.Clean's slash form
// (mirroring osfs.ledgerKey's fallback; these paths are never edited, so the key
// shape only needs both call sites to agree). The key is stable across the two
// cross-form call sites (RecordRead and WasReadUnchanged) because both apply the
// same normalization.
func (w *fsWorkspace) ledgerKey(path string) string {
	if abs, err := w.absPath(path); err == nil {
		return abs
	}
	return filepath.Clean(filepath.ToSlash(path))
}

// RecordRead stores the buffer-keyed fingerprint of path: it re-reads through
// fs/read_text_file and records the sha256 of the editor's buffer content. The
// caller-supplied version is ignored (the adapter computes its own authoritative
// fingerprint, exactly like osfs) — but here the authority is the BUFFER, not
// disk, so WasReadUnchanged compares against what the editor would actually
// overwrite. A delegation fault leaves the path unrecorded (so a later edit is
// refused as "not read", fail-safe). The ledger key is the canonical absolute
// form (see ledgerKey), so an absolute path and the equivalent relative path
// share one entry.
func (w *fsWorkspace) RecordRead(path string, version string) {
	key := w.ledgerKey(path)
	fp, err := w.fingerprint(context.Background(), path)
	if err != nil {
		// Best effort: fall back to the caller's token so an unchanged-comparison can
		// still be attempted; if even that is empty the file is simply unrecorded.
		fp = version
	}
	w.mu.Lock()
	w.ledger[key] = fp
	w.mu.Unlock()
}

// WasReadUnchanged reports whether path was recorded via RecordRead AND the
// editor's current buffer content still matches the recorded fingerprint. It
// returns false if never recorded or if the buffer changed (or the read now
// faults), mirroring osfs's "vanished file is changed, not an error" semantics.
// The lookup uses the same canonical ledger key as RecordRead, so a read by
// absolute path and a check by relative path (or the reverse) agree.
func (w *fsWorkspace) WasReadUnchanged(ctx context.Context, path string) (bool, error) {
	key := w.ledgerKey(path)
	w.mu.Lock()
	recorded, ok := w.ledger[key]
	w.mu.Unlock()
	if !ok {
		return false, nil
	}
	current, err := w.fingerprint(ctx, path)
	if err != nil {
		return false, nil
	}
	return current == recorded, nil
}

// fingerprint computes the sha256 of the editor's buffer content for path, via
// fs/read_text_file. The path is confined by Read's absPath before delegation.
func (w *fsWorkspace) fingerprint(ctx context.Context, path string) (string, error) {
	data, err := w.Read(ctx, path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
