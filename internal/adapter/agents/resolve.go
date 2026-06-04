package agents

import (
	"path/filepath"

	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// Conventional agent-def sub-paths (Claude-Code-style "extra paths"). A def is
// laid out as <conventional-dir>/<name>.md.
const (
	// ProjectDirMecatl is the project-level agents dir under the workspace.
	ProjectDirMecatl = ".mecatl/agents"
	// ProjectDirClaude is the Claude-Code-compatible project-level agents dir.
	ProjectDirClaude = ".claude/agents"
	// userSubdirMecatl is the user-level agents sub-path under the XDG config dir.
	userSubdirMecatl = "mecatl/agents"
	// userSubdirClaude is the Claude-Code-compatible user-level agents sub-path,
	// rooted at the home directory (~/.claude/agents).
	userSubdirClaude = ".claude/agents"
)

// ResolveOptions configures the known-path resolver. The zero value resolves
// NOTHING (no explicit paths, conventional set OFF) — discovery stays strictly
// opt-in unless the operator asks for it, since a def body steers the model.
type ResolveOptions struct {
	// Explicit are operator-configured directories (e.g. from a repeatable
	// --agents-dir flag), highest precedence, in the order given. Always honoured
	// regardless of Conventional.
	Explicit []string
	// Conventional, when true, adds the built-in conventional project- and
	// user-level locations (lower precedence than Explicit). Default false: a
	// strict opt-in.
	Conventional bool
	// Workspace is the session workspace root used to resolve the project-level
	// conventional paths. Only consulted when Conventional is true and non-empty.
	Workspace string
}

// ResolveSources builds the ORDERED, highest-precedence-first Source list from the
// conventional locations plus any explicit paths, ready to hand to NewMultiSource.
// The precedence is:
//
//	explicit (--agents-dir, in flag order)                   [highest]
//	  > project: <workspace>/.mecatl/agents, <workspace>/.claude/agents
//	    > user: $XDG_CONFIG_HOME/mecatl/agents (or ~/.config/mecatl/agents),
//	            ~/.claude/agents                                  [lowest]
//
// so a project def overrides a personal one of the same name, and an explicit def
// overrides both. Each location becomes a labelled DirSource; missing directories
// are harmless. When Conventional is false only the Explicit paths are included.
func ResolveSources(opts ResolveOptions) []AgentSource {
	return resolveSourcesEnv(opts, xdgconfig.OSEnv)
}

// resolveSourcesEnv is ResolveSources with an injectable environment, for tests.
func resolveSourcesEnv(opts ResolveOptions, env xdgconfig.ResolveEnv) []AgentSource {
	var sources []AgentSource

	for _, dir := range opts.Explicit {
		if dir == "" {
			continue
		}
		sources = append(sources, DirSource{Dir: dir, Label: "explicit"})
	}

	if !opts.Conventional {
		return sources
	}

	if opts.Workspace != "" {
		sources = append(sources,
			DirSource{Dir: filepath.Join(opts.Workspace, ProjectDirMecatl), Label: "project(.mecatl)"},
			DirSource{Dir: filepath.Join(opts.Workspace, ProjectDirClaude), Label: "project(.claude)"},
		)
	}

	if cfg := xdgconfig.UserConfigDir(env); cfg != "" {
		sources = append(sources, DirSource{Dir: filepath.Join(cfg, userSubdirMecatl), Label: "user(xdg)"})
	}
	if home, err := env.UserHomeDir(); err == nil && home != "" {
		sources = append(sources, DirSource{Dir: filepath.Join(home, userSubdirClaude), Label: "user(.claude)"})
	}

	return sources
}
