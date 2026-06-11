package llmresilience_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/governance"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/llmresilience"
)

// These are the engine+llmresilience INTEGRATION tests: they drive a real
// agent.Engine through the resilience wrapper and assert the loop's terminal
// behavior. They live here (not in engine/agent) because the wrapper under
// test is this adapter — the engine tree stays self-contained, importing no
// internal/ package even from tests.

// --- local copies of the engine test helpers ---------------------------------

func newEngine(d agent.Deps) *agent.Engine {
	if d.Policy == nil {
		d.Policy = permpolicy.NewPolicy([]governance.Rule{{Effect: governance.Allow}}, nil)
	}
	if d.Model == "" {
		d.Model = "test-model"
	}
	return agent.NewEngine(d)
}

func newSession(t *testing.T, limits session.Limits) *session.Session {
	t.Helper()
	return session.New("s1", session.ModeDefault, "/ws", limits, time.Unix(0, 0))
}

func catalogWith(t *testing.T, tools ...tool.Tool) *tool.Catalog {
	t.Helper()
	c := tool.NewCatalog()
	for _, tl := range tools {
		c.MustRegister(tl)
	}
	return c
}

func drain(r *agent.Run) []session.Event {
	var evs []session.Event
	for ev := range r.Events() {
		evs = append(evs, ev)
	}
	return evs
}

func lastResult(t *testing.T, evs []session.Event) *session.ResultPayload {
	t.Helper()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == session.EvResult {
			return evs[i].Result
		}
	}
	types := make([]session.EventType, len(evs))
	for i, e := range evs {
		types[i] = e.Type
	}
	t.Fatalf("no result event in %v", types)
	return nil
}

// --- provider fakes -----------------------------------------------------------

// errProvider returns a real, NON-context error as the outer Stream error on
// every call — the shape of a provider 400 reaching the establishment seam.
type errProvider struct {
	err error
}

func (*errProvider) Capabilities() port.ProviderCapabilities { return port.ProviderCapabilities{} }

func (p *errProvider) Stream(context.Context, port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	return nil, p.err
}

// firstChunkErrProvider returns a NON-nil iterator whose FIRST yielded chunk
// carries a real, NON-context error — the shape of a provider 400 surfacing as
// the first SSE chunk rather than as the outer Stream error. This drives the
// "first-chunk cerr" establishment seam in llmresilience (distinct from the
// outer-Stream-error seam that errProvider exercises).
type firstChunkErrProvider struct {
	err error
}

func (*firstChunkErrProvider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{}
}

func (p *firstChunkErrProvider) Stream(context.Context, port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	return func(yield func(port.Chunk, error) bool) {
		yield(port.Chunk{}, p.err)
	}, nil
}

// --- tests --------------------------------------------------------------------

// TestEstablishErrorTerminatesStopError is the end-to-end reproduction of the
// reported symptom: a real establishment error (NOT a context error), wrapped
// through llmresilience with a per-attempt timeout, must terminate the turn as
// StopError with the provider message surfaced — NOT as StopCancelled with an
// empty/silent result.
func TestEstablishErrorTerminatesStopError(t *testing.T) {
	provErr := fmt.Errorf("upstream rejected request: bad tool schema (400)")
	inner := &errProvider{err: provErr}
	llm := llmresilience.Wrap(inner, llmresilience.Config{
		MaxAttempts:       1,
		PerAttemptTimeout: time.Second,
	})

	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	res := lastResult(t, drain(r))
	if res.Stop != session.StopError {
		t.Fatalf("stop = %q, want error (NOT cancelled)", res.Stop)
	}
	if !strings.Contains(res.Error, "bad tool schema (400)") {
		t.Fatalf("result error = %q, want the provider message surfaced", res.Error)
	}
}

// TestFirstChunkErrorTerminatesStopError is the end-to-end counterpart of
// TestEstablishErrorTerminatesStopError for the OTHER establishment seam: here
// the inner Stream succeeds (non-nil iterator) but the FIRST CHUNK yields a real
// provider error (the "first-chunk cerr" path in llmresilience.establish). Wrapped
// through llmresilience with a per-attempt timeout, this too must terminate the
// turn as StopError with the provider message surfaced — NOT as StopCancelled.
// If the cerr-site masking fix (cause := attemptCtx.Err() read BEFORE cancel) is
// reverted, the cleanup cancel() masks the real error as context.Canceled, the
// loop treats it as a caller-cancel, and this test fails on StopCancelled.
func TestFirstChunkErrorTerminatesStopError(t *testing.T) {
	provErr := fmt.Errorf("upstream rejected request: bad tool schema (400)")
	inner := &firstChunkErrProvider{err: provErr}
	llm := llmresilience.Wrap(inner, llmresilience.Config{
		MaxAttempts:       1,
		PerAttemptTimeout: time.Second,
	})

	e := newEngine(agent.Deps{LLM: llm, Catalog: catalogWith(t)})
	r := e.Run(context.Background(), newSession(t, session.Limits{}), memfs.NewWorkspace("/ws"), "go")

	res := lastResult(t, drain(r))
	if res.Stop != session.StopError {
		t.Fatalf("stop = %q, want error (NOT cancelled)", res.Stop)
	}
	if !strings.Contains(res.Error, "bad tool schema (400)") {
		t.Fatalf("result error = %q, want the provider message surfaced", res.Error)
	}
}
