// Package soul implements issue #14, Phase 1: a user-scoped, agent-READ-ONLY
// persona/"soul" fragment loaded into the turn-0 conversation as data.
//
// SCOPING: the soul is USER-scoped, not project-scoped. It lives at
// $XDG_CONFIG_HOME/mecatl/soul.md (fallback ~/.config/mecatl/soul.md) — OUTSIDE
// any session workspace root. It is therefore resolved against the process
// environment (the shared xdgconfig.ResolveEnv for path resolution, plus a local
// bounded-read seam), NOT through the per-session WorkspaceReader (which is rooted
// at the session workspace).
//
// SECURITY / "no write path" invariant: the agent loop can NEVER mutate the soul.
// This package exposes exactly ONE method — Load — and contains NO os.WriteFile/
// Create/MkdirAll and NO Catalog/tool registration. A writable identity anchor is
// the central trap the spike (docs/design/SOUL-SPIKE.md §4) warns against: a
// prompt injection that rewrites "who the agent is" would persist across every
// future session. So identity is read-only-if-present and bootstrapped by hand
// (a text editor), never by a tool. The soul is additionally injection-scanned
// (reusing skills.ScanForInjection) and byte-capped at load; any hit, an empty/
// whitespace-only/oversized/unreadable file all degrade to "" (fail-soft, no
// fragment) — never an error that aborts a run.
//
// BOUNDED READ (CWE-789): the file read is bounded to MaxBytes+1 bytes so a
// --soul-file pointed at /dev/zero or a multi-GB file cannot allocate unbounded at
// load time, and the cap is measured on RAW bytes read (before TrimSpace), so the
// "20 KiB byte cap" actually bounds the file, not just the trimmed string.
//
// Unlike memory.Store there is NO flock here: this is a read-only loader (no RMW,
// no temp-file rename), so there is no cross-process write to serialise. A
// concurrent hand-edit is harmless — the next Load re-reads the file.
package soul

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// DefaultMaxBytes is the load-time byte ceiling on the soul body (matching the
// Hermes 20 KiB cap the spike cites). Over the cap the soul is REJECTED (no
// fragment), not truncated: a half-truncated persona is worse than none.
const DefaultMaxBytes = 20 * 1024

// soulSubpath is the conventional soul file relative to the XDG config base, i.e.
// <config>/mecatl/soul.md (fallback ~/.config/mecatl/soul.md). Mirrors
// permconfig.userSubdirMecatl / skills.userSubdirMecatl path conventions.
const soulSubpath = "mecatl/soul.md"

// soulCloseTag is the data-fence close delimiter the prompt renderer wraps the
// body in (prompt.renderSoul uses "<soul>"/"</soul>"). A body containing this
// literal could close the fence early and let trailing text escape the data zone,
// so Load rejects any body that contains it. It is hardcoded here (with this
// comment) rather than imported from internal/prompt to avoid coupling the adapter
// to a domain magic-constant path; the two must stay in sync (one cheap string).
const soulCloseTag = "</soul>"

// readFunc opens a soul file for a bounded read and returns its content limited to
// at most limit bytes (the caller passes MaxBytes+1 to detect an over-cap file
// without reading the whole thing). The real binding (osRead) streams through an
// io.LimitReader so a pathological file cannot allocate unbounded; tests inject a
// fake that honours the same limit. A missing/unreadable file returns an error.
type readFunc func(path string, limit int64) ([]byte, error)

// osRead is the real bounded read: open the file and io.ReadAll a LimitReader so at
// most limit bytes are ever buffered.
func osRead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied (--soul-file) or the conventional user-config location; this is a read-only loader.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle; close error is irrelevant
	return io.ReadAll(io.LimitReader(f, limit))
}

// Options configures a Store.
type Options struct {
	// Path is an explicit soul file path (e.g. from --soul-file). When empty the
	// store resolves the conventional <xdg>/mecatl/soul.md (fallback
	// ~/.config/mecatl/soul.md). An explicit path overrides the conventional one.
	Path string
	// MaxBytes caps the loaded body; 0 uses DefaultMaxBytes. A body over the cap is
	// rejected (no fragment), not truncated. The cap bounds the RAW file read.
	MaxBytes int
}

// Store is a user-scoped, agent-read-only persona loader. It satisfies
// prompt.SoulSource. The zero value is not usable; construct with New.
type Store struct {
	path     string // explicit path; "" means resolve the conventional location
	maxBytes int
	env      xdgconfig.ResolveEnv // path resolution (Getenv/UserHomeDir) only
	read     readFunc             // bounded file read seam
}

// New constructs a Store from opts, binding the real process environment.
func New(opts Options) *Store {
	return newWith(opts, xdgconfig.OSEnv, osRead)
}

// newWith is New with an injectable environment + bounded-read seam, for tests.
func newWith(opts Options, env xdgconfig.ResolveEnv, read readFunc) *Store {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Store{path: opts.Path, maxBytes: maxBytes, env: env, read: read}
}

// resolvePath returns the soul file path: the explicit Path when set, else the
// conventional <xdg>/mecatl/soul.md (fallback ~/.config/mecatl/soul.md). It
// returns "" when no path can be resolved (no explicit path and neither
// XDG_CONFIG_HOME nor a home dir is available) — the caller then loads nothing.
func (s *Store) resolvePath() string {
	if s.path != "" {
		return s.path
	}
	if base := xdgconfig.UserConfigDir(s.env); base != "" {
		return filepath.Join(base, soulSubpath)
	}
	return ""
}

// Load reads, validates, and returns the soul body, or ("", nil) when there is no
// usable soul. It is the ONLY method on Store, and it FAILS SOFT at every branch:
// an unresolvable path, a missing/unreadable file, an over-cap file, an empty/
// whitespace-only body, a fence-breakout body, or an injection-scan hit all yield
// ("", nil) — never an error that aborts a run. Each branch logs at Debug.
//
// The read is BOUNDED to maxBytes+1 bytes and the cap is checked on RAW bytes
// (before TrimSpace), so an over-cap or pathological file is rejected without
// allocating its full contents.
//
// It satisfies prompt.SoulSource.
func (s *Store) Load(_ context.Context) (string, error) {
	path := s.resolvePath()
	if path == "" {
		slog.Debug("soul: no path could be resolved (no --soul-file and no XDG/home); no soul loaded")
		return "", nil
	}

	// Read at most maxBytes+1 RAW bytes: enough to DETECT an over-cap file without
	// buffering more than one byte past the ceiling.
	raw, err := s.read(path, int64(s.maxBytes)+1)
	if err != nil {
		// Missing file is the common case (soul is opt-in-by-presence); a genuine
		// read error is equally best-effort. Either way: no fragment, no abort.
		slog.Debug("soul: file not read; no soul loaded", "path", path, "err", err)
		return "", nil
	}

	if len(raw) > s.maxBytes {
		slog.Debug("soul: file over byte cap; rejected (not truncated)",
			"path", path, "bytes_read", len(raw), "max", s.maxBytes)
		return "", nil
	}

	body := strings.TrimSpace(string(raw))
	if body == "" {
		slog.Debug("soul: file empty or whitespace-only; no soul loaded", "path", path)
		return "", nil
	}

	if marker, found := scanForInjection(body); found {
		slog.Debug("soul: injection marker detected; rejected", "path", path, "marker", marker)
		return "", nil
	}

	// Fence-integrity guard: a body containing the literal close-tag could close the
	// data fence early and smuggle trailing text out of the data zone. Reject it.
	if strings.Contains(body, soulCloseTag) {
		slog.Debug("soul: body contains the data-fence close-tag; rejected", "path", path, "tag", soulCloseTag)
		return "", nil
	}

	return body, nil
}
