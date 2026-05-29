package source

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/mcp"
)

func TestStaticSourceReturnsConfigs(t *testing.T) {
	cfgs := []mcp.ServerConfig{
		{Name: "a", URL: "http://a/mcp"},
		{Name: "b", URL: "http://b/mcp"},
	}
	s := StaticSource{Configs: cfgs}
	if s.Name() != "static" {
		t.Errorf("Name() = %q, want static", s.Name())
	}
	got, skips, err := s.Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(skips) != 0 {
		t.Fatalf("static source should never skip, got %v", skips)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("static configs returned wrong: %v", got)
	}
}

func TestStaticSourceEmptyIsNotAnError(t *testing.T) {
	got, skips, err := StaticSource{}.Servers(context.Background())
	if err != nil {
		t.Fatalf("empty static source must not error, got %v", err)
	}
	if len(got) != 0 || len(skips) != 0 {
		t.Fatalf("empty static source should yield nothing, got %v / %v", got, skips)
	}
}
