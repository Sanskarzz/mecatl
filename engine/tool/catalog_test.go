package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stacklok/mecatl/engine/session"
)

// fakeTool is a minimal Tool used to exercise the Catalog.
type fakeTool struct {
	name     string
	readOnly bool
}

func (f fakeTool) Spec() ToolSpec { return ToolSpec{Name: f.name} }
func (f fakeTool) ReadOnly() bool { return f.readOnly }
func (fakeTool) Execute(context.Context, session.ToolCall, Workspace) (session.ToolResult, error) {
	return session.ToolResult{}, nil
}

func names(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Spec().Name
	}
	return out
}

func TestCatalogLookupAndRegister(t *testing.T) {
	c := NewCatalog()
	read := fakeTool{name: "Read", readOnly: true}
	if err := c.Register(read); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := c.Lookup("Read")
	if !ok || got.Spec().Name != "Read" {
		t.Fatalf("Lookup(Read) = %+v, %v", got, ok)
	}
	if _, ok := c.Lookup("Nope"); ok {
		t.Fatalf("Lookup of missing tool returned ok")
	}
}

func TestCatalogDuplicateRegistration(t *testing.T) {
	c := NewCatalog()
	if err := c.Register(fakeTool{name: "Read", readOnly: true}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := c.Register(fakeTool{name: "Read", readOnly: true})
	if !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("duplicate Register err = %v, want ErrDuplicateTool", err)
	}
}

func TestCatalogMustRegisterPanicsOnDuplicate(t *testing.T) {
	c := NewCatalog()
	c.MustRegister(fakeTool{name: "Read", readOnly: true})
	defer func() {
		if recover() == nil {
			t.Fatalf("MustRegister did not panic on duplicate")
		}
	}()
	c.MustRegister(fakeTool{name: "Read", readOnly: true})
}

func TestCatalogPlanModeFiltersNonReadOnly(t *testing.T) {
	c := NewCatalog()
	c.MustRegister(fakeTool{name: "Read", readOnly: true})
	c.MustRegister(fakeTool{name: "Grep", readOnly: true})
	c.MustRegister(fakeTool{name: "Edit", readOnly: false})
	c.MustRegister(fakeTool{name: "Write", readOnly: false})

	plan := names(c.Available(session.ModePlan))
	wantPlan := []string{"Grep", "Read"} // sorted, read-only only
	if !equal(plan, wantPlan) {
		t.Fatalf("plan-mode tools = %v, want %v", plan, wantPlan)
	}

	def := names(c.Available(session.ModeDefault))
	wantDef := []string{"Edit", "Grep", "Read", "Write"} // sorted, all
	if !equal(def, wantDef) {
		t.Fatalf("default-mode tools = %v, want %v", def, wantDef)
	}
}

func TestCatalogSpecsRespectMode(t *testing.T) {
	c := NewCatalog()
	c.MustRegister(fakeTool{name: "Read", readOnly: true})
	c.MustRegister(fakeTool{name: "Edit", readOnly: false})

	specs := c.Specs(session.ModePlan)
	if len(specs) != 1 || specs[0].Name != "Read" {
		t.Fatalf("plan-mode specs = %v, want [Read]", specs)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
