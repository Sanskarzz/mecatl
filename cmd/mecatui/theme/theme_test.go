package theme

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestBuiltinsSlotCompleteness asserts every built-in theme populates every
// palette slot — a missing slot would render as terminal-default and break the
// visual language. Reflection over the Palette struct keeps this honest as new
// slots are added.
func TestBuiltinsSlotCompleteness(t *testing.T) {
	for name, th := range builtins {
		v := reflect.ValueOf(th.Palette)
		ty := v.Type()
		for i := 0; i < v.NumField(); i++ {
			got := v.Field(i).String()
			if strings.TrimSpace(got) == "" {
				t.Errorf("theme %q: slot %q is empty", name, ty.Field(i).Name)
			}
			if !strings.HasPrefix(got, "#") {
				t.Errorf("theme %q: slot %q = %q, want #rrggbb", name, ty.Field(i).Name, got)
			}
		}
	}
}

// TestStylesCompiled asserts compile() populates every named style slot the ui
// asks for. If a renderer references a slot the theme never compiles, Style()
// silently returns an empty style — this guards against that drift.
func TestStylesCompiled(t *testing.T) {
	want := []string{
		"header", "footer", "viewport", "userBlock", "userLabel",
		"assistantLabel", "toolCard", "toolName", "toolArgs", "toolOk",
		"toolErr", "askCard", "askTitle", "askButton", "askButtonActive",
		"spinner", "muted", "errorText",
	}
	th := New("aztec", aztecPalette)
	for _, slot := range want {
		if _, ok := th.styles[slot]; !ok {
			t.Errorf("style slot %q not compiled", slot)
		}
	}
}

// TestParseThemeMergeOverBase asserts a partial palette JSON merges over the
// Aztec base: overridden slots change, untouched slots inherit Aztec.
func TestParseThemeMergeOverBase(t *testing.T) {
	raw := []byte(`{"name":"Custom","palette":{"accent":"#FF00FF","error":"#000000"}}`)
	th, err := ParseTheme(raw)
	if err != nil {
		t.Fatalf("ParseTheme: %v", err)
	}
	if th.Name != "custom" {
		t.Errorf("name = %q, want lowercased %q", th.Name, "custom")
	}
	if th.Palette.Accent != "#FF00FF" {
		t.Errorf("accent override = %q, want #FF00FF", th.Palette.Accent)
	}
	if th.Palette.Error != "#000000" {
		t.Errorf("error override = %q, want #000000", th.Palette.Error)
	}
	// Untouched slot must inherit Aztec.
	if th.Palette.Primary != aztecPalette.Primary {
		t.Errorf("primary = %q, want inherited Aztec %q", th.Palette.Primary, aztecPalette.Primary)
	}
}

// TestThemeJSONRoundTrip asserts a theme marshals to the fileTheme shape and
// parses back to an equal palette (after the merge, which is a no-op for a full
// palette).
func TestThemeJSONRoundTrip(t *testing.T) {
	orig := New("aztec", aztecPalette)
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParseTheme(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got.Palette, orig.Palette) {
		t.Errorf("round-trip palette mismatch:\n got %+v\nwant %+v", got.Palette, orig.Palette)
	}
	if got.Name != orig.Name {
		t.Errorf("round-trip name = %q, want %q", got.Name, orig.Name)
	}
}

// TestRegistryResolve covers the resolution contract: empty → default, known →
// itself, unknown → default with ok=false.
func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	if th, ok := r.Resolve(""); !ok || th.Name != "aztec" {
		t.Errorf("Resolve(\"\") = %q,%v; want aztec,true", th.Name, ok)
	}
	if th, ok := r.Resolve("mono"); !ok || th.Name != "mono" {
		t.Errorf("Resolve(mono) = %q,%v; want mono,true", th.Name, ok)
	}
	if th, ok := r.Resolve("nope"); ok || th.Name != "aztec" {
		t.Errorf("Resolve(nope) = %q,%v; want aztec,false", th.Name, ok)
	}
}

// TestRegistryCaseInsensitive asserts Get/Resolve match regardless of the
// requested case (keys are stored lowercase), so "--theme Aztec" resolves rather
// than silently falling back to the default.
func TestRegistryCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	if th, ok := r.Resolve("Aztec"); !ok || th.Name != "aztec" {
		t.Errorf("Resolve(Aztec) = %q,%v; want aztec,true", th.Name, ok)
	}
	if th, ok := r.Get("MONO"); !ok || th.Name != "mono" {
		t.Errorf("Get(MONO) = %q,%v; want mono,true", th.Name, ok)
	}
}

// TestGlamourStyleColoured asserts the glamour config is driven by the palette:
// heading colour matches mdHeading and a code chroma colour matches a syntax
// slot. This locks the "markdown obeys the theme" contract.
func TestGlamourStyleColoured(t *testing.T) {
	th := New("aztec", aztecPalette)
	gs := th.GlamourStyle()
	if gs.Heading.Color == nil || *gs.Heading.Color != aztecPalette.MdHeading {
		t.Errorf("heading colour = %v, want %q", gs.Heading.Color, aztecPalette.MdHeading)
	}
	if gs.CodeBlock.Chroma == nil {
		t.Fatal("chroma not set")
	}
	if c := gs.CodeBlock.Chroma.Keyword.Color; c == nil || *c != aztecPalette.SynKeyword {
		t.Errorf("chroma keyword = %v, want %q", c, aztecPalette.SynKeyword)
	}
}

// TestColorUnknownSlot asserts an unknown slot yields nil (terminal default),
// not a panic.
func TestColorUnknownSlot(t *testing.T) {
	th := New("aztec", aztecPalette)
	if c := th.Color("does-not-exist"); c != nil {
		t.Errorf("unknown slot colour = %v, want nil", c)
	}
}
