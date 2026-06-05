package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// TestMultiProviderE2E drives the FULL composition (app.Build → server.Service)
// with TWO available real provider ids (openai / openrouter) backed by DISTINCT
// mock providers via the S3 composition-only providerConstructor seam, plus a fake
// two-key env. All offline (mockllm, real temp workspace, no network). It proves:
//
//  1. ListModels returns models for BOTH available providers, secret-free.
//  2. CreateSession{provider_id:"openrouter"} registers a per-session engine and a
//     turn routes to the OpenRouter-bound provider (distinct reply).
//  3. CreateSession{provider_id:"openai"} routes to the openai-bound provider.
//  4. CreateSession{provider_id:"anthropic"} (not in registry) ⇒ InvalidArgument.
//  5. CreateSession{} (zero selector) ⇒ the shared default-provider engine.
func TestMultiProviderE2E(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()

	const sentinelKey = "sk-SENTINEL-multiprovider"

	built, err := Build(ctx, Config{
		Workspace: workspace,
		NoSoul:    true,
		// Fake two-key env ⇒ openai + openrouter both AVAILABLE in the registry.
		envDetector: fakeEnv(map[string]string{
			"OPENAI_API_KEY":     sentinelKey,
			"OPENROUTER_API_KEY": sentinelKey,
		}),
		// Mock-per-id seam: each available provider id gets a DISTINCT mock that
		// replies with its own id, so a routed turn proves which provider was bound.
		// Several identical turns are scripted because the default (openai) provider
		// is exercised by more than one turn below (a mock exhausts after its turns).
		providerConstructor: func(_ Config, id, _, _ string) port.LLMProvider {
			reply := "REPLY-FROM-" + id
			return mockllm.New(
				mockllm.TextTurn(reply), mockllm.TextTurn(reply),
				mockllm.TextTurn(reply), mockllm.TextTurn(reply),
			)
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()
	svc := built.Service

	// (1) ListModels returns models for both providers, never the sentinel key.
	models := svc.ListModels(ctx)
	if len(models) == 0 {
		t.Fatal("ListModels returned no models for two available providers")
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.GetProviderId()] = true
		for _, f := range []string{m.GetId(), m.GetProviderId(), m.GetDisplayName()} {
			if strings.Contains(f, sentinelKey) {
				t.Fatalf("secret leaked into ListModels field %q", f)
			}
		}
	}
	if !seen[providerOpenAI] || !seen[providerOpenRouter] {
		t.Fatalf("ListModels missing a provider; saw %v", seen)
	}

	// (2) provider_id="openrouter" routes a turn to the OpenRouter-bound provider.
	if got := runProviderTurn(t, svc, workspace, server.ProviderSelector{ProviderID: providerOpenRouter}); got != "REPLY-FROM-openrouter" {
		t.Fatalf("openrouter selector routed to %q, want REPLY-FROM-openrouter", got)
	}

	// (3) provider_id="openai" routes to the openai-bound provider (distinct reply).
	if got := runProviderTurn(t, svc, workspace, server.ProviderSelector{ProviderID: providerOpenAI}); got != "REPLY-FROM-openai" {
		t.Fatalf("openai selector routed to %q, want REPLY-FROM-openai", got)
	}

	// (4) unknown provider ⇒ InvalidArgument (never a silent fallback).
	_, err = svc.CreateSessionWithProvider(ctx, workspace, session.ModeDefault, defaultLimits(),
		server.ProviderSelector{ProviderID: "anthropic"})
	if !errors.Is(err, server.ErrInvalidArgument) {
		t.Fatalf("unknown-provider create error = %v, want ErrInvalidArgument", err)
	}

	// (5) zero selector ⇒ the shared default-provider engine (openai is the default).
	zeroSess, err := svc.CreateSession(ctx, workspace, session.ModeDefault, defaultLimits())
	if err != nil {
		t.Fatalf("CreateSession(zero): %v", err)
	}
	run, err := svc.StartRun(ctx, zeroSess.ID, "hi")
	if err != nil {
		t.Fatalf("StartRun(zero): %v", err)
	}
	if got := drainRun(run); got != "REPLY-FROM-openai" {
		t.Fatalf("zero-selector turn routed to %q, want the default (openai) provider's reply", got)
	}
}

// runProviderTurn creates a session bound to sel, drives one turn, and returns the
// terminal text (so the test can assert which provider backed it).
func runProviderTurn(t *testing.T, svc *server.Service, workspace string, sel server.ProviderSelector) string {
	t.Helper()
	sess, err := svc.CreateSessionWithProvider(context.Background(), workspace, session.ModeDefault, defaultLimits(), sel)
	if err != nil {
		t.Fatalf("CreateSessionWithProvider(%+v): %v", sel, err)
	}
	run, err := svc.StartRun(context.Background(), sess.ID, "hi")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return drainRun(run)
}
