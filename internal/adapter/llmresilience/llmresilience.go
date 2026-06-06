// Package llmresilience provides a harness-level resilience decorator around any
// port.LLMProvider. It adds bounded retries with exponential backoff and jitter,
// a consecutive-failure circuit breaker, per-attempt timeouts, and pluggable
// error classification.
//
// The single load-bearing correctness rule is no-replay-after-first-chunk: a
// model turn may stream text, reasoning, and tool calls, none of which can be
// safely re-issued once partially observed. Therefore this layer only retries
// failures that occur while establishing the stream — that is, before the first
// port.Chunk is yielded. The decorator buffers exactly the first chunk of each
// attempt: if the attempt fails before producing one, it is eligible for retry;
// once any chunk has been emitted to the caller, a subsequent mid-stream error
// is surfaced verbatim and never retried.
//
// The breaker counts consecutive TRANSIENT establishment failures across calls
// (HTTP 429/408/5xx, network errors, per-attempt timeouts — see
// isTransientForBreaker). Permanent client errors (4xx other than 408/429, e.g.
// a policy-blocked or unavailable model returning 400/403/404) and caller
// cancellations are breaker-neutral: they neither open the breaker nor reset it.
// After BreakerThreshold transient failures it opens and Stream fails fast with
// *BreakerError for BreakerCooldown; it then half-opens to admit a single trial.
// Any success resets it. All breaker state is concurrency-safe.
//
// The package depends only on the standard library and internal/port; the
// classifier reaches *openai.Error via errors.As to read its StatusCode, which
// is acceptable for an adapter.
package llmresilience

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	oai "github.com/openai/openai-go/v3"

	"github.com/stacklok/mecatl/internal/port"
)

// Config tunes the resilience decorator. The zero value is usable but inert
// (MaxAttempts <= 1 means a single attempt, no breaker); supply sensible values
// via Wrap.
type Config struct {
	// MaxAttempts is the total number of attempts for establishing the stream
	// (the initial call plus retries). Values < 1 are treated as 1.
	MaxAttempts int
	// BaseBackoff is the backoff before the first retry; it grows exponentially.
	BaseBackoff time.Duration
	// MaxBackoff caps the per-attempt backoff. 0 means no cap.
	MaxBackoff time.Duration
	// PerAttemptTimeout bounds each attempt's establishment (connect + first
	// chunk). 0 disables it. It never overrides a shorter caller deadline.
	PerAttemptTimeout time.Duration
	// BreakerThreshold is the number of consecutive failed attempts that opens
	// the breaker. Values < 1 disable the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long the breaker stays open before half-opening.
	BreakerCooldown time.Duration
	// Classifier reports whether an error is retryable. nil selects
	// DefaultClassifier.
	Classifier func(error) bool
	// Clock returns the current time; injectable for tests. nil selects
	// time.Now.
	Clock func() time.Time
}

// Wrap decorates inner with the resilience behaviour described by cfg and
// returns a port.LLMProvider. The returned provider is safe for concurrent use.
func Wrap(inner port.LLMProvider, cfg Config) port.LLMProvider {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.Classifier == nil {
		cfg.Classifier = DefaultClassifier
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &resilientProvider{inner: inner, cfg: cfg}
}

// BreakerError is returned by Stream while the circuit breaker is open. It
// carries the time at which the breaker is next eligible to half-open so callers
// can surface a meaningful terminal error.
type BreakerError struct {
	// RetryAfter is how long until the breaker half-opens.
	RetryAfter time.Duration
}

func (e *BreakerError) Error() string {
	return fmt.Sprintf("llmresilience: circuit breaker open, retry after %s", e.RetryAfter)
}

// ExhaustedError is returned when every attempt to establish the stream failed.
// It wraps the last underlying error.
type ExhaustedError struct {
	// Attempts is how many attempts were made.
	Attempts int
	// Err is the final underlying error.
	Err error
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("llmresilience: stream not established after %d attempt(s): %v", e.Attempts, e.Err)
}

// Unwrap exposes the final underlying error to errors.Is/As.
func (e *ExhaustedError) Unwrap() error { return e.Err }

// breakerState is the closed/open/half-open state machine, guarded by mu.
type breakerState struct {
	mu sync.Mutex
	// consecutiveFailures counts failed attempts since the last success.
	consecutiveFailures int
	// open is true while the breaker is open or half-open after cooldown.
	open bool
	// openedAt is when the breaker last opened.
	openedAt time.Time
	// halfOpen is true when a single trial is permitted after cooldown.
	halfOpen bool
}

type resilientProvider struct {
	inner   port.LLMProvider
	cfg     Config
	breaker breakerState
}

// Capabilities forwards the wrapped provider's capabilities unchanged: the
// resilience decorator adds retries/breaker behaviour only and never alters what
// kinds of prompt input the underlying provider consumes.
func (p *resilientProvider) Capabilities() port.ProviderCapabilities {
	return p.inner.Capabilities()
}

// allow checks the breaker before an attempt. It returns a *BreakerError if the
// breaker is open and the cooldown has not elapsed. When the cooldown has
// elapsed it transitions to half-open and admits the call.
func (p *resilientProvider) allow(now time.Time) error {
	if p.cfg.BreakerThreshold < 1 {
		return nil
	}
	p.breaker.mu.Lock()
	defer p.breaker.mu.Unlock()
	if !p.breaker.open {
		return nil
	}
	elapsed := now.Sub(p.breaker.openedAt)
	if elapsed < p.cfg.BreakerCooldown {
		return &BreakerError{RetryAfter: p.cfg.BreakerCooldown - elapsed}
	}
	// Cooldown elapsed: admit a single half-open trial.
	p.breaker.halfOpen = true
	return nil
}

// recordSuccess resets the breaker after a successful establishment.
func (p *resilientProvider) recordSuccess() {
	if p.cfg.BreakerThreshold < 1 {
		return
	}
	p.breaker.mu.Lock()
	defer p.breaker.mu.Unlock()
	p.breaker.consecutiveFailures = 0
	p.breaker.open = false
	p.breaker.halfOpen = false
}

// recordFailure tallies a failed attempt and opens the breaker once the
// threshold is reached (or immediately again on a failed half-open trial).
// Stream calls it only for TRANSIENT establishment failures (HTTP 429/408/5xx,
// network errors, per-attempt timeouts — see isTransientForBreaker); permanent
// client errors (4xx other than 408/429) and caller cancellations are
// breaker-neutral and never reach here.
func (p *resilientProvider) recordFailure(now time.Time) {
	if p.cfg.BreakerThreshold < 1 {
		return
	}
	p.breaker.mu.Lock()
	defer p.breaker.mu.Unlock()
	p.breaker.consecutiveFailures++
	if p.breaker.halfOpen {
		// A failed trial re-opens the breaker and restarts the cooldown.
		p.breaker.halfOpen = false
		p.breaker.open = true
		p.breaker.openedAt = now
		return
	}
	if p.breaker.consecutiveFailures >= p.cfg.BreakerThreshold {
		p.breaker.open = true
		p.breaker.openedAt = now
	}
}

// firstChunk is the buffered head of an attempt's stream: the first chunk the
// inner iterator produced, whether the stream was empty, and a continuation
// iterator (restSeq) that yields the remainder and performs cleanup.
type firstChunk struct {
	chunk   port.Chunk
	empty   bool
	restSeq iter.Seq2[port.Chunk, error]
}

// Stream establishes the inner stream with retries and breaker protection, then
// returns an iterator that replays the buffered first chunk followed by the rest
// of the inner stream. Mid-stream errors (after the first chunk) are surfaced,
// never retried.
func (p *resilientProvider) Stream(ctx context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	var lastErr error
	for attempt := 0; attempt < p.cfg.MaxAttempts; attempt++ {
		// Honour caller cancellation before doing any work.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := p.allow(p.cfg.Clock()); err != nil {
			return nil, err
		}

		head, err := p.establish(ctx, req)
		if err == nil {
			// Stream established and (if non-empty) first chunk in hand.
			p.recordSuccess()
			return wrap(head), nil
		}

		lastErr = err

		// Caller cancellation is never retried and is breaker-neutral.
		if isCallerCanceled(ctx, err) {
			return nil, err
		}
		// Only TRANSIENT failures count toward the shared breaker; permanent
		// client errors (4xx) and caller cancels leave its counters untouched.
		// (A half-open trial that fails with a PERMANENT error therefore leaves
		// the breaker in open&halfOpen — the next allow re-admits a trial after
		// cooldown; permanent errors never drive breaker state.)
		if isTransientForBreaker(err) {
			p.recordFailure(p.cfg.Clock())
		}
		// Permanent (non-retryable) errors are surfaced verbatim, not retried.
		if !p.cfg.Classifier(err) {
			return nil, err
		}
		// Backoff before the next attempt, unless this was the last one.
		if attempt < p.cfg.MaxAttempts-1 {
			if berr := p.backoff(ctx, attempt); berr != nil {
				return nil, berr
			}
		}
	}
	return nil, &ExhaustedError{Attempts: p.cfg.MaxAttempts, Err: lastErr}
}

// establish performs a single attempt: it applies the per-attempt timeout,
// calls the inner Stream, and pulls exactly the first chunk so that a pre-chunk
// error is observed here (and thus retryable). On success it returns the inner
// iterator, the buffered head, and a nil error. On failure it returns the error
// and cancels the per-attempt context.
//
// The per-attempt context is intentionally left live on success: it is cancelled
// when the returned iterator finishes or the caller stops early (handled in
// wrap), so the timeout no longer applies once streaming proper has begun.
func (p *resilientProvider) establish(ctx context.Context, req port.LLMRequest) (*firstChunk, error) {
	attemptCtx := ctx
	var cancel context.CancelFunc
	if p.cfg.PerAttemptTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, p.cfg.PerAttemptTimeout)
	}

	seq, err := p.inner.Stream(attemptCtx, req)
	if err != nil {
		// Capture the TRUE per-attempt cause BEFORE cancel() runs: once cancel()
		// fires, attemptCtx.Err() becomes context.Canceled and would mask the real
		// error (e.g. a 400) as a cancellation.
		cause := attemptCtx.Err()
		if cancel != nil {
			cancel()
		}
		return nil, attemptError(cause, err)
	}

	// Pull the first chunk using a pull iterator so we can stop after one.
	next, stop := iter.Pull2(seq)
	chunk, cerr, ok := next()
	if !ok {
		// Empty stream: not a failure — treat as a successful (empty) stream.
		stop()
		if cancel != nil {
			cancel()
		}
		return &firstChunk{empty: true}, nil
	}
	if cerr != nil {
		// Error before any real chunk: retryable establishment failure. Capture the
		// TRUE per-attempt cause BEFORE cancel() so the real error is not masked as a
		// cancellation (see attemptError).
		cause := attemptCtx.Err()
		stop()
		if cancel != nil {
			cancel()
		}
		return nil, attemptError(cause, cerr)
	}

	// We have a real first chunk. Build a continuation iterator that yields the
	// remainder and cleans up the pull iterator and per-attempt ctx.
	rest := func(yield func(port.Chunk, error) bool) {
		defer stop()
		if cancel != nil {
			defer cancel()
		}
		for {
			c, e, ok := next()
			if !ok {
				return
			}
			if !yield(c, e) {
				return
			}
			if e != nil {
				return
			}
		}
	}
	return &firstChunk{chunk: chunk, restSeq: rest}, nil
}

// attemptError annotates an establishment error with the per-attempt context's
// cause when a per-attempt deadline (or genuine caller-cancel) fired, so the
// classifier and caller-cancel check observe the right underlying cause.
//
// cause MUST be the per-attempt context's Err() read BEFORE the cleanup cancel()
// has run — never attemptCtx.Err() read afterwards. The cleanup cancel() we issue
// in establish() always sets attemptCtx.Err() to context.Canceled; reading it
// after cancel() would therefore mask EVERY real establishment error (e.g. a 400)
// as `context.Canceled: <real>`, which the loop then treats as a caller cancel and
// terminates as "cancelled" with the real provider message discarded.
//
// With the pre-cancel cause:
//   - cause == nil (no per-attempt deadline, no caller cancel): return err
//     VERBATIM so the real error surfaces (the classifier / loop see e.g. the 400
//     and report StopError with the provider message).
//   - cause != nil: wrap it as `cause: err` so a genuine per-attempt deadline stays
//     retryable (DefaultClassifier) and a genuine caller-cancel stays a cancel
//     (the loop's StopCancelled / wedge-recovery path).
func attemptError(cause, err error) error {
	if cause != nil && !errors.Is(err, cause) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

// wrap composes the buffered first chunk with the remainder of the inner stream.
func wrap(head *firstChunk) iter.Seq2[port.Chunk, error] {
	return func(yield func(port.Chunk, error) bool) {
		if head.empty {
			return
		}
		if !yield(head.chunk, nil) {
			// Caller stopped after the first chunk; drain the rest to trigger its
			// cleanup (stop + cancel) without yielding further.
			if head.restSeq != nil {
				for range head.restSeq {
					break
				}
			}
			return
		}
		if head.restSeq != nil {
			head.restSeq(yield)
		}
	}
}

// backoff sleeps before the next attempt with full-jitter exponential backoff,
// honouring ctx (it aborts promptly on cancellation and returns ctx.Err()).
func (p *resilientProvider) backoff(ctx context.Context, attempt int) error {
	d := p.backoffDuration(attempt)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backoffDuration computes the full-jitter backoff for the given zero-based
// attempt index: a random value in [0, min(MaxBackoff, Base*2^attempt)].
func (p *resilientProvider) backoffDuration(attempt int) time.Duration {
	if p.cfg.BaseBackoff <= 0 {
		return 0
	}
	// Exponential growth with overflow guard.
	exp := p.cfg.BaseBackoff
	for i := 0; i < attempt; i++ {
		exp *= 2
		if p.cfg.MaxBackoff > 0 && exp >= p.cfg.MaxBackoff {
			exp = p.cfg.MaxBackoff
			break
		}
		if exp <= 0 { // overflow
			exp = p.cfg.MaxBackoff
			break
		}
	}
	if p.cfg.MaxBackoff > 0 && exp > p.cfg.MaxBackoff {
		exp = p.cfg.MaxBackoff
	}
	if exp <= 0 {
		return 0
	}
	// Full jitter. A weak RNG is appropriate: jitter only spreads retries to
	// avoid thundering herds; it carries no security requirement.
	return time.Duration(rand.Int64N(int64(exp)) + 1) //nolint:gosec // jitter, not crypto
}

// isCallerCanceled reports whether err stems from the caller's ctx being
// cancelled or deadline-exceeded (as opposed to a per-attempt timeout, which is
// a retryable failure).
func isCallerCanceled(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// DefaultClassifier is the default retry policy. It retries:
//   - net errors and timeouts (net.Error, os timeouts),
//   - HTTP 408, 409, 429, and any 5xx from *openai.Error,
//   - context.DeadlineExceeded NOT tied to the caller (per-attempt timeouts).
//
// It does not retry:
//   - context.Canceled / context.DeadlineExceeded from the caller (handled
//     earlier in Stream, but also reported non-retryable here for safety),
//   - 4xx other than 408/429.
//
// Unknown errors are treated as non-retryable to avoid replaying ambiguous
// failures.
func DefaultClassifier(err error) bool {
	if err == nil {
		return false
	}
	// Caller-style context cancellation is never retryable.
	if errors.Is(err, context.Canceled) {
		return false
	}

	// OpenAI typed API error: classify on HTTP status.
	var apiErr *oai.Error
	if errors.As(err, &apiErr) {
		return retryableStatus(apiErr.StatusCode)
	}

	// Generic status-bearing errors (interface escape hatch for non-openai
	// providers that expose a StatusCode).
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		return retryableStatus(sc.StatusCode())
	}

	// Network errors and timeouts are retryable.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// A bare DeadlineExceeded (e.g. a per-attempt timeout surfaced without a
	// net.Error wrapper) is retryable.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return false
}

// retryableStatus reports whether an HTTP status code is retryable.
func retryableStatus(code int) bool {
	switch {
	case code == 408 || code == 409 || code == 429:
		return true
	case code >= 500 && code <= 599:
		return true
	default:
		return false
	}
}

// isTransientForBreaker is the breaker-health predicate: it reports whether an
// establishment failure is a sign the PROVIDER is unhealthy and should count
// toward the shared circuit breaker. It is DISTINCT from the retry classifier
// (cfg.Classifier): the breaker must NOT be coupled to the caller-injectable
// classifier, so it keeps its own status switch.
//
// The two predicates deliberately DIVERGE on HTTP 409: a request conflict is
// retryable per-request (retryableStatus returns true) but is NOT a sign the
// provider is unhealthy, so 409 must not trip a shared breaker and is excluded
// here. The breaker counts only transient provider-health failures:
//   - true: HTTP 408, 429, any 5xx; net.Error; bare context.DeadlineExceeded
//     (a per-attempt timeout).
//   - false: HTTP 409 (request-conflict ≠ provider-unhealthy), all other 4xx
//     (400/401/403/404/...), context.Canceled, unknown errors, nil.
//
// It mirrors DefaultClassifier's errors.As chain but writes its own status
// switch inline (rather than reusing retryableStatus) so the 409 divergence is
// explicit and self-documenting.
func isTransientForBreaker(err error) bool {
	if err == nil {
		return false
	}
	// Caller-style context cancellation is breaker-neutral.
	if errors.Is(err, context.Canceled) {
		return false
	}

	// breakerStatus is the breaker's OWN status switch, intentionally distinct
	// from retryableStatus: 408/429/5xx count as provider-health signals; 409
	// does NOT (request-conflict is not provider-unhealthy), and neither does any
	// other 4xx.
	breakerStatus := func(code int) bool {
		switch {
		case code == 408 || code == 429:
			return true
		case code >= 500 && code <= 599:
			return true
		default:
			return false
		}
	}

	// OpenAI typed API error: classify on HTTP status.
	var apiErr *oai.Error
	if errors.As(err, &apiErr) {
		return breakerStatus(apiErr.StatusCode)
	}

	// Generic status-bearing errors (interface escape hatch for non-openai
	// providers that expose a StatusCode).
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		return breakerStatus(sc.StatusCode())
	}

	// Network errors and timeouts are transient provider-health signals.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// A bare DeadlineExceeded (a per-attempt timeout surfaced without a net.Error
	// wrapper) is transient.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return false
}

// Compile-time assertion that the decorator satisfies the port.
var _ port.LLMProvider = (*resilientProvider)(nil)
