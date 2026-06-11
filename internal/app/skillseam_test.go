package app

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/skills"
)

// These helpers are the MECHANICAL retargets for the pre-seam
// resolveSkills/resolveSkillIndex/skillReadRoots test call sites (Phase C1
// deleted those composition helpers): each drives the REAL production seam
// (resolveSkillSeam — the same function buildCatalog calls) and projects its
// outputs back onto the legacy shapes the assertions read, so the trust-gate /
// preload / read-root assertions keep pinning the live path rather than a
// test-only reimplementation.

func seamForTest(t *testing.T, cfg Config) skillSeam {
	t.Helper()
	seam, err := resolveSkillSeam(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("resolveSkillSeam: %v", err)
	}
	if seam.close != nil {
		t.Cleanup(seam.close)
	}
	return seam
}

// resolveSkillsForTest returns the seam's discovered skills in the legacy
// []skills.Skill shape (name/description/body — assertions read names).
func resolveSkillsForTest(t *testing.T, cfg Config) []skills.Skill {
	t.Helper()
	seam := seamForTest(t, cfg)
	return skillValues(seam.metas, seam.index)
}

// resolveSkillIndexForTest returns the seam's name→body preload index.
func resolveSkillIndexForTest(t *testing.T, cfg Config) skillIndex {
	t.Helper()
	return seamForTest(t, cfg).index
}

// assetDirsForTest returns the seam's read-only allowed roots (the old
// skillReadRoots computation, now FSSource.AssetDirs inside the seam).
func assetDirsForTest(t *testing.T, cfg Config) []string {
	t.Helper()
	return seamForTest(t, cfg).readRoots
}
