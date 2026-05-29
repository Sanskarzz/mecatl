package repomap

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	sitter "github.com/malivvan/tree-sitter"
)

// bodyNodeKinds is the set of node kinds that represent a definition's *body* —
// the part elided from a rendered signature. When a captured definition node has
// a direct named child of one of these kinds, the signature is the source from
// the definition's start up to that body's start byte. Go type declarations have
// no body block (the struct/interface is part of the declaration), so the whole
// node is rendered for those.
var bodyNodeKinds = map[string]bool{
	"block":           true, // Go funcs/methods, Python funcs/classes
	"statement_block": true, // JS/TS functions and methods
	"class_body":      true, // JS/TS classes
}

// nameNodeKinds is the set of node kinds that can carry a definition's name.
// Name extraction scans a definition node's direct named children for the first
// child of one of these kinds (see definitionName); method names use
// field_identifier (Go) or property_identifier (JS/TS), type/func/class names use
// identifier or type_identifier.
var nameNodeKinds = map[string]bool{
	"identifier":          true,
	"field_identifier":    true,
	"property_identifier": true,
	"type_identifier":     true,
}

// parseSession wraps a single wazero-backed tree-sitter module instance plus a
// per-grammar Language cache. A session is created once per repo-map Execute call
// and reused across every file in that call; it is NOT safe for concurrent use
// (the underlying WASM module has a single linear memory), which is fine because
// parseAll walks files sequentially.
type parseSession struct {
	ts    *sitter.TreeSitter
	langs map[string]*sitter.Language
}

// newParseSession instantiates a fresh tree-sitter WASM module. The embedded
// grammar blob is decompressed and compiled once per process (cached in the
// library); each session gets its own module instance. No vfs or stdout is wired
// up because the repo map neither reads files through WASI nor needs grammar
// output. Fully offline: the grammar bytes are embedded in the dependency.
func newParseSession() (*parseSession, error) {
	ts, err := sitter.New(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("repomap: init tree-sitter wasm runtime: %w", err)
	}
	return &parseSession{ts: ts, langs: make(map[string]*sitter.Language)}, nil
}

// language returns the cached tree-sitter Language for a grammar name, loading it
// from the WASM module on first use.
func (s *parseSession) language(grammar string) (*sitter.Language, error) {
	if l, ok := s.langs[grammar]; ok {
		return l, nil
	}
	l, err := s.ts.Language(grammar)
	if err != nil {
		return nil, err
	}
	s.langs[grammar] = l
	return l, nil
}

// parseFile parses one file's source and returns its extracted file node, or nil
// if the language is unsupported or the file yielded no definitions and no
// references. ctx cancellation is checked between query matches.
func (s *parseSession) parseFile(ctx context.Context, relPath string, src []byte) (*fileNode, error) {
	lang := detectLanguage(relPath)
	if lang == nil {
		return nil, nil // unsupported language: skipped gracefully
	}

	tsLang, err := s.language(lang.grammar)
	if err != nil {
		return nil, err
	}

	parser, err := s.ts.NewParser()
	if err != nil {
		return nil, err
	}
	defer func() { _ = parser.Close() }()
	if err := parser.SetLanguage(tsLang); err != nil {
		return nil, err
	}

	tree, err := parser.ParseString(string(src))
	if err != nil {
		return nil, err
	}
	root, err := tree.RootNode()
	if err != nil {
		return nil, err
	}

	query, err := s.ts.NewQuery(lang.query, tsLang)
	if err != nil {
		return nil, fmt.Errorf("repomap: compile %s query: %w", lang.name, err)
	}
	cursor, err := s.ts.NewQueryCursor()
	if err != nil {
		return nil, err
	}
	if err := cursor.Exec(query, root); err != nil {
		return nil, err
	}

	node := &fileNode{
		path: relPath,
		lang: lang.name,
		refs: make(map[string]int),
	}
	if err := collectCaptures(ctx, cursor, query, node, src); err != nil {
		return nil, err
	}

	if len(node.defs) == 0 && len(node.refs) == 0 {
		return nil, nil
	}
	return node, nil
}

// collectCaptures drives the query cursor to completion, routing each @def.* /
// @ref.* capture into the file node. ctx cancellation is honored between matches.
func collectCaptures(ctx context.Context, cursor *sitter.QueryCursor, query *sitter.Query, node *fileNode, src []byte) error {
	seenDef := make(map[string]bool) // dedupe defs by name+line
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		match, ok, err := cursor.NextMatch()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		for _, cap := range match.Captures {
			capName, err := query.CaptureNameForID(cap.ID)
			if err != nil {
				return err
			}
			switch {
			case strings.HasPrefix(capName, "def."):
				if err := addDefinition(node, cap.Node, src, seenDef); err != nil {
					return err
				}
			case strings.HasPrefix(capName, "ref."):
				text, err := nodeText(cap.Node, src)
				if err != nil {
					return err
				}
				node.refs[text]++
			}
		}
	}
}

// addDefinition extracts the name and a body-elided signature from a captured
// whole-declaration node and appends a symbol to the file node (deduped by
// name+line). A definition whose name cannot be located is skipped.
func addDefinition(node *fileNode, def *sitter.Node, src []byte, seen map[string]bool) error {
	kind, err := def.Kind()
	if err != nil {
		return err
	}
	defKind, isDef := defNodeKinds[kind]
	if !isDef {
		return nil
	}

	name, err := definitionName(def, src)
	if err != nil {
		return err
	}
	if name == "" {
		return nil // anonymous/unnameable declaration: skip
	}

	startByte, err := def.StartByte()
	if err != nil {
		return err
	}
	line := lineOf(src, clampOffset(startByte, len(src)))
	key := name + ":" + itoa(line)
	if seen[key] {
		return nil
	}
	seen[key] = true

	sig, err := renderSignature(def, src)
	if err != nil {
		return err
	}
	node.defs = append(node.defs, symbol{
		path:      node.path,
		name:      name,
		signature: sig,
		kind:      defKind,
		line:      line,
	})
	return nil
}

// definitionName returns the identifier naming a definition node. It scans the
// node's direct named children for the first name-bearing child; for Go type
// declarations (which nest the name inside a type_spec) it descends one level.
func definitionName(def *sitter.Node, src []byte) (string, error) {
	kind, err := def.Kind()
	if err != nil {
		return "", err
	}
	// Go: `type Foo ...` nests the name under a type_spec child.
	if kind == "type_declaration" {
		spec, err := firstChildOfKind(def, "type_spec")
		if err != nil || spec == nil {
			return "", err
		}
		def = spec
	}
	count, err := def.NamedChildCount()
	if err != nil {
		return "", err
	}
	for i := uint64(0); i < count; i++ {
		child, err := def.NamedChild(i)
		if err != nil {
			return "", err
		}
		ck, err := child.Kind()
		if err != nil {
			return "", err
		}
		if nameNodeKinds[ck] {
			return nodeText(child, src)
		}
	}
	return "", nil
}

// renderSignature returns a single-line, body-elided signature for a definition
// node: the source from the node start up to its body block (if any), collapsed
// to one line. Declarations without a body block (e.g. Go type declarations) are
// rendered whole.
func renderSignature(def *sitter.Node, src []byte) (string, error) {
	startByte, err := def.StartByte()
	if err != nil {
		return "", err
	}
	endByte, err := def.EndByte()
	if err != nil {
		return "", err
	}
	end := clampOffset(endByte, len(src))

	body, err := firstBodyChild(def)
	if err != nil {
		return "", err
	}
	if body != nil {
		bodyStart, err := body.StartByte()
		if err != nil {
			return "", err
		}
		end = clampOffset(bodyStart, len(src))
	}

	start := clampOffset(startByte, len(src))
	if start > end {
		return "", fmt.Errorf("repomap: signature byte range [%d:%d] out of bounds (len %d)", start, end, len(src))
	}
	return collapse(string(src[start:end])), nil
}

// firstBodyChild returns the definition node's first direct named child whose
// kind is a body kind, or nil if the declaration has no body block.
func firstBodyChild(def *sitter.Node) (*sitter.Node, error) {
	count, err := def.NamedChildCount()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < count; i++ {
		child, err := def.NamedChild(i)
		if err != nil {
			return nil, err
		}
		ck, err := child.Kind()
		if err != nil {
			return nil, err
		}
		if bodyNodeKinds[ck] {
			return child, nil
		}
	}
	return nil, nil
}

// firstChildOfKind returns the first direct named child of n with the given
// kind, or nil if none.
func firstChildOfKind(n *sitter.Node, kind string) (*sitter.Node, error) {
	count, err := n.NamedChildCount()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < count; i++ {
		child, err := n.NamedChild(i)
		if err != nil {
			return nil, err
		}
		ck, err := child.Kind()
		if err != nil {
			return nil, err
		}
		if ck == kind {
			return child, nil
		}
	}
	return nil, nil
}

// nodeText returns the source slice spanned by a node.
func nodeText(n *sitter.Node, src []byte) (string, error) {
	startByte, err := n.StartByte()
	if err != nil {
		return "", err
	}
	endByte, err := n.EndByte()
	if err != nil {
		return "", err
	}
	start, end := clampOffset(startByte, len(src)), clampOffset(endByte, len(src))
	if start > end {
		return "", fmt.Errorf("repomap: node byte range [%d:%d] out of bounds (len %d)", start, end, len(src))
	}
	return string(src[start:end]), nil
}

// clampOffset converts a tree-sitter WASM byte offset (uint64) to an in-bounds
// int index into a source buffer of length srcLen, clamping out-of-range values.
// This is the single guarded uint64->int narrowing for the package: every byte
// offset the WASM module returns is bounded to [0, srcLen], so the conversion
// cannot overflow or index out of range.
func clampOffset(off uint64, srcLen int) int {
	// srcLen is a slice length, so it is always non-negative and fits in uint64.
	if off > uint64(srcLen) { //nolint:gosec // srcLen = len(src) >= 0, no overflow
		return srcLen
	}
	// off <= srcLen here, and srcLen is a valid int, so the narrowing is safe.
	return int(off) //nolint:gosec // bounded above by srcLen (a valid int)
}

// lineOf returns the 1-based line number of byte offset off in src. off is
// assumed already clamped to [0, len(src)] (see clampOffset).
func lineOf(src []byte, off int) int {
	return 1 + bytes.Count(src[:off], []byte{'\n'})
}

// collapse flattens whitespace runs (including newlines) in s to single spaces
// and trims the result, yielding a compact one-line signature.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// itoa is a tiny stdlib-free int-to-string for the dedupe key. It avoids pulling
// strconv into this path for a trivial conversion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
