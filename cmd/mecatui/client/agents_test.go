package client

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// fakeAgentsClient is a scripted HarnessServiceClient for the ListAgents wrapper
// tests. It embeds the interface and overrides only the one RPC under test, so
// the proto→plain mapping runs offline.
type fakeAgentsClient struct {
	mecatlv1.HarnessServiceClient

	resp *mecatlv1.ListAgentsResponse
	err  error

	lastReq *mecatlv1.ListAgentsRequest
}

func (f *fakeAgentsClient) ListAgents(_ context.Context, in *mecatlv1.ListAgentsRequest, _ ...grpc.CallOption) (*mecatlv1.ListAgentsResponse, error) {
	f.lastReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestMapAgentsFieldsAndToolsCopy(t *testing.T) {
	src := []string{"Read", "Grep"}
	in := []*mecatlv1.AgentInfo{
		{
			Name:           "scout",
			Description:    "explore the codebase",
			Model:          "gpt-5",
			PermissionMode: "plan",
			Color:          "cyan",
			Tools:          src,
		},
	}
	out := mapAgents(in)
	if len(out) != 1 {
		t.Fatalf("agents = %d, want 1", len(out))
	}
	a := out[0]
	if a.Name != "scout" || a.Description != "explore the codebase" || a.Model != "gpt-5" ||
		a.PermissionMode != "plan" || a.Color != "cyan" {
		t.Fatalf("agent fields mismapped: %+v", a)
	}
	if len(a.Tools) != 2 || a.Tools[0] != "Read" || a.Tools[1] != "Grep" {
		t.Fatalf("tools = %v, want [Read Grep]", a.Tools)
	}
	// The mapped slice must be a COPY: mutating the proto source must not leak into
	// the plain struct.
	src[0] = "MUTATED"
	if a.Tools[0] != "Read" {
		t.Fatalf("tools slice shares backing storage with the proto response: %v", a.Tools)
	}
}

func TestMapAgentsNilSafe(t *testing.T) {
	if got := mapAgents(nil); len(got) != 0 {
		t.Fatalf("mapAgents(nil) = %v, want empty", got)
	}
	// An AgentInfo with no tools must yield a nil/empty Tools slice, never panic.
	out := mapAgents([]*mecatlv1.AgentInfo{{Name: "bare"}})
	if len(out) != 1 || out[0].Name != "bare" || len(out[0].Tools) != 0 {
		t.Fatalf("bare agent mismapped: %+v", out)
	}
}

func TestListAgentsMapping(t *testing.T) {
	fake := &fakeAgentsClient{resp: &mecatlv1.ListAgentsResponse{Agents: []*mecatlv1.AgentInfo{
		{Name: "scout", Description: "explore", Model: "gpt-5", Tools: []string{"Read"}},
		{Name: "writer", Description: "draft prose"},
	}}}
	cl := newFakeClient(fake)

	agents, err := cl.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if fake.lastReq == nil {
		t.Fatal("request not sent")
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
	if agents[0].Name != "scout" || agents[0].Model != "gpt-5" || agents[0].Tools[0] != "Read" {
		t.Fatalf("agents[0] = %+v", agents[0])
	}
	if agents[1].Name != "writer" || len(agents[1].Tools) != 0 {
		t.Fatalf("agents[1] = %+v", agents[1])
	}
}

func TestListAgentsMappingNilSafe(t *testing.T) {
	fake := &fakeAgentsClient{resp: &mecatlv1.ListAgentsResponse{}}
	cl := newFakeClient(fake)

	agents, err := cl.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("agents = %d, want 0 (empty response)", len(agents))
	}
}

func TestListAgentsCmdSuccess(t *testing.T) {
	fake := &fakeAgentsClient{resp: &mecatlv1.ListAgentsResponse{Agents: []*mecatlv1.AgentInfo{
		{Name: "scout", Description: "explore"},
	}}}
	cl := newFakeClient(fake)

	msg := ListAgentsCmd(context.Background(), cl)()
	am, ok := msg.(AgentsMsg)
	if !ok {
		t.Fatalf("msg type = %T, want AgentsMsg", msg)
	}
	if am.Err != nil {
		t.Fatalf("unexpected err: %v", am.Err)
	}
	if len(am.Agents) != 1 || am.Agents[0].Name != "scout" {
		t.Fatalf("agents = %+v", am.Agents)
	}
}

func TestListAgentsCmdError(t *testing.T) {
	fake := &fakeAgentsClient{err: errors.New("boom")}
	cl := newFakeClient(fake)

	msg := ListAgentsCmd(context.Background(), cl)()
	am, ok := msg.(AgentsMsg)
	if !ok {
		t.Fatalf("msg type = %T, want AgentsMsg", msg)
	}
	if am.Err == nil {
		t.Fatalf("expected an error in AgentsMsg")
	}
}
