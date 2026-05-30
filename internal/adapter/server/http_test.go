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

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/server"
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
func parseSSE(t *testing.T, r *bufio.Reader) []*mecatlv1.Event {
	t.Helper()
	var out []*mecatlv1.Event
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if data, ok := strings.CutPrefix(trimmed, "data: "); ok {
				var ev mecatlv1.Event
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
	var events []*mecatlv1.Event
	var approved bool
	for {
		line, rerr := r.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: "); ok {
			var ev mecatlv1.Event
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

// --- team HTTP/SSE parity ----------------------------------------------------

// parseTeamSSE reads an SSE body and returns the decoded proto TeamEvents from
// each `data:` line until the stream ends — the team analogue of parseSSE.
func parseTeamSSE(t *testing.T, r *bufio.Reader) []*mecatlv1.TeamEvent {
	t.Helper()
	var out []*mecatlv1.TeamEvent
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if data, ok := strings.CutPrefix(trimmed, "data: "); ok {
				var te mecatlv1.TeamEvent
				if jerr := json.Unmarshal([]byte(data), &te); jerr != nil {
					t.Fatalf("decode SSE data %q: %v", data, jerr)
				}
				out = append(out, &te)
			}
		}
		if err != nil {
			return out
		}
	}
}

// TestHTTPTeamLifecycle drives the full team REST surface: create with a roster
// (lead + read-only worker) → 201 with the enrolled members; GET lists them;
// POST /run streams TeamEvents and closes; POST /messages → 204; DELETE → 204.
func TestHTTPTeamLifecycle(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("done"))
	svc := teamService(t, llm)
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	// POST /v1/teams with an initial roster → 201, body has team_id + members.
	createBody := `{"workspace":"/ws","name":"test","members":[` +
		`{"name":"lead","lead":true,"initial_prompt":"go"},` +
		`{"name":"worker"}]}`
	resp, err := http.Post(srv.URL+"/v1/teams", "application/json", strings.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /v1/teams: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created mecatlv1.CreateTeamResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	teamID := created.GetTeamId()
	if teamID == "" {
		t.Fatal("empty team id")
	}
	if got := created.GetMembers(); len(got) != 2 || got[0].GetName() != "lead" || got[1].GetName() != "worker" {
		t.Fatalf("create members = %v, want [lead worker]", created.GetMembers())
	}

	// GET /v1/teams/{id} → 200 lists both members.
	gresp, err := http.Get(srv.URL + "/v1/teams/" + teamID)
	if err != nil {
		t.Fatalf("GET /v1/teams/{id}: %v", err)
	}
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", gresp.StatusCode)
	}
	var listed mecatlv1.ListTeamResponse
	if err := json.NewDecoder(gresp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.GetMembers()) != 2 {
		t.Fatalf("list members = %d, want 2", len(listed.GetMembers()))
	}

	// POST /v1/teams/{id}/messages → 204 (operator → lead).
	msgBody := `{"to":"lead","body":"ping"}`
	mresp, err := http.Post(srv.URL+"/v1/teams/"+teamID+"/messages", "application/json", strings.NewReader(msgBody))
	if err != nil {
		t.Fatalf("POST messages: %v", err)
	}
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusNoContent {
		t.Fatalf("messages status = %d, want 204", mresp.StatusCode)
	}

	// POST /v1/teams/{id}/run → 200 text/event-stream; at least the lead's events
	// arrive (a result frame) and the stream closes.
	rresp, err := http.Post(srv.URL+"/v1/teams/"+teamID+"/run", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, want 200", rresp.StatusCode)
	}
	if ct := rresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("run content-type = %q", ct)
	}
	events := parseTeamSSE(t, bufio.NewReader(rresp.Body))
	var sawLeadResult bool
	for _, te := range events {
		if te.GetMember() == "lead" && te.GetEvent().GetType() == "result" {
			sawLeadResult = true
		}
	}
	if !sawLeadResult {
		t.Fatalf("no lead result frame in stream of %d events", len(events))
	}

	// DELETE /v1/teams/{id} → 204.
	dreq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/teams/"+teamID, nil)
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", dresp.StatusCode)
	}
}

// TestHTTPTeamsDisabled asserts POST /v1/teams against a Service with no
// MemberEngine → 412 Precondition Failed (ErrTeamsDisabled).
func TestHTTPTeamsDisabled(t *testing.T) {
	svc := newService(t, mockllm.New(), allowRules()) // no MemberEngine wired
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/teams", "application/json", strings.NewReader(`{"workspace":"/ws"}`))
	if err != nil {
		t.Fatalf("POST /v1/teams: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", resp.StatusCode)
	}
}

// TestHTTPTeamNotFound asserts GET /v1/teams/{id} on an unknown id → 404.
func TestHTTPTeamNotFound(t *testing.T) {
	svc := teamService(t, mockllm.New(mockllm.TextTurn("x")))
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/teams/team-nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHTTPTeamTooMany asserts CreateTeam past MaxTeams → 429 Too Many Requests
// (ErrTooManyTeams).
func TestHTTPTeamTooMany(t *testing.T) {
	svc := teamServiceMaxTeams(t, mockllm.New(mockllm.TextTurn("x")), 1)
	srv := httptest.NewServer(server.NewHTTPHandler(svc))
	defer srv.Close()

	// First create fills the only slot.
	r1, err := http.Post(srv.URL+"/v1/teams", "application/json", strings.NewReader(`{"workspace":"/ws"}`))
	if err != nil {
		t.Fatalf("POST #1: %v", err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("create #1 status = %d, want 201", r1.StatusCode)
	}

	// Second create is over the cap → 429.
	r2, err := http.Post(srv.URL+"/v1/teams", "application/json", strings.NewReader(`{"workspace":"/ws"}`))
	if err != nil {
		t.Fatalf("POST #2: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("create #2 status = %d, want 429", r2.StatusCode)
	}
}
