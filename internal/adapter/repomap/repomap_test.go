package repomap

import (
	"context"
	"strings"
	"testing"

	"github.com/stacklok/ozzharness/internal/adapter/memfs"
	"github.com/stacklok/ozzharness/internal/session"
	"github.com/stacklok/ozzharness/internal/tool"
)

// seedWorkspace returns an in-memory workspace populated with the given files.
func seedWorkspace(t *testing.T, files map[string]string) tool.Workspace {
	t.Helper()
	ws := memfs.NewWorkspace("/repo")
	for p, body := range files {
		if err := ws.Write(context.Background(), p, []byte(body)); err != nil {
			t.Fatalf("seed write %s: %v", p, err)
		}
	}
	return ws
}

// run executes the tool with the given JSON args and returns the result.
func run(t *testing.T, ws tool.Workspace, args string) session.ToolResult {
	t.Helper()
	call := session.NewToolCall("call-1", "RepoMap", []byte(args))
	res, err := NewTool().Execute(context.Background(), call, ws)
	if err != nil {
		t.Fatalf("Execute returned harness error: %v", err)
	}
	return res
}

// hubFiles is a small Go corpus where util.go is referenced by many other files,
// so it should rank highest under uniform PageRank.
func hubFiles() map[string]string {
	return map[string]string{
		"util.go": `package app

func Helper(x int) int { return x * 2 }

type Config struct{ Name string }
`,
		"a.go": `package app

func DoA() int { return Helper(1) }
`,
		"b.go": `package app

func DoB() int { return Helper(2) }
`,
		"c.go": `package app

func DoC() int { return Helper(3) }
`,
		"lonely.go": `package app

func Unused() string { return "x" }
`,
	}
}

func TestRepoMap_ListsSymbolsAndSignatures(t *testing.T) {
	ws := seedWorkspace(t, hubFiles())
	res := run(t, ws, "{}")
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	want := []string{
		"util.go",
		"func Helper(x int) int",
		"type Config struct{ Name string }",
		"a.go",
		"func DoA() int",
	}
	for _, w := range want {
		if !strings.Contains(res.Content, w) {
			t.Errorf("map missing %q\n---map---\n%s", w, res.Content)
		}
	}
}

func TestRepoMap_HubRanksHighest(t *testing.T) {
	ws := seedWorkspace(t, hubFiles())
	res := run(t, ws, "{}")
	utilIdx := strings.Index(res.Content, "util.go")
	lonelyIdx := strings.Index(res.Content, "lonely.go")
	if utilIdx < 0 || lonelyIdx < 0 {
		t.Fatalf("expected both files in map:\n%s", res.Content)
	}
	if utilIdx > lonelyIdx {
		t.Errorf("expected util.go (referenced by many) to rank above lonely.go\n%s", res.Content)
	}
}

func TestRepoMap_FocusBiasesRanking(t *testing.T) {
	ws := seedWorkspace(t, hubFiles())

	// Without focus, lonely.go ranks last (no inbound refs).
	base := run(t, ws, "{}")
	baseLonely := strings.Index(base.Content, "lonely.go")

	// Focusing on lonely.go must pull it above the unfocused baseline position.
	focused := run(t, ws, `{"focus": ["lonely.go"]}`)
	focusedLonely := strings.Index(focused.Content, "lonely.go")
	if focusedLonely < 0 {
		t.Fatalf("focused map missing lonely.go:\n%s", focused.Content)
	}
	if focusedLonely >= baseLonely {
		t.Errorf("focus did not raise lonely.go (focused idx %d, base idx %d)\n%s",
			focusedLonely, baseLonely, focused.Content)
	}
	if !strings.Contains(focused.Content, "focus file") {
		t.Errorf("expected focus note in header:\n%s", focused.Content)
	}
}

func TestRepoMap_FocusBySymbolName(t *testing.T) {
	ws := seedWorkspace(t, hubFiles())
	res := run(t, ws, `{"focus": ["Unused"]}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	// The symbol "Unused" is defined in lonely.go, so focus by symbol name must
	// pull lonely.go up to the top region of the map.
	lonelyIdx := strings.Index(res.Content, "lonely.go")
	utilIdx := strings.Index(res.Content, "util.go")
	if lonelyIdx < 0 || utilIdx < 0 {
		t.Fatalf("missing files:\n%s", res.Content)
	}
	if lonelyIdx > utilIdx {
		t.Errorf("symbol focus did not raise lonely.go above util.go\n%s", res.Content)
	}
}

func TestRepoMap_MaxFilesCaps(t *testing.T) {
	ws := seedWorkspace(t, hubFiles())
	res := run(t, ws, `{"max_files": 2}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "showing top 2") {
		t.Errorf("expected cap note 'showing top 2':\n%s", res.Content)
	}
	// Exactly two of the five files should appear as rendered entries (lines that
	// end with the language tag "  (go)").
	count := strings.Count(res.Content, "  (go)")
	if count != 2 {
		t.Errorf("expected 2 file entries rendered, got %d\n%s", count, res.Content)
	}
}

func TestRepoMap_PythonSupported(t *testing.T) {
	ws := seedWorkspace(t, map[string]string{
		"mod.py": "def helper(a):\n    return a\n\nclass Widget:\n    def render(self):\n        return helper(1)\n",
	})
	res := run(t, ws, "{}")
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, w := range []string{"mod.py", "(python)", "def helper(a)", "class Widget"} {
		if !strings.Contains(res.Content, w) {
			t.Errorf("python map missing %q\n%s", w, res.Content)
		}
	}
}

func TestRepoMap_TypeScriptSupported(t *testing.T) {
	ws := seedWorkspace(t, map[string]string{
		"app.ts": "export function greet(name: string): string { return name; }\nclass Svc { run() { return greet(\"x\"); } }\n",
	})
	res := run(t, ws, "{}")
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, w := range []string{"app.ts", "(typescript)", "function greet(name: string): string", "class Svc"} {
		if !strings.Contains(res.Content, w) {
			t.Errorf("typescript map missing %q\n%s", w, res.Content)
		}
	}
}

func TestRepoMap_UnsupportedLanguageSkipped(t *testing.T) {
	ws := seedWorkspace(t, map[string]string{
		"util.go":   "package app\nfunc Helper() {}\n",
		"README.md": "# title\nsome text\n",
		"data.json": `{"k": 1}`,
	})
	res := run(t, ws, "{}")
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "README.md") || strings.Contains(res.Content, "data.json") {
		t.Errorf("unsupported files should not appear in the map:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "util.go") {
		t.Errorf("supported file missing:\n%s", res.Content)
	}
}

func TestRepoMap_NestedDirectories(t *testing.T) {
	ws := seedWorkspace(t, map[string]string{
		"root.go":           "package app\nfunc Root() int { return Deep() }\n",
		"pkg/mid.go":        "package pkg\nfunc Mid() int { return 1 }\n",
		"pkg/inner/deep.go": "package inner\nfunc Deep() int { return 2 }\n",
	})
	res := run(t, ws, "{}")
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, w := range []string{"root.go", "pkg/mid.go", "pkg/inner/deep.go"} {
		if !strings.Contains(res.Content, w) {
			t.Errorf("nested file missing from map: %q\n%s", w, res.Content)
		}
	}
}

func TestRepoMap_EmptyWorkspace(t *testing.T) {
	ws := seedWorkspace(t, map[string]string{})
	res := run(t, ws, "{}")
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no supported source files") {
		t.Errorf("expected empty-workspace message, got:\n%s", res.Content)
	}
}

func TestRepoMap_BadArgs(t *testing.T) {
	ws := seedWorkspace(t, hubFiles())
	res := run(t, ws, `{"focus": "not-an-array"}`)
	if !res.IsError {
		t.Errorf("expected error result for malformed args, got:\n%s", res.Content)
	}
}

// TestRepoMap_EmbeddedGrammarLoadsOffline asserts the WebAssembly tree-sitter
// runtime and grammars load from the embedded blob with no network access. The
// grammar bytes are compiled into the binary (go:embed inside the dependency), so
// a successful parse here proves the parser is fully offline: there is no code
// path that reaches out to the network, and this test exercises the real WASM
// runtime end to end (instantiate module, load the Go grammar, run the query).
func TestRepoMap_EmbeddedGrammarLoadsOffline(t *testing.T) {
	sess, err := newParseSession()
	if err != nil {
		t.Fatalf("creating parse session (embedded WASM runtime) failed: %v", err)
	}
	src := []byte("package app\nfunc Helper(x int) int { return Other(x) }\n")
	node, err := sess.parseFile(context.Background(), "util.go", src)
	if err != nil {
		t.Fatalf("offline parse failed: %v", err)
	}
	if node == nil {
		t.Fatal("expected a parsed file node, got nil")
	}
	if len(node.defs) != 1 || node.defs[0].name != "Helper" {
		t.Fatalf("expected one def named Helper, got %+v", node.defs)
	}
	if node.defs[0].signature != "func Helper(x int) int" {
		t.Errorf("unexpected signature %q", node.defs[0].signature)
	}
	if node.refs["Other"] == 0 {
		t.Errorf("expected a reference to Other, got refs %v", node.refs)
	}
}

func TestRepoMap_SpecAndReadOnly(t *testing.T) {
	tl := NewTool()
	if !tl.ReadOnly() {
		t.Error("RepoMap must be read-only")
	}
	spec := tl.Spec()
	if spec.Name != "RepoMap" {
		t.Errorf("unexpected name %q", spec.Name)
	}
	if len(spec.Description) < 100 {
		t.Error("expected a documentation-quality description")
	}
	if len(spec.Schema) == 0 {
		t.Error("expected a non-empty schema")
	}
}
