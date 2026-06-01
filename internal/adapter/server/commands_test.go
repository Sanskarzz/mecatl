package server_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/memfs"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/permpolicy"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/memstore"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/tool"
)

// stubCommandLister is a canned server.CommandLister for the ListCommands RPC
// tests: it returns a fixed slice (recording the root it was asked about), or a
// fixed error.
type stubCommandLister struct {
	cmds []server.Command
	err  error

	gotRoot string
}

func (s *stubCommandLister) List(_ context.Context, root string) ([]server.Command, error) {
	s.gotRoot = root
	return s.cmds, s.err
}

// commandsService builds a Service carrying the given command lister, mirroring
// agentsService for the ListCommands RPC.
func commandsService(t *testing.T, lister server.CommandLister) *server.Service {
	t.Helper()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(allowRules()),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      memstore.New(),
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:        func() time.Time { return time.Unix(0, 0) },
		Commands:   lister,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func cannedCommands() *stubCommandLister {
	return &stubCommandLister{cmds: []server.Command{
		{Name: "fix", Description: "fix a failing test"},
		{Name: "review", Description: "review a pull request"},
	}}
}

func TestGRPCListCommands(t *testing.T) {
	lister := cannedCommands()
	svc := commandsService(t, lister)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.ListCommands(context.Background(), &mecatlv1.ListCommandsRequest{Workspace: "/proj"})
	if err != nil {
		t.Fatalf("ListCommands: %v", err)
	}
	if len(resp.GetCommands()) != 2 {
		t.Fatalf("commands = %d, want 2", len(resp.GetCommands()))
	}
	if resp.GetCommands()[0].GetName() != "fix" || resp.GetCommands()[0].GetDescription() != "fix a failing test" {
		t.Fatalf("command[0] = %+v", resp.GetCommands()[0])
	}
	if resp.GetCommands()[1].GetName() != "review" {
		t.Fatalf("command[1] = %+v", resp.GetCommands()[1])
	}
	if lister.gotRoot != "/proj" {
		t.Fatalf("lister asked for root %q, want /proj", lister.gotRoot)
	}
}

func TestGRPCListCommandsNoLister(t *testing.T) {
	// No lister wired (command expansion disabled) => empty list, no error.
	svc := commandsService(t, nil)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.ListCommands(context.Background(), &mecatlv1.ListCommandsRequest{Workspace: "/proj"})
	if err != nil {
		t.Fatalf("ListCommands: %v", err)
	}
	if len(resp.GetCommands()) != 0 {
		t.Fatalf("commands = %d, want 0", len(resp.GetCommands()))
	}
}

func TestGRPCListCommandsEmptyWorkspace(t *testing.T) {
	// Empty workspace yields an empty list and never consults the lister.
	lister := cannedCommands()
	svc := commandsService(t, lister)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.ListCommands(context.Background(), &mecatlv1.ListCommandsRequest{})
	if err != nil {
		t.Fatalf("ListCommands: %v", err)
	}
	if len(resp.GetCommands()) != 0 {
		t.Fatalf("commands = %d, want 0", len(resp.GetCommands()))
	}
	if lister.gotRoot != "" {
		t.Fatalf("lister was consulted for an empty workspace (root=%q)", lister.gotRoot)
	}
}

func TestServiceListCommandsDiscoveryFault(t *testing.T) {
	// A discovery fault surfaces as ErrInternal so the wire adapters classify it.
	svc := commandsService(t, &stubCommandLister{err: errors.New("disk gone")})
	_, err := svc.ListCommands(context.Background(), "/proj")
	if !errors.Is(err, server.ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
}

func TestHTTPListCommands(t *testing.T) {
	svc := commandsService(t, cannedCommands())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	var resp mecatlv1.ListCommandsResponse
	if code := httpGet(t, srv, "/v1/commands?workspace=/proj", &resp); code != 200 {
		t.Fatalf("GET /v1/commands status = %d", code)
	}
	if len(resp.GetCommands()) != 2 {
		t.Fatalf("http commands = %d, want 2", len(resp.GetCommands()))
	}
	if resp.GetCommands()[0].GetName() != "fix" || resp.GetCommands()[1].GetName() != "review" {
		t.Fatalf("http commands = %+v", resp.GetCommands())
	}

	// No lister still returns 200 with an empty list.
	emptySrv := httptest.NewServer(server.NewHTTPHandler(commandsService(t, nil)))
	defer emptySrv.Close()
	var empty mecatlv1.ListCommandsResponse
	if code := httpGet(t, emptySrv, "/v1/commands?workspace=/proj", &empty); code != 200 {
		t.Fatalf("empty GET /v1/commands status = %d", code)
	}
	if len(empty.GetCommands()) != 0 {
		t.Fatalf("empty http commands = %d, want 0", len(empty.GetCommands()))
	}
}
