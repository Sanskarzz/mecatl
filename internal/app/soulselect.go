package app

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/stacklok/mecatl/internal/adapter/soul"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
	"github.com/stacklok/mecatl/internal/prompt"
)

// soulEnv is the PATH-RESOLUTION environment used to resolve the conventional
// user-scoped soul (<xdg>/mecatl/soul.md). It defaults to the real process
// environment; tests override it (with t.Cleanup-style restoration) so the
// user-soul precedence checks run fully offline against a faked XDG/home — never the
// developer's real ~/.config. It is used only to compute a path string (no write).
var soulEnv = xdgconfig.OSEnv

// soulselect.go is the composition-layer SOUL PROVENANCE + TRUST GATE (issue #14,
// Phase 3, Item 2). The soul is fenced DATA, never a permission scope, so this is
// NOT routed through internal/governance — but it REUSES the issue-#13 trust gate
// (Config.TrustProject, the --trust-project flag) so an imported/project-sourced
// soul is governed by the EXACT same operator gesture that gates a project's ALLOW
// permission rules. No new trust concept, no new flag.
//
// Two soul PROVENANCES:
//   - USER: the conventional user-scoped <xdg>/mecatl/soul.md (fallback
//     ~/.config/mecatl/soul.md), or an explicit --soul-file PATH. ALWAYS trusted —
//     it is the operator's own file, NEVER trust-gated (correction (C)).
//   - PROJECT: a discovered <workspace>/.mecatl/soul.md (parallel to
//     .mecatl/settings.yaml and to how RootAssembler discovers AGENTS.md/CLAUDE.md).
//     This is the imported/project-provenance soul. UNTRUSTED by default: it
//     contributes NOTHING unless --trust-project is set.
//
// PRECEDENCE = USER-WINS (single identity anchor; MVP, NOT a merge): when a
// user-scoped soul is present it is used and the project soul is IGNORED. The
// project soul is used ONLY when (a) --trust-project is set AND (b) no user-scoped
// soul is present. Two simultaneous soul blocks are explicitly avoided (a
// contradiction + a double injection surface).
//
// An untrusted project soul is dropped SILENTLY-BUT-LOGGED (slog.Warn, mirroring
// applyTrustGate's report posture), never an error.

// projectSoulSubpath is the conventional project-scoped soul file relative to the
// workspace root, i.e. <workspace>/.mecatl/soul.md. It mirrors permconfig's
// projectFileMecatl (".mecatl/settings.yaml") path convention — the soul lives in
// the SAME per-project .mecatl/ dir as the project permission config.
const projectSoulSubpath = ".mecatl/soul.md"

// soulProvenance records WHERE a selected soul originated, for Item 3's read-only
// TUI inspector. It is a composition-level concern: the soul adapter is a pure
// loader (it neither knows nor cares about provenance), and internal/prompt stays
// trust-unaware (the SoulSource interface is unchanged).
type soulProvenance int

const (
	// soulNone means no soul was selected (none present, or an untrusted project
	// soul was dropped, or --no-soul).
	soulNone soulProvenance = iota
	// soulUser means the selected soul is the user-scoped one (always trusted).
	soulUser
	// soulProject means the selected soul is the project-scoped
	// <workspace>/.mecatl/soul.md (only selected when --trust-project is set).
	soulProject
)

func (p soulProvenance) String() string {
	switch p {
	case soulUser:
		return "user"
	case soulProject:
		return "project"
	default:
		return "none"
	}
}

// soulMeta is the composition-level metadata ABOUT the selected soul, produced HERE
// (internal/app) and intended for consumption by Item 3's read-only TUI inspector
// (which is OUT of scope for Item 2 — no proto, no RPC, no TUI yet). It carries no
// behaviour; it is a value snapshot of the selection outcome. The zero value
// (Present:false, Provenance:soulNone) means "no soul this run".
type soulMeta struct {
	// Present is true when a soul fragment WILL be contributed this run.
	Present bool
	// Provenance is where the selected soul came from (User|Project|None).
	Provenance soulProvenance
	// Trusted reflects whether the selected soul's provenance is trusted. A user
	// soul is always trusted; a project soul is trusted only with --trust-project.
	Trusted bool
	// Drifted is true when the selected soul's content hash differs from its
	// recorded baseline (Item 1). It is informational: by default a drifted soul
	// still loads (Present stays true) unless --soul-strict dropped it.
	Drifted bool
	// SHA256 is the content hash of the selected soul's clean body, or "" for none.
	SHA256 string
	// Size is the byte length of the selected soul's clean body, or 0 for none.
	Size int
}

// selectSoulSource owns the full Item-2 selection policy: USER-wins precedence, the
// project-soul trust gate, and the Item-1 drift check applied to WHICHEVER soul
// wins. It returns the chosen prompt.SoulSource (nil when nothing is contributed)
// plus the soulMeta snapshot for Item 3.
//
// It is fail-soft end to end: every candidate is loaded through the SAME soul.Store
// discipline (byte cap, injection scan, fence reject), and an absent/rejected soul
// simply yields no fragment. The drift baseline (checkSoulDrift) runs against the
// SELECTED soul's path only — never both — so there is no double-baseline.
func selectSoulSource(cfg Config, io baselineIO) (prompt.SoulSource, soulMeta) {
	if cfg.NoSoul {
		slog.Info("soul DISABLED (--no-soul)")
		return nil, soulMeta{}
	}

	// USER candidate first (always trusted). An explicit --soul-file is a user-scoped
	// override of the conventional path; either way it is the operator's own file.
	userStore := soul.NewWithEnv(soul.Options{Path: cfg.SoulPath}, soulEnv)
	userRes, _ := userStore.LoadWithMeta(context.Background())
	if userRes.Body != "" {
		// USER-WINS: a present user soul is selected; the project soul is ignored.
		if cfg.SoulPath != "" {
			slog.Info("soul ENABLED (user provenance, read-only persona)", "path", cfg.SoulPath)
		} else {
			slog.Info("soul ENABLED (user provenance, read-only persona)",
				"path", "conventional <xdg>/mecatl/soul.md")
		}
		drifted := checkSoulDrift(io, userStore.ResolvedPath(), userRes.SHA256, cfg.ApproveSoul)
		if drifted && cfg.SoulStrict {
			slog.Warn("soul: drifted persona refused (--soul-strict); no soul fragment this run",
				"path", userStore.ResolvedPath(), "provenance", soulUser.String())
			return nil, soulMeta{}
		}
		return userStore, soulMeta{
			Present:    true,
			Provenance: soulUser,
			Trusted:    true,
			Drifted:    drifted,
			SHA256:     userRes.SHA256,
			Size:       userRes.Size,
		}
	}

	// No user soul. Consider the PROJECT soul: <workspace>/.mecatl/soul.md.
	// Resolution is per the build-time cfg.Workspace (the single-workspace embedded
	// server). An empty workspace means there is no project soul to discover.
	if cfg.Workspace == "" {
		slog.Info("soul: no fragment (no user soul present; no workspace to discover a project soul)")
		return nil, soulMeta{}
	}
	projectPath := filepath.Join(cfg.Workspace, projectSoulSubpath)
	projectStore := soul.NewWithEnv(soul.Options{Path: projectPath}, soulEnv)
	projRes, _ := projectStore.LoadWithMeta(context.Background())
	if projRes.Body == "" {
		// No project soul on disk (or it was rejected by the loader's discipline).
		slog.Info("soul: no fragment (no user or project soul present; fail-soft)")
		return nil, soulMeta{}
	}

	// A project soul EXISTS. It is UNTRUSTED unless --trust-project is set — gated by
	// the EXACT issue-#13 mechanism. An untrusted project soul is dropped
	// silently-but-LOGGED (Warn), never an error.
	if !cfg.TrustProject {
		slog.Warn("soul: a project-sourced soul was discovered but is UNTRUSTED; dropping it (no fragment). Pass --trust-project to honour a soul discovered in this repo (only for a repo you trust)",
			"path", projectPath)
		return nil, soulMeta{Provenance: soulProject, Trusted: false, SHA256: projRes.SHA256, Size: projRes.Size}
	}

	slog.Info("soul ENABLED (PROJECT provenance, read-only persona; trusted via --trust-project)", "path", projectPath)
	drifted := checkSoulDrift(io, projectStore.ResolvedPath(), projRes.SHA256, cfg.ApproveSoul)
	if drifted && cfg.SoulStrict {
		slog.Warn("soul: drifted persona refused (--soul-strict); no soul fragment this run",
			"path", projectPath, "provenance", soulProject.String())
		return nil, soulMeta{Provenance: soulProject, Trusted: true}
	}
	return projectStore, soulMeta{
		Present:    true,
		Provenance: soulProject,
		Trusted:    true,
		Drifted:    drifted,
		SHA256:     projRes.SHA256,
		Size:       projRes.Size,
	}
}
