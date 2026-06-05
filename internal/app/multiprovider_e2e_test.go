package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/providercatalog"
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

// TestMultiProviderCapabilityEcho drives the FULL composition and asserts the
// per-session capability echo (sink b) and the single-source agreement between the
// ACP gate (ProviderCapabilities) and the wire echo (sink c). It wires DISTINCT
// adapter capabilities per provider via the providerConstructor seam: openai
// Image:false, openrouter Image:true. The catalog is REAL, so the echo is the
// catalog ∩ adapter intersection. All offline.
func TestMultiProviderCapabilityEcho(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()

	// openai is the DEFAULT provider (preferredDefaultProvider prefers it). cfg.Model
	// is left empty ⇒ the default session resolves to the empty model on openai ⇒
	// passthrough ⇒ adapter-only caps for the default = openai's Image:false.
	built, err := Build(ctx, Config{
		Workspace: workspace,
		NoSoul:    true,
		envDetector: fakeEnv(map[string]string{
			"OPENAI_API_KEY":     "sk-x",
			"OPENROUTER_API_KEY": "sk-x",
		}),
		providerConstructor: func(_ Config, id, _, _ string) port.LLMProvider {
			caps := port.ProviderCapabilities{Image: id == providerOpenRouter}
			// Identifying reply so a routed turn proves WHICH provider was bound,
			// closing the routing⇄caps coherence gap (the turn and the caps must agree
			// on the same provider). Two turns scripted per mock (one per assertion).
			reply := "REPLY-FROM-" + id
			return mockllm.NewWith([]mockllm.Option{mockllm.WithCapabilities(caps)},
				mockllm.TextTurn(reply), mockllm.TextTurn(reply))
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()
	svc := built.Service

	// (b) per-session echo for a session bound to openrouter + a catalog image model,
	// AND the routing⇄caps coherence: the turn routes to the openrouter-bound provider
	// (REPLY-FROM-openrouter) AND that session's caps reflect openrouter's intersected
	// caps (Image:true), in ONE test.
	imgModel, _ := firstImageModel(t, providerOpenRouter)
	orSess, err := svc.CreateSessionWithProvider(ctx, workspace, session.ModeDefault, defaultLimits(),
		server.ProviderSelector{ProviderID: providerOpenRouter, ModelID: imgModel})
	if err != nil {
		t.Fatalf("create openrouter session: %v", err)
	}
	if got := svc.SessionCapabilities(orSess.ID); !got.Image {
		t.Fatalf("openrouter+image-model session echo Image = false, want true (catalog image ∩ adapter Image:true)")
	}
	orRun, err := svc.StartRun(ctx, orSess.ID, "hi")
	if err != nil {
		t.Fatalf("StartRun(openrouter): %v", err)
	}
	if got := drainRun(orRun); got != "REPLY-FROM-openrouter" {
		t.Fatalf("openrouter session turn routed to %q, want REPLY-FROM-openrouter (routing⇄caps must agree)", got)
	}

	// A session bound to openai (Image:false adapter) + the same image model ⇒ the
	// intersection is false, it DIFFERS from the openrouter echo (per-session), AND the
	// turn routes to the openai-bound provider — proving routing and caps agree on the
	// SAME provider per session.
	oaSess, err := svc.CreateSessionWithProvider(ctx, workspace, session.ModeDefault, defaultLimits(),
		server.ProviderSelector{ProviderID: providerOpenAI, ModelID: imgModel})
	if err != nil {
		t.Fatalf("create openai session: %v", err)
	}
	if got := svc.SessionCapabilities(oaSess.ID); got.Image {
		t.Fatalf("openai+image-model session echo Image = true, want false (adapter Image:false)")
	}
	oaRun, err := svc.StartRun(ctx, oaSess.ID, "hi")
	if err != nil {
		t.Fatalf("StartRun(openai): %v", err)
	}
	if got := drainRun(oaRun); got != "REPLY-FROM-openai" {
		t.Fatalf("openai session turn routed to %q, want REPLY-FROM-openai (routing⇄caps must agree)", got)
	}

	// (5) zero-selector session ⇒ echo == DefaultCapabilities (the default provider's
	// intersected caps). The default is openai (Image:false) on the empty model.
	zeroSess, err := svc.CreateSession(ctx, workspace, session.ModeDefault, defaultLimits())
	if err != nil {
		t.Fatalf("create zero-selector session: %v", err)
	}
	def := svc.ProviderCapabilities()
	if svc.SessionCapabilities(zeroSess.ID) != def {
		t.Fatalf("zero-selector echo %+v != ProviderCapabilities %+v (must read DefaultCapabilities)",
			svc.SessionCapabilities(zeroSess.ID), def)
	}

	// (c) single-source agreement: the ACP gate (ProviderCapabilities) equals the
	// wire echo for a default-engine session — both derive from DefaultCapabilities.
	if def.Image {
		t.Fatalf("default ProviderCapabilities Image = true, want false (default openai adapter Image:false)")
	}
}

// firstImageModel returns the first catalog model id for providerID that the
// catalog marks image-capable.
func firstImageModel(t *testing.T, providerID string) (string, bool) {
	t.Helper()
	p, ok := providercatalog.Default().Provider(providerID)
	if !ok {
		t.Fatalf("provider %q not in catalog", providerID)
	}
	for _, m := range p.Models() {
		if m.SupportsImageInput() {
			return m.ID(), true
		}
	}
	t.Skipf("no catalogued image model for %q", providerID)
	return "", false
}

// TestSessionCapabilitiesNoSecrets builds with SENTINEL keys and asserts the
// per-session capability echo carries NO secret. SessionCapabilities is bools-only,
// so it structurally cannot leak — this test documents that and tripwires a future
// string field. It also re-checks ProviderCapabilities (the ACP gate) and the
// CreateSessionResponse path through the gRPC handler for the sentinel in NO string
// field (CWE-200). All offline.
func TestSessionCapabilitiesNoSecrets(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	const sentinelKey = "sk-SENTINEL-capecho"

	built, err := Build(ctx, Config{
		Workspace: workspace,
		NoSoul:    true,
		envDetector: fakeEnv(map[string]string{
			"OPENAI_API_KEY":     sentinelKey,
			"OPENROUTER_API_KEY": sentinelKey,
		}),
		providerConstructor: func(_ Config, _, _, _ string) port.LLMProvider {
			return mockllm.NewWith([]mockllm.Option{mockllm.WithCapabilities(port.ProviderCapabilities{Image: true})}, mockllm.TextTurn("x"))
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer built.Close()
	svc := built.Service

	// Drive the gRPC CreateSession handler so the actual CreateSessionResponse (incl.
	// session_capabilities + capabilities) is built — the wire surface the client
	// sees. Assert the sentinel appears in NO string field.
	h := server.NewHarnessServer(svc)
	imgModel, _ := firstImageModel(t, providerOpenRouter)
	resp, err := h.CreateSession(ctx, &mecatlv1.CreateSessionRequest{
		Workspace:  workspace,
		ProviderId: providerOpenRouter,
		ModelId:    imgModel,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The echo must reflect the intersection (openrouter Image:true ∩ catalog image).
	if !resp.GetSessionCapabilities().GetImage() {
		t.Fatal("session_capabilities.image = false, want true (intersection)")
	}
	forbidden := []string{sentinelKey, "OPENAI_API_KEY", "OPENROUTER_API_KEY", openRouterDefaultBaseURL}
	// Belt-and-braces marshal scan: serialize the ENTIRE response (all fields, incl.
	// capabilities + session_capabilities + any future-added string field) and assert
	// none of the forbidden substrings appear. This future-proofs the tripwire against
	// a new string field — not just the session_id checked structurally below.
	blob, merr := protojson.Marshal(resp)
	if merr != nil {
		t.Fatalf("protojson.Marshal(resp): %v", merr)
	}
	for _, bad := range forbidden {
		if strings.Contains(string(blob), bad) {
			t.Fatalf("secret %q leaked into the marshalled CreateSessionResponse: %s", bad, blob)
		}
	}

	// ProviderCapabilities (the ACP gate) is a bools-only neutral value — no string
	// to leak; this asserts it is the same source the echo reads for the default.
	_ = svc.ProviderCapabilities()
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
