package repomap

import "sort"

// symbol is one extracted top-level definition: where it lives, its identifier,
// and a rendered single-line signature (e.g. "func Foo(a int) int").
type symbol struct {
	// path is the workspace-relative file the symbol is defined in.
	path string
	// name is the symbol identifier (function/method/type/class name).
	name string
	// signature is the rendered, body-elided single-line signature.
	signature string
	// kind classifies the definition for ordering and display.
	kind definitionKind
	// line is the 1-based line the definition starts on.
	line int
}

// fileNode aggregates everything extracted from one parsed file.
type fileNode struct {
	// path is the workspace-relative file path.
	path string
	// lang is the human-readable language name.
	lang string
	// defs are the symbols defined in this file.
	defs []symbol
	// refs is the multiset of identifiers this file references, by count.
	refs map[string]int
	// rank is the personalized-PageRank score assigned to this file.
	rank float64
}

// graph is the symbol-reference multigraph over files. Edges point from a file
// that references an identifier to every file that defines a symbol of that
// name, weighted by the number of references. This is the structure PageRank
// runs over to surface the most-depended-upon files.
type graph struct {
	// files holds every parsed file node, keyed by path.
	files map[string]*fileNode
	// defSites maps an identifier name to the set of file paths defining it.
	defSites map[string][]string
}

// newGraph builds the reference graph from parsed file nodes.
func newGraph(nodes []*fileNode) *graph {
	g := &graph{
		files:    make(map[string]*fileNode, len(nodes)),
		defSites: make(map[string][]string),
	}
	for _, n := range nodes {
		g.files[n.path] = n
		seen := make(map[string]bool)
		for _, d := range n.defs {
			if seen[d.name] {
				continue
			}
			seen[d.name] = true
			g.defSites[d.name] = append(g.defSites[d.name], n.path)
		}
	}
	for name := range g.defSites {
		sort.Strings(g.defSites[name])
	}
	return g
}

// edge is a weighted directed link from one file to another in the ref graph.
type edge struct {
	to     string
	weight float64
}

// adjacency builds the weighted out-edge lists. A reference in file A to an
// identifier defined in file(s) B contributes weight (reference count) to the
// A→B edges, split evenly across all definition sites of that name. Self-edges
// (a file referencing its own definitions) are dropped so PageRank credit flows
// outward, matching Aider's intent that a file's rank reflects external demand.
func (g *graph) adjacency() map[string][]edge {
	adj := make(map[string][]edge, len(g.files))
	for path, n := range g.files {
		weights := make(map[string]float64)
		for name, count := range n.refs {
			sites := g.defSites[name]
			if len(sites) == 0 {
				continue
			}
			share := float64(count) / float64(len(sites))
			for _, site := range sites {
				if site == path {
					continue // drop self-references
				}
				weights[site] += share
			}
		}
		dests := make([]string, 0, len(weights))
		for dst := range weights {
			dests = append(dests, dst)
		}
		sort.Strings(dests)
		for _, dst := range dests {
			adj[path] = append(adj[path], edge{to: dst, weight: weights[dst]})
		}
	}
	return adj
}

// pageRank computes personalized PageRank over the file reference graph and
// stores the resulting score on each fileNode.rank.
//
// The teleport (personalization) vector biases random restarts toward the
// focus files when focus is non-empty; otherwise it is uniform. Dangling files
// (no out-edges) redistribute their mass over the teleport vector each
// iteration so probability is conserved. damping is the standard 0.85; the walk
// runs to a fixed iteration cap with an L1 convergence early-exit. The whole
// thing is deterministic: nodes and edges are visited in sorted order.
func (g *graph) pageRank(focus map[string]bool, damping float64, iters int) {
	paths := make([]string, 0, len(g.files))
	for p := range g.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	n := len(paths)
	if n == 0 {
		return
	}

	adj := g.adjacency()
	outSum := make(map[string]float64, n)
	for p, edges := range adj {
		for _, e := range edges {
			outSum[p] += e.weight
		}
	}

	tele := teleportVector(paths, focus)
	rank := make(map[string]float64, n)
	for _, p := range paths {
		rank[p] = 1.0 / float64(n)
	}

	for it := 0; it < iters; it++ {
		next := pageRankStep(paths, adj, outSum, rank, tele, damping)
		if l1Diff(paths, rank, next) < 1e-9 {
			rank = next
			break
		}
		rank = next
	}

	for p, r := range rank {
		g.files[p].rank = r
	}
}

// teleportVector builds the personalization (restart) distribution: uniform over
// focus files when focus is non-empty, else uniform over all files.
func teleportVector(paths []string, focus map[string]bool) map[string]float64 {
	tele := make(map[string]float64, len(paths))
	focusCount := 0
	for _, p := range paths {
		if focus[p] {
			focusCount++
		}
	}
	for _, p := range paths {
		switch {
		case focusCount > 0 && focus[p]:
			tele[p] = 1.0 / float64(focusCount)
		case focusCount > 0:
			tele[p] = 0
		default:
			tele[p] = 1.0 / float64(len(paths))
		}
	}
	return tele
}

// pageRankStep performs one power-method iteration: it spreads each node's rank
// over its out-edges, folds in the teleport mass and the dangling-node mass
// (both following the personalization vector), and returns the next rank vector.
func pageRankStep(paths []string, adj map[string][]edge, outSum, rank, tele map[string]float64, damping float64) map[string]float64 {
	next := make(map[string]float64, len(paths))
	var dangling float64
	for _, p := range paths {
		if outSum[p] == 0 {
			dangling += rank[p]
		}
	}
	for _, p := range paths {
		next[p] = (1-damping)*tele[p] + damping*dangling*tele[p]
	}
	for _, p := range paths {
		if outSum[p] == 0 {
			continue
		}
		for _, e := range adj[p] {
			next[e.to] += damping * rank[p] * e.weight / outSum[p]
		}
	}
	return next
}

// l1Diff is the L1 distance between two rank vectors, used as the convergence
// criterion for the power method.
func l1Diff(paths []string, a, b map[string]float64) float64 {
	var diff float64
	for _, p := range paths {
		d := b[p] - a[p]
		if d < 0 {
			d = -d
		}
		diff += d
	}
	return diff
}
