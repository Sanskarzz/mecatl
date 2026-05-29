package llmresilience

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// fakeProvider is a programmable port.LLMProvider for tests. Each call to Stream
// consumes the next entry in steps; if steps is exhausted the last entry is
// reused. A step either fails to establish (outerErr != nil), or yields the
// given chunks (optionally ending with a mid-stream error via midErr).
type fakeProvider struct {
	mu    sync.Mutex
	calls int32
	steps []step
	// onAttempt, if set, is invoked at the start of each Stream call with the
	// (zero-based) call index, before the step is evaluated. Useful to observe
	// timing / ctx state.
	onAttempt func(ctx context.Context, n int)
}

type step struct {
	outerErr error        // non-nil: Stream returns this as the outer error.
	chunks   []port.Chunk // chunks to yield before midErr.
	midErr   error        // non-nil: yielded after chunks as a terminal error.
	// block, if true, blocks on ctx.Done() before yielding anything (to exercise
	// per-attempt timeout and caller cancellation).
	block    bool
	blockErr error // error returned after block unblocks (default ctx.Err()).
}

func (f *fakeProvider) Stream(ctx context.Context, _ port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	n := int(atomic.AddInt32(&f.calls, 1)) - 1
	if f.onAttempt != nil {
		f.onAttempt(ctx, n)
	}
	f.mu.Lock()
	var s step
	switch {
	case n < len(f.steps):
		s = f.steps[n]
	case len(f.steps) > 0:
		s = f.steps[len(f.steps)-1]
	}
	f.mu.Unlock()

	if s.block {
		<-ctx.Done()
		err := s.blockErr
		if err == nil {
			err = ctx.Err()
		}
		return func(yield func(port.Chunk, error) bool) {
			yield(port.Chunk{}, err)
		}, nil
	}
	if s.outerErr != nil {
		return nil, s.outerErr
	}
	return func(yield func(port.Chunk, error) bool) {
		for _, c := range s.chunks {
			if !yield(c, nil) {
				return
			}
		}
		if s.midErr != nil {
			yield(port.Chunk{}, s.midErr)
		}
	}, nil
}

func (f *fakeProvider) Calls() int { return int(atomic.LoadInt32(&f.calls)) }

// manualClock is a controllable time source for deterministic breaker tests.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func textTurn(s string) []port.Chunk {
	return []port.Chunk{
		{Kind: port.ChunkText, Text: s},
		{Kind: port.ChunkUsage, Usage: &session.Usage{}},
		{Kind: port.ChunkDone, Stop: session.StopEndTurn},
	}
}

func drain(t *testing.T, seq iter.Seq2[port.Chunk, error]) ([]port.Chunk, error) {
	t.Helper()
	var got []port.Chunk
	for c, err := range seq {
		if err != nil {
			return got, err
		}
		got = append(got, c)
	}
	return got, nil
}

// tinyBackoffCfg returns a Config with negligible backoff so retry tests do not
// sleep meaningfully.
func tinyBackoffCfg(maxAttempts int) Config {
	return Config{
		MaxAttempts: maxAttempts,
		BaseBackoff: time.Nanosecond,
		MaxBackoff:  time.Nanosecond,
	}
}

func TestFailsThenSucceedsWithinMaxAttempts(t *testing.T) {
	conn := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	f := &fakeProvider{steps: []step{
		{outerErr: conn},
		{outerErr: conn},
		{chunks: textTurn("ok")},
	}}
	p := Wrap(f, tinyBackoffCfg(3))

	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	got, derr := drain(t, seq)
	if derr != nil {
		t.Fatalf("drain error: %v", derr)
	}
	if len(got) != 3 || got[0].Text != "ok" {
		t.Fatalf("got %+v, want textTurn(ok)", got)
	}
	if f.Calls() != 3 {
		t.Fatalf("inner called %d times, want 3", f.Calls())
	}
}

func TestExceedsMaxAttemptsReturnsExhausted(t *testing.T) {
	conn := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	f := &fakeProvider{steps: []step{{outerErr: conn}}}

	var backoffs int
	clk := &manualClock{t: time.Unix(0, 0)}
	cfg := Config{
		MaxAttempts: 3,
		BaseBackoff: time.Nanosecond,
		MaxBackoff:  time.Nanosecond,
		Clock:       clk.Now,
	}
	// Count attempts via onAttempt; backoff itself uses real (tiny) timers.
	f.onAttempt = func(_ context.Context, _ int) { backoffs++ }

	p := Wrap(f, cfg)
	_, err := p.Stream(context.Background(), port.LLMRequest{})
	var ex *ExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("err = %v, want *ExhaustedError", err)
	}
	if ex.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", ex.Attempts)
	}
	if !errors.Is(err, conn) {
		t.Fatalf("ExhaustedError does not wrap the conn error: %v", err)
	}
	if f.Calls() != 3 {
		t.Fatalf("inner called %d times, want 3", f.Calls())
	}
}

func TestMidStreamErrorAfterFirstChunkNotRetried(t *testing.T) {
	boom := errors.New("transport blew up mid-stream")
	f := &fakeProvider{steps: []step{
		{chunks: []port.Chunk{{Kind: port.ChunkText, Text: "partial"}}, midErr: boom},
		{chunks: textTurn("should-not-be-used")},
	}}
	p := Wrap(f, tinyBackoffCfg(3))

	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream returned outer error: %v", err)
	}
	got, derr := drain(t, seq)
	if !errors.Is(derr, boom) {
		t.Fatalf("drain error = %v, want boom", derr)
	}
	if len(got) != 1 || got[0].Text != "partial" {
		t.Fatalf("got %+v, want one 'partial' chunk before error", got)
	}
	if f.Calls() != 1 {
		t.Fatalf("inner called %d times, want 1 (no replay after first chunk)", f.Calls())
	}
}

func TestCtxCancelDuringBackoffAbortsPromptly(t *testing.T) {
	conn := &net.OpError{Op: "dial", Err: errors.New("refused")}
	f := &fakeProvider{steps: []step{{outerErr: conn}}}
	cfg := Config{
		MaxAttempts: 5,
		BaseBackoff: time.Hour, // long enough that only cancellation ends it.
		MaxBackoff:  time.Hour,
	}
	p := Wrap(f, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := p.Stream(ctx, port.LLMRequest{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stream took %s, want prompt abort on cancel", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCallerCanceledNotRetried(t *testing.T) {
	f := &fakeProvider{steps: []step{{block: true}}}
	p := Wrap(f, tinyBackoffCfg(5))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := p.Stream(ctx, port.LLMRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.Calls() != 1 {
		t.Fatalf("inner called %d times, want 1 (caller cancel not retried)", f.Calls())
	}
}

func TestPerAttemptTimeoutIsRetryable(t *testing.T) {
	// First attempt blocks until its per-attempt deadline; second succeeds.
	f := &fakeProvider{steps: []step{
		{block: true}, // returns ctx.Err() == DeadlineExceeded for the attempt ctx
		{chunks: textTurn("recovered")},
	}}
	cfg := Config{
		MaxAttempts:       2,
		BaseBackoff:       time.Nanosecond,
		MaxBackoff:        time.Nanosecond,
		PerAttemptTimeout: 20 * time.Millisecond,
	}
	p := Wrap(f, cfg)

	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	got, derr := drain(t, seq)
	if derr != nil {
		t.Fatalf("drain error: %v", derr)
	}
	if len(got) == 0 || got[0].Text != "recovered" {
		t.Fatalf("got %+v, want recovered", got)
	}
	if f.Calls() != 2 {
		t.Fatalf("inner called %d times, want 2", f.Calls())
	}
}

func TestBreakerOpensFailsFastHalfOpensRecovers(t *testing.T) {
	conn := &net.OpError{Op: "dial", Err: errors.New("refused")}
	clk := &manualClock{t: time.Unix(1000, 0)}
	f := &fakeProvider{}
	// Program: keep failing until we flip it.
	failing := step{outerErr: conn}
	f.steps = []step{failing}

	cfg := Config{
		MaxAttempts:      1, // one attempt per Stream so each call is one failure
		BaseBackoff:      time.Nanosecond,
		MaxBackoff:       time.Nanosecond,
		BreakerThreshold: 3,
		BreakerCooldown:  30 * time.Second,
		Clock:            clk.Now,
	}
	p := Wrap(f, cfg)

	// Three consecutive failures open the breaker.
	for i := 0; i < 3; i++ {
		if _, err := p.Stream(context.Background(), port.LLMRequest{}); err == nil {
			t.Fatalf("attempt %d: want error", i)
		}
	}
	callsAtOpen := f.Calls()
	if callsAtOpen != 3 {
		t.Fatalf("inner called %d times, want 3", callsAtOpen)
	}

	// Breaker open: fails fast with *BreakerError, inner not called.
	_, err := p.Stream(context.Background(), port.LLMRequest{})
	var be *BreakerError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want *BreakerError", err)
	}
	if f.Calls() != callsAtOpen {
		t.Fatalf("inner called during open breaker (calls=%d)", f.Calls())
	}

	// Advance past cooldown; breaker half-opens and admits one trial. Make the
	// next inner call succeed so the breaker resets.
	f.mu.Lock()
	f.steps = []step{{chunks: textTurn("back")}}
	f.calls = 0 // restart the step cursor for clarity
	f.mu.Unlock()
	clk.Advance(31 * time.Second)

	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if err != nil {
		t.Fatalf("half-open trial: unexpected error: %v", err)
	}
	if _, derr := drain(t, seq); derr != nil {
		t.Fatalf("half-open drain error: %v", derr)
	}
	if f.Calls() != 1 {
		t.Fatalf("half-open: inner called %d times, want 1", f.Calls())
	}

	// After recovery the breaker is closed: subsequent calls flow through.
	if _, err := p.Stream(context.Background(), port.LLMRequest{}); err != nil {
		t.Fatalf("post-recovery Stream error: %v", err)
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	conn := &net.OpError{Op: "dial", Err: errors.New("refused")}
	clk := &manualClock{t: time.Unix(0, 0)}
	f := &fakeProvider{steps: []step{{outerErr: conn}}}
	cfg := Config{
		MaxAttempts:      1,
		BaseBackoff:      time.Nanosecond,
		MaxBackoff:       time.Nanosecond,
		BreakerThreshold: 2,
		BreakerCooldown:  10 * time.Second,
		Clock:            clk.Now,
	}
	p := Wrap(f, cfg)

	for i := 0; i < 2; i++ {
		_, _ = p.Stream(context.Background(), port.LLMRequest{})
	}
	// Open now: fast-fail.
	if _, err := p.Stream(context.Background(), port.LLMRequest{}); !errorsAsBreaker(err) {
		t.Fatalf("want breaker error while open, got %v", err)
	}
	// Half-open: cooldown elapses, the trial fails, breaker reopens.
	clk.Advance(11 * time.Second)
	callsBefore := f.Calls()
	if _, err := p.Stream(context.Background(), port.LLMRequest{}); err == nil {
		t.Fatalf("half-open trial should have failed")
	}
	if f.Calls() != callsBefore+1 {
		t.Fatalf("half-open should admit exactly one trial")
	}
	// Reopened: fast-fail again without calling inner.
	c := f.Calls()
	if _, err := p.Stream(context.Background(), port.LLMRequest{}); !errorsAsBreaker(err) {
		t.Fatalf("want breaker error after reopen, got %v", err)
	}
	if f.Calls() != c {
		t.Fatalf("inner called while reopened")
	}
}

func errorsAsBreaker(err error) bool {
	var be *BreakerError
	return errors.As(err, &be)
}

// apiErr builds an *oai.Error with enough populated for its Error() method to
// render without dereferencing nil Request/Response.
func apiErr(code int) *oai.Error {
	req, _ := http.NewRequest(http.MethodPost, "https://api.example/v1/responses", nil)
	return &oai.Error{
		StatusCode: code,
		Request:    req,
		Response:   &http.Response{StatusCode: code},
	}
}

func TestDefaultClassifier(t *testing.T) {
	mk := func(code int) error { return apiErr(code) }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"500 retryable", mk(500), true},
		{"503 retryable", mk(503), true},
		{"429 retryable", mk(429), true},
		{"408 retryable", mk(408), true},
		{"409 retryable", mk(409), true},
		{"400 not", mk(400), false},
		{"401 not", mk(401), false},
		{"404 not", mk(404), false},
		{"connection error retryable", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"deadline exceeded retryable", context.DeadlineExceeded, true},
		{"context canceled not", context.Canceled, false},
		{"wrapped canceled not", fmt.Errorf("x: %w", context.Canceled), false},
		{"unknown not", errors.New("mystery"), false},
		{"nil not", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultClassifier(tc.err); got != tc.want {
				t.Fatalf("DefaultClassifier(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNonRetryableNotRetried(t *testing.T) {
	f := &fakeProvider{steps: []step{{outerErr: apiErr(400)}}}
	p := Wrap(f, tinyBackoffCfg(5))
	_, err := p.Stream(context.Background(), port.LLMRequest{})
	var apiErr *oai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("err = %v, want 400 surfaced", err)
	}
	if f.Calls() != 1 {
		t.Fatalf("inner called %d times, want 1 (400 not retried)", f.Calls())
	}
}

func TestEmptyStreamIsSuccess(t *testing.T) {
	f := &fakeProvider{steps: []step{{}}} // no chunks, no error: empty stream
	p := Wrap(f, tinyBackoffCfg(3))
	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	got, derr := drain(t, seq)
	if derr != nil {
		t.Fatalf("drain error: %v", derr)
	}
	if len(got) != 0 {
		t.Fatalf("got %d chunks, want 0", len(got))
	}
	if f.Calls() != 1 {
		t.Fatalf("inner called %d times, want 1", f.Calls())
	}
}

func TestBackoffDurationRespectsMaxAndJitter(t *testing.T) {
	p := &resilientProvider{cfg: Config{BaseBackoff: 100 * time.Millisecond, MaxBackoff: 1 * time.Second}}
	for attempt := 0; attempt < 10; attempt++ {
		d := p.backoffDuration(attempt)
		if d <= 0 || d > p.cfg.MaxBackoff {
			t.Fatalf("attempt %d: backoff %s out of (0, %s]", attempt, d, p.cfg.MaxBackoff)
		}
	}
}
