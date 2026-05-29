package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ozzv1 "github.com/stacklok/ozzharness/contracts/gen/go/ozz/v1"
	"github.com/stacklok/ozzharness/internal/adapter/mockllm"
	"github.com/stacklok/ozzharness/internal/adapter/server"
)

// createHTTPSession POSTs /v1/sessions and returns the new session id.
func createHTTPSession(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	body := strings.NewReader(`{"workspace":"/ws"}`)
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", body)
	if err != nil {
		t.Fatalf("POST /v1/sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if out.SessionID == "" {
		t.Fatalf("empty session id")
	}
	return out.SessionID
}

// parseSSE reads an SSE body and returns the decoded proto Events from each
// `data:` line until the stream ends.
func parseSSE(t *testing.T, r *bufio.Reader) []*ozzv1.Event {
	t.Helper()
	var out []*ozzv1.Event
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if data, ok := strings.CutPrefix(trimmed, "data: "); ok {
				var ev ozzv1.Event
				if jerr := json.Unmarshal([]byte(data), &ev); jerr != nil {
					t.Fatalf("decode SSE data %q: %v", data, jerr)
				}
				out = append(out, &ev)
			}
		}
		if err != nil {
			return out
		}
	}
}

// TestHTTPPromptSSE drives /v1/sessions then /v1/sessions/{id}/prompt and
// asserts the SSE stream carries the event taxonomy and a terminal result.
func TestHTTPPromptSSE(t *testing.T) {
	read := &scriptTool{name: "Read", readOnly: true, content: "body"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Read", `{"path":"a"}`)),
		mockllm.TextTurn("all done"),
	)
	svc := newService(t, llm, allowRules(), read)
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	id := createHTTPSession(t, srv)

	resp, err := http.Post(srv.URL+"/v1/sessions/"+id+"/prompt", "application/json",
		strings.NewReader(`{"text":"look"}`))
	if err != nil {
		t.Fatalf("POST prompt: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	events := parseSSE(t, bufio.NewReader(resp.Body))
	if !hasType(events, "tool.call") || !hasType(events, "tool.result") {
		t.Fatalf("missing tool events: %v", typesOf(events))
	}
	res := lastResult(t, events)
	if res.GetStop() != "end_turn" || res.GetText() != "all done" {
		t.Fatalf("result = %+v", res)
	}
}

// TestHTTPApprove mirrors the gRPC headline test over HTTP: a prompt pauses on
// a permission.ask; a concurrent POST /approve resolves it; the run completes.
func TestHTTPApprove(t *testing.T) {
	write := &scriptTool{name: "Write", readOnly: false, content: "wrote"}
	llm := mockllm.New(
		mockllm.ToolCallTurn(call("c1", "Write", `{"path":"a"}`)),
		mockllm.TextTurn("done"),
	)
	svc := newService(t, llm, nil, write) // nil rules => Ask
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	id := createHTTPSession(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/v1/sessions/"+id+"/prompt", strings.NewReader(`{"text":"go"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST prompt: %v", err)
	}
	defer resp.Body.Close()

	// Read the SSE stream incrementally; when the ask arrives, POST /approve.
	r := bufio.NewReader(resp.Body)
	var events []*ozzv1.Event
	var approved bool
	for {
		line, rerr := r.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok {
			var ev ozzv1.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("decode: %v", err)
			}
			events = append(events, &ev)
			if ev.GetType() == "permission.ask" && !approved {
				approved = true
				body, _ := json.Marshal(map[string]any{"ask_id": ev.GetAsk().GetAskId(), "allow": true})
				ar, aerr := http.Post(srv.URL+"/v1/sessions/"+id+"/approve",
					"application/json", bytes.NewReader(body))
				if aerr != nil {
					t.Fatalf("POST approve: %v", aerr)
				}
				ar.Body.Close()
				if ar.StatusCode != http.StatusNoContent {
					t.Fatalf("approve status = %d", ar.StatusCode)
				}
			}
			if ev.GetType() == "result" {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	if !approved {
		t.Fatalf("no permission.ask seen: %v", typesOf(events))
	}
	if !write.ran() {
		t.Fatalf("approved tool did not run")
	}
	res := lastResult(t, events)
	if res.GetStop() != "end_turn" {
		t.Fatalf("stop = %q, want end_turn", res.GetStop())
	}
}

// TestHTTPGetSession round-trips a created session through GET.
func TestHTTPGetSession(t *testing.T) {
	svc := newService(t, mockllm.New(), allowRules())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	id := createHTTPSession(t, srv)
	resp, err := http.Get(srv.URL + "/v1/sessions/" + id)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SessionID != id || out.State != "idle" || out.Workspace != "/ws" {
		t.Fatalf("snapshot = %+v", out)
	}
}

// TestHTTPGetSessionNotFound returns 404 for an unknown id.
func TestHTTPGetSessionNotFound(t *testing.T) {
	svc := newService(t, mockllm.New(), allowRules())
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/sessions/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
