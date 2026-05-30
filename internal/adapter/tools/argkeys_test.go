package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEditWriteArgKeysAreStable is a DRIFT GUARD for the mecatui TUI.
//
// cmd/mecatui/ui hand-mirrors editArgs/writeArgs (as editDiffArgs/writeDiffArgs)
// to render Edit/Write tool cards as colourised diffs. That mirror crosses an
// architectural boundary (the ui must not import internal/...), so there is no
// compile-time link: if a JSON tag here is renamed, the TUI silently falls back
// to raw-JSON rendering with no failing test.
//
// This test marshals the REAL arg structs and asserts the emitted JSON carries
// the exact keys the TUI keys off. If you rename a tag here, this fails loudly —
// update cmd/mecatui/ui/render.go's mirrored structs in the same change.
func TestEditWriteArgKeysAreStable(t *testing.T) {
	editJSON, err := json.Marshal(editArgs{
		Path:       "p",
		OldString:  "o",
		NewString:  "n",
		ReplaceAll: true,
	})
	if err != nil {
		t.Fatalf("marshal editArgs: %v", err)
	}
	for _, key := range []string{`"path"`, `"old_string"`, `"new_string"`, `"replace_all"`} {
		if !strings.Contains(string(editJSON), key) {
			t.Errorf("editArgs JSON %s is missing key %s — the mecatui diff renderer "+
				"(cmd/mecatui/ui/render.go editDiffArgs) depends on it; update both together", editJSON, key)
		}
	}

	writeJSON, err := json.Marshal(writeArgs{Path: "p", Content: "c"})
	if err != nil {
		t.Fatalf("marshal writeArgs: %v", err)
	}
	for _, key := range []string{`"path"`, `"content"`} {
		if !strings.Contains(string(writeJSON), key) {
			t.Errorf("writeArgs JSON %s is missing key %s — the mecatui diff renderer "+
				"(cmd/mecatui/ui/render.go writeDiffArgs) depends on it; update both together", writeJSON, key)
		}
	}
}
