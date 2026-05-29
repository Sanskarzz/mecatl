package repomap

import (
	"context"
	"path"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// defNodeTypes is the set of AST node types that represent a renderable
// top-level definition. When a @def.* capture fires, the extractor climbs from
// the captured name node to the nearest ancestor of one of these types and
// renders that ancestor's signature (header up to the body).
var defNodeTypes = map[string]definitionKind{
	// Go
	"function_declaration": kindFunc,
	"method_declaration":   kindMethod,
	"type_declaration":     kindType,
	// Python
	"function_definition": kindFunc,
	"class_definition":    kindClass,
	// TypeScript / TSX
	"class_declaration": kindClass,
	"method_definition": kindMethod,
}

// detectLanguage returns the language for a path's extension, or nil if the
// extension is not one of the supported languages (caller skips the file).
func detectLanguage(p string) *language {
	return langRegistry[strings.ToLower(path.Ext(p))]
}

// parseFile parses one file's source and returns its extracted file node, or
// nil if the language is unsupported or the file yielded no definitions and no
// references. ctx cancellation is honored by the tree-sitter parser.
func parseFile(ctx context.Context, relPath string, src []byte) (*fileNode, error) {
	lang := detectLanguage(relPath)
	if lang == nil {
		return nil, nil // unsupported language: skipped gracefully
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang.grammar)
	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	node := &fileNode{
		path: relPath,
		lang: lang.name,
		refs: make(map[string]int),
	}

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(lang.query, tree.RootNode())

	seenDef := make(map[string]bool) // dedupe defs by name+line
	for {
		match, ok := qc.NextMatch()
		if !ok {
			break
		}
		for _, cap := range match.Captures {
			name := lang.query.CaptureNameForId(cap.Index)
			text := cap.Node.Content(src)
			switch {
			case strings.HasPrefix(name, "def."):
				addDefinition(node, cap.Node, src, text, seenDef)
			case strings.HasPrefix(name, "ref."):
				node.refs[text]++
			}
		}
	}

	if len(node.defs) == 0 && len(node.refs) == 0 {
		return nil, nil
	}
	return node, nil
}

// addDefinition climbs from a captured name node to its enclosing definition
// node, renders a body-elided signature, and appends a symbol to the file node
// (deduped by name+line).
func addDefinition(node *fileNode, nameNode *sitter.Node, src []byte, name string, seen map[string]bool) {
	def := enclosingDef(nameNode)
	if def == nil {
		return
	}
	line := int(def.StartPoint().Row) + 1
	key := name + ":" + itoa(line)
	if seen[key] {
		return
	}
	seen[key] = true
	node.defs = append(node.defs, symbol{
		path:      node.path,
		name:      name,
		signature: renderSignature(def, src),
		kind:      defNodeTypes[def.Type()],
		line:      line,
	})
}

// enclosingDef walks ancestors from n until it finds a node whose type is a
// known definition kind, or nil if none is found within the file.
func enclosingDef(n *sitter.Node) *sitter.Node {
	for cur := n; cur != nil; cur = cur.Parent() {
		if _, ok := defNodeTypes[cur.Type()]; ok {
			return cur
		}
	}
	return nil
}

// renderSignature returns a single-line, body-elided signature for a definition
// node: the source text from the node start up to its body block (if any),
// collapsed to one line. For type/class declarations without a body field the
// whole declaration is rendered (also collapsed).
func renderSignature(def *sitter.Node, src []byte) string {
	end := def.EndByte()
	if body := def.ChildByFieldName("body"); body != nil {
		end = body.StartByte()
	}
	raw := string(src[def.StartByte():end])
	return collapse(raw)
}

// collapse flattens whitespace runs (including newlines) in s to single spaces
// and trims the result, yielding a compact one-line signature.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// itoa is a tiny stdlib-free int-to-string for the dedupe key. It avoids
// pulling strconv into this hot path for a trivial conversion.
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
