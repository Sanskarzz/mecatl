package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// modelrouter_internal_test.go drives the Subagent run() router HOOK directly (the
// parentCaps.routeTask seam) to assert PRECEDENCE, FAIL-SOFT, and the NO-NESTING guard —
// the seams the composition end-to-end test cannot reach in isolation.

// markerEngine builds a default explorer-style child engine whose single turn returns a
// MARKER text, so a test can tell WHICH engine (default vs routed) actually drove.
func markerEngine(marker string) *Engine {
	return NewEngine(Deps{
		LLM:     mockllm.New(mockllm.TextTurn(marker)),
		Catalog: tool.NewCatalog(),
		Policy:  allowAllInt(),
		Model:   marker,
	})
}

// routerTool builds a Subagent tool whose default engine returns "DEFAULT" and whose
// per-call factory returns an engine returning "ROUTED:<model>" for any model — so the
// result text reveals whether the router's model override took effect.
func routerTool() *SubagentTool {
	factory := func(model string) (*Engine, bool) {
		return markerEngine("ROUTED:" + model), true
	}
	return NewSubagentTool(markerEngine("DEFAULT"), WithSubagentEngineFactory(factory)).(*SubagentTool)
}

// A plain default delegation with a wired routeTask mints the child on the ROUTED model.
func TestRunRouteTaskRoutesPlainDelegation(t *testing.T) {
	tl := routerTool()
	caps := parentCaps{children: newChildRunRegistry(), routeTask: func(string) (string, string, bool) {
		return "large", "big-model", true
	}}
	res, err := tl.ExecuteWithParent(context.Background(),
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"deep work"}`)),
		memfs.NewWorkspace("/ws"), nil, caps)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !strings.Contains(res.Content, "ROUTED:big-model") {
		t.Fatalf("plain delegation must run on the routed model; got %q", res.Content)
	}
}

// PRECEDENCE: an explicit `model` arg PINS the engine — the router must NOT fire (the
// run() hook gates routing on args.Model==""). The routeTask here would route to a
// DIFFERENT model; the result must reflect the explicit one, and routeTask must be
// untouched (call count 0).
func TestRunExplicitModelBeatsRouter(t *testing.T) {
	tl := routerTool()
	var calls int
	caps := parentCaps{children: newChildRunRegistry(), routeTask: func(string) (string, string, bool) {
		calls++
		return "large", "router-model", true
	}}
	res, err := tl.ExecuteWithParent(context.Background(),
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"x","model":"explicit-model"}`)),
		memfs.NewWorkspace("/ws"), nil, caps)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !strings.Contains(res.Content, "ROUTED:explicit-model") {
		t.Fatalf("explicit model must win; got %q", res.Content)
	}
	if calls != 0 {
		t.Fatalf("routeTask must NOT be consulted when an explicit model is set; called %d times", calls)
	}
}

// PRECEDENCE: fork does NOT route (a fork inherits the parent engine). The router would
// route otherwise; routeTask must be untouched. (fork also requires forkHistory; we
// supply a trivial one so the fork precondition passes and the run reaches the gate.)
func TestRunForkDoesNotRoute(t *testing.T) {
	tl := routerTool()
	var calls int
	caps := parentCaps{
		children:    newChildRunRegistry(),
		forkHistory: func() []session.Message { return nil }, // turn-0 fork → empty snapshot (benign)
		routeTask: func(string) (string, string, bool) {
			calls++
			return "large", "router-model", true
		},
	}
	_, err := tl.ExecuteWithParent(context.Background(),
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"x","fork":true}`)),
		memfs.NewWorkspace("/ws"), nil, caps)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("routeTask must NOT be consulted on a fork; called %d times", calls)
	}
}

// PRECEDENCE: a named `agent` PINS its specialist engine — the router must NOT fire (the
// run() hook gates routing on args.Agent==""). A regression dropping `args.Agent != ""`
// from the gate would route a specialist's child through the classifier and silently
// override its def-pinned model. The routeTask here would route elsewhere; the result
// must be the SPECIALIST engine's output, and routeTask must be untouched (calls==0).
func TestRunNamedAgentBeatsRouter(t *testing.T) {
	specialist := markerEngine("SPECIALIST")
	tl := NewSubagentTool(markerEngine("DEFAULT"),
		WithSubagentEngineFactory(func(model string) (*Engine, bool) { return markerEngine("ROUTED:" + model), true }),
		WithAgentEngines(map[string]*Engine{"reviewer": specialist},
			[]AgentMeta{{Name: "reviewer", Description: "a specialist"}}),
	).(*SubagentTool)
	var calls int
	caps := parentCaps{children: newChildRunRegistry(), routeTask: func(string) (string, string, bool) {
		calls++
		return "large", "router-model", true
	}}
	res, err := tl.ExecuteWithParent(context.Background(),
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"x","agent":"reviewer"}`)),
		memfs.NewWorkspace("/ws"), nil, caps)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("routeTask must NOT be consulted when a named agent is set; called %d times", calls)
	}
	if !strings.Contains(res.Content, "SPECIALIST") {
		t.Fatalf("a named agent must run the specialist engine, not a routed one; got %q", res.Content)
	}
}

// PRECEDENCE: a `resume` call continues a persisted child on the default explorer engine
// (the v1 resume invariant) — the router must NOT fire (the run() hook gates routing on
// !resuming). A regression dropping the `resuming` guard would re-classify a resumed
// child, undetected. Asserted at the gate (maybeRouteModel), the single source of the
// precedence decision, so the assertion is deterministic and needs no store fixture.
func TestRunResumeDoesNotRoute(t *testing.T) {
	var calls int
	route := func(string) (string, string, bool) { calls++; return "large", "router-model", true }
	caps := parentCaps{children: newChildRunRegistry(), routeTask: route}
	args := subagentArgs{Prompt: "x", Resume: "subagent-abc"}
	cat, model := maybeRouteModel(args, true /*resuming*/, caps)
	if calls != 0 {
		t.Fatalf("routeTask must NOT be consulted on a resume; called %d times", calls)
	}
	if cat != "" || model != "" {
		t.Fatalf("a resume must not route; got (%q, %q)", cat, model)
	}
}

// FAIL-SOFT: a routeTask MISS (ok=false) falls through to the DEFAULT explorer engine —
// the delegation still completes, never errors.
func TestRunRouteTaskMissInheritsDefault(t *testing.T) {
	tl := routerTool()
	caps := parentCaps{children: newChildRunRegistry(), routeTask: func(string) (string, string, bool) {
		return "", "", false // miss
	}}
	res, err := tl.ExecuteWithParent(context.Background(),
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"x"}`)),
		memfs.NewWorkspace("/ws"), nil, caps)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("a router miss must still complete on the default engine, got error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "DEFAULT") {
		t.Fatalf("a router miss must run on the DEFAULT explorer; got %q", res.Content)
	}
}

// NO-NESTING: a nil routeTask (the child posture — a child has no parentCaps.routeTask)
// runs the default engine with no routing. A child structurally cannot route.
func TestRunNilRouteTaskNoRouting(t *testing.T) {
	tl := routerTool()
	res, err := tl.ExecuteWithParent(context.Background(),
		session.NewToolCall("p1", "Subagent", json.RawMessage(`{"prompt":"x"}`)),
		memfs.NewWorkspace("/ws"), nil, parentCaps{children: newChildRunRegistry()}) // routeTask nil
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !strings.Contains(res.Content, "DEFAULT") {
		t.Fatalf("a nil routeTask must run the default engine; got %q", res.Content)
	}
}

// The per-run router breaker opens after defaultModelRouterMaxMisses CONSECUTIVE misses
// and then SKIPS the classifier for the rest of the run. This drives the breaker through
// the Engine.parentCaps closure (the production binding), so the threshold + skip are
// exercised end-to-end, not just the bare helper.
func TestRouterBreakerOpensAfterConsecutiveMisses(t *testing.T) {
	var (
		mu        sync.Mutex
		callCount int
	)
	// A router closure that ALWAYS misses (ok=false), counting how often it is consulted.
	mainEngine := NewEngine(Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  allowAllInt(),
		Model:   "main",
		SubagentModelRouter: func(string) (string, string, bool) {
			mu.Lock()
			callCount++
			mu.Unlock()
			return "", "", false
		},
	})
	// Build a Run carrying the breaker (RunContentWith arms it when the router is wired),
	// then derive the production routeTask via parentCaps.
	run := &Run{router: &modelRouterBreaker{max: defaultModelRouterMaxMisses}, children: newChildRunRegistry()}
	caps := mainEngine.parentCaps(run, nil, 0)
	if caps.routeTask == nil {
		t.Fatal("routeTask must be wired when SubagentModelRouter is set")
	}
	// Call past the threshold: the first defaultModelRouterMaxMisses calls consult the
	// underlying router (all miss), the breaker opens, and subsequent calls SKIP it.
	for i := 0; i < defaultModelRouterMaxMisses+3; i++ {
		caps.routeTask("task")
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != defaultModelRouterMaxMisses {
		t.Fatalf("underlying router consulted %d times, want exactly %d (breaker opens then skips)", callCount, defaultModelRouterMaxMisses)
	}
}

// A successful classification RESETS the breaker's consecutive-miss count.
func TestRouterBreakerResetsOnSuccess(t *testing.T) {
	var (
		mu        sync.Mutex
		callCount int
		hit       bool
	)
	mainEngine := NewEngine(Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  allowAllInt(),
		Model:   "main",
		SubagentModelRouter: func(string) (string, string, bool) {
			mu.Lock()
			callCount++
			h := hit
			mu.Unlock()
			if h {
				return "large", "big", true
			}
			return "", "", false
		},
	})
	run := &Run{router: &modelRouterBreaker{max: defaultModelRouterMaxMisses}, children: newChildRunRegistry()}
	caps := mainEngine.parentCaps(run, nil, 0)
	// Two misses (below the threshold of 3), then a success resets, then more misses must
	// not trip immediately — proving the reset.
	caps.routeTask("t")
	caps.routeTask("t")
	mu.Lock()
	hit = true
	mu.Unlock()
	if _, _, ok := caps.routeTask("t"); !ok {
		t.Fatal("a success must classify")
	}
	mu.Lock()
	hit = false
	mu.Unlock()
	// Three more misses are needed to re-open (the count was reset).
	caps.routeTask("t")
	caps.routeTask("t")
	caps.routeTask("t")
	caps.routeTask("t") // this one should be skipped (breaker open again)
	mu.Lock()
	defer mu.Unlock()
	// 2 (initial misses) + 1 (success) + 3 (re-trip) = 6 underlying consultations; the 7th
	// is skipped. Had the success not reset, the breaker would have opened at the 3rd call.
	if callCount != 6 {
		t.Fatalf("underlying router consulted %d times, want 6 (success reset the consecutive count)", callCount)
	}
}

// TestRouterBreakerSerializesConcurrentCalls hardens the "a Subagent fan-out cannot
// multiply classifier spend in parallel" claim: the breaker mutex is held across the
// WHOLE routeTask call, so concurrent calls are serialised and the consecutive-miss
// count stays deterministic. With an always-miss router and N concurrent calls past the
// threshold, EXACTLY `max` underlying consultations happen (the breaker opens once and
// the rest are skipped) — never more, however the goroutines interleave. Run under -race
// it also proves the closure + breaker are data-race-clean.
func TestRouterBreakerSerializesConcurrentCalls(t *testing.T) {
	var (
		mu        sync.Mutex
		callCount int
	)
	mainEngine := NewEngine(Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  allowAllInt(),
		Model:   "main",
		SubagentModelRouter: func(string) (string, string, bool) {
			mu.Lock()
			callCount++
			mu.Unlock()
			return "", "", false // always miss → the breaker must open after `max`
		},
	})
	run := &Run{router: &modelRouterBreaker{max: defaultModelRouterMaxMisses}, children: newChildRunRegistry()}
	caps := mainEngine.parentCaps(run, nil, 0)

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			caps.routeTask("concurrent task")
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// The breaker serialises: the first `max` calls consult the underlying router (each a
	// miss), the breaker opens, and every remaining concurrent call is skipped. So the
	// underlying router is consulted EXACTLY `max` times regardless of interleaving — a
	// non-serialised breaker would let several goroutines read consecutiveMiss < max
	// before any incremented it, over-consulting (and racing the field under -race).
	if callCount != defaultModelRouterMaxMisses {
		t.Fatalf("underlying router consulted %d times under %d concurrent calls, want exactly %d (serialised breaker)",
			callCount, goroutines, defaultModelRouterMaxMisses)
	}
}
