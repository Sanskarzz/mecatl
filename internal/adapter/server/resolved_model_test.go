package server_test

import (
	"context"
	"testing"
	"time"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/engine/adapter/memfs"
	"github.com/stacklok/mecatl/engine/adapter/memstore"
	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/adapter/permpolicy"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// newResolvedModelService builds a Service whose shared/default engine resolves to
// DefaultResolvedModel, with the supplied per-session factory. It mirrors
// newMCPServiceStore but pins a DefaultResolvedModel so the default-path echo can be
// asserted.
func newResolvedModelService(t *testing.T, dflt server.ResolvedModel, factory server.SessionEngineFactory) *server.Service {
	t.Helper()
	shared := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("SHARED")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "test-model",
	})
	svc, err := server.NewService(server.Config{
		Engine:               shared,
		Store:                memstore.New(),
		Workspaces:           func(root string) tool.Workspace { return memfs.NewWorkspace(root) },
		DefaultLimits:        session.Limits{MaxTurns: 10, MaxToolCalls: 20},
		Now:                  func() time.Time { return time.Unix(0, 0) },
		SessionEngine:        factory,
		DefaultResolvedModel: dflt,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// TestServiceResolvedModelDefaultPath: a zero-selector session (no per-session
// engine) reports Config.DefaultResolvedModel verbatim — the same composition
// single-source discipline as SessionCapabilities. The value is NOT read back from
// the request (which carries an empty model_id for the default session).
func TestServiceResolvedModelDefaultPath(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	svc := newResolvedModelService(t, dflt, nil)

	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, server.ProviderSelector{})
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(zero): %v", err)
	}
	got := svc.ResolvedModel(sess.ID)
	if got != dflt {
		t.Fatalf("ResolvedModel(default session) = %+v, want %+v (the composition DefaultResolvedModel)", got, dflt)
	}
	// An unregistered id also falls back to the default (mirrors SessionCapabilities).
	if got := svc.ResolvedModel("no-such-session"); got != dflt {
		t.Fatalf("ResolvedModel(unknown) = %+v, want the default %+v", got, dflt)
	}
}

// TestServiceResolvedModelPerSession: an explicit selector registers a per-session
// engine carrying the factory's resolved ProviderID/ModelID/ContextWindow, and
// ResolvedModel returns THAT (not the default). After CloseSession evicts the
// per-session engine, it falls back to the default.
func TestServiceResolvedModelPerSession(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	perSession := agent.NewEngine(agent.Deps{
		LLM:     mockllm.New(mockllm.TextTurn("PER-SESSION")),
		Catalog: tool.NewCatalog(),
		Policy:  permpolicy.NewPolicy(nil, nil),
		Model:   "anthropic/claude-opus-4.5",
	})
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        perSession,
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelService(t, dflt, factory)

	sel := server.ProviderSelector{ProviderID: "anthropic", ModelID: "claude-opus-4.5"}
	sess, err := svc.CreateSessionWithProvider(context.Background(), "/ws", session.ModeDefault, session.Limits{}, sel)
	if err != nil {
		t.Fatalf("CreateSessionWithProvider: %v", err)
	}
	want := server.ResolvedModel{ProviderID: "anthropic", ModelID: "claude-opus-4.5", ContextWindow: 200000}
	if got := svc.ResolvedModel(sess.ID); got != want {
		t.Fatalf("ResolvedModel(per-session) = %+v, want the resolved selector %+v", got, want)
	}
	svc.CloseSession(sess.ID)
	if got := svc.ResolvedModel(sess.ID); got != dflt {
		t.Fatalf("ResolvedModel after CloseSession = %+v, want fallback to default %+v", got, dflt)
	}
}

// TestGRPCCreateSessionEchoesResolvedModel: the gRPC CreateSession handler echoes
// resolved_model from Service.ResolvedModel for both the default path AND an
// explicit selector — asserting the EFFECTIVE (resolved) values cross the wire, not
// the raw request fields.
func TestGRPCCreateSessionEchoesResolvedModel(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        agent.NewEngine(agent.Deps{LLM: mockllm.New(mockllm.TextTurn("X")), Catalog: tool.NewCatalog(), Policy: permpolicy.NewPolicy(nil, nil), Model: "m"}),
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelService(t, dflt, factory)
	h := server.NewHarnessServer(svc)

	t.Run("default selector echoes the composition default", func(t *testing.T) {
		resp, err := h.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		rm := resp.GetResolvedModel()
		if rm.GetProviderId() != "openai" || rm.GetModelId() != "gpt-default" || rm.GetContextWindow() != 128000 {
			t.Fatalf("resolved_model = %+v, want the composition default openai/gpt-default/128000", rm)
		}
	})

	t.Run("explicit selector echoes the resolved values, not the raw request model", func(t *testing.T) {
		// The request model_id is "claude-opus-4.5"; the factory resolved it under the
		// "anthropic" provider with a 200000 window. The echo must reflect the RESOLVED
		// values from Service.ResolvedModel, not be a naive read-back of the request.
		resp, err := h.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{
			Workspace:  "/ws",
			ProviderId: "anthropic",
			ModelId:    "claude-opus-4.5",
		})
		if err != nil {
			t.Fatalf("CreateSession(selector): %v", err)
		}
		rm := resp.GetResolvedModel()
		if rm.GetProviderId() != "anthropic" || rm.GetModelId() != "claude-opus-4.5" || rm.GetContextWindow() != 200000 {
			t.Fatalf("resolved_model = %+v, want anthropic/claude-opus-4.5/200000", rm)
		}
	})
}

// TestGRPCGetSessionEchoesResolvedModel proves the gRPC GetSession handler threads
// Service.ResolvedModel(sess.ID) into the Session snapshot (toProtoSession), not a
// zero ResolvedModel. Uses an explicit selector so the per-session resolved value is
// distinct from the default — a handler dropping it would echo zero and fail.
func TestGRPCGetSessionEchoesResolvedModel(t *testing.T) {
	dflt := server.ResolvedModel{ProviderID: "openai", ModelID: "gpt-default", ContextWindow: 128000}
	factory := func(_ context.Context, sel server.ProviderSelector, _ []mcp.ServerConfig, _ server.SessionProfile, _ string) (server.SessionEngineResult, error) {
		return server.SessionEngineResult{
			Engine:        agent.NewEngine(agent.Deps{LLM: mockllm.New(mockllm.TextTurn("X")), Catalog: tool.NewCatalog(), Policy: permpolicy.NewPolicy(nil, nil), Model: "m"}),
			ProviderID:    sel.ProviderID,
			ModelID:       sel.ModelID,
			ContextWindow: 200000,
			Close:         func() error { return nil },
		}, nil
	}
	svc := newResolvedModelService(t, dflt, factory)
	h := server.NewHarnessServer(svc)

	createResp, err := h.CreateSession(context.Background(), &mecatlv1.CreateSessionRequest{
		Workspace:  "/ws",
		ProviderId: "anthropic",
		ModelId:    "claude-opus-4.5",
	})
	if err != nil {
		t.Fatalf("CreateSession(selector): %v", err)
	}
	getResp, err := h.GetSession(context.Background(), &mecatlv1.GetSessionRequest{SessionId: createResp.GetSessionId()})
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	rm := getResp.GetSession().GetResolvedModel()
	if rm.GetProviderId() != "anthropic" || rm.GetModelId() != "claude-opus-4.5" || rm.GetContextWindow() != 200000 {
		t.Fatalf("Session snapshot resolved_model = %+v, want the Service-resolved anthropic/claude-opus-4.5/200000 (handler must thread Service.ResolvedModel, not zero)", rm)
	}
}
