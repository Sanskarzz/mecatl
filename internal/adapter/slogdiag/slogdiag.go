// Package slogdiag is an outbound adapter implementing port.Diagnostics over the
// standard library's log/slog. It is the production sink for the harness's
// general-purpose operational logging: composition decisions, degraded-mode
// warnings, lifecycle notes. The composition layer (cmd mains, internal/app)
// constructs one and injects it; the port keeps the agent loop and domain free of
// any slog dependency.
//
// The adapter owns the port.Level → slog.Level mapping and the text-vs-JSON
// handler choice, so callers pick a destination/format/min-level and otherwise
// speak only the neutral port.Diagnostics contract.
package slogdiag

import (
	"context"
	"io"
	"log/slog"

	"github.com/stacklok/mecatl/internal/port"
)

// Diagnostics is a port.Diagnostics backed by a *slog.Logger.
type Diagnostics struct {
	log *slog.Logger
}

// compile-time interface check.
var _ port.Diagnostics = (*Diagnostics)(nil)

// New constructs a Diagnostics writing to w. When jsonFormat is true it uses a
// slog JSON handler, otherwise a text handler. Records below minLevel are
// dropped by the handler. The composition layer decides these knobs.
func New(w io.Writer, jsonFormat bool, minLevel port.Level) *Diagnostics {
	opts := &slog.HandlerOptions{Level: toSlogLevel(minLevel)}
	var h slog.Handler
	if jsonFormat {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return &Diagnostics{log: slog.New(h)}
}

// NewText is the convenience constructor for the common case: human-readable
// text to w at the Info threshold (mecated's default operator posture).
func NewText(w io.Writer) *Diagnostics {
	return New(w, false, port.LevelInfo)
}

// NewFromLogger wraps an already-constructed *slog.Logger. It lets a caller that
// already owns a slog.Logger (e.g. one shared with another subsystem) expose it
// through the Diagnostics port without re-deriving a handler. A nil logger
// falls back to slog.Default so the result is always usable.
func NewFromLogger(l *slog.Logger) *Diagnostics {
	if l == nil {
		l = slog.Default()
	}
	return &Diagnostics{log: l}
}

// Log emits one record at level. The ctx is forwarded to slog's context-aware
// path (LogAttrs-style) so a handler may read trace context from it; the adapter
// never derives cancellation from ctx.
func (d *Diagnostics) Log(ctx context.Context, level port.Level, msg string, args ...any) {
	d.log.Log(ctx, toSlogLevel(level), msg, args...)
}

// With returns a child Diagnostics carrying args bound onto every record, leaving
// the receiver unchanged (slog.Logger.With returns a new logger).
func (d *Diagnostics) With(args ...any) port.Diagnostics {
	return &Diagnostics{log: d.log.With(args...)}
}

// toSlogLevel maps the neutral port.Level onto slog's level type.
func toSlogLevel(l port.Level) slog.Level {
	switch l {
	case port.LevelDebug:
		return slog.LevelDebug
	case port.LevelWarn:
		return slog.LevelWarn
	case port.LevelError:
		return slog.LevelError
	case port.LevelInfo:
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}
