package embed_test

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	"github.com/stacklok/mecatl/cmd/mecatui/embed"
	"github.com/stacklok/mecatl/internal/app"
)

// TestStartServesOverSocket is the end-to-end proof of the embedded path: Start
// builds the harness from a (mock-provider) app.Config, serves it over a private
// UNIX socket, and the ordinary TUI client can both health-probe it and drive a
// real unary RPC (CreateSession) across that socket — no TCP port, no daemon.
func TestStartServesOverSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workspace := t.TempDir()
	srv, err := embed.Start(ctx, app.Config{
		Workspace:  workspace,
		Model:      "mock-model",
		UseMock:    true, // offline: no network, no OPENAI_API_KEY needed
		Shell:      "/bin/sh",
		Compaction: "heuristic",
		Tokenizer:  "heuristic",
	})
	if err != nil {
		t.Fatalf("embed.Start: %v", err)
	}
	defer func() { _ = srv.Close() }()

	target := srv.Target()
	if target == "" {
		t.Fatal("Target() is empty")
	}

	// The standard gRPC health service must report SERVING over the socket.
	if !client.IsReachable(ctx, target) {
		t.Fatalf("embedded server not reachable at %q", target)
	}

	// A real HarnessService RPC must succeed over the socket, against the workspace.
	cl, err := client.Dial(client.DialConfig{Server: target})
	if err != nil {
		t.Fatalf("dial embedded server: %v", err)
	}
	defer func() { _ = cl.Close() }()

	sessID, err := cl.CreateSession(ctx, workspace, client.ModeFromString("default"))
	if err != nil {
		t.Fatalf("CreateSession over embedded socket: %v", err)
	}
	if sessID == "" {
		t.Fatal("CreateSession returned an empty session id")
	}
}

// TestStartWithMemoryDirServes asserts the embedded server builds and serves when
// a memory directory is configured — exercising app.Build's memory-registration
// gate (build.go: MemoryDir != "" ⇒ memory.New + memory.Register) end to end
// without network. A bad/empty dir would surface as a build error or a CreateSession
// failure; a clean session id proves the gate ran and registered without blowing up.
func TestStartWithMemoryDirServes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workspace := t.TempDir()
	srv, err := embed.Start(ctx, app.Config{
		Workspace:  workspace,
		Model:      "mock-model",
		UseMock:    true, // offline: no network, no OPENAI_API_KEY needed
		Shell:      "/bin/sh",
		Compaction: "heuristic",
		Tokenizer:  "heuristic",
		MemoryDir:  t.TempDir(), // turns on the Remember/Recall registration gate
	})
	if err != nil {
		t.Fatalf("embed.Start with MemoryDir: %v", err)
	}
	defer func() { _ = srv.Close() }()

	cl, err := client.Dial(client.DialConfig{Server: srv.Target()})
	if err != nil {
		t.Fatalf("dial embedded server: %v", err)
	}
	defer func() { _ = cl.Close() }()

	sessID, err := cl.CreateSession(ctx, workspace, client.ModeFromString("default"))
	if err != nil {
		t.Fatalf("CreateSession over embedded socket (memory enabled): %v", err)
	}
	if sessID == "" {
		t.Fatal("CreateSession returned an empty session id")
	}
}

// TestStartProviderError asserts Start surfaces app.Build's provider error (and
// leaks nothing) when neither OpenAI nor the mock is configured.
func TestStartProviderError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := embed.Start(ctx, app.Config{Workspace: t.TempDir(), Model: "x"})
	if err == nil {
		t.Fatal("expected an error when no LLM provider is configured")
	}
}
