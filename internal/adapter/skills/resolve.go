package skills

import (
	"path/filepath"

	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// Conventional skills sub-paths (Claude-Code-style "extra paths"). A skills
// directory is laid out as <conventional-dir>/<name>/SKILL.md.
const (
	// ProjectDirMecatl is the project-level skills dir under the workspace.
	ProjectDirMecatl = ".mecatl/skills"
	// ProjectDirClaude is the Claude-Code-compatible project-level skills dir.
	ProjectDirClaude = ".claude/skills"
	// userSubdirMecatl is the user-level skills sub-path under the XDG config dir.
	userSubdirMecatl = "mecatl/skills" // joined under <config>/...
	// userSubdirClaude is the Claude-Code-compatible user-level skills sub-path,
	// rooted at the home directory (~/.claude/skills).
	userSubdirClaude = ".claude/skills"
)

// ResolveOptions configures the known-path resolver. The zero value resolves
// NOTHING (no explicit paths, conventional set OFF) — discovery stays strictly
// opt-in unless the operator asks for it, preserving the project's stance.
type ResolveOptions struct {
	// Explicit are operator-configured directories (e.g. from a repeatable
	// --skills-dir / --skills-path flag), highest precedence, in the order given.
	// They are always honoured regardless of Conventional.
	Explicit []string
	// Conventional, when true, adds the built-in conventional project- and
	// user-level locations (lower precedence than Explicit). Default false: a
	// strict opt-in, mirroring the project's stance (an untrusted SKILL.md under a
	// conventional path must never enter context unless the operator opted in).
	Conventional bool
	// Workspace is the session workspace root used to resolve the project-level
	// conventional paths (<workspace>/.mecatl/skills, <workspace>/.claude/skills).
	// Only consulted when Conventional is true and non-empty.
	Workspace string
}

// ResolveSources builds the ORDERED, highest-precedence-first Source list from the
// conventional locations plus any explicit paths, ready to hand to NewMultiSource.
// The precedence is:
//
//	explicit (--skills-dir / --skills-path, in flag order)   [highest]
//	  > project: <workspace>/.mecatl/skills, <workspace>/.claude/skills
//	    > user: $XDG_CONFIG_HOME/mecatl/skills (or ~/.config/mecatl/skills),
//	            ~/.claude/skills                                   [lowest]
//
// so a project skill overrides a personal one of the same name, and an explicit
// skill overrides both. Each location becomes a labelled DirSource; missing
// directories are harmless (DirSource treats an absent dir as "no skills"). When
// Conventional is false only the Explicit paths are included — preserving the
// strict opt-in default.
func ResolveSources(opts ResolveOptions) []Source {
	return resolveSourcesEnv(opts, xdgconfig.OSEnv)
}

// resolveSourcesEnv is ResolveSources with an injectable environment, for tests.
func resolveSourcesEnv(opts ResolveOptions, env xdgconfig.ResolveEnv) []Source {
	var sources []Source

	// Highest precedence: explicit operator-configured paths, in order.
	for _, dir := range opts.Explicit {
		if dir == "" {
			continue
		}
		sources = append(sources, DirSource{Dir: dir, Label: "explicit"})
	}

	if !opts.Conventional {
		return sources
	}

	// Project-level (under the workspace), beats user-level.
	if opts.Workspace != "" {
		sources = append(sources,
			DirSource{Dir: filepath.Join(opts.Workspace, ProjectDirMecatl), Label: "project(.mecatl)"},
			DirSource{Dir: filepath.Join(opts.Workspace, ProjectDirClaude), Label: "project(.claude)"},
		)
	}

	// User-level (lowest precedence). XDG-respecting for the mecatl path.
	if cfg := xdgconfig.UserConfigDir(env); cfg != "" {
		sources = append(sources, DirSource{Dir: filepath.Join(cfg, userSubdirMecatl), Label: "user(xdg)"})
	}
	if home, err := env.UserHomeDir(); err == nil && home != "" {
		sources = append(sources, DirSource{Dir: filepath.Join(home, userSubdirClaude), Label: "user(.claude)"})
	}

	return sources
}
