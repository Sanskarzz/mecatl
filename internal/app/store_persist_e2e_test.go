package app

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/store/jsonlstore"
)

// TestStorePersistsAcrossBuildsE2E is the issue-#79 durable-store gate through
// the FULL composition (app.Build + server.Service over the HTTP SSE relay),
// offline over a real on-disk jsonlstore. It proves a session driven to a
// terminal result in one process is recoverable from the SAME StoreDir after a
// restart: both the latest snapshot (SessionStore.Load) AND the relayed event
// timeline (EventLog.Read) survive.
//
//  1. built1 over a shared StoreDir. The model returns one text turn that ends
//     the run cleanly. Drive it to the terminal EvResult over the HTTP /prompt
//     relay (the relay is what Appends every event to the durable EventLog —
//     the loop itself never does), then close (process death: in-memory state
//     gone, only the durable files remain).
//  2. A FRESH jsonlstore over the SAME dir Loads the session (snapshot present,
//     completed) and EventLog.Read yields the recorded events (incl. the
//     terminal EvResult). Reading the store directly (not via a second Build)
//     keeps the assertion focused on durability, not on the rehydration path.
func TestStorePersistsAcrossBuildsE2E(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	memoryDir := t.TempDir()
	workspace := t.TempDir()

	cfg := Config{
		Workspace:           workspace,
		NoSoul:              true,
		StoreDir:            storeDir,
		MemoryDir:           memoryDir,
		envDetector:         fakeEnv(map[string]string{"OPENAI_API_KEY": "sk-openai"}),
		liveModelHTTPClient: offlineHTTPClient(),
		providerConstructor: func(_ Config, _, _, _ string) port.LLMProvider {
			return mockllm.New(mockllm.TextTurn("all done, no tools needed"))
		},
	}
	built1, err := Build(ctx, cfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	sess, err := built1.Service.CreateSession(ctx, workspace, session.ModeDefault, session.Limits{})
	if err != nil {
		built1.Close()
		t.Fatalf("CreateSession: %v", err)
	}

	// Drive the run over /prompt: the relay Appends every event to the durable
	// EventLog. The text turn ends cleanly, so the stream terminates on its own.
	srv1 := httptest.NewServer(server.NewHTTPHandler(built1.Service))
	var sawResult bool
	promptOverHTTP(t, srv1.URL, string(sess.ID), "say hello", func(ev sseEvent) {
		if ev.Type == "result" {
			sawResult = true
		}
	})
	srv1.Close()
	if !sawResult {
		built1.Close()
		t.Fatal("built1 run produced no terminal result event over the relay")
	}
	built1.Close() // process death: only the durable files remain.

	// A fresh store over the SAME dir: the snapshot Loads back, completed.
	store, err := jsonlstore.New(storeDir)
	if err != nil {
		t.Fatalf("reopen jsonlstore: %v", err)
	}
	loaded, err := store.Load(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Load after restart: %v (the session snapshot did not persist)", err)
	}
	if loaded.State != session.StateCompleted {
		t.Fatalf("loaded state = %q, want completed", loaded.State)
	}

	// The durable event log replays the recorded timeline, including the result.
	var logged int
	var loggedResult bool
	for ev, rerr := range store.Read(ctx, sess.ID) {
		if rerr != nil {
			t.Fatalf("EventLog.Read: %v", rerr)
		}
		logged++
		if ev.Type == session.EvResult {
			loggedResult = true
		}
	}
	if logged == 0 {
		t.Fatal("EventLog.Read yielded no events — the durable event log did not persist")
	}
	if !loggedResult {
		t.Errorf("EventLog.Read yielded %d events but none was the terminal EvResult", logged)
	}
}
