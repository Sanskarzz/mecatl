// Package main (ozzdemo) is the ozzharness end-to-end demo driver. Its core,
// RunScenario, drives the real agent.Engine through a scripted session that
// proves the whole shape of the loop — an auto-allowed tool call, a tool call
// that requires approval (and is approved), and a final assistant message — and
// returns every streamed session.Event so the same scenario backs both the
// printed CLI output and the hermetic e2e test.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stacklok/ozzharness/internal/adapter/hookexec"
	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/permpolicy"
	"github.com/stacklok/ozzharness/internal/adapter/store/memstore"
	"github.com/stacklok/ozzharness/internal/adapter/tools"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/governance"
	"github.com/stacklok/ozzharness/internal/port"
	"github.com/stacklok/ozzharness/internal/prompt"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// demoWorkspaceRoot is the root the in-memory demo workspace is mounted at.
const demoWorkspaceRoot = "/workspace"

// demoFilePath is the file the scenario seeds and the model Reads.
const demoFilePath = "greeting.txt"

// demoFileContent is the seeded file body, surfaced through the Read tool result.
const demoFileContent = "hello from the ozzharness demo workspace\n"

// demoModel is the model identifier stamped into requests and the prompt env.
const demoModel = "mock-model"

// RunScenario drives one full offline session against the provided LLMProvider
// and returns every emitted session.Event in order. The caller supplies the
// provider so the same scenario runs against mockllm (offline default) or the
// real OpenAI adapter. It auto-approves the single permission ask the script
// raises, simulating a client clicking "allow", so the loop resumes to a final
// result.
//
// It is the single source of truth for the demo: main prints these events, the
// e2e test asserts over them.
func RunScenario(ctx context.Context, provider port.LLMProvider) ([]session.Event, error) {
	// Workspace: an in-memory FS seeded with one file so Read returns content.
	ws := memfs.NewWorkspace(demoWorkspaceRoot)
	if err := ws.Write(ctx, demoFilePath, []byte(demoFileContent)); err != nil {
		return nil, fmt.Errorf("seed workspace: %w", err)
	}

	engine := buildEngine(provider)

	sess := session.New(
		"demo-session",
		session.ModeDefault,
		demoWorkspaceRoot,
		session.Limits{MaxTurns: 8, MaxToolCalls: 16, MaxConsecutiveFailures: 3},
		time.Now(),
	)

	run := engine.Run(ctx, sess, ws, "Read greeting.txt and then save a note, then summarize.")

	var events []session.Event
	for ev := range run.Events() {
		events = append(events, ev)
		// Simulate a client approving the permission ask so the loop resumes.
		if ev.Type == session.EvPermissionAsk && ev.Ask != nil {
			run.Approve(ev.Ask.AskID, true)
		}
	}
	return events, nil
}

// buildEngine assembles the agent.Engine for the demo with the always-available
// tool catalog (the demo exercises only Read/Write, so it runs shell-less: no
// Bash tool is registered), the default deny/ask/allow policy (Read auto-allowed,
// Write asks), no hooks, an in-memory store, and a deterministic prompt config.
func buildEngine(provider port.LLMProvider) *agent.Engine {
	cat := tool.NewCatalog()
	for _, t := range tools.All() {
		cat.MustRegister(t)
	}

	policy := permpolicy.NewPolicy([]governance.Rule{
		{Scope: governance.ScopeManaged, Tool: "Read", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Grep", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Glob", Effect: governance.Allow},
		{Scope: governance.ScopeManaged, Tool: "Write", Effect: governance.Ask},
		{Scope: governance.ScopeManaged, Tool: "Edit", Effect: governance.Ask},
	})

	return agent.NewEngine(agent.Deps{
		LLM:     provider,
		Catalog: cat,
		Policy:  policy,
		Hooks:   hookexec.New(nil),
		Store:   memstore.New(),
		PromptConfig: prompt.Config{
			Env: prompt.Env{
				Cwd:   demoWorkspaceRoot,
				OS:    "linux",
				Model: demoModel,
				Date:  "2026-05-29",
				Mode:  string(session.ModeDefault),
			},
		},
		Model: demoModel,
	})
}

// mockProvider returns the canned, offline LLMProvider that scripts the demo's
// three-act session:
//
//  1. assistant text + an auto-allowed Read tool call,
//  2. assistant text + a Write tool call that requires approval (permission.ask),
//  3. a final assistant message that ends the turn (terminal result).
//
// Each turn's tool call references the seeded file so the tools execute against
// real workspace content.
func mockProvider() *mockllm.Provider {
	readCall := session.NewToolCall(
		"call-read-1",
		"Read",
		json.RawMessage(fmt.Sprintf(`{"path":%q}`, demoFilePath)),
	)
	writeCall := session.NewToolCall(
		"call-write-1",
		"Write",
		json.RawMessage(`{"path":"note.txt","content":"reviewed the greeting\n"}`),
	)

	return mockllm.New(
		// Turn 1: prose + an auto-allowed Read.
		mockllm.ChunksTurn(
			mockllm.TextChunk("I'll read the greeting file first."),
			mockllm.ToolCallChunk(readCall),
			mockllm.UsageChunk(session.Usage{InputTokens: 1200, OutputTokens: 40, CacheReadTokens: 1000}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		// Turn 2: prose + a Write that must be approved.
		mockllm.ChunksTurn(
			mockllm.TextChunk("Now I'll save a short note, which needs your approval."),
			mockllm.ToolCallChunk(writeCall),
			mockllm.UsageChunk(session.Usage{InputTokens: 1400, OutputTokens: 55, CacheReadTokens: 1200}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
		// Turn 3: final summary, no tools -> terminal result.
		mockllm.ChunksTurn(
			mockllm.TextChunk("Done: I read greeting.txt and saved note.txt."),
			mockllm.UsageChunk(session.Usage{InputTokens: 1500, OutputTokens: 30, CacheReadTokens: 1400}),
			mockllm.DoneChunk(session.StopEndTurn),
		),
	)
}
