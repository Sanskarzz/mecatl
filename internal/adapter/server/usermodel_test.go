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

// fakeUserModel is a scripted server.UserModelLister for the GetUserModel tests:
// it returns the canned entries (or an error), reflecting a LIVE store read.
type fakeUserModel struct {
	entries []server.UserModelEntry
	err     error
	calls   int
}

func (f *fakeUserModel) List(context.Context) ([]server.UserModelEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// userModelService builds a Service carrying the given user-model lister. A nil
// lister means the user model is disabled (capabilities().UserModel false;
// GetUserModel returns an empty response).
func userModelService(t *testing.T, lister server.UserModelLister) *server.Service {
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
		UserModel:  lister,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func cannedUserModel() *fakeUserModel {
	return &fakeUserModel{entries: []server.UserModelEntry{
		{Key: "name", Description: "the operator's name"},
		{Key: "stack", Description: "prefers Go + hexagonal architecture"},
	}}
}

func TestGRPCGetUserModel(t *testing.T) {
	fum := cannedUserModel()
	svc := userModelService(t, fum)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.GetUserModel(context.Background(), &mecatlv1.GetUserModelRequest{})
	if err != nil {
		t.Fatalf("GetUserModel: %v", err)
	}
	if len(resp.GetEntries()) != 2 {
		t.Fatalf("entries = %d, want 2", len(resp.GetEntries()))
	}
	if resp.GetEntries()[0].GetKey() != "name" || resp.GetEntries()[0].GetDescription() != "the operator's name" {
		t.Fatalf("entry[0] = %+v", resp.GetEntries()[0])
	}
	if resp.GetSizeBytes() == 0 || resp.GetSha256() == "" {
		t.Errorf("aggregate size/hash not populated: size=%d sha=%q", resp.GetSizeBytes(), resp.GetSha256())
	}
	if fum.calls != 1 {
		t.Errorf("List calls = %d, want 1 (live read)", fum.calls)
	}
}

func TestGRPCGetUserModelEmpty(t *testing.T) {
	// No lister wired (disabled) => empty response, no error.
	svc := userModelService(t, nil)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.GetUserModel(context.Background(), &mecatlv1.GetUserModelRequest{})
	if err != nil {
		t.Fatalf("GetUserModel: %v", err)
	}
	if len(resp.GetEntries()) != 0 {
		t.Fatalf("entries = %d, want 0", len(resp.GetEntries()))
	}
}

func TestGRPCGetUserModelError(t *testing.T) {
	svc := userModelService(t, &fakeUserModel{err: errors.New("boom")})
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	_, err := client.GetUserModel(context.Background(), &mecatlv1.GetUserModelRequest{})
	if err == nil {
		t.Fatal("GetUserModel should surface a store fault as an error")
	}
}

func TestHTTPGetUserModel(t *testing.T) {
	svc := userModelService(t, cannedUserModel())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	var resp mecatlv1.GetUserModelResponse
	if code := httpGet(t, srv, "/v1/usermodel", &resp); code != 200 {
		t.Fatalf("GET /v1/usermodel status = %d", code)
	}
	if len(resp.GetEntries()) != 2 {
		t.Fatalf("http entries = %d, want 2", len(resp.GetEntries()))
	}
}

// TestUserModelCapability asserts the user_model cap flips with a wired lister.
func TestUserModelCapability(t *testing.T) {
	on := capsFromCreate(t, userModelService(t, cannedUserModel()))
	if !on.GetUserModel() {
		t.Error("user_model cap = false, want true (lister wired)")
	}
	off := capsFromCreate(t, userModelService(t, nil))
	if off.GetUserModel() {
		t.Error("user_model cap = true, want false (no lister)")
	}
}
