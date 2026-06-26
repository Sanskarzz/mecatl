package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func newPromptManager(t *testing.T) *Manager {
	t.Helper()
	url := newTestServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := NewManager(ctx, []ServerConfig{{Name: "a", URL: url}}, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestListPrompts(t *testing.T) {
	m := newPromptManager(t)
	prompts, err := m.ListPrompts(context.Background(), "a")
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	p := prompts[0]
	if p.Name != "greet" {
		t.Errorf("prompt name = %q, want greet", p.Name)
	}
	if p.Server != "a" {
		t.Errorf("prompt server = %q, want a", p.Server)
	}
	if len(p.Arguments) != 1 || p.Arguments[0].Name != "who" || !p.Arguments[0].Required {
		t.Errorf("prompt args = %+v, want one required 'who'", p.Arguments)
	}
}

func TestGetPromptWithRequiredArg(t *testing.T) {
	m := newPromptManager(t)
	res, err := m.GetPrompt(context.Background(), "a", "greet", map[string]string{"who": "Ada"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(res.Messages))
	}
	flat := flattenPromptResult(res)
	// Role-tagged, multi-message flatten.
	if !strings.Contains(flat, "user: Say hi to Ada") {
		t.Errorf("flatten missing user message; got %q", flat)
	}
	if !strings.Contains(flat, "assistant: Hi, Ada!") {
		t.Errorf("flatten missing assistant message; got %q", flat)
	}
}

func TestGetPromptWithoutRequiredArg(t *testing.T) {
	m := newPromptManager(t)
	// The SDK enforces required arguments server-side, so omitting "who" surfaces
	// as a Go error from GetPrompt.
	if _, err := m.GetPrompt(context.Background(), "a", "greet", nil); err == nil {
		t.Errorf("GetPrompt without required arg = nil error, want error")
	}
}

func TestGetPromptUnknownServer(t *testing.T) {
	m := newPromptManager(t)
	if _, err := m.GetPrompt(context.Background(), "nope", "greet", nil); err == nil {
		t.Errorf("GetPrompt(unknown server) = nil error, want error")
	}
}
