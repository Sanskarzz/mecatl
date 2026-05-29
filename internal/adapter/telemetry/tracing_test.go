package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stacklok/mecatl/internal/session"
)

func newTestTracing(t *testing.T) (*Tracing, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	return NewTracing(tp), exp
}

func spanByName(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

func attrInt(s tracetest.SpanStub, key string) (int64, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsInt64(), true
		}
	}
	return 0, false
}

func TestTracingRunSpan(t *testing.T) {
	tr, exp := newTestTracing(t)

	tr.Emit(session.Event{Type: session.EvSessionInit})
	tr.Emit(session.Event{Type: session.EvResult, Result: &session.ResultPayload{
		Stop:  session.StopEndTurn,
		Usage: session.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20, CacheWriteTokens: 5},
	}})

	spans := exp.GetSpans()
	run, ok := spanByName(spans, "mecatl.run")
	if !ok {
		t.Fatalf("no mecatl.run span; got %d spans", len(spans))
	}
	if run.Status.Code != codes.Ok {
		t.Errorf("run status = %v, want Ok", run.Status.Code)
	}
	if v, ok := attrInt(run, "mecatl.tokens.input"); !ok || v != 100 {
		t.Errorf("mecatl.tokens.input = %v (ok=%v), want 100", v, ok)
	}
	if v, ok := attrInt(run, "mecatl.tokens.cache_read"); !ok || v != 20 {
		t.Errorf("mecatl.tokens.cache_read = %v (ok=%v), want 20", v, ok)
	}
	var hasStop bool
	for _, kv := range run.Attributes {
		if kv.Key == attribute.Key("mecatl.run.stop") && kv.Value.AsString() == "end_turn" {
			hasStop = true
		}
	}
	if !hasStop {
		t.Errorf("run span missing mecatl.run.stop=end_turn")
	}
}

func TestTracingToolChildSpan(t *testing.T) {
	tr, exp := newTestTracing(t)

	tr.Emit(session.Event{Type: session.EvSessionInit})
	tr.Emit(session.Event{Type: session.EvTurnStart, Turn: 0})
	call := session.NewToolCall("c1", "bash", nil)
	tr.Emit(session.Event{Type: session.EvToolCall, ToolCall: &call})
	res := session.NewToolError("c1", "boom")
	tr.Emit(session.Event{Type: session.EvToolResult, ToolResult: &res})
	tr.Emit(session.Event{Type: session.EvResult, Result: &session.ResultPayload{Stop: session.StopEndTurn}})

	spans := exp.GetSpans()
	tool, ok := spanByName(spans, "mecatl.tool")
	if !ok {
		t.Fatalf("no mecatl.tool span; got %d spans", len(spans))
	}
	run, ok := spanByName(spans, "mecatl.run")
	if !ok {
		t.Fatal("no mecatl.run span")
	}
	turn, ok := spanByName(spans, "mecatl.turn")
	if !ok {
		t.Fatal("no mecatl.turn span")
	}
	// Tool span's parent chain should root at the run span.
	if tool.Parent.SpanID() != turn.SpanContext.SpanID() {
		t.Errorf("tool parent = %v, want turn span %v", tool.Parent.SpanID(), turn.SpanContext.SpanID())
	}
	if turn.Parent.SpanID() != run.SpanContext.SpanID() {
		t.Errorf("turn parent = %v, want run span %v", turn.Parent.SpanID(), run.SpanContext.SpanID())
	}
	if tool.Status.Code != codes.Error {
		t.Errorf("errored tool span status = %v, want Error", tool.Status.Code)
	}
	var nameOK bool
	for _, kv := range tool.Attributes {
		if kv.Key == attribute.Key("mecatl.tool.name") && kv.Value.AsString() == "bash" {
			nameOK = true
		}
	}
	if !nameOK {
		t.Errorf("tool span missing mecatl.tool.name=bash")
	}
}

func TestTracingErrorStopReason(t *testing.T) {
	tr, exp := newTestTracing(t)

	tr.Emit(session.Event{Type: session.EvSessionInit})
	tr.Emit(session.Event{Type: session.EvResult, Result: &session.ResultPayload{Stop: session.StopError}})

	run, ok := spanByName(exp.GetSpans(), "mecatl.run")
	if !ok {
		t.Fatal("no mecatl.run span")
	}
	if run.Status.Code != codes.Error {
		t.Errorf("run status = %v, want Error", run.Status.Code)
	}
}
