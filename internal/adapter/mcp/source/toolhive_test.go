package source

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stacklok/toolhive/pkg/container/runtime"
	"github.com/stacklok/toolhive/pkg/core"
	"github.com/stacklok/toolhive/pkg/transport/types"
)

// fakeLister is an in-memory workloadLister for offline tests. It records the
// listAll arg it was called with so a test can assert running-only listing, and
// returns its canned workloads (or error). No ToolHive manager, no Docker.
type fakeLister struct {
	workloads   []core.Workload
	err         error
	gotListAll  bool
	listAllSeen bool
	calls       int // number of times ListWorkloads was invoked
}

func (f *fakeLister) ListWorkloads(_ context.Context, listAll bool, _ ...string) ([]core.Workload, error) {
	f.gotListAll = listAll
	f.listAllSeen = true
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.workloads, nil
}

// sourceWith builds a ToolHiveSource wired to a fake lister (no runtime).
func sourceWith(group string, lister workloadLister, newErr error) ToolHiveSource {
	return ToolHiveSource{
		group: group,
		newLister: func(context.Context) (workloadLister, error) {
			if newErr != nil {
				return nil, newErr
			}
			return lister, nil
		},
	}
}

func running(name, url string, tt types.TransportType, group string) core.Workload {
	return core.Workload{
		Name:          name,
		URL:           url,
		TransportType: tt,
		Status:        runtime.WorkloadStatusRunning,
		Group:         group,
	}
}

func TestToolHiveMapsRunningStreamable(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("github", "http://127.0.0.1:8080/mcp", types.TransportTypeStreamableHTTP, "default"),
		running("fetch", "http://127.0.0.1:8081/mcp", types.TransportTypeStreamableHTTP, "default"),
	}}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if !f.listAllSeen || f.gotListAll {
		t.Errorf("expected ListWorkloads called with listAll=false, gotListAll=%v seen=%v", f.gotListAll, f.listAllSeen)
	}
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 servers, got %d: %v", len(got), got)
	}
	if got[0].Name != "github" || got[0].URL != "http://127.0.0.1:8080/mcp" {
		t.Errorf("first server wrong: %+v", got[0])
	}
}

func TestToolHiveSkipsSSE(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("legacy", "http://127.0.0.1:9000/sse", types.TransportTypeSSE, "default"),
	}}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SSE workload must not be mapped, got %v", got)
	}
	if len(skips) != 1 || skips[0].Server != "legacy" || !strings.Contains(skips[0].Reason, "non-streamable") {
		t.Fatalf("expected one non-streamable skip for legacy, got %v", skips)
	}
}

func TestToolHiveSkipsStdio(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("local", "", types.TransportTypeStdio, "default"),
	}}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stdio workload must not be mapped, got %v", got)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "non-streamable") {
		t.Fatalf("expected one non-streamable skip for stdio, got %v", skips)
	}
}

func TestToolHiveSkipsDoubleUnderscoreName(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("bad__name", "http://127.0.0.1:8080/mcp", types.TransportTypeStreamableHTTP, "default"),
	}}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("__-named workload must not be mapped, got %v", got)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "__") {
		t.Fatalf("expected one __ skip, got %v", skips)
	}
}

func TestToolHiveSkipsNonRunning(t *testing.T) {
	stopped := core.Workload{
		Name: "stopped", URL: "http://127.0.0.1:8080/mcp",
		TransportType: types.TransportTypeStreamableHTTP,
		Status:        runtime.WorkloadStatus("stopped"), Group: "default",
	}
	f := &fakeLister{workloads: []core.Workload{stopped}}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stopped workload must not be mapped, got %v", got)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "not running") {
		t.Fatalf("expected one not-running skip, got %v", skips)
	}
}

func TestToolHiveGroupFilterApplied(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("a", "http://127.0.0.1:1/mcp", types.TransportTypeStreamableHTTP, "team-x"),
		running("b", "http://127.0.0.1:2/mcp", types.TransportTypeStreamableHTTP, "default"),
	}}
	got, _, err := sourceWith("team-x", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("group filter not applied, got %v", got)
	}
}

func TestToolHiveEmptyGroupDefaults(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("a", "http://127.0.0.1:1/mcp", types.TransportTypeStreamableHTTP, "default"),
		running("b", "http://127.0.0.1:2/mcp", types.TransportTypeStreamableHTTP, "other"),
	}}
	src := sourceWith("", f, nil) // empty group -> "default"
	if src.Name() != "toolhive(default)" {
		t.Errorf("empty group should label default, got %q", src.Name())
	}
	got, _, err := src.Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("empty group should filter to default, got %v", got)
	}
}

func TestToolHiveNoRuntimeDegrades(t *testing.T) {
	// Constructor error (no container runtime) -> (nil, one diagnostic, nil).
	got, skips, err := sourceWith("default", nil, errors.New("no socket")).Servers(context.Background())
	if err != nil {
		t.Fatalf("no-runtime must NOT be a fatal error, got %v", err)
	}
	if got != nil {
		t.Errorf("no-runtime should yield zero servers, got %v", got)
	}
	if len(skips) != 1 || skips[0].Server != "toolhive" || !strings.Contains(skips[0].Reason, "runtime unavailable") {
		t.Fatalf("expected one runtime-unavailable diagnostic, got %v", skips)
	}
}

func TestToolHiveListErrorDegrades(t *testing.T) {
	// Runtime present but list call fails -> degrade, not fatal.
	f := &fakeLister{err: errors.New("daemon down")}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("list error must NOT be fatal, got %v", err)
	}
	if got != nil {
		t.Errorf("list error should yield zero servers, got %v", got)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "listing ToolHive workloads failed") {
		t.Fatalf("expected one list-failed diagnostic, got %v", skips)
	}
}

func TestToolHiveEmptyGroupNoMatches(t *testing.T) {
	f := &fakeLister{workloads: []core.Workload{
		running("a", "http://127.0.0.1:1/mcp", types.TransportTypeStreamableHTTP, "other"),
	}}
	got, skips, err := sourceWith("default", f, nil).Servers(context.Background())
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("no workloads in group -> empty, got %v", got)
	}
	if len(skips) != 0 {
		t.Fatalf("filtered-out-by-group workloads should not produce skips, got %v", skips)
	}
}
