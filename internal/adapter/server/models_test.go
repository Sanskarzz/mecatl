package server_test

import (
	"context"
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

// modelsService builds a Service carrying the given selectable-model snapshot,
// mirroring agentsService but for the ListModels RPC.
func modelsService(t *testing.T, snapshot []*mecatlv1.ModelInfo) *server.Service {
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
		Models:     snapshot,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func cannedModels() []*mecatlv1.ModelInfo {
	return []*mecatlv1.ModelInfo{
		{
			Id:           "gpt-5",
			ProviderId:   "openai",
			DisplayName:  "GPT-5",
			Image:        true,
			Reasoning:    true,
			ContextLimit: 400000,
		},
		{
			Id:           "anthropic/claude-opus-4.5",
			ProviderId:   "openrouter",
			DisplayName:  "Claude Opus 4.5",
			Image:        true,
			Reasoning:    false,
			ContextLimit: 200000,
		},
	}
}

func TestGRPCListModels(t *testing.T) {
	svc := modelsService(t, cannedModels())
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.ListModels(context.Background(), &mecatlv1.ListModelsRequest{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(resp.GetModels()) != 2 {
		t.Fatalf("models = %d, want 2", len(resp.GetModels()))
	}
	first := resp.GetModels()[0]
	if first.GetId() != "gpt-5" || first.GetProviderId() != "openai" || first.GetDisplayName() != "GPT-5" {
		t.Fatalf("model[0] metadata = %+v", first)
	}
	if !first.GetImage() || !first.GetReasoning() || first.GetContextLimit() != 400000 {
		t.Fatalf("model[0] catalog-derived fields = %+v", first)
	}
	second := resp.GetModels()[1]
	if second.GetProviderId() != "openrouter" || second.GetReasoning() {
		t.Fatalf("model[1] = %+v", second)
	}
}

func TestGRPCListModelsEmpty(t *testing.T) {
	// No snapshot (zero providers available) => empty list, no error.
	svc := modelsService(t, nil)
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	resp, err := client.ListModels(context.Background(), &mecatlv1.ListModelsRequest{})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(resp.GetModels()) != 0 {
		t.Fatalf("empty models = %d, want 0", len(resp.GetModels()))
	}
}

func TestHTTPListModels(t *testing.T) {
	svc := modelsService(t, cannedModels())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	var resp mecatlv1.ListModelsResponse
	if code := httpGet(t, srv, "/v1/models", &resp); code != 200 {
		t.Fatalf("GET /v1/models status = %d", code)
	}
	if len(resp.GetModels()) != 2 {
		t.Fatalf("http models = %d, want 2", len(resp.GetModels()))
	}
	if resp.GetModels()[0].GetId() != "gpt-5" || resp.GetModels()[1].GetProviderId() != "openrouter" {
		t.Fatalf("http models = %+v", resp.GetModels())
	}

	// Empty snapshot still returns 200 with an empty list.
	emptySrv := httptest.NewServer(server.NewHTTPHandler(modelsService(t, nil)))
	defer emptySrv.Close()
	var empty mecatlv1.ListModelsResponse
	if code := httpGet(t, emptySrv, "/v1/models", &empty); code != 200 {
		t.Fatalf("empty GET /v1/models status = %d", code)
	}
	if len(empty.GetModels()) != 0 {
		t.Fatalf("empty http models = %d, want 0", len(empty.GetModels()))
	}
}

// TestCapabilitiesModelSelection asserts the model_selection cap flips with a
// non-empty Config.Models snapshot (mirrors the agents cap test).
func TestCapabilitiesModelSelection(t *testing.T) {
	on := capsFromCreate(t, modelsService(t, cannedModels()))
	if !on.GetModelSelection() {
		t.Errorf("model_selection cap = false, want true (non-empty Models snapshot)")
	}
	off := capsFromCreate(t, modelsService(t, nil))
	if off.GetModelSelection() {
		t.Errorf("model_selection cap = true, want false (empty Models snapshot)")
	}
}
