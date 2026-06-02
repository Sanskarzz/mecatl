// Package repomap implements an Aider-style, read-only "repo map" tool for the
// mecatl tool catalog. It walks the session Workspace, parses source files
// with tree-sitter to extract top-level definitions (functions, methods, types,
// classes) and their signatures, builds a file-level symbol-reference graph, and
// ranks files with personalized PageRank. The result is a compact, ranked map of
// path -> key signatures that orients a model in an unfamiliar repository in one
// turn, instead of burning a sequence of glob/grep/read calls.
//
// The map is the strongest non-embedding context-discovery primitive (see
// docs/harnesses/05-comparative-harnesses.md): tree-sitter parses every file
// into def/ref tags, references form a directed multigraph over files, and
// personalized PageRank — biased toward an optional caller-supplied focus set —
// surfaces the files the rest of the repo most depends on. It needs no API key,
// no embedding index, and no indexing latency.
//
// Languages: Go, Python, and TypeScript/TSX are parsed; files in any other
// language are skipped gracefully (counted in the trailer, never an error).
package repomap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stacklok/mecatl/internal/adapter/toolkit"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// Tuning constants for the repo map. These bound both the work done and the
// size of the rendered output so a single call cannot blow the context window.
const (
	// defaultMaxFiles is the default cap on how many ranked files are rendered
	// when the caller does not pass max_files.
	defaultMaxFiles = 50
	// maxFilesHardCap bounds a caller-supplied max_files so the model cannot
	// request an unbounded map.
	maxFilesHardCap = 200
	// maxSymbolsPerFile caps how many signatures are rendered per file so one
	// large file cannot dominate the map.
	maxSymbolsPerFile = 30
	// maxScannedFiles bounds how many candidate files are read+parsed, guarding
	// against pathologically large workspaces. Files past the cap are skipped.
	maxScannedFiles = 5000
	// maxFileBytes caps the size of a single file the repo map will parse. This
	// bounds worst-case single-file tree-sitter parse time: the WASM ParseString is
	// uninterruptible, so one pathologically large file can stall the whole call.
	// Huge generated/minified files add little to a structural symbol map, so
	// skipping them costs almost nothing.
	maxFileBytes = 256 << 10 // 256 KiB
	// pageRankDamping is the standard PageRank damping factor.
	pageRankDamping = 0.85
	// pageRankIters is the iteration cap for the power method (with an L1
	// convergence early-exit well before this in practice).
	pageRankIters = 100
)

// supportedExts are the file extensions the repo map parses.
var supportedExts = []string{".go", ".py", ".ts", ".tsx"}

// ignoredDirs is the denylist of path segments whose subtrees the repo map never
// descends into: vendored/installed dependency trees that duplicate or balloon
// the map without adding first-party structure.
var ignoredDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
}

// isIgnoredPath reports whether a candidate path falls under an ignored directory.
// A path is ignored if any of its "/"-separated segments is in ignoredDirs OR
// begins with "." (a dotfile/dotdir). This matters because filepath.Glob's "*"
// metacharacter MATCHES dotfiles, so without this guard discovery descends into
// .git/, .claude/worktrees/<agent>/ (a full duplicate of the repo), and vendor/
// node_modules — over-scanning the workspace and, in the in-process TUI, stalling
// the whole call on the duplicate tree.
func isIgnoredPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if ignoredDirs[seg] || strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// maxGlobDepth is how many nested directory levels the discovery globs reach.
// The tool.Workspace.Glob seam matches with path.Match semantics, which does
// NOT honor a recursive "**"; so to cover a tree we enumerate one glob per depth
// ("*.go", "*/*.go", "*/*/*.go", ...). This depth comfortably covers typical
// source layouts; deeper files are simply not mapped.
const maxGlobDepth = 12

// scanGlobs returns the discovery globs: each supported extension at every
// directory depth up to maxGlobDepth. Building them here (rather than as a
// const) keeps the depth/extension matrix in one place.
func scanGlobs() []string {
	globs := make([]string, 0, len(supportedExts)*maxGlobDepth)
	for _, ext := range supportedExts {
		prefix := ""
		for d := 0; d < maxGlobDepth; d++ {
			globs = append(globs, prefix+"*"+ext)
			prefix += "*/"
		}
	}
	return globs
}

const repoMapDescription = `Produce a ranked, Aider-style "repo map": a compact overview of the codebase as path -> key signatures (functions, methods, types/classes), with the most depended-upon files ranked first.

When to use:
- To orient yourself in an unfamiliar or large repository in a single call,
  instead of burning several Glob/Grep/Read turns guessing where things live.
- To find the central files and the public surface (signatures) before deciding
  what to read in full.
- Pass "focus" with file paths or symbol names you care about to bias the
  ranking toward the part of the codebase relevant to your task.

When NOT to use:
- To read the full contents of a file you already know you need (use Read).
- To search for a specific string or pattern (use Grep) or to list files by name
  (use Glob).

How it works:
- Files are parsed with tree-sitter; top-level definitions and their references
  form a graph that is ranked with personalized PageRank. Only Go, Python, and
  TypeScript/TSX are parsed; other files are skipped (and counted in a trailer).

Arguments:
- focus    (optional): array of strings; each is a file path or a symbol name to
  bias the ranking toward.
- max_files (optional): maximum number of files to include (default 50, max 200).

Example:
  {"focus": ["internal/agent/loop.go", "PageRank"], "max_files": 25}

Limits:
- Output is capped (~25000 bytes) and per file shows at most the top signatures;
  use focus/max_files to narrow it.`

const repoMapSchema = `{
  "type": "object",
  "properties": {
    "focus": {
      "type": "array",
      "items": {"type": "string"},
      "description": "File paths or symbol names to bias the PageRank toward."
    },
    "max_files": {
      "type": "integer",
      "description": "Maximum number of files to include (default 50, max 200)."
    }
  }
}`

// RepoMap is the read-only repo-map tool. It carries no mutable state; a single
// value is safe for concurrent Execute calls (each call constructs its own
// parsers and graph).
type RepoMap struct{}

// Compile-time assertion that RepoMap implements tool.Tool.
var _ tool.Tool = RepoMap{}

// NewTool constructs the repo-map tool ready for registration in the catalog,
// e.g. cat.MustRegister(repomap.NewTool()).
func NewTool() tool.Tool { return RepoMap{} }

// repoMapArgs is the JSON argument shape for the repo-map tool.
type repoMapArgs struct {
	Focus    []string `json:"focus"`
	MaxFiles int      `json:"max_files"`
}

// Spec returns the model-facing specification of the repo-map tool.
func (RepoMap) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name:        "RepoMap",
		Description: repoMapDescription,
		Schema:      toolkit.Schema(repoMapSchema),
	}
}

// ReadOnly reports that the repo map only reads the workspace.
func (RepoMap) ReadOnly() bool { return true }

// progressEvery is how often parseAll emits a "parsed K/N files" progress line:
// frequent enough that a long scan visibly advances, infrequent enough not to
// flood the event stream.
const progressEvery = 64

// Execute walks the workspace, parses supported source files, ranks them with
// personalized PageRank, and returns a compact ranked map. It is the ordinary
// (non-observed) entry point: it delegates to ExecuteObserved with a nil emit, so
// its behaviour is byte-identical to the observed path minus the transient
// progress events.
func (r RepoMap) Execute(ctx context.Context, in session.ToolCall, ws tool.Workspace) (session.ToolResult, error) {
	return r.ExecuteObserved(ctx, in, ws, nil)
}

// ExecuteObserved is the observability seam (matching internal/agent's
// observableTool): when the dispatcher supplies a non-nil emit closure, the repo
// map forwards transient, human-readable progress lines at its phase boundaries
// (discovery, periodic parse, ranking) so a long-running scan does not look dead
// in the TUI/editor. The events are advisory — EvToolProgress is never persisted
// or recorded to model history. With emit == nil (the Execute path) no progress
// is emitted and the result is byte-identical to before. Recoverable problems (no
// source files found) are returned as tool results, not Go errors; a Go error is
// reserved for harness-level faults (e.g. context cancellation).
//
// This adapter must NOT import internal/agent (import cycle): the dispatcher's
// observableTool assertion is structural, so matching this method set is enough.
func (RepoMap) ExecuteObserved(ctx context.Context, in session.ToolCall, ws tool.Workspace, emit func(session.Event)) (session.ToolResult, error) {
	// progress is a nil-safe local: with emit == nil it is a no-op, so the Execute
	// path emits nothing. We do NOT set Turn/Seq here — the dispatcher's closure
	// stamps Turn and e.emit assigns Seq.
	progress := func(text string) {
		if emit != nil {
			emit(session.Event{Type: session.EvToolProgress, Text: text})
		}
	}

	var args repoMapArgs
	if msg, ok := toolkit.ParseArgs(in, &args); !ok {
		return session.NewToolError(in.ID, msg), nil
	}

	maxFiles := args.MaxFiles
	switch {
	case maxFiles <= 0:
		maxFiles = defaultMaxFiles
	case maxFiles > maxFilesHardCap:
		maxFiles = maxFilesHardCap
	}

	paths, err := discoverFiles(ctx, ws)
	if err != nil {
		return session.ToolResult{}, err
	}
	if len(paths) == 0 {
		return session.NewToolResult(in.ID, "no supported source files found (looked for Go, Python, TypeScript/TSX)"), nil
	}
	progress(fmt.Sprintf("repo map: scanning %d candidate files", len(paths)))

	nodes, skipped, err := parseAll(ctx, ws, paths, progress)
	if err != nil {
		return session.ToolResult{}, err
	}
	if len(nodes) == 0 {
		return session.NewToolResult(in.ID, "no parseable definitions found in the workspace"), nil
	}

	progress(fmt.Sprintf("repo map: ranking %d files", len(nodes)))
	g := newGraph(nodes)
	focus := resolveFocus(g, args.Focus)
	g.pageRank(focus, pageRankDamping, pageRankIters)

	out := render(g, focus, maxFiles, skipped)
	return session.NewToolResult(in.ID, toolkit.Truncate(out, toolkit.MaxOutputBytes)), nil
}

// discoverFiles globs the workspace for candidate source files across all
// supported extensions, de-duplicating and sorting the result.
func discoverFiles(ctx context.Context, ws tool.Workspace) ([]string, error) {
	seen := make(map[string]bool)
	var paths []string
	for _, pattern := range scanGlobs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches, err := ws.Glob(ctx, pattern)
		if err != nil {
			// A glob the adapter cannot honor (e.g. "**") is not fatal; skip it
			// and rely on the others.
			continue
		}
		for _, m := range matches {
			// Skip matches under ignored directories before recording them:
			// filepath.Glob's "*" matches dotfiles, and worktrees/vendor/node_modules
			// duplicate or balloon the map (see isIgnoredPath).
			if isIgnoredPath(m) {
				continue
			}
			if !seen[m] {
				seen[m] = true
				paths = append(paths, m)
			}
		}
	}
	sort.Strings(paths)
	if len(paths) > maxScannedFiles {
		paths = paths[:maxScannedFiles]
	}
	return paths, nil
}

// parseAll reads and parses each candidate file, returning the non-nil file
// nodes plus a count of files skipped (read error, oversized, or empty/unsupported
// parse). A single tree-sitter WASM session is created for the whole call and
// reused across files (the parser is sequential, so the non-concurrent session is
// safe here). progress is a nil-safe callback the caller threads in to surface
// periodic "parsed K/N files" lines; it is never called when emit was nil.
func parseAll(ctx context.Context, ws tool.Workspace, paths []string, progress func(string)) (nodes []*fileNode, skipped int, err error) {
	sess, err := newParseSession()
	if err != nil {
		return nil, 0, err
	}
	total := len(paths)
	for i, p := range paths {
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, cerr
		}
		// Surface progress every progressEvery files (and never on the very first
		// iteration, which the "scanning N candidate files" line already covers).
		if progress != nil && i > 0 && i%progressEvery == 0 {
			progress(fmt.Sprintf("repo map: parsed %d/%d files", i, total))
		}
		data, rerr := ws.Read(ctx, p)
		if rerr != nil {
			skipped++
			continue
		}
		// Skip files larger than maxFileBytes: this bounds worst-case single-file
		// tree-sitter parse time (ParseString is uninterruptible), and huge
		// generated/minified files add little to a structural symbol map.
		if len(data) > maxFileBytes {
			skipped++
			continue
		}
		node, perr := sess.parseFile(ctx, p, data)
		if perr != nil {
			skipped++
			continue
		}
		if node == nil {
			skipped++
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes, skipped, nil
}

// resolveFocus maps caller focus tokens to the set of file paths PageRank should
// be biased toward. A token matches a file path if it equals, is a suffix of, or
// is contained in the path; it also matches every file that defines a symbol of
// that name. Returns an empty set when no token resolves (uniform PageRank).
func resolveFocus(g *graph, tokens []string) map[string]bool {
	focus := make(map[string]bool)
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		for p := range g.files {
			if p == tok || strings.HasSuffix(p, "/"+tok) || strings.Contains(p, tok) {
				focus[p] = true
			}
		}
		for _, site := range g.defSites[tok] {
			focus[site] = true
		}
	}
	return focus
}
