package repomap

import (
	"fmt"
	"sort"
	"strings"
)

// render produces the textual repo map: a short header, then the top-ranked
// files (capped at maxFiles), each with its top signatures, then a trailer
// summarizing totals and any skipped files. Files are ordered by descending
// PageRank, ties broken by path for determinism.
func render(g *graph, focus map[string]bool, maxFiles, skipped int) string {
	ranked := make([]*fileNode, 0, len(g.files))
	for _, n := range g.files {
		ranked = append(ranked, n)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank > ranked[j].rank
		}
		return ranked[i].path < ranked[j].path
	})

	total := len(ranked)
	shown := ranked
	if len(shown) > maxFiles {
		shown = shown[:maxFiles]
	}

	var b strings.Builder
	b.WriteString("Repo map (ranked by personalized PageRank over the symbol-reference graph).\n")
	if len(focus) > 0 {
		fmt.Fprintf(&b, "Ranking biased toward %d focus file(s).\n", len(focus))
	}
	b.WriteString("Each entry is path followed by its key signatures (bodies elided).\n\n")

	for _, n := range shown {
		writeFile(&b, n)
	}

	b.WriteString("\n---\n")
	fmt.Fprintf(&b, "%d file(s) mapped", total)
	if len(shown) < total {
		fmt.Fprintf(&b, " (showing top %d)", len(shown))
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "; %d file(s) skipped (unsupported language, unreadable, or no symbols)", skipped)
	}
	b.WriteString(".\n")
	return b.String()
}

// writeFile renders a single ranked file: its path and language, then its top
// signatures ordered by source line, capped at maxSymbolsPerFile.
func writeFile(b *strings.Builder, n *fileNode) {
	fmt.Fprintf(b, "%s  (%s)\n", n.path, n.lang)

	defs := make([]symbol, len(n.defs))
	copy(defs, n.defs)
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].line != defs[j].line {
			return defs[i].line < defs[j].line
		}
		return defs[i].name < defs[j].name
	})

	limit := len(defs)
	if limit > maxSymbolsPerFile {
		limit = maxSymbolsPerFile
	}
	for i := 0; i < limit; i++ {
		fmt.Fprintf(b, "  %s\n", defs[i].signature)
	}
	if len(defs) > limit {
		fmt.Fprintf(b, "  ... (%d more)\n", len(defs)-limit)
	}
	if len(defs) == 0 {
		b.WriteString("  (no top-level definitions)\n")
	}
	b.WriteString("\n")
}
