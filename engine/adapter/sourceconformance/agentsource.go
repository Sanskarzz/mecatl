package sourceconformance

import (
	"context"
	"reflect"
	"testing"

	"github.com/stacklok/mecatl/engine/tool"
)

// AgentFixture is the canonical agent-definition set RunAgentSource asserts
// against, sorted by Name. It covers the three shapes the suite needs: a
// minimal def (name + description + body only), a fully-loaded def (tools,
// disallowed tools, model, provider, permission mode, limits, color, skills,
// hooks), and an MCP-bearing def (one REFERENCE entry + one INLINE entry with
// secret-shaped headers).
//
// The fixture is authored to ROUND-TRIP the filesystem frontmatter parser:
// bodies are trimmed, descriptions are single-line and under the
// always-in-context cap, and hooks/headers are pre-normalized (trimmed keys
// and values, no empties) — so the FS backend writing these out as <name>.md
// and re-discovering them answers byte-identically. Origin is deliberately
// UNSET here: each backend stamps its own admission tier, and the suite
// asserts only that it is non-empty.
var AgentFixture = []tool.AgentDef{
	{
		Name:        "echo",
		Description: "Repeats the task back with findings.",
		Body:        "Restate the task, then answer it directly.",
	},
	{
		Name:            "full-stack",
		Description:     "A fully-loaded specialist exercising every optional field.",
		Tools:           []string{"Read", "Grep", "Glob"},
		DisallowedTools: []string{"Write"},
		Model:           "sonnet",
		Provider:        "openrouter",
		PermissionMode:  "plan",
		MaxTurns:        7,
		MaxToolCalls:    21,
		Color:           "blue",
		Skills:          []string{"review", "research"},
		Hooks: map[string]string{
			"PreToolUse": "echo pre",
			"Stop":       "echo done",
		},
		Body: "You are a meticulous full-stack specialist.\nInspect before you conclude.",
	},
	{
		Name:        "mcp-ops",
		Description: "Operates over scoped MCP servers.",
		MCPServers: []tool.AgentMCPServer{
			{Name: "github"}, // reference: a configured main server's tools
			{
				Name:    "inline-auth",
				URL:     "https://example.test/mcp",
				Headers: map[string]string{"Authorization": "Bearer fixture-token"},
			},
		},
		Body: "Use the scoped MCP tools to inspect the project boards.",
	},
}

// fixtureAgent returns the fixture entry for name.
func fixtureAgent(t *testing.T, name string) tool.AgentDef {
	t.Helper()
	for _, d := range AgentFixture {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no fixture agent def %q", name)
	return tool.AgentDef{}
}

// RunAgentSource executes the shared AgentDefSource conformance table against
// the source produced by newSource. newSource must return a fresh source
// serving EXACTLY the canonical AgentFixture each call.
func RunAgentSource(t *testing.T, newSource func(t *testing.T) tool.AgentDefSource) {
	t.Helper()
	ctx := context.Background()

	t.Run("list matches fixture", func(t *testing.T) {
		src := newSource(t)
		defs, err := src.ListAgentDefs(ctx)
		if err != nil {
			t.Fatalf("ListAgentDefs: %v", err)
		}
		if len(defs) != len(AgentFixture) {
			t.Fatalf("ListAgentDefs returned %d defs, want %d: %+v", len(defs), len(AgentFixture), defs)
		}
		seen := map[string]bool{}
		for i, got := range defs {
			if seen[got.Name] {
				t.Errorf("duplicate agent name %q (names must be unique)", got.Name)
			}
			seen[got.Name] = true
			if i > 0 && defs[i-1].Name >= got.Name {
				t.Errorf("ListAgentDefs not sorted by name: %q before %q", defs[i-1].Name, got.Name)
			}
			if got.Origin == "" {
				t.Errorf("def %q Origin is empty (a tier label is required for observability)", got.Name)
			}
			// Deep-equal EVERY field except Origin (each backend stamps its own
			// admission tier).
			want := fixtureAgent(t, got.Name)
			want.Origin = got.Origin
			if !reflect.DeepEqual(got, want) {
				t.Errorf("def %q mismatch:\n got %+v\nwant %+v", got.Name, got, want)
			}
		}
	})

	t.Run("list deterministic", func(t *testing.T) {
		src := newSource(t)
		first, err := src.ListAgentDefs(ctx)
		if err != nil {
			t.Fatalf("ListAgentDefs #1: %v", err)
		}
		second, err := src.ListAgentDefs(ctx)
		if err != nil {
			t.Fatalf("ListAgentDefs #2: %v", err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("ListAgentDefs is not stable across calls (snapshot semantics):\n#1 %+v\n#2 %+v", first, second)
		}
	})

	t.Run("caps respected", func(t *testing.T) {
		// Every listed def must respect the canonical port-side caps
		// (tool.MaxAgentDescriptionBytes / tool.MaxAgentBodyBytes): the FS parser
		// truncates on discovery, the driver client re-truncates wire data, and
		// the fixture itself is authored under both — so an over-cap def
		// escaping ANY backend is a normalization regression.
		src := newSource(t)
		defs, err := src.ListAgentDefs(ctx)
		if err != nil {
			t.Fatalf("ListAgentDefs: %v", err)
		}
		for _, d := range defs {
			if len(d.Description) > tool.MaxAgentDescriptionBytes {
				t.Errorf("def %q Description is %d bytes, over tool.MaxAgentDescriptionBytes (%d)",
					d.Name, len(d.Description), tool.MaxAgentDescriptionBytes)
			}
			if len(d.Body) > tool.MaxAgentBodyBytes {
				t.Errorf("def %q Body is %d bytes, over tool.MaxAgentBodyBytes (%d)",
					d.Name, len(d.Body), tool.MaxAgentBodyBytes)
			}
		}
	})
}

// NewAgentFixtureSource returns the in-memory REFERENCE tool.AgentDefSource
// serving exactly the canonical AgentFixture, with the explicit tier stamped.
// It lives here (not in a _test.go file) because the driver conformance
// fixtures mount it behind a wire server; it is also the suite's self-test
// subject, so the suite cannot smuggle filesystem-shaped assumptions.
func NewAgentFixtureSource() tool.AgentDefSource {
	return agentFixtureSource{}
}

type agentFixtureSource struct{}

func (agentFixtureSource) ListAgentDefs(_ context.Context) ([]tool.AgentDef, error) {
	out := make([]tool.AgentDef, len(AgentFixture))
	copy(out, AgentFixture)
	for i := range out {
		out[i].Origin = tool.AgentOriginExplicit
	}
	return out, nil
}
