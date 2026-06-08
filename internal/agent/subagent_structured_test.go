package agent_test

import (
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/agent"
)

// personSchema is a small structured-output schema reused across the tests: an object
// with a required string `name` and an integer `age`.
const personSchema = `{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`

// TestTaskStructuredOutputHappyPath is the MODEL-FACING e2e: a Task call with an
// output_schema where the child calls SubmitResult with a VALID payload. The Task
// RESULT is the validated JSON (carried after the agentId trailer), and the child only
// drove once (no correction needed).
func TestTaskStructuredOutputHappyPath(t *testing.T) {
	childLLM := mockllm.New(
		mockllm.ToolCallTurn(toolCall("k1", "SubmitResult", `{"name":"Ada","age":36}`)),
		mockllm.TextTurn("done"),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task",
			`{"prompt":"profile Ada","output_schema":`+personSchema+`}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].IsError {
		t.Fatalf("valid structured output must be a success result, got error: %q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, `"name":"Ada"`) || !strings.Contains(results[0].Content, `"age":36`) {
		t.Fatalf("result must carry the validated payload, got %q", results[0].Content)
	}
	// The agentId trailer is UNIVERSAL — it must ride the structured-output success path
	// too, not only the free-text path (QA SHOULD #6).
	if !strings.Contains(results[0].Content, "agentId: subagent-p1") {
		t.Fatalf("structured-output success must carry the agentId trailer, got %q", results[0].Content)
	}
}

// TestTaskStructuredOutputRetryCorrects is the ADVERSARIAL/uncooperative-mock test: the
// child submits a SCHEMA-VIOLATING payload twice (missing the required `age`, then a
// wrong type) before a valid one. The bounded retry must correct it and the run must
// succeed with the eventually-valid payload.
func TestTaskStructuredOutputRetryCorrects(t *testing.T) {
	childLLM := mockllm.New(
		// Attempt 0: missing required `age`.
		mockllm.ToolCallTurn(toolCall("k1", "SubmitResult", `{"name":"Ada"}`)),
		mockllm.TextTurn("oops"),
		// Attempt 1 (after correction): wrong type for `age`.
		mockllm.ToolCallTurn(toolCall("k2", "SubmitResult", `{"name":"Ada","age":"old"}`)),
		mockllm.TextTurn("oops again"),
		// Attempt 2 (after correction): valid.
		mockllm.ToolCallTurn(toolCall("k3", "SubmitResult", `{"name":"Ada","age":36}`)),
		mockllm.TextTurn("done"),
	)
	childEngine := childEngineWith(childLLM, catalogWith(t))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task",
			`{"prompt":"profile Ada","output_schema":`+personSchema+`}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("bounded retry must correct the payload and succeed, got %+v", results[0])
	}
	if !strings.Contains(results[0].Content, `"age":36`) {
		t.Fatalf("result must carry the eventually-valid payload, got %q", results[0].Content)
	}
}

// TestTaskStructuredOutputExhaustionFails is the ADVERSARIAL exhaustion test: the child
// NEVER produces a valid payload. The bounded retry must give up and the Task RESULT
// must be a MODEL-VISIBLE structured-output validation-failure tool error (a recoverable
// terminal — StopStructuredOutput — never `failed`/StopError), carrying the last
// validation message.
func TestTaskStructuredOutputExhaustionFails(t *testing.T) {
	// Every SubmitResult is missing the required `age` — never valid. Script enough
	// turns to outlast the bounded retry budget.
	var script []mockllm.Turn
	for i := 0; i < 8; i++ {
		script = append(script,
			mockllm.ToolCallTurn(toolCall("k", "SubmitResult", `{"name":"Ada"}`)),
			mockllm.TextTurn("still wrong"),
		)
	}
	childLLM := mockllm.New(script...)
	childEngine := childEngineWith(childLLM, catalogWith(t))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task",
			`{"prompt":"profile Ada","output_schema":`+personSchema+`}`)),
		mockllm.TextTurn("parent recovered"),
	)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].IsError {
		t.Fatalf("retry exhaustion must be a MODEL-VISIBLE tool error, got success: %q", results[0].Content)
	}
	if !strings.Contains(results[0].Content, "schema") {
		t.Fatalf("structured-output failure must name the schema mismatch, got %q", results[0].Content)
	}
	// The last validation error (missing required age) must reach the model.
	if !strings.Contains(results[0].Content, "required") {
		t.Fatalf("failure should carry the last validation error, got %q", results[0].Content)
	}
}

// TestTaskFreeTextUnchangedByStructuredPath is the regression guard: a Task call with NO
// output_schema is byte-identical to today — the child's free-text summary is returned
// (after the agentId trailer), no SubmitResult involved.
func TestTaskFreeTextUnchangedByStructuredPath(t *testing.T) {
	childEngine := childEngineWith(mockllm.New(mockllm.TextTurn("FREE TEXT SUMMARY")), catalogWith(t))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"investigate"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("free-text path must be unchanged, got %+v", results[0])
	}
	if !strings.Contains(results[0].Content, "FREE TEXT SUMMARY") {
		t.Fatalf("result must carry the free-text summary, got %q", results[0].Content)
	}
}

// TestTaskAgentIdTrailerInResultText is the RUNTIME-DISCOVERABILITY guard (R2/D5): the
// child session id must appear IN THE RESULT TEXT (where the model reads it), not only on
// the client-only subagent.* events. The id is the deterministic "subagent-<callID>".
func TestTaskAgentIdTrailerInResultText(t *testing.T) {
	childEngine := childEngineWith(mockllm.New(mockllm.TextTurn("summary")), catalogWith(t))
	task := agent.NewTaskTool(childEngine)

	results, _ := taskParentResults(t, task,
		mockllm.ToolCallTurn(toolCall("p1", "Task", `{"prompt":"x"}`)),
		mockllm.TextTurn("parent done"),
	)
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("want 1 success result, got %+v", results[0])
	}
	if !strings.Contains(results[0].Content, "agentId: subagent-p1") {
		t.Fatalf("result text must carry the agentId trailer (discoverability), got %q", results[0].Content)
	}
}
