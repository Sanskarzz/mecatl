package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// staticSource is a test AgentSource that returns a fixed set of defs and skips.
type staticSource struct {
	defs  []AgentDef
	skips []SkipError
	err   error
}

func (s staticSource) Agents(context.Context) ([]AgentDef, []SkipError, error) {
	return s.defs, s.skips, s.err
}

// TestMultiSourcePrecedenceShadows asserts earlier-source-wins on a name
// collision, with a shadow diagnostic for the dropped lower-precedence def.
func TestMultiSourcePrecedenceShadows(t *testing.T) {
	high := staticSource{defs: []AgentDef{{Name: "rev", Description: "high", Path: "/high/rev.md"}}}
	low := staticSource{defs: []AgentDef{
		{Name: "rev", Description: "low", Path: "/low/rev.md"},
		{Name: "only-low", Description: "L", Path: "/low/only.md"},
	}}

	defs, skips, err := NewMultiSource(high, low).Agents(t.Context())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("want 2 defs (rev + only-low), got %d: %+v", len(defs), defs)
	}
	// rev kept from the high-precedence source.
	for _, d := range defs {
		if d.Name == "rev" && d.Description != "high" {
			t.Fatalf("rev should be the high-precedence def, got %q", d.Description)
		}
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "shadowed") {
		t.Fatalf("want 1 shadow skip, got %v", skips)
	}
}

func TestMultiSourceFatalErrorPropagates(t *testing.T) {
	boom := errors.New("io fault")
	_, _, err := NewMultiSource(staticSource{err: boom}).Agents(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("want propagated fatal error, got %v", err)
	}
}

func TestMultiSourceNilEntriesIgnored(t *testing.T) {
	defs, _, err := NewMultiSource(nil, staticSource{defs: []AgentDef{{Name: "a", Description: "d"}}}, nil).Agents(t.Context())
	if err != nil || len(defs) != 1 {
		t.Fatalf("nil sources should be skipped, got %v err=%v", defs, err)
	}
}
