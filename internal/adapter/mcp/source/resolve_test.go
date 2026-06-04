package source

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/toolhive/pkg/core"
	"github.com/stacklok/toolhive/pkg/transport/types"

	"github.com/stacklok/mecatl/internal/adapter/mcp"
)

func TestResolveSourcesStaticOnlyWhenDisabled(t *testing.T) {
	got := ResolveSources(ResolveOptions{
		StaticServers:   []mcp.ServerConfig{{Name: "a", URL: "http://a/mcp"}},
		ToolHiveEnabled: false,
	})
	if len(got) != 1 {
		t.Fatalf("disabled ToolHive should yield only the static source, got %d", len(got))
	}
	if _, ok := got[0].(StaticSource); !ok {
		t.Fatalf("first source should be StaticSource, got %T", got[0])
	}
}

func TestResolveSourcesIncludesToolHiveWhenEnabled(t *testing.T) {
	got := ResolveSources(ResolveOptions{
		StaticServers:   []mcp.ServerConfig{{Name: "a", URL: "http://a/mcp"}},
		ToolHiveEnabled: true,
		ToolHiveGroup:   "team-x",
	})
	if len(got) != 2 {
		t.Fatalf("enabled ToolHive should yield static + toolhive, got %d", len(got))
	}
	// Static is highest precedence (first).
	if _, ok := got[0].(StaticSource); !ok {
		t.Fatalf("source[0] should be StaticSource, got %T", got[0])
	}
	th, ok := got[1].(ToolHiveSource)
	if !ok {
		t.Fatalf("source[1] should be ToolHiveSource, got %T", got[1])
	}
	if th.Name() != "toolhive(team-x)" {
		t.Errorf("group not plumbed through, got %q", th.Name())
	}
}

func TestResolveSourcesEmptyGroupDefaultsLabel(t *testing.T) {
	got := ResolveSources(ResolveOptions{ToolHiveEnabled: true})
	if len(got) != 2 {
		t.Fatalf("want static + toolhive, got %d", len(got))
	}
	if got[1].Name() != "toolhive(default)" {
		t.Errorf("empty group should label default, got %q", got[1].Name())
	}
}

func TestInspectSourcesBuildsInventory(t *testing.T) {
	// Use fake sources so the inspection is fully offline and deterministic.
	static := fakeSource{name: "static", cfgs: []mcp.ServerConfig{{Name: "a", URL: "http://a/mcp"}}}
	thLike := fakeSource{
		name:  "toolhive(default)",
		cfgs:  []mcp.ServerConfig{{Name: "b", URL: "http://b/mcp"}},
		skips: []SkipError{{Server: "legacy", Reason: "non-streamable transport"}},
	}
	infos := InspectSources(context.Background(), []Source{static, thLike}, ResolveOptions{ToolHiveEnabled: true})
	if len(infos) != 2 {
		t.Fatalf("want 2 source infos, got %d", len(infos))
	}
	if infos[0].Name != "static" || len(infos[0].Servers) != 1 || infos[0].Servers[0].Name != "a" {
		t.Errorf("static info wrong: %+v", infos[0])
	}
	if infos[1].Name != "toolhive(default)" || len(infos[1].Servers) != 1 {
		t.Errorf("toolhive info wrong: %+v", infos[1])
	}
	if len(infos[1].Diagnostics) != 1 {
		t.Errorf("toolhive diagnostics not captured: %+v", infos[1].Diagnostics)
	}
	if infos[1].Servers[0].Transport != "streamable-http" {
		t.Errorf("server transport label wrong: %q", infos[1].Servers[0].Transport)
	}
}

func TestResolveWalksSourcesOnce(t *testing.T) {
	// A real ToolHive source backed by a counting lister: Resolve must list the
	// live workloads EXACTLY ONCE, even though it returns both the merged configs
	// and the inventory (the old double InspectSources + MultiSource.Servers walk
	// listed twice). Pair it with a static source whose name collides so the merge
	// + shadow path is exercised in the same pass.
	lister := &fakeLister{workloads: []core.Workload{
		running("th", "http://127.0.0.1:1/mcp", types.TransportTypeStreamableHTTP, types.ProxyModeStreamableHTTP, "default"),
		running("dup", "http://127.0.0.1:2/mcp", types.TransportTypeStreamableHTTP, types.ProxyModeStreamableHTTP, "default"),
	}}
	static := StaticSource{Configs: []mcp.ServerConfig{{Name: "dup", URL: "http://static/mcp"}}}
	th := sourceWith("default", lister, nil)

	merged, inventory, skips := Resolve(context.Background(), []Source{static, th})

	if lister.calls != 1 {
		t.Fatalf("ListWorkloads called %d times, want exactly 1", lister.calls)
	}
	// Merge: static "dup" wins; "th" survives. Sorted by name -> dup, th.
	if len(merged) != 2 || merged[0].Name != "dup" || merged[0].URL != "http://static/mcp" || merged[1].Name != "th" {
		t.Fatalf("merged = %+v, want static dup + th", merged)
	}
	// Inventory mirrors each source's OWN pre-shadow view.
	if len(inventory) != 2 {
		t.Fatalf("inventory = %d sources, want 2", len(inventory))
	}
	if inventory[0].Kind != kindStatic || inventory[1].Kind != kindToolHive || inventory[1].Group != "default" {
		t.Fatalf("inventory classification wrong: %+v", inventory)
	}
	if len(inventory[1].Servers) != 2 {
		t.Fatalf("toolhive inventory should list both its servers pre-shadow, got %+v", inventory[1].Servers)
	}
	// The cross-source collision on "dup" surfaces as a shadow skip.
	var shadow bool
	for _, s := range skips {
		if s.Server == "dup" && strings.Contains(s.Reason, "shadowed") {
			shadow = true
		}
	}
	if !shadow {
		t.Fatalf("expected a shadow skip for dup, got %v", skips)
	}
}

func TestInspectSourcesKindAndGroupForRealSources(t *testing.T) {
	// Real source types so Kind/Group classification is exercised. The ToolHive
	// source uses a fake lister so no runtime is touched.
	static := StaticSource{Configs: []mcp.ServerConfig{{Name: "a", URL: "http://a/mcp"}}}
	th := sourceWith("team-x", &fakeLister{}, nil)
	infos := InspectSources(context.Background(),
		[]Source{static, th},
		ResolveOptions{ToolHiveEnabled: true, ToolHiveGroup: "team-x"})
	if infos[0].Kind != kindStatic {
		t.Errorf("static kind wrong: %q", infos[0].Kind)
	}
	if infos[1].Kind != kindToolHive || infos[1].Group != "team-x" {
		t.Errorf("toolhive kind/group wrong: %+v", infos[1])
	}
}
