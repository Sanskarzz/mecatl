package source

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/mcp"
)

// fakeSource is an in-memory Source for testing MultiSource composition.
type fakeSource struct {
	name  string
	cfgs  []mcp.ServerConfig
	skips []SkipError
	err   error
}

func (f fakeSource) Servers(context.Context) ([]mcp.ServerConfig, []SkipError, error) {
	return f.cfgs, f.skips, f.err
}
func (f fakeSource) Name() string { return f.name }

func TestMultiSourceAggregates(t *testing.T) {
	ms := NewMultiSource(
		fakeSource{name: "x", cfgs: []mcp.ServerConfig{{Name: "b", URL: "u-b"}}},
		fakeSource{name: "y", cfgs: []mcp.ServerConfig{{Name: "a", URL: "u-a"}}},
	)
	got, skips, err := ms.Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("not merged-and-sorted by name: %v", got)
	}
}

func TestMultiSourcePrecedenceEarlierWins(t *testing.T) {
	high := fakeSource{name: "static", cfgs: []mcp.ServerConfig{{Name: "dup", URL: "high"}}}
	low := fakeSource{name: "toolhive(default)", cfgs: []mcp.ServerConfig{{Name: "dup", URL: "low"}}}

	got, skips, err := NewMultiSource(high, low).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("collision should yield 1 server, got %d", len(got))
	}
	if got[0].URL != "high" {
		t.Errorf("earlier source should win, got %+v", got[0])
	}
	if len(skips) != 1 {
		t.Fatalf("expected 1 shadow notice, got %d: %v", len(skips), skips)
	}
	if skips[0].Server != "dup" || !strings.Contains(skips[0].Reason, "shadowed") {
		t.Errorf("shadow notice malformed: %+v", skips[0])
	}
	// The notice should name both the kept and dropped source for traceability.
	if !strings.Contains(skips[0].Reason, "static") || !strings.Contains(skips[0].Reason, "toolhive(default)") {
		t.Errorf("shadow notice should name kept+dropped sources, got %q", skips[0].Reason)
	}
}

func TestMultiSourceAggregatesDiagnosticsInOrder(t *testing.T) {
	a := fakeSource{name: "a", cfgs: []mcp.ServerConfig{{Name: "a"}}, skips: []SkipError{{Server: "sa", Reason: "from a"}}}
	b := fakeSource{name: "b", cfgs: []mcp.ServerConfig{{Name: "b"}}, skips: []SkipError{{Server: "sb", Reason: "from b"}}}
	_, skips, err := NewMultiSource(a, b).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(skips) != 2 || skips[0].Reason != "from a" || skips[1].Reason != "from b" {
		t.Fatalf("diagnostics out of source order: %v", skips)
	}
}

func TestMultiSourceFatalErrorPropagates(t *testing.T) {
	boom := errors.New("infra fault")
	ms := NewMultiSource(
		fakeSource{name: "ok", cfgs: []mcp.ServerConfig{{Name: "ok"}}},
		fakeSource{name: "bad", err: boom},
	)
	got, _, err := ms.Servers(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("a source's fatal error must propagate, got %v", err)
	}
	if got != nil {
		t.Errorf("on fatal error no servers should be returned, got %v", got)
	}
}

func TestNewMultiSourceIgnoresNil(t *testing.T) {
	ms := NewMultiSource(nil, fakeSource{name: "x", cfgs: []mcp.ServerConfig{{Name: "x"}}}, nil)
	got, _, err := ms.Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("nil sources should be ignored, got %+v", got)
	}
}
