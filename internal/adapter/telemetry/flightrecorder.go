package telemetry

import (
	"bytes"
	"io"
	"runtime/trace"
	"sync"
	"sync/atomic"
	"time"
)

// Bounded defaults for the flight-recorder window. The recorder keeps a ring
// buffer of recent execution-trace events in memory; these bounds cap that
// overhead. MaxBytes takes precedence over MinAge (stdlib semantics): the
// window holds at most ~DefaultFlightRecorderMaxBytes of trace data, covering
// at least DefaultFlightRecorderMinAge of wall time when activity is light.
//
// Sized for "the last few seconds of activity, a few MB resident": low enough
// to be safe to arm by default (decision: --flight-recorder defaults ON), large
// enough that a snapshot taken right after a slow turn still contains it.
const (
	DefaultFlightRecorderMaxBytes uint64 = 8 << 20 // 8 MiB ring buffer
	DefaultFlightRecorderMinAge          = 5 * time.Second
)

// FlightRecorder wraps the stdlib runtime/trace.FlightRecorder (Go 1.26) with a
// bounded in-memory window. It continuously records execution-trace events into
// a ring buffer; Snapshot writes the current window out as a parseable trace
// (the trigger Phase-2's perf-over-MCP server calls on a tail-latency turn).
//
// SECURITY: an execution trace can embed goroutine stacks and timing that
// correlate to request data. Snapshots must only be served on the loopback
// admin surface — never the public service mux.
//
// The wrapper is concurrency-safe: Start/Stop are guarded so a double Start or a
// Stop-before-Start is a no-op rather than an error, which keeps the daemon
// startup/shutdown wiring simple and idempotent.
type FlightRecorder struct {
	mu      sync.Mutex
	fr      *trace.FlightRecorder
	started bool
}

// NewFlightRecorder constructs a recorder with the given config. A zero-value
// config field falls back to the bounded defaults above, so NewFlightRecorder
// (FlightRecorderConfig{}) is the recommended "sane bounded window" call.
func NewFlightRecorder(cfg trace.FlightRecorderConfig) *FlightRecorder {
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultFlightRecorderMaxBytes
	}
	if cfg.MinAge == 0 {
		cfg.MinAge = DefaultFlightRecorderMinAge
	}
	return &FlightRecorder{fr: trace.NewFlightRecorder(cfg)}
}

// Start begins recording. It is idempotent: a second Start on THIS instance
// while already running is a no-op returning nil. Only one flight recorder may
// be active process-wide (a stdlib constraint) — prefer ProcessFlightRecorder,
// which hands every caller the single shared instance and enforces that
// invariant, over arming a second distinct recorder directly.
func (f *FlightRecorder) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started {
		return nil
	}
	if err := f.fr.Start(); err != nil {
		return err
	}
	f.started = true
	return nil
}

// Stop ends recording and releases the ring buffer. It is idempotent: a Stop
// when not running is a no-op. Call it from the daemon's shutdown path so the
// recorder's background subscription is torn down (important for the upcoming
// goleak gate — a leaked recorder would otherwise hold runtime trace state).
func (f *FlightRecorder) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return
	}
	f.fr.Stop()
	f.started = false
}

// Enabled reports whether the recorder is currently running.
func (f *FlightRecorder) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

// Snapshot writes the current trace window to w and returns the byte count. It
// errors if the recorder is not running (nothing to snapshot) or if a
// concurrent Snapshot is already in progress (a stdlib WriteTo constraint).
func (f *FlightRecorder) Snapshot(w io.Writer) (int64, error) {
	f.mu.Lock()
	started := f.started
	fr := f.fr
	f.mu.Unlock()
	if !started {
		return 0, errNotStarted
	}
	// WriteTo runs outside the wrapper lock: it can be slow, and the stdlib
	// recorder guards its own concurrent-WriteTo case.
	return fr.WriteTo(w)
}

// SnapshotBytes returns the current trace window as a byte slice. It is the
// convenience the Phase-2 MCP tool and the Phase-1 /debug/flightrecorder
// endpoint use; the returned buffer begins with the Go execution-trace magic
// header and is parseable by golang.org/x/exp/trace.
func (f *FlightRecorder) SnapshotBytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := f.Snapshot(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// errNotStarted is returned by Snapshot when the recorder is not running.
var errNotStarted = errFlightRecorder("flight recorder not started")

// ErrFlightRecorderAlreadyActive is returned by ProcessFlightRecorder when an
// arming attempt is COALESCED onto an already-started shared recorder, or
// REJECTED because a distinct recorder already owns the single process-wide
// slot. It is exported so a second consumer (e.g. the embedded server in
// cmd/mecatui, decision 7) can recognise the loss with errors.Is and fall back
// to the existing instance rather than treating it as a fatal start failure. The
// returned *FlightRecorder (when non-nil) is still the live shared instance, so
// the caller can use it; the error signals only that the caller did not arm it.
var ErrFlightRecorderAlreadyActive = errFlightRecorder("flight recorder already active process-wide")

type errFlightRecorder string

func (e errFlightRecorder) Error() string { return string(e) }

// processFlightRecorder is the single process-wide FlightRecorder. The stdlib
// permits exactly one active flight recorder per process, so this enforces that
// as an invariant rather than leaving it to caller convention: the first
// ProcessFlightRecorder call constructs + arms it under processFROnce; every
// later call returns the SAME instance.
var (
	processFROnce sync.Once
	processFR     *FlightRecorder
	processFRErr  error
)

// ProcessFlightRecorder returns the single shared, started FlightRecorder for
// this process, constructing and arming it on the first call with the given
// config and returning the same instance on every later call. cfg is honoured
// ONLY on the first call (it constructs the singleton); a later call's cfg is
// ignored and the call is COALESCED — it returns the existing instance wrapped
// with ErrFlightRecorderAlreadyActive so the caller knows it did not own the
// arming. If the first arming itself failed (e.g. the slot was taken by a
// recorder created outside this accessor), every call returns that error.
//
// This is the wiring every composition root should use (cmd/mecated today, the
// cmd/mecatui embedded server next), so a second consumer cannot silently lose a
// Start against the one-recorder-per-process stdlib constraint.
//
// LIMITATION — the sync.Once is process-lifetime, not re-armable. The singleton
// is constructed and started exactly once; once its OWNER Stops it (the caller
// that received a nil error armed it and is the only one that should Stop it),
// it CANNOT be re-armed in the same process — a subsequent ProcessFlightRecorder
// returns the now-stopped instance, and Start() on it will fail because the
// stdlib slot has already been used and torn down. This is fine for the
// production wiring (one mecated, or one embedded server, per process, armed at
// startup and stopped at shutdown), but it is a hard constraint for a test
// binary or any future multi-embed: only ONE owner per process may arm-then-stop
// the recorder; later owners must coalesce (and must NOT Stop what they did not
// arm — see cmd/mecatui/embed's ownsRecorder gate).
func ProcessFlightRecorder(cfg trace.FlightRecorderConfig) (*FlightRecorder, error) {
	processFROnce.Do(func() {
		fr := NewFlightRecorder(cfg)
		if err := fr.Start(); err != nil {
			processFRErr = err
			return
		}
		processFR = fr
	})
	if processFRErr != nil {
		return nil, processFRErr
	}
	if processFRCoalesced.Swap(true) {
		// A prior caller already armed the singleton; this caller is coalesced.
		return processFR, ErrFlightRecorderAlreadyActive
	}
	return processFR, nil
}

// processFRCoalesced flips true once the singleton has been handed to its FIRST
// successful caller, so the SECOND and later callers are told (via
// ErrFlightRecorderAlreadyActive) that they were coalesced rather than that they
// armed the recorder. It is separate from processFROnce because Once.Do runs its
// body exactly once but does not itself distinguish first vs later callers.
var processFRCoalesced atomic.Bool
