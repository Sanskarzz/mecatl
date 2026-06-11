package tool

import (
	"context"
	"errors"
	"strings"
)

// SkillOrigin classifies the ADMISSION TIER a skill entered the catalog
// through. It is a tier label, NEVER a location: no implementation may put a
// path, directory, URL, or any locator in it (that is the adapter's private
// business). Enforcement of trust happens at SOURCE CONSTRUCTION time in the
// composition layer (an untrusted workspace's project tier is never
// constructed); Origin exists for observability and inspection only.
type SkillOrigin string

// The CLOSED admission-tier label set — implementations must never mint a new
// label (a consumer that does not recognise one normalizes to Driver).
const (
	SkillOriginExplicit SkillOrigin = "explicit" // operator-configured location/flag
	SkillOriginProject  SkillOrigin = "project"  // workspace-tier (trust-gated at construction)
	SkillOriginUser     SkillOrigin = "user"     // user-tier (never trust-gated)
	SkillOriginDriver   SkillOrigin = "driver"   // operator-configured remote driver
)

// SkillMeta is the always-in-context metadata layer of one skill: what the
// Skill tool's description enumerates. Pure value object — no body, no
// behaviour, NO file/path/root concept.
type SkillMeta struct {
	Name        string      // stable activation key; non-empty
	Description string      // one-line trigger metadata; non-empty, single-line, byte-capped by the source
	Origin      SkillOrigin // admission tier (observability only)
	HasAssets   bool        // whether ListSkillAssets will return at least one asset
}

// SkillAsset describes one auxiliary payload of a skill, addressed by LOGICAL
// name. A logical name is a slash-separated, RELATIVE identifier in the
// skill's own namespace (e.g. "references/api.md", "scripts/run.sh") — the
// exact namespace skill instruction bodies already reference. Not an OS path:
// no leading separator, no "."/".." segments, no backslashes, no NUL
// (ValidSkillAssetName is the single shared validator).
type SkillAsset struct {
	Name       string
	Size       int64 // payload size in bytes (advisory; readers re-enforce caps)
	Executable bool  // payload should carry the executable bit if materialized
}

// Sentinel errors every SkillSource implementation returns (wrapped, so
// errors.Is holds) for an unknown skill (SkillBody/ListSkillAssets) or an
// unknown skill/asset pair (ReadSkillAsset).
var (
	ErrSkillNotFound      = errors.New("tool: skill not found")
	ErrSkillAssetNotFound = errors.New("tool: skill asset not found")
)

// SkillSource is the read-only seam skills cross into the harness: a LOGICAL
// BUNDLE of identity + trigger metadata (ListSkills), an instruction body
// (SkillBody), and auxiliary payloads addressed by logical name
// (ListSkillAssets/ReadSkillAsset). Where a bundle comes from — directories,
// a database, a registry process — is entirely the implementation's private
// business; no path, directory, or root concept appears here, so the engine
// cannot tell a filesystem source from a remote one.
//
// Lifecycle: sources are SNAPSHOT-semantics — ListSkills is stable for the
// life of the source (the harness resolves once at build; there is no watch
// seam, deliberately matching the build-once trust-gate invariant).
type SkillSource interface {
	ListSkills(ctx context.Context) ([]SkillMeta, error)                     // sorted by Name, unique
	SkillBody(ctx context.Context, name string) (string, error)              // unknown → ErrSkillNotFound
	ListSkillAssets(ctx context.Context, name string) ([]SkillAsset, error)  // unknown → ErrSkillNotFound
	ReadSkillAsset(ctx context.Context, skill, asset string) ([]byte, error) // unknown → ErrSkillAssetNotFound; invalid name → error, never content
}

// ValidSkillAssetName reports whether name is a valid LOGICAL asset name:
// non-empty, slash-separated, relative, no empty/"."/".."/ segments, no
// backslash, no NUL — the ONE validator every implementation/consumer shares.
func ValidSkillAssetName(name string) bool {
	if name == "" {
		return false
	}
	if strings.ContainsAny(name, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
