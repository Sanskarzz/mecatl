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
	// caps is the capability set this fake advertises, so the forwarding test can
	// assert the decorator returns the inner provider's value verbatim.
	caps port.ProviderCapabilities
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

func (f *fakeProvider) Capabilities() port.ProviderCapabilities { return f.caps }

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

// TestRealEstablishErrorNotMaskedAsCanceled is the regression guard for the
// cleanup-cancel-masking bug: with a per-attempt timeout configured, a real
// establishment error (a 400) must NOT be reported as context.Canceled merely
// because establish() cancels the per-attempt context during cleanup. The real
// *oai.Error must remain recoverable via errors.As. Covers BOTH establish error
// paths — the outer Stream error and the first-chunk cerr.
func TestRealEstablishErrorNotMaskedAsCanceled(t *testing.T) {
	cfg := Config{MaxAttempts: 1, PerAttemptTimeout: time.Second}

	cases := []struct {
		name string
		step step
	}{
		{"outer error", step{outerErr: apiErr(400)}},
		{"first-chunk cerr", step{chunks: nil, midErr: apiErr(400)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProvider{steps: []step{tc.step}}
			p := Wrap(f, cfg)
			_, err := p.Stream(context.Background(), port.LLMRequest{})
			if err == nil {
				t.Fatalf("Stream returned nil error, want the 400")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("err masked as context.Canceled: %v", err)
			}
			var apiErr *oai.Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
				t.Fatalf("err = %v, want recoverable 400 *oai.Error", err)
			}
			if f.Calls() != 1 {
				t.Fatalf("inner called %d times, want 1", f.Calls())
			}
		})
	}
}

// TestGenuineCallerCancelSurfacesCanceled asserts that a genuine caller-cancel,
// even WITH a per-attempt timeout configured, still surfaces as context.Canceled
// (preserving the loop's StopCancelled / wedge-recovery path). This guards against
// an over-correction of the masking fix.
func TestGenuineCallerCancelSurfacesCanceled(t *testing.T) {
	f := &fakeProvider{steps: []step{{block: true}}}
	cfg := Config{MaxAttempts: 5, PerAttemptTimeout: time.Hour}
	p := Wrap(f, cfg)

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

// TestPerAttemptDeadlineChainHasDeadlineExceeded asserts that a genuine
// single-attempt per-attempt timeout produces an error chain that still satisfies
// errors.Is(_, context.DeadlineExceeded) — the property DefaultClassifier keys on
// to keep per-attempt timeouts retryable.
func TestPerAttemptDeadlineChainHasDeadlineExceeded(t *testing.T) {
	f := &fakeProvider{steps: []step{{block: true}}}
	cfg := Config{MaxAttempts: 1, PerAttemptTimeout: 20 * time.Millisecond}
	p := Wrap(f, cfg)

	_, err := p.Stream(context.Background(), port.LLMRequest{})
	if err == nil {
		t.Fatalf("Stream returned nil, want a per-attempt deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err chain = %v, want errors.Is(_, context.DeadlineExceeded)", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err masked as context.Canceled: %v", err)
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

// TestBreakerHalfOpenPermanentErrorStaysOpen pins the designed (IMPLEMENTATION-NOTES)
// behaviour of a half-open trial that fails with a PERMANENT error: such an error is
// breaker-neutral, so recordFailure is never reached for it. The breaker therefore
// stays in its post-cooldown open&halfOpen shape — it is NOT hard-reopened with a fresh
// cooldown (the half-open reopen branch), NOR reset. Consequently the next allow() after
// cooldown re-admits a trial that reaches inner and surfaces the next programmed step,
// rather than wedging behind a permanent *BreakerError lockout.
func TestBreakerHalfOpenPermanentErrorStaysOpen(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	// Step 0..2: transient burst opens the breaker. Step 3: the half-open trial fails
	// with a PERMANENT 400. Step 4: the next half-open trial succeeds, proving the
	// breaker kept admitting trials (no permanent lockout).
	f := &fakeProvider{steps: []step{
		{outerErr: apiErr(503)},
		{outerErr: apiErr(503)},
		{outerErr: apiErr(503)},
		{outerErr: apiErr(400)},
		{chunks: textTurn("recovered")},
	}}
	cfg := Config{
		MaxAttempts:      1, // one attempt per Stream, so each call is one establishment
		BaseBackoff:      time.Nanosecond,
		MaxBackoff:       time.Nanosecond,
		BreakerThreshold: 3,
		BreakerCooldown:  30 * time.Second,
		Clock:            clk.Now,
	}
	p := Wrap(f, cfg)

	// 1. Three transient failures open the breaker.
	for i := 0; i < 3; i++ {
		if _, err := p.Stream(context.Background(), port.LLMRequest{}); err == nil {
			t.Fatalf("burst attempt %d: want error", i)
		}
	}
	if f.Calls() != 3 {
		t.Fatalf("inner called %d times during burst, want 3", f.Calls())
	}
	// Breaker is open: fails fast without calling inner.
	if _, err := p.Stream(context.Background(), port.LLMRequest{}); !errorsAsBreaker(err) {
		t.Fatalf("want *BreakerError while open, got %v", err)
	}
	if f.Calls() != 3 {
		t.Fatalf("inner called while breaker open (calls=%d, want 3)", f.Calls())
	}

	// 2. Advance past cooldown so the next allow() admits a HALF-OPEN trial.
	clk.Advance(31 * time.Second)

	// 3. The half-open trial (step 3) returns a PERMANENT 400.
	_, err := p.Stream(context.Background(), port.LLMRequest{})

	// Assert: the 400 surfaces VERBATIM, NOT wrapped as a *BreakerError.
	if errorsAsBreaker(err) {
		t.Fatalf("half-open permanent trial returned *BreakerError, want the verbatim 400: %v", err)
	}
	var got400 *oai.Error
	if !errors.As(err, &got400) || got400.StatusCode != 400 {
		t.Fatalf("half-open trial err = %v, want verbatim 400 *oai.Error", err)
	}
	if f.Calls() != 4 {
		t.Fatalf("half-open permanent trial: inner called %d times, want 4 (trial WAS admitted)", f.Calls())
	}

	// 4. WITHOUT advancing the clock further beyond cooldown again, the breaker keeps
	// admitting trials — the permanent error neither re-armed (hard-reopened with a
	// fresh cooldown) nor reset it. The next Stream therefore reaches inner (step 4)
	// and surfaces the programmed success, not a permanent *BreakerError lockout.
	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if errorsAsBreaker(err) {
		t.Fatalf("breaker wedged after half-open permanent error (fast-failed instead of admitting a trial): %v", err)
	}
	if err != nil {
		t.Fatalf("post-permanent-trial Stream error: %v", err)
	}
	gotChunks, derr := drain(t, seq)
	if derr != nil {
		t.Fatalf("drain error: %v", derr)
	}
	if len(gotChunks) == 0 || gotChunks[0].Text != "recovered" {
		t.Fatalf("got %+v, want textTurn(recovered)", gotChunks)
	}
	if f.Calls() != 5 {
		t.Fatalf("post-permanent-trial: inner called %d times, want 5 (trial admitted again)", f.Calls())
	}
}

func errorsAsBreaker(err error) bool {
	var be *BreakerError
	return errors.As(err, &be)
}

// TestBreakerCountsTransientNotPermanent is the unit table for the breaker-health
// predicate isTransientForBreaker, distinct from DefaultClassifier (see ~499):
// 408/429/5xx/net/timeout are transient and count; 409 (the deliberate
// divergence), all other 4xx, caller cancel, unknown and nil are breaker-neutral.
func TestBreakerCountsTransientNotPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 transient", apiErr(429), true},
		{"408 transient", apiErr(408), true},
		{"500 transient", apiErr(500), true},
		{"503 transient", apiErr(503), true},
		{"net error transient", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"deadline exceeded transient", context.DeadlineExceeded, true},
		{"400 permanent", apiErr(400), false},
		{"401 permanent", apiErr(401), false},
		{"403 permanent", apiErr(403), false},
		{"404 permanent", apiErr(404), false},
		{"409 permanent for breaker (divergence)", apiErr(409), false},
		{"context canceled neutral", context.Canceled, false},
		{"unknown neutral", errors.New("mystery"), false},
		{"nil neutral", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientForBreaker(tc.err); got != tc.want {
				t.Fatalf("isTransientForBreaker(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBreakerOpensOnTransientBurst asserts a burst of TRANSIENT establishment
// failures opens the shared breaker: after BreakerThreshold failures the next
// Stream fails fast with *BreakerError without calling inner.
func TestBreakerOpensOnTransientBurst(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"429 burst", apiErr(429)},
		{"503 burst", apiErr(503)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &manualClock{t: time.Unix(1000, 0)}
			f := &fakeProvider{steps: []step{{outerErr: tc.err}}}
			cfg := Config{
				MaxAttempts:      1,
				BaseBackoff:      time.Nanosecond,
				MaxBackoff:       time.Nanosecond,
				BreakerThreshold: 3,
				BreakerCooldown:  30 * time.Second,
				Clock:            clk.Now,
			}
			p := Wrap(f, cfg)

			for i := 0; i < 3; i++ {
				if _, err := p.Stream(context.Background(), port.LLMRequest{}); err == nil {
					t.Fatalf("attempt %d: want error", i)
				}
			}
			callsAtOpen := f.Calls()
			if callsAtOpen != 3 {
				t.Fatalf("inner called %d times, want 3", callsAtOpen)
			}

			_, err := p.Stream(context.Background(), port.LLMRequest{})
			var be *BreakerError
			if !errors.As(err, &be) {
				t.Fatalf("4th Stream err = %v, want *BreakerError", err)
			}
			if f.Calls() != callsAtOpen {
				t.Fatalf("inner called during open breaker (calls=%d, want %d)", f.Calls(), callsAtOpen)
			}
		})
	}
}

// TestBreakerDoesNotOpenOnPermanentErrors is the core regression guard for the
// live bug: a burst of breaker-NEUTRAL client errors must NOT trip the shared
// breaker, so a working model still flows after the burst. This covers the
// permanent 4xx (400/401/403/404, e.g. a policy-blocked or unavailable model) AND
// the 409 divergence: 409 is RETRYABLE per cfg.Classifier yet breaker-neutral per
// isTransientForBreaker (a request-conflict is not provider-unhealthy), so a 409
// burst must likewise leave the breaker closed even though it is retryable. With
// MaxAttempts:1 each 409 surfaces in a single attempt like the other rows. This
// row goes red if 409 were added to the breaker's transient set.
func TestBreakerDoesNotOpenOnPermanentErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"400 burst", apiErr(400)},
		{"401 burst", apiErr(401)},
		{"403 burst", apiErr(403)},
		{"404 burst", apiErr(404)},
		{"409 burst (retryable but breaker-neutral)", apiErr(409)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &manualClock{t: time.Unix(1000, 0)}
			f := &fakeProvider{steps: []step{{outerErr: tc.err}}}
			cfg := Config{
				MaxAttempts:      1,
				BaseBackoff:      time.Nanosecond,
				MaxBackoff:       time.Nanosecond,
				BreakerThreshold: 3,
				BreakerCooldown:  30 * time.Second,
				Clock:            clk.Now,
			}
			p := Wrap(f, cfg)

			// More than the threshold's worth of permanent errors.
			for i := 0; i < 5; i++ {
				if _, err := p.Stream(context.Background(), port.LLMRequest{}); err == nil {
					t.Fatalf("attempt %d: want error", i)
				}
			}
			callsAfterBurst := f.Calls()
			if callsAfterBurst != 5 {
				t.Fatalf("inner called %d times during burst, want 5", callsAfterBurst)
			}

			// Flip to a working model; it must succeed — the breaker stayed closed.
			f.mu.Lock()
			f.steps = []step{{chunks: textTurn("works")}}
			f.mu.Unlock()

			seq, err := p.Stream(context.Background(), port.LLMRequest{})
			if errorsAsBreaker(err) {
				t.Fatalf("working model blocked by breaker after permanent-error burst: %v", err)
			}
			if err != nil {
				t.Fatalf("working model Stream error: %v", err)
			}
			got, derr := drain(t, seq)
			if derr != nil {
				t.Fatalf("drain error: %v", derr)
			}
			if len(got) == 0 || got[0].Text != "works" {
				t.Fatalf("got %+v, want textTurn(works)", got)
			}
			if f.Calls() != callsAfterBurst+1 {
				t.Fatalf("working model: inner called %d times, want %d (inner WAS called)", f.Calls(), callsAfterBurst+1)
			}
		})
	}
}

// TestBreakerNeutralOnCallerCancel asserts caller cancellations are
// breaker-neutral: even more cancels than the threshold never open the breaker,
// and a subsequent working step succeeds.
func TestBreakerNeutralOnCallerCancel(t *testing.T) {
	clk := &manualClock{t: time.Unix(1000, 0)}
	f := &fakeProvider{steps: []step{{block: true}}}
	cfg := Config{
		MaxAttempts:      1,
		BaseBackoff:      time.Nanosecond,
		MaxBackoff:       time.Nanosecond,
		BreakerThreshold: 3,
		BreakerCooldown:  30 * time.Second,
		Clock:            clk.Now,
	}
	p := Wrap(f, cfg)

	// Exceed the threshold's worth of caller-cancels.
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		_, err := p.Stream(ctx, port.LLMRequest{})
		if errorsAsBreaker(err) {
			t.Fatalf("cancel %d returned *BreakerError: %v", i, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel %d err = %v, want context.Canceled", i, err)
		}
		cancel()
	}

	// A subsequent working step succeeds — the breaker never opened.
	f.mu.Lock()
	f.steps = []step{{chunks: textTurn("works")}}
	f.mu.Unlock()

	seq, err := p.Stream(context.Background(), port.LLMRequest{})
	if errorsAsBreaker(err) {
		t.Fatalf("working step blocked by breaker after caller-cancel burst: %v", err)
	}
	if err != nil {
		t.Fatalf("working step Stream error: %v", err)
	}
	got, derr := drain(t, seq)
	if derr != nil {
		t.Fatalf("drain error: %v", derr)
	}
	if len(got) == 0 || got[0].Text != "works" {
		t.Fatalf("got %+v, want textTurn(works)", got)
	}
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

// TestCapabilitiesForwarded asserts the resilience decorator returns the wrapped
// provider's capabilities verbatim (it adds retries/breaker only, never alters
// what input the provider consumes).
func TestCapabilitiesForwarded(t *testing.T) {
	want := port.ProviderCapabilities{Image: true, Audio: false, EmbeddedContext: true}
	inner := &fakeProvider{caps: want}
	wrapped := Wrap(inner, Config{MaxAttempts: 1})
	if got := wrapped.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}
