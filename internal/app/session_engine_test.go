package app

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
)

// fakeSink records nothing; it exists only so a test can pass a non-nil EventSink
// and assert it threads through baseEngineDeps onto the per-session engine.
type fakeSink struct{}

func (fakeSink) Emit(session.Event) {}

// configWithCollaborators returns a Config that turns ON the optional collaborators
// whose silent loss the [High] review flagged: a cascade compactor (a DISTINCT type
// from NewEngine's HeuristicCompactor default), the tiktoken counter, and slash
// commands (so CommandExpander is a real DirCommandExpander, not the NoopExpander).
func configWithCollaborators() Config {
	return Config{
		Model:          "test-model",
		Compaction:     "cascade",
		Tokenizer:      "tiktoken",
		EnableCommands: true,
		Sink:           fakeSink{},
	}
}

// TestBaseEngineDepsCarriesFullCollaboratorSet asserts the shared deps builder
// populates EVERY collaborator the main engine needs — the drift guard the [High]
// review required. Because both buildEngine and sessionEngineFactory build their
// agent.Deps through baseEngineDeps, a populated result here proves the per-session
// engine cannot silently drop the compactor, token counter, command expander,
// store, sink, logger, policy, or hooks.
func TestBaseEngineDepsCarriesFullCollaboratorSet(t *testing.T) {
	cfg := configWithCollaborators()
	provider := mockllm.New(mockllm.TextTurn("x"))
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	hooks := hookexec.New(nil)
	counter := buildTokenCounter(cfg)

	deps := baseEngineDeps(cfg, provider, store, policy, hooks, counter, nil, prompt.RootAssembler{})

	if deps.Instructions == nil {
		t.Fatal("Instructions is nil — turn-0 project instructions / memory index would not assemble")
	}
	if deps.Compactor == nil {
		t.Fatal("Compactor is nil — per-session engine would never compact")
	}
	if _, ok := deps.Compactor.(agent.CascadeCompactor); !ok {
		t.Fatalf("Compactor = %T, want the configured agent.CascadeCompactor (not NewEngine's default)", deps.Compactor)
	}
	if deps.TokenCounter == nil {
		t.Fatal("TokenCounter is nil — compaction trigger would have no counter")
	}
	if deps.CommandExpander == nil {
		t.Fatal("CommandExpander is nil")
	}
	if _, isNoop := deps.CommandExpander.(prompt.NoopExpander); isNoop {
		t.Fatal("CommandExpander is the NoopExpander — slash commands would silently stop expanding")
	}
	if deps.Store == nil {
		t.Fatal("Store is nil — weaker durability")
	}
	if deps.Sink == nil {
		t.Fatal("Sink is nil — operator observability would not fire")
	}
	if deps.Policy == nil {
		t.Fatal("Policy is nil")
	}
	if deps.Hooks == nil {
		t.Fatal("Hooks is nil")
	}
	if deps.ContextWindowTokens != defaultContextWindowTokens {
		t.Fatalf("ContextWindowTokens = %d, want %d", deps.ContextWindowTokens, defaultContextWindowTokens)
	}
	if deps.CompactionRatio != defaultCompactionRatio {
		t.Fatalf("CompactionRatio = %v, want %v", deps.CompactionRatio, defaultCompactionRatio)
	}
}

// TestSessionEngineFactoryBuildsUsableEngine asserts the per-session factory builds
// a non-nil engine and a non-nil close even with NO MCP specs reachable (the
// best-effort path), proving it wires through baseEngineDeps. The factory is given
// an empty spec list so it connects nothing (offline — no real MCP server), and we
// assert it still returns a usable engine + close.
func TestSessionEngineFactoryBuildsUsableEngine(t *testing.T) {
	cfg := configWithCollaborators()
	provider := mockllm.New(mockllm.TextTurn("x"))
	store := memstore.New()
	policy := permpolicy.NewPolicy(defaultRules(), nil)
	hooks := hookexec.New(nil)
	counter := buildTokenCounter(cfg)

	factory := sessionEngineFactory(cfg, provider, store, policy, hooks, counter, nil, prompt.RootAssembler{})

	eng, closeFn, err := factory(context.Background(), []mcp.ServerConfig{})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if eng == nil {
		t.Fatal("factory returned a nil engine")
	}
	if closeFn == nil {
		t.Fatal("factory returned a nil close")
	}
	if cerr := closeFn(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
}
