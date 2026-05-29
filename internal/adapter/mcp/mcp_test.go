package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// echoArgs is the input for the fake "echo" tool.
type echoArgs struct {
	Text string `json:"text"`
}

// noArgs is an empty argument type for tools that take no input.
type noArgs struct{}

// newTestServer stands up an in-process MCP server exposing three fake tools and
// serves it over a real httptest server speaking the Streamable HTTP transport.
// It returns the server URL and a cleanup func.
//
// gotAuth, if non-nil, receives the Authorization header seen on the most recent
// request, so a test can assert header injection.
func newTestServer(t *testing.T, gotAuth *string) (string, func()) {
	t.Helper()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake", Version: "v1"}, nil)

	// Normal, mutating tool: echoes its text back.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "echo",
		Description: "echoes the input text",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoArgs) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo:" + in.Text}},
		}, nil, nil
	})

	// Read-only tool: advertises readOnlyHint == true.
	readOnly := true
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "peek",
		Description: "reads without mutating",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ noArgs) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "peeked"}},
		}, nil, nil
	})

	// Failing tool: returns an MCP tool-level error (isError == true).
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "boom",
		Description: "always fails",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ noArgs) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "kaboom"}},
		}, nil, nil
	})

	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		nil,
	)

	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		handler.ServeHTTP(w, r)
	}))

	return httpSrv.URL, httpSrv.Close
}

func connectTest(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func toolsByName(tools []tool.Tool) map[string]tool.Tool {
	m := make(map[string]tool.Tool, len(tools))
	for _, tl := range tools {
		m[tl.Spec().Name] = tl
	}
	return m
}

func TestConnectListsAndNamespacesTools(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	s := connectTest(t, ServerConfig{Name: "fake", URL: url})

	tools := s.Tools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	byName := toolsByName(tools)
	for _, want := range []string{"mcp__fake__echo", "mcp__fake__peek", "mcp__fake__boom"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing namespaced tool %q; got %v", want, keys(byName))
		}
	}

	// Spec/description/schema mapping for echo.
	echo := byName["mcp__fake__echo"]
	if got := echo.Spec().Description; got != "echoes the input text" {
		t.Errorf("echo description = %q", got)
	}
	var schema map[string]any
	if err := json.Unmarshal(echo.Spec().Schema, &schema); err != nil {
		t.Fatalf("echo schema not valid JSON: %v (%s)", err, echo.Spec().Schema)
	}
	if schema["type"] != "object" {
		t.Errorf("echo schema type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["text"]; !ok {
		t.Errorf("echo schema missing 'text' property; schema=%s", echo.Spec().Schema)
	}
}

func TestReadOnlyHintMapping(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	s := connectTest(t, ServerConfig{Name: "fake", URL: url})
	byName := toolsByName(s.Tools())

	if byName["mcp__fake__peek"].ReadOnly() != true {
		t.Errorf("peek should be ReadOnly (readOnlyHint=true)")
	}
	if byName["mcp__fake__echo"].ReadOnly() != false {
		t.Errorf("echo should default to not ReadOnly")
	}
	if byName["mcp__fake__boom"].ReadOnly() != false {
		t.Errorf("boom should default to not ReadOnly")
	}
}

func TestExecuteRoundTripsArgs(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	s := connectTest(t, ServerConfig{Name: "fake", URL: url})
	echo := toolsByName(s.Tools())["mcp__fake__echo"]

	call := session.NewToolCall("call-1", "mcp__fake__echo", json.RawMessage(`{"text":"hello"}`))
	res, err := echo.Execute(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if res.CallID != "call-1" {
		t.Errorf("CallID = %q, want call-1", res.CallID)
	}
	if res.Content != "echo:hello" {
		t.Errorf("Content = %q, want echo:hello", res.Content)
	}
}

func TestExecuteMapsIsError(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	s := connectTest(t, ServerConfig{Name: "fake", URL: url})
	boom := toolsByName(s.Tools())["mcp__fake__boom"]

	call := session.NewToolCall("call-2", "mcp__fake__boom", json.RawMessage(`{}`))
	res, err := boom.Execute(context.Background(), call, nil)
	if err != nil {
		t.Fatalf("Execute returned hard error, want tool error result: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result, got %+v", res)
	}
	if res.Content != "kaboom" {
		t.Errorf("Content = %q, want kaboom", res.Content)
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	s := connectTest(t, ServerConfig{Name: "fake", URL: url})
	echo := toolsByName(s.Tools())["mcp__fake__echo"]

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before executing

	call := session.NewToolCall("call-3", "mcp__fake__echo", json.RawMessage(`{"text":"x"}`))
	_, err := echo.Execute(ctx, call, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestAuthHeaderInjected(t *testing.T) {
	var seen string
	url, stop := newTestServer(t, &seen)
	defer stop()

	connectTest(t, ServerConfig{
		Name:    "fake",
		URL:     url,
		Headers: map[string]string{"Authorization": "Bearer secret-token"},
	})

	if seen != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want Bearer secret-token", seen)
	}
}

func TestRegisterIntoCatalog(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	s := connectTest(t, ServerConfig{Name: "fake", URL: url})

	cat := tool.NewCatalog()
	if err := Register(cat, s.Tools()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := cat.Lookup("mcp__fake__echo"); !ok {
		t.Errorf("echo not registered in catalog")
	}
}

func TestManagerSkipsUnreachableServer(t *testing.T) {
	url, stop := newTestServer(t, nil)
	defer stop()

	var skipped []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m, err := NewManager(ctx, []ServerConfig{
		{Name: "good", URL: url},
		{Name: "bad", URL: "http://127.0.0.1:1/mcp", Timeout: 500 * time.Millisecond},
	}, func(cfg ServerConfig, _ error) {
		skipped = append(skipped, cfg.Name)
	})
	if err != nil {
		t.Fatalf("NewManager should not fail when one server connects: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if len(m.Servers()) != 1 {
		t.Errorf("expected 1 connected server, got %d", len(m.Servers()))
	}
	if len(skipped) != 1 || skipped[0] != "bad" {
		t.Errorf("expected 'bad' to be skipped, got %v", skipped)
	}
	if len(m.Tools()) != 3 {
		t.Errorf("expected 3 tools from the good server, got %d", len(m.Tools()))
	}
}

func TestConnectValidatesConfig(t *testing.T) {
	ctx := context.Background()
	cases := []ServerConfig{
		{Name: "", URL: "http://x"},
		{Name: "has__sep", URL: "http://x"},
		{Name: "ok", URL: ""},
	}
	for _, cfg := range cases {
		if _, err := Connect(ctx, cfg); err == nil {
			t.Errorf("Connect(%+v) = nil error, want validation error", cfg)
		}
	}
}

func keys(m map[string]tool.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
