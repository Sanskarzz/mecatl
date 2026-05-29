package repomap

import (
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// language describes how to parse one source language: its tree-sitter grammar
// and a tag query that captures top-level definitions (@def.*) and call/use
// references (@ref.*). The query mirrors Aider's def/ref tag approach: @def.*
// captures name the file *provides*, @ref.* captures names the file *uses*. The
// graph builder links a ref in file A to a def of the same name in file B.
type language struct {
	// name is the human-readable language label rendered in the map.
	name string
	// grammar is the tree-sitter language the parser is configured with.
	grammar *sitter.Language
	// query is the compiled def/ref tag query (see langQuery for the source).
	query *sitter.Query
}

// definitionKind classifies a captured definition for signature rendering.
type definitionKind int

const (
	kindFunc definitionKind = iota
	kindMethod
	kindType
	kindClass
)

// langRegistry maps a lowercase file extension (with leading dot) to the
// language used to parse it. It is built once at package init from the bundled
// grammars; unknown extensions are skipped gracefully (see detectLanguage).
var langRegistry = map[string]*language{}

// goTagQuery captures Go top-level functions, methods, type/interface/struct
// definitions, and call/type references.
const goTagQuery = `
(function_declaration name: (identifier) @def.func)
(method_declaration name: (field_identifier) @def.method)
(type_declaration (type_spec name: (type_identifier) @def.type))
(call_expression function: (identifier) @ref.call)
(call_expression function: (selector_expression field: (field_identifier) @ref.call))
(type_identifier) @ref.type
`

// pyTagQuery captures Python function and class definitions and call references.
const pyTagQuery = `
(function_definition name: (identifier) @def.func)
(class_definition name: (identifier) @def.class)
(call function: (identifier) @ref.call)
(call function: (attribute attribute: (identifier) @ref.call))
`

// tsTagQuery captures TypeScript/TSX function, method, and class definitions and
// call references. It covers function declarations, arrow-bound consts, class
// and method definitions, and call expressions.
const tsTagQuery = `
(function_declaration name: (identifier) @def.func)
(class_declaration name: (type_identifier) @def.class)
(method_definition name: (property_identifier) @def.method)
(call_expression function: (identifier) @ref.call)
(call_expression function: (member_expression property: (property_identifier) @ref.call))
`

// registerLanguage compiles a tag query for grammar and registers it under each
// of the given extensions. A query that fails to compile is fatal at init: the
// query strings are package constants, so a compile failure is a programming
// bug, not a runtime input error.
func registerLanguage(name string, grammar *sitter.Language, query string, exts ...string) {
	q, err := sitter.NewQuery([]byte(query), grammar)
	if err != nil {
		panic("repomap: bad tag query for " + name + ": " + err.Error())
	}
	lang := &language{name: name, grammar: grammar, query: q}
	for _, ext := range exts {
		langRegistry[ext] = lang
	}
}

func init() {
	registerLanguage("go", golang.GetLanguage(), goTagQuery, ".go")
	registerLanguage("python", python.GetLanguage(), pyTagQuery, ".py")
	registerLanguage("typescript", typescript.GetLanguage(), tsTagQuery, ".ts")
	registerLanguage("tsx", tsx.GetLanguage(), tsTagQuery, ".tsx")
}
