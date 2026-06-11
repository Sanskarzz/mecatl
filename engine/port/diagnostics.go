package port

import "context"

// Level is the severity of a Diagnostics record. It is a small, provider-neutral
// set kept in the port package so domain/agent code can name a level without
// importing log/slog or any adapter. Adapters map it onto their backend's own
// level type (slogdiag maps it onto slog.Level).
type Level int

// The four diagnostic levels, low→high severity.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Diagnostics is the general-purpose operational logging seam: low-volume
// human-readable lines about what the harness is doing (composition decisions,
// degraded-mode warnings, lifecycle notes). It is DISTINCT from
// ToolCallRecorder (the per-tool audit seam) and from EventSink (the model's
// conversation stream). The agent loop and the composition layer write to it;
// domain packages do not take it (they stay silent).
//
// The contract is deliberately tiny and slog-shaped (a message plus alternating
// key/value args) so the obvious adapter is a thin wrapper over log/slog, but
// the port itself imports only context + stdlib so it never drags slog or an
// adapter inward. Callers that inject no sink get NopDiagnostics (Build defaults
// to it), so every consumer is nil-safe by construction.
type Diagnostics interface {
	// Log emits one record at level with msg and zero or more alternating
	// key/value args (slog-style). The ctx is a trace/baggage carrier only:
	// implementers MUST NOT derive cancellation or deadlines from it.
	Log(ctx context.Context, level Level, msg string, args ...any)
	// With returns a child Diagnostics that carries the supplied key/value args
	// bound onto every subsequent record, leaving the receiver unchanged.
	With(args ...any) Diagnostics
}

// NopDiagnostics is the no-op Diagnostics: it drops every record and returns
// itself from With. Its zero value is usable, so it is the safe default sink for
// callers (and child engines) that inject nothing.
type NopDiagnostics struct{}

// Log discards the record.
func (NopDiagnostics) Log(context.Context, Level, string, ...any) {}

// With returns the receiver unchanged (no bound attributes to carry).
func (n NopDiagnostics) With(...any) Diagnostics { return n }
