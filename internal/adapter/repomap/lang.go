package repomap

import (
	"path"
	"strings"
)

// language describes how to parse one source language with the WASM tree-sitter
// runtime: the grammar name to load from the embedded module and a tag query
// that captures whole top-level definition nodes (@def.*) plus call/use
// references (@ref.*).
//
// Unlike a bottom-up binding that captures a definition's *name* node and walks
// to the enclosing declaration via Parent(), the WASM Node API exposes no
// Parent()/ChildByFieldName(). So the queries here capture the WHOLE declaration
// node (@def.func/@def.method/@def.type/@def.class); the extractor then descends
// into that node's children to find the name and the body (see parse.go). @ref.*
// captures are leaf identifiers and need no walking. This mirrors Aider's def/ref
// tag model: @def.* is what a file provides, @ref.* is what it uses; the graph
// builder links a ref in file A to a def of the same name in file B.
type language struct {
	// name is the human-readable language label rendered in the map.
	name string
	// grammar is the tree-sitter grammar name exported by the WASM module
	// (e.g. "go", "python", "javascript").
	grammar string
	// query is the def/ref tag query source (compiled per parse session because
	// a compiled query is bound to a module instance; see parse.go).
	query string
}

// definitionKind classifies a captured definition for signature rendering.
type definitionKind int

const (
	kindFunc definitionKind = iota
	kindMethod
	kindType
	kindClass
)

// defNodeKinds maps a captured top-level declaration node kind to its definition
// kind. A @def.* capture fires on the whole declaration node (see lang.go's
// package note); the extractor looks up the node's kind here to classify it and
// to confirm it is a renderable definition before extracting name/signature.
var defNodeKinds = map[string]definitionKind{
	// Go
	"function_declaration": kindFunc,
	"method_declaration":   kindMethod,
	"type_declaration":     kindType,
	// Python
	"function_definition": kindFunc,
	"class_definition":    kindClass,
	// JavaScript / TypeScript / TSX
	"class_declaration": kindClass,
	"method_definition": kindMethod,
}

// langRegistry maps a lowercase file extension (with leading dot) to the
// language used to parse it. Unknown extensions are skipped gracefully (see
// detectLanguage). TypeScript/TSX and JavaScript are all parsed with the
// "javascript" grammar: the embedded WASM module exports go/python/javascript
// (not a standalone typescript grammar), and the JavaScript grammar recovers
// function/class/method declarations from TS source — TS-only type annotations
// surface as benign ERROR subtrees that do not disturb def/ref extraction.
var langRegistry = map[string]*language{}

// goTagQuery captures Go top-level functions, methods, type/interface/struct
// definitions (whole declaration nodes), and call/type references.
const goTagQuery = `
(function_declaration) @def.func
(method_declaration) @def.method
(type_declaration) @def.type
(call_expression function: (identifier) @ref.call)
(call_expression function: (selector_expression field: (field_identifier) @ref.call))
(type_identifier) @ref.type
`

// pyTagQuery captures Python function and class definitions (whole declaration
// nodes) and call references.
const pyTagQuery = `
(function_definition) @def.func
(class_definition) @def.class
(call function: (identifier) @ref.call)
(call function: (attribute attribute: (identifier) @ref.call))
`

// jsTagQuery captures JavaScript/TypeScript/TSX function, method, and class
// definitions (whole declaration nodes) and call references. It covers function
// declarations, class and method definitions, and call expressions.
const jsTagQuery = `
(function_declaration) @def.func
(class_declaration) @def.class
(method_definition) @def.method
(call_expression function: (identifier) @ref.call)
(call_expression function: (member_expression property: (property_identifier) @ref.call))
`

// registerLanguage registers a language under each of the given extensions.
func registerLanguage(name, grammar, query string, exts ...string) {
	lang := &language{name: name, grammar: grammar, query: query}
	for _, ext := range exts {
		langRegistry[ext] = lang
	}
}

func init() {
	registerLanguage("go", "go", goTagQuery, ".go")
	registerLanguage("python", "python", pyTagQuery, ".py")
	// TS/TSX/JS all parse with the JavaScript grammar; the rendered language
	// label preserves the source kind so the map reads naturally.
	registerLanguage("typescript", "javascript", jsTagQuery, ".ts", ".tsx")
	registerLanguage("javascript", "javascript", jsTagQuery, ".js", ".jsx", ".mjs", ".cjs")
}

// detectLanguage returns the language for a path's extension, or nil if the
// extension is not one of the supported languages (caller skips the file).
func detectLanguage(p string) *language {
	return langRegistry[strings.ToLower(path.Ext(p))]
}
