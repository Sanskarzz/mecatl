package skills

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/osfs"
)

// staticSource is a path-free Source over a fixed slice, for tests that used
// to hand NewTool a []Skill directly.
type staticSource []Skill

func (s staticSource) Skills(context.Context) ([]Skill, []SkipError, error) { return s, nil, nil }

// newToolOver builds the Skill tool over the given skills through the REAL
// seam (NewFSSource snapshot + NewSnapshotActivator) — the mechanical
// adaptation of the pre-seam NewTool([]Skill) call sites to NewTool(metas,
// activator). Behavior is byte-identical (TestFSSkillActivationByteIdentical
// is the golden proof).
func newToolOver(t *testing.T, sk []Skill) Tool {
	t.Helper()
	src, skips, err := NewFSSource(context.Background(), staticSource(sk))
	if err != nil {
		t.Fatalf("NewFSSource: %v", err)
	}
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	metas, err := src.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	return NewTool(metas, NewSnapshotActivator(src))
}

// TestFSSkillActivationByteIdentical is the GOLDEN invariance proof for the
// Activator seam: for FS skills, NewTool(metas, NewSnapshotActivator(src))
// must render Execute output AND the Spec().Description byte-for-byte equal
// to the pre-seam NewTool([]Skill) rendering. The expected strings below are
// the pre-change format spelled out literally (NOT derived from the new code),
// so any drift in the preamble, the header lines, or the body framing fails
// here. Editing descriptionPreamble or the header strings is a defect against
// the phase-C1 plan.
func TestFSSkillActivationByteIdentical(t *testing.T) {
	t.Run("skill with a source path renders the base-directory header", func(t *testing.T) {
		dir := t.TempDir()
		writeSkill(t, dir, "with-files", "---\nname: with-files\ndescription: has bundled files\n---\nUse scripts/run.sh.\n")

		src, skips, err := NewFSSource(context.Background(), DirSource{Dir: dir})
		if err != nil || len(skips) != 0 {
			t.Fatalf("NewFSSource: %v skips=%v", err, skips)
		}
		metas, _ := src.ListSkills(context.Background())
		tl := NewTool(metas, NewSnapshotActivator(src))

		canon, err := osfs.ResolveRoot(filepath.Join(dir, "with-files"))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// The PRE-CHANGE rendering, byte for byte (tool.go's Execute before the
		// Activator seam): header, canonical base dir, bundled-files guidance,
		// blank line, body.
		want := "Skill: with-files\n" +
			"Base directory: " + canon + "\n" +
			"Bundled files (references/, scripts/, assets/) live under the base directory; read them with the Read tool by absolute path, and run bundled scripts via Bash with their absolute path.\n" +
			"\n" +
			"Use scripts/run.sh."
		res := exec(t, tl, call(t, map[string]any{"name": "with-files"}))
		if res.IsError {
			t.Fatalf("Execute errored: %s", res.Content)
		}
		if res.Content != want {
			t.Errorf("activation output drifted from the pre-seam rendering:\n got %q\nwant %q", res.Content, want)
		}

		// The Spec().Description likewise: the (unchanged) preamble plus the
		// pre-change "\n- <name>: <description>" enumeration.
		wantDesc := descriptionPreamble + "\n- with-files: has bundled files"
		if got := tl.Spec().Description; got != wantDesc {
			t.Errorf("Spec().Description drifted from the pre-seam rendering:\n got %q\nwant %q", got, wantDesc)
		}
	})

	t.Run("skill without a source path omits the base-directory block", func(t *testing.T) {
		tl := newToolOver(t, []Skill{{Name: "review", Description: "Run a structured code review.", Body: "Look for correctness, then style."}})
		want := "Skill: review\n" +
			"\n" +
			"Look for correctness, then style."
		res := exec(t, tl, call(t, map[string]any{"name": "review"}))
		if res.IsError {
			t.Fatalf("Execute errored: %s", res.Content)
		}
		if res.Content != want {
			t.Errorf("activation output drifted from the pre-seam rendering:\n got %q\nwant %q", res.Content, want)
		}
	})
}

// TestSourceActivatorFailureIsModelAddressable pins the driver-path error
// surface: a failed activation (the materializer rejecting a bundle) is a
// MODEL-addressable tool error naming the skill and the available
// alternatives — never a harness-level error.
func TestSourceActivatorFailureIsModelAddressable(t *testing.T) {
	src := failingAssetSource{}
	metas, _ := src.ListSkills(context.Background())
	mat := NewAssetMaterializer(src, t.TempDir())
	tl := NewTool(metas, NewSourceActivator(src, mat))

	res, err := tl.Execute(context.Background(), call(t, map[string]any{"name": "evil"}), nil)
	if err != nil {
		t.Fatalf("Execute must not surface a harness error, got %v", err)
	}
	if !res.IsError {
		t.Fatal("a failed activation must be an error RESULT")
	}
	if !strings.Contains(res.Content, `"evil"`) {
		t.Errorf("error must name the skill, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "available skills are: evil, good") {
		t.Errorf("error must carry the available-skills hint, got %q", res.Content)
	}

	// The well-behaved sibling still activates (per-skill isolation).
	res = exec(t, tl, call(t, map[string]any{"name": "good"}))
	if res.IsError {
		t.Fatalf("the asset-less sibling must activate, got error %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, "Skill: good\n\n") {
		t.Errorf("asset-less driver skill must omit the Base-directory block, got %q", res.Content)
	}
}
