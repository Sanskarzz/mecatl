package lint

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCheckDesignDocLifecycle_Fixtures(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		content string
		want    []LifecycleKind // empty = clean
	}{
		{
			name: "design record banner, no status — clean",
			doc:  "FOO.md",
			content: "# Foo\n\n> **Design record.** Captured during the foo work; the rationale here is frozen.\n" +
				"> Current behaviour: architecture.md · shipped/deferred state: PRODUCTION-READINESS.md.\n\n## Body\nprose\n",
			want: nil,
		},
		{
			name:    "historical banner — clean",
			doc:     "OLD.md",
			content: "# Old\n\n> **Historical.** superseded by docs/architecture.md. Preserved for rationale; not maintained.\n\n## Body\n",
			want:    nil,
		},
		{
			name:    "research note banner — clean",
			doc:     "RES.md",
			content: "# Res\n\n> **Research note.** Captured 2026-01-01. A point-in-time study. Frozen.\n\n## Body\n",
			want:    nil,
		},
		{
			name:    "missing banner",
			doc:     "BARE.md",
			content: "# Bare\n\nSome prose with no banner.\n\n## Body\n",
			want:    []LifecycleKind{MissingBanner},
		},
		{
			name: "status line in design doc",
			doc:  "STAT.md",
			content: "# Stat\n\n> **Design record.** Captured during the stat work; frozen.\n\n" +
				"Status: **shipped**\n\n## Body\n",
			want: []LifecycleKind{StatusInDesignDoc},
		},
		{
			name:    "blockquoted bold status line",
			doc:     "STAT2.md",
			content: "# Stat2\n\n> **Design record.** frozen.\n\n> **Status:** Phase 0 deliverable\n\n## Body\n",
			want:    []LifecycleKind{StatusInDesignDoc},
		},
		{
			name:    "status inside a code fence is allowed",
			doc:     "CODE.md",
			content: "# Code\n\n> **Design record.** frozen.\n\n```json\nStatus: 200\n```\n\n## Body\n",
			want:    nil,
		},
		{
			name:    "banner's own 'state:' wording does not trip the status rule",
			doc:     "BANNER.md",
			content: "# Banner\n\n> **Design record.** Captured during the work; the rationale here is frozen.\n> Current behaviour: architecture.md · shipped/deferred state: PRODUCTION-READINESS.md.\n\n## Body\n",
			want:    nil,
		},
		{
			name:    "exempt tracker may carry status lines",
			doc:     "PRODUCTION-READINESS.md",
			content: "# PR\n\nStatus legend: ✅ Done\n\nStatus: whatever\n",
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDesignDocLifecycle(tc.doc, tc.content)
			var kinds []LifecycleKind
			for _, p := range got {
				kinds = append(kinds, p.Kind)
			}
			if len(kinds) != len(tc.want) {
				t.Fatalf("got %d problems %v, want %d %v", len(kinds), kinds, len(tc.want), tc.want)
			}
			for i := range kinds {
				if kinds[i] != tc.want[i] {
					t.Errorf("problem %d: got kind %v, want %v", i, kinds[i], tc.want[i])
				}
			}
		})
	}
}

// TestRealDesignDocsLifecycle is the live gate over docs/design/*.md — the
// companion to TestRealDesignDocsCitations. It enforces ADR 0002: every design
// record declares a lifecycle banner and carries no mutable status line.
func TestRealDesignDocsLifecycle(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "docs", "design", "*.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no design docs matched (wrong repo root?)")
	}
	sort.Strings(matches)

	var all []LifecycleProblem
	for _, doc := range matches {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		all = append(all, CheckDesignDocLifecycle(filepath.Base(doc), string(data))...)
	}

	if len(all) > 0 {
		var b strings.Builder
		b.WriteString("documentation-lifecycle violations in docs/design/*.md " +
			"(fix the doc, not this test); see docs/adr/0002-documentation-lifecycle.md:\n")
		for _, p := range all {
			b.WriteString("  - " + p.Error() + "\n")
		}
		t.Fatal(b.String())
	}
}
