package app

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/stacklok/mecatl/internal/adapter/osfs"
	"github.com/stacklok/mecatl/internal/adapter/permconfig"
	"github.com/stacklok/mecatl/internal/adapter/soul"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/tool"
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
// (Config.TrustProject) so an imported/project-sourced soul is governed by the
// EXACT same operator gesture that gates a project's ALLOW permission rules. As of
// the Workspace-Trust feature (Phase 1), Config.TrustProject carries the FOLDED
// TrustDecision (trust.go): the --trust-project flag OR a settings.yaml
// `trustedWorkspaces:` declaration. A declared-trusted workspace therefore honours
// a project soul exactly as --trust-project does — through this same gate, never a
// bypass.
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
	// PermEffect is the resolved effect of the synthetic "soul:apply" action for this
	// run (issue #14). It is informational (for the slog narration + future use): on a
	// Deny or Ask the soul is WITHHELD (Present stays false) and this records WHY. The
	// zero value ("") means the gate was not consulted (legacy/nil-gate path) or
	// resolved to Allow. There is no proto/RPC/TUI surface for it this pass.
	PermEffect governance.Effect
}

// soulGate resolves the permission effect of the synthetic "soul:apply" action
// (issue #14). It is the composition-layer seam the soul load-gate consults BEFORE
// running the USER/project selection: it shares the SAME governance evaluator +
// file-config resolver the real tool policy uses, so a `soul:apply` rule in
// .mecatl/settings.yaml or the user settings is honoured exactly like a tool rule
// (project rules gated by --trust-project, resolved against cfg.Workspace). It is an
// injectable seam so tests can drive each effect with a fake; the real binding is
// buildSoulGate.
type soulGate interface {
	// Effect returns the resolved governance.Effect for the "soul:apply" action.
	Effect() governance.Effect
}

// soulGateFunc adapts a function to soulGate (the lightweight-closure idiom used
// elsewhere in this package, e.g. commandListerFunc).
type soulGateFunc func() governance.Effect

// Effect implements soulGate.
func (f soulGateFunc) Effect() governance.Effect { return f() }

// buildSoulGate constructs the real soul load-gate: it evaluates the synthetic
// "soul:apply" action against the SAME ruleset (mainRules) + file-config resolver
// (permconfig) the main tool policy uses, so the soul is governed identically. It
// REUSES the existing permission stack rather than building a parallel one: the
// resolver is constructed from the SAME Config fields buildEngine passes to
// permconfig.New, and project ALLOW rules stay gated behind --trust-project and
// resolved against cfg.Workspace via a READ-ONLY osfs workspace (exactly like the
// real per-session policy, which resolves against each session's root).
//
// A nil/unopenable workspace (or a resolver that wants no project sources) simply
// means no project-scoped soul:apply rule applies — selection then falls back to the
// built-in floor Allow (the default-on posture). The gate never fails: it is a pure
// read of config the process already trusts.
func buildSoulGate(cfg Config) soulGate {
	rules := mainRules(cfg)
	eval := governance.NewEvaluator(rules)
	resolver := permconfig.New(permconfig.Options{
		Conventional:  cfg.PermissionsConventional,
		ImportClaude:  cfg.ImportClaudePermissions,
		TrustProject:  cfg.TrustProject,
		ExplicitFiles: cfg.PermissionConfigs,
	})
	return soulGateFunc(func() governance.Effect {
		var ws tool.WorkspaceReader
		if cfg.Workspace != "" {
			if w, err := osfs.NewWorkspace(cfg.Workspace); err == nil {
				ws = w
			}
			// FAIL-OPEN-TO-FLOOR BY DESIGN: an unopenable workspace leaves ws nil, so the
			// resolver yields only user/CLI rules (no project soul:apply rule) and the gate
			// falls back to the built-in floor Allow. This is deliberate: a user-scoped soul
			// should still load when the PROJECT workspace can't be read. It is distinct
			// from a configured Deny/Ask, which DOES withhold (those are real rules, honoured).
		}
		var extra []governance.Rule
		if resolver != nil {
			extra = resolver.Resolve(context.Background(), ws)
		}
		// planMode=false: the soul is build-time data, not a mutating tool call; plan
		// mode does not apply. EvaluateWith folds the floor Allow with any config
		// rule deny-dominantly, so a config Ask/Deny on soul:apply still wins.
		return eval.EvaluateWith(SoulApplyAction, nil, false, extra).Effect
	})
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
//
// BEFORE any selection it consults the soul load-gate (issue #14): the synthetic
// "soul:apply" governance action, resolved through the SAME evaluator/resolver the
// real tool policy uses. Allow ⇒ proceed; Deny ⇒ withhold (no fragment); Ask ⇒
// withhold-with-warn (the soul is applied at build time with no interactive gate, so
// Ask cannot be satisfied — set soul:apply→allow to apply it). gate may be nil in
// tests/legacy callers, which is treated as Allow (the default-on posture).
func selectSoulSource(cfg Config, io baselineIO, gate soulGate) (prompt.SoulSource, soulMeta) {
	if cfg.NoSoul {
		slog.Info("soul DISABLED (--no-soul)")
		return nil, soulMeta{}
	}

	// Soul load-gate: consult the "soul:apply" permission BEFORE selecting a source.
	// A nil gate (tests/legacy) defaults to Allow — the built-in floor posture.
	if gate != nil {
		switch eff := gate.Effect(); eff {
		case governance.Deny:
			slog.Warn("soul: withheld by permission policy (soul:apply → deny)")
			return nil, soulMeta{PermEffect: governance.Deny}
		case governance.Ask:
			slog.Warn("soul: soul:apply resolved to Ask, but the soul is applied at build time with no interactive gate; withholding this run — set soul:apply→allow to apply it")
			return nil, soulMeta{PermEffect: governance.Ask}
		default:
			// Allow (or unrecognised, fail-safe to the default-on posture): proceed.
			_ = eff
		}
	}

	// USER candidate first (always trusted). An explicit --soul-file is a user-scoped
	// override of the conventional path; either way it is the operator's own file.
	userStore := soul.NewWithEnv(soul.Options{Path: cfg.SoulPath}, soulEnv)
	userRes, _ := userStore.LoadWithMeta(context.Background())
	if userRes.Body != "" {
		// USER-WINS: a present user soul is selected; the project soul is ignored.
		if cfg.SoulPath != "" {
			slog.Info("soul ENABLED (user provenance, read-only persona); permission: soul:apply allow (built-in default, overridable to ask/deny via settings)", "path", cfg.SoulPath)
		} else {
			slog.Info("soul ENABLED (user provenance, read-only persona); permission: soul:apply allow (built-in default, overridable to ask/deny via settings)",
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

	slog.Info("soul ENABLED (PROJECT provenance, read-only persona; trusted via --trust-project); permission: soul:apply allow (built-in default, overridable to ask/deny via settings)", "path", projectPath)
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
