package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartGoroutineWatchdogWarnsAboveThreshold asserts the watchdog logs a Warn
// when the sampled count exceeds the threshold, and that the emitted record
// carries the structured attributes (goroutines=<n>, threshold=<t>) — so a
// regression that drops or swaps the structured fields is caught, not just the
// message substring. It uses a JSON handler so the payload can be parsed.
func TestStartGoroutineWatchdogWarnsAboveThreshold(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&safeWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		sampled   = 50
		threshold = 10
	)
	count := func() int { return sampled }
	StartGoroutineWatchdog(ctx, threshold, time.Millisecond, count, logger)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if strings.Contains(got, "possible goroutine leak") {
			assertWarnPayload(t, got, sampled, threshold)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("watchdog never warned; log buffer: %q", buf.String())
}

// assertWarnPayload parses the first JSON log line and asserts the Warn record
// carries the structured goroutines/threshold attributes with the expected
// values — guarding the alarm's machine-readable payload, not just its message.
func assertWarnPayload(t *testing.T, logOutput string, wantGoroutines, wantThreshold int) {
	t.Helper()

	line := logOutput
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var rec struct {
		Level      string `json:"level"`
		Msg        string `json:"msg"`
		Goroutines *int   `json:"goroutines"`
		Threshold  *int   `json:"threshold"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("warn record is not valid JSON: %v; line: %q", err, line)
	}
	if rec.Level != "WARN" {
		t.Errorf("warn record level = %q, want WARN", rec.Level)
	}
	if rec.Goroutines == nil {
		t.Errorf("warn record missing structured attribute %q; line: %q", "goroutines", line)
	} else if *rec.Goroutines != wantGoroutines {
		t.Errorf("warn record goroutines = %d, want %d", *rec.Goroutines, wantGoroutines)
	}
	if rec.Threshold == nil {
		t.Errorf("warn record missing structured attribute %q; line: %q", "threshold", line)
	} else if *rec.Threshold != wantThreshold {
		t.Errorf("warn record threshold = %d, want %d", *rec.Threshold, wantThreshold)
	}
}

// TestStartGoroutineWatchdogQuietBelowThreshold asserts no warning fires when the
// count stays at or below the threshold.
func TestStartGoroutineWatchdogQuietBelowThreshold(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&safeWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartGoroutineWatchdog(ctx, 100, time.Millisecond, func() int { return 100 }, logger)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if got != "" {
		t.Fatalf("watchdog warned at/below threshold; log buffer: %q", got)
	}
}

// TestStartGoroutineWatchdogStopsOnCancel asserts the watchdog goroutine stops
// sampling after ctx is cancelled — it must not leak (decision 10's irony).
//
// To stay robust under a loaded -race box, it does not rely on a fixed
// post-cancel sleep (which can race a final in-flight tick that fires between
// reading the counter and the goroutine observing ctx.Done). Instead it polls
// for QUIESCENCE: read the counter, wait one observation window, read again,
// and only conclude "stopped" once two consecutive reads are identical. A
// still-running ticker would keep incrementing across windows and never settle.
func TestStartGoroutineWatchdogStopsOnCancel(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	StartGoroutineWatchdog(ctx, 1<<30, time.Millisecond, func() int {
		calls.Add(1)
		return 0
	}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))

	// Let it sample a few times so we know it is actually running.
	time.Sleep(20 * time.Millisecond)
	cancel()

	// Quiesce: with a 1ms ticker, a window an order of magnitude larger
	// guarantees that if the goroutine were still alive it would tick (and
	// increment) at least once per window. Two consecutive equal reads across
	// such a window means it has stopped — and the second read can't be racing
	// a final in-flight tick, because that tick would have landed in the
	// preceding window and broken the equality.
	const window = 30 * time.Millisecond
	const maxWindows = 20 // generous ceiling for a loaded -race box
	prev := calls.Load()
	for i := 0; i < maxWindows; i++ {
		time.Sleep(window)
		cur := calls.Load()
		if cur == prev {
			return // quiescent: two consecutive reads agree, watchdog stopped
		}
		prev = cur
	}
	t.Fatalf("watchdog never quiesced after cancel; still sampling at %d", prev)
}

// TestStartGoroutineWatchdogProductionWiring smoke-covers the EXACT injection the
// composition roots use — runtime.NumGoroutine + slog.Default — proving the real
// production wiring starts and stops cleanly without panic. The 1<<30 threshold
// ensures it never actually warns regardless of the live goroutine count.
func TestStartGoroutineWatchdogProductionWiring(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	StartGoroutineWatchdog(ctx, 1<<30, time.Millisecond, runtime.NumGoroutine, slog.Default())
	time.Sleep(5 * time.Millisecond) // let it sample at least once
	cancel()
}

// TestStartGoroutineWatchdogDisabledAtZero asserts threshold 0 spawns nothing.
func TestStartGoroutineWatchdogDisabledAtZero(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartGoroutineWatchdog(ctx, 0, time.Millisecond, func() int {
		calls.Add(1)
		return 1 << 30
	}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))

	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("disabled watchdog still sampled %d times", calls.Load())
	}
}

type safeWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
