package client

import (
	"context"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// The agent-definition inventory discovery surface: a plain client-owned struct
// mirroring the proto AgentInfo message, the unary RPC wrapper that maps proto →
// the struct, and the tea.Cmd constructor the ui's /agents panel calls. As with
// the skills and MCP surfaces, NO proto type leaks past this file — the ui
// renders purely from the structs and msgs below, and the mapping is exercised
// offline against a fake client.

// Agent is one resolved agent definition (proto AgentInfo, proto-free): the
// def's routing name, its one-line routing description, the resolved provider
// model (empty = inherit parent), the raw frontmatter permission mode, the
// optional colour UX hint, and the effective read-only tool scope at the Task
// call site. Definitions are the routing targets for Task delegations — this is
// discovery only; activation stays the model's run-path concern.
type Agent struct {
	Name           string
	Description    string
	Model          string
	PermissionMode string
	Color          string
	Tools          []string
}

// AgentsMsg carries a ListAgents result for the /agents panel. Err is set on
// failure; the panel surfaces it rather than silently degrading, mirroring the
// skills panel's error handling.
type AgentsMsg struct {
	Agents []Agent
	Err    error
}

// ListAgents lists the resolved agent-definition inventory (a startup snapshot
// server-side).
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	resp, err := c.svc.ListAgents(ctx, &mecatlv1.ListAgentsRequest{})
	if err != nil {
		return nil, err
	}
	return mapAgents(resp.GetAgents()), nil
}

// mapAgents maps proto AgentInfos to the plain structs (nil-safe). The tools
// slice is copied into a fresh slice so the ui never shares backing storage with
// the proto response.
func mapAgents(in []*mecatlv1.AgentInfo) []Agent {
	out := make([]Agent, 0, len(in))
	for _, a := range in {
		var tools []string
		if src := a.GetTools(); len(src) > 0 {
			tools = make([]string, len(src))
			copy(tools, src)
		}
		out = append(out, Agent{
			Name:           a.GetName(),
			Description:    a.GetDescription(),
			Model:          a.GetModel(),
			PermissionMode: a.GetPermissionMode(),
			Color:          a.GetColor(),
			Tools:          tools,
		})
	}
	return out
}

// AgentLister is the subset of *Client the ui's /agents panel needs. Splitting
// it out keeps the ui injectable with a fake for offline tests; *Client
// satisfies it.
type AgentLister interface {
	ListAgents(ctx context.Context) ([]Agent, error)
}

// ListAgentsCmd fetches the agent-definition inventory off the update goroutine;
// the result (success or error) arrives as an AgentsMsg.
func ListAgentsCmd(ctx context.Context, c AgentLister) tea.Cmd {
	return func() tea.Msg {
		agents, err := c.ListAgents(ctx)
		if err != nil {
			return AgentsMsg{Err: err}
		}
		return AgentsMsg{Agents: agents}
	}
}
