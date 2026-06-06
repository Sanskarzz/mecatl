package port

import (
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// TestDiagnosticsImportsStayMinimal is an import-surface TRIPWIRE for the
// Diagnostics port: diagnostics.go must import EXACTLY {context} — nothing else.
// The port is a provider-neutral, domain-free logging seam; pulling in log/slog,
// session, tool, or any adapter would drag a backend (or a domain cycle) inward
// across the layering boundary the repo enforces by hand. A new import here is a
// deliberate decision that must update this guard — mirror the spirit of
// llm_neutral_test.go's DTO tripwire.
func TestDiagnosticsImportsStayMinimal(t *testing.T) {
	want := []string{`"context"`}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "diagnostics.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse diagnostics.go: %v", err)
	}
	got := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		got = append(got, imp.Path.Value)
	}
	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("diagnostics.go imports = %v, want %v.\n"+
			"The Diagnostics port must stay domain-free and backend-free: import "+
			"only context + stdlib. log/slog belongs on the slogdiag ADAPTER, never "+
			"this port. Update this guard only with a deliberate decision.", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("diagnostics.go imports = %v, want %v", got, want)
		}
	}
}
