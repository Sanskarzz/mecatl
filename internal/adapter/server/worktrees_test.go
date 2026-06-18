package server_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// stubWorktreeLister is a canned server.WorktreeLister for the ListWorktrees RPC
// tests: it returns a fixed slice (recording the root it was asked about), or a
// fixed error.
type stubWorktreeLister struct {
	wts []server.Worktree
	err error

	gotRoot string
}

func (s *stubWorktreeLister) List(_ context.Context, root string) ([]server.Worktree, error) {
	s.gotRoot = root
	return s.wts, s.err
}

// worktreesService builds a Service carrying the given worktree lister, mirroring
// commandsService.
func worktreesService(t *testing.T, lister server.WorktreeLister) *server.Service {
	t.Helper()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(allowRules(), nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:     engine,
		Store:      memstore.New(),
		Workspaces: func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:        func() time.Time { return time.Unix(0, 0) },
		Worktrees:  lister,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func cannedWorktrees() *stubWorktreeLister {
	return &stubWorktreeLister{wts: []server.Worktree{
		{Path: "/repo", Branch: "main", Head: "abcdef1"},
		{Path: "/repo-wt", Branch: "feature", Head: "1234567"},
	}}
}

func TestGRPCListWorktrees(t *testing.T) {
	lister := cannedWorktrees()
	svc := worktreesService(t, lister)
	cl, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := cl.ListWorktrees(context.Background(), &mecatlv1.ListWorktreesRequest{Workspace: "/repo"})
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(resp.GetWorktrees()) != 2 {
		t.Fatalf("worktrees = %d, want 2", len(resp.GetWorktrees()))
	}
	if resp.GetWorktrees()[0].GetPath() != "/repo" || resp.GetWorktrees()[0].GetBranch() != "main" {
		t.Fatalf("worktree[0] = %+v", resp.GetWorktrees()[0])
	}
	if resp.GetWorktrees()[1].GetPath() != "/repo-wt" || resp.GetWorktrees()[1].GetHead() != "1234567" {
		t.Fatalf("worktree[1] = %+v", resp.GetWorktrees()[1])
	}
	if lister.gotRoot != "/repo" {
		t.Fatalf("lister asked for root %q, want /repo", lister.gotRoot)
	}
}

func TestGRPCListWorktreesNoLister(t *testing.T) {
	// No lister wired (worktree discovery disabled — a no-FS/cloud server) => empty
	// list, no error.
	svc := worktreesService(t, nil)
	cl, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := cl.ListWorktrees(context.Background(), &mecatlv1.ListWorktreesRequest{Workspace: "/repo"})
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(resp.GetWorktrees()) != 0 {
		t.Fatalf("worktrees = %d, want 0", len(resp.GetWorktrees()))
	}
}

func TestGRPCListWorktreesEmptyWorkspace(t *testing.T) {
	// Empty workspace yields an empty list and never consults the lister.
	lister := cannedWorktrees()
	svc := worktreesService(t, lister)
	cl, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := cl.ListWorktrees(context.Background(), &mecatlv1.ListWorktreesRequest{})
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(resp.GetWorktrees()) != 0 {
		t.Fatalf("worktrees = %d, want 0", len(resp.GetWorktrees()))
	}
	if lister.gotRoot != "" {
		t.Fatalf("lister was consulted for an empty workspace (root=%q)", lister.gotRoot)
	}
}

func TestServiceListWorktreesDiscoveryFault(t *testing.T) {
	// A discovery fault surfaces as ErrInternal so the wire adapters classify it.
	svc := worktreesService(t, &stubWorktreeLister{err: errors.New("git gone")})
	_, err := svc.ListWorktrees(context.Background(), "/repo")
	if !errors.Is(err, server.ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
}

func TestHTTPListWorktrees(t *testing.T) {
	svc := worktreesService(t, cannedWorktrees())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	var resp mecatlv1.ListWorktreesResponse
	if code := httpGet(t, srv, "/v1/worktrees?workspace=/repo", &resp); code != 200 {
		t.Fatalf("GET /v1/worktrees status = %d", code)
	}
	if len(resp.GetWorktrees()) != 2 {
		t.Fatalf("http worktrees = %d, want 2", len(resp.GetWorktrees()))
	}
	if resp.GetWorktrees()[0].GetPath() != "/repo" || resp.GetWorktrees()[1].GetPath() != "/repo-wt" {
		t.Fatalf("http worktrees = %+v", resp.GetWorktrees())
	}

	// No lister still returns 200 with an empty list (cloud/no-FS posture).
	emptySrv := httptest.NewServer(server.NewHTTPHandler(worktreesService(t, nil)))
	defer emptySrv.Close()
	var empty mecatlv1.ListWorktreesResponse
	if code := httpGet(t, emptySrv, "/v1/worktrees?workspace=/repo", &empty); code != 200 {
		t.Fatalf("empty GET /v1/worktrees status = %d", code)
	}
	if len(empty.GetWorktrees()) != 0 {
		t.Fatalf("empty http worktrees = %d, want 0", len(empty.GetWorktrees()))
	}
}

// TestServiceListWorktreesWorkspaceClamped asserts that when DefaultWorkspace is
// configured, a request for a DIFFERENT workspace returns an empty list and does
// NOT call the lister (security clamp: no git shell-out against an arbitrary path).
func TestServiceListWorktreesWorkspaceClamped(t *testing.T) {
	lister := cannedWorktrees()
	engine := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(allowRules(), nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:           engine,
		Store:            memstore.New(),
		Workspaces:       func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		Now:              func() time.Time { return time.Unix(0, 0) },
		Worktrees:        lister,
		DefaultWorkspace: "/repo/main",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	wts, err := svc.ListWorktrees(context.Background(), "/some/other/path")
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(wts) != 0 {
		t.Fatalf("worktrees = %d, want 0 (clamped)", len(wts))
	}
	if lister.gotRoot != "" {
		t.Fatalf("lister was called (root=%q), want NOT called for a non-default workspace", lister.gotRoot)
	}
}

// TestCapabilitiesWorktrees asserts the ServerCapabilities.worktrees bit mirrors
// whether a lister is wired (the honest affordance gate for the client overlay).
// It drives the bit through the CreateSession response (the shared create path),
// mirroring capsFromCreate.
func TestCapabilitiesWorktrees(t *testing.T) {
	with := worktreesService(t, cannedWorktrees())
	if caps := capsFromCreate(t, with); !caps.GetWorktrees() {
		t.Error("with lister: caps.Worktrees = false, want true")
	}
	without := worktreesService(t, nil)
	if caps := capsFromCreate(t, without); caps.GetWorktrees() {
		t.Error("without lister: caps.Worktrees = true, want false")
	}
}
