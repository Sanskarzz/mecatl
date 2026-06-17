package lint

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCheckADRShape_Fixtures(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		content string
		want    []string // missing fields, empty = clean
	}{
		{
			name:    "well-formed ADR",
			doc:     "0099-example.md",
			content: "# ADR 0099 — Example\n\n- Status: Accepted\n- Date: 2026-06-17\n- Scope: x\n\n## Context\n",
			want:    nil,
		},
		{
			name:    "status with trailing detail",
			doc:     "0100-example.md",
			content: "# ADR 0100\n\n- Status: Accepted (substrate shipped)\n- Date: 2026\n",
			want:    nil,
		},
		{
			name:    "missing date",
			doc:     "0101-example.md",
			content: "# ADR 0101\n\n- Status: Accepted\n- Scope: x\n",
			want:    []string{"Date"},
		},
		{
			name:    "missing status",
			doc:     "0102-example.md",
			content: "# ADR 0102\n\n- Date: 2026\n",
			want:    []string{"Status"},
		},
		{
			name:    "missing both",
			doc:     "0103-example.md",
			content: "# ADR 0103\n\nsome prose\n",
			want:    []string{"Status", "Date"},
		},
		{
			name:    "README is exempt",
			doc:     "README.md",
			content: "# Architecture Decision Records\n\nindex\n",
			want:    nil,
		},
		{
			name:    "template is exempt",
			doc:     "template.md",
			content: "# ADR NNNN — title\n\n- Status: Proposed | Accepted | Superseded\n",
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckADRShape(tc.doc, tc.content)
			var missing []string
			for _, p := range got {
				missing = append(missing, p.Missing)
			}
			if len(missing) != len(tc.want) {
				t.Fatalf("got %v, want %v", missing, tc.want)
			}
			for i := range missing {
				if missing[i] != tc.want[i] {
					t.Errorf("problem %d: got %q, want %q", i, missing[i], tc.want[i])
				}
			}
		})
	}
}

// TestRealADRShape is the live gate over docs/adr/*.md — the companion to
// TestRealDesignDocsCitations. It enforces ADR 0003: every ADR carries a
// `- Status:` and a `- Date:` header line.
func TestRealADRShape(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "docs", "adr", "*.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no ADRs matched (wrong repo root?)")
	}
	sort.Strings(matches)

	var all []ADRProblem
	for _, doc := range matches {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		all = append(all, CheckADRShape(filepath.Base(doc), string(data))...)
	}

	if len(all) > 0 {
		var b strings.Builder
		b.WriteString("ADR-shape violations in docs/adr/*.md (fix the doc, not this test); " +
			"see docs/adr/template.md:\n")
		for _, p := range all {
			b.WriteString("  - " + p.Error() + "\n")
		}
		t.Fatal(b.String())
	}
}
