package app

import (
	"log/slog"

	"github.com/stacklok/mecatl/internal/adapter/workspacetrust"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// trust.go is the COMPOSITION-LAYER workspace-trust resolver (Workspace-Trust
// feature, Phase 1; MUST-FIX 2). It folds the per-invocation --trust-project flag
// and the declarative `trustedWorkspaces:` list (read from the user-global
// settings.yaml by the workspacetrust adapter) into ONE answer — a TrustDecision
// — produced once per process here in internal/app.
//
// The decision's .Trusted bool is fed to the SAME consumers --trust-project fed
// before: permconfig.Options.TrustProject (so a project's ALLOW rules are
// honoured only when trusted) and the soul provenance gate (so a project soul is
// admitted only when trusted). Adapters still take a plain bool — no signature
// churn. Trust is MONOTONIC-POSITIVE: it only ever GRANTS admission; it never
// suppresses a Deny or a configured Ask (those compose deny-dominantly in the
// evaluator regardless — see internal/adapter/permconfig).
//
// TrustDecision / TrustSource are COMPOSITION types, not domain or adapter types
// — governance/session/prompt/tool stay trust-unaware, exactly as they are for
// --trust-project today. Trust is a composition gate that FEEDS admission.

// TrustSource records WHY a workspace is (or is not) trusted, for the slog
// narration that mirrors the soul-selection narration (soulMeta).
type TrustSource int

const (
	// TrustNone means the workspace is not trusted this run.
	TrustNone TrustSource = iota
	// TrustFlag means trust came from the --trust-project one-shot flag.
	TrustFlag
	// TrustDeclared means the workspace's realpath is in the operator-authored
	// settings.yaml `trustedWorkspaces:` list.
	TrustDeclared
	// TrustRemembered is reserved for Phase 2 (the machine-written trust.yaml
	// registry). It is declared here so the value taxonomy is stable across
	// phases; Phase 1 never produces it.
	TrustRemembered
)

// String renders a TrustSource for the slog narration.
func (s TrustSource) String() string {
	switch s {
	case TrustFlag:
		return "flag"
	case TrustDeclared:
		return "declared"
	case TrustRemembered:
		return "remembered"
	default:
		return "none"
	}
}

// TrustDecision is the single composition-level answer to "is this workspace
// trusted, and why?" (MUST-FIX 2). Its Trusted field is the effective bool fed to
// the admission consumers; Source drives the log narration. Drifted is reserved
// for Phase 2 (a registry entry whose identity-anchor hash mismatched) and stays
// false/unused in Phase 1.
type TrustDecision struct {
	// Trusted is the effective admission bool: honour the project's ALLOW rules
	// and project soul when true.
	Trusted bool
	// Source records why (for the slog narration).
	Source TrustSource
	// Drifted is reserved for Phase 2 (registry anchor mismatch). Always false here.
	Drifted bool
}

// trustEnv is the PATH-RESOLUTION environment the declarative-trust reader uses to
// locate the user-global settings.yaml. It defaults to the real process
// environment; tests override it (with t.Cleanup-style restoration) so the
// declarative-trust checks run fully offline against a faked XDG/home — never the
// developer's real ~/.config.
var trustEnv = xdgconfig.OSEnv

// resolveTrust folds, highest first, the per-invocation flag and the declarative
// list into a TrustDecision for cfg.Workspace (MUST-FIX 2; R1.2):
//
//	--trust-project        ⇒ TrustFlag       (one-shot operator override)
//	trustedWorkspaces match ⇒ TrustDeclared   (realpath-keyed; MUST-FIX 5.1)
//	otherwise               ⇒ TrustNone
//
// The flag wins over the declared list only for the SOURCE label; both yield
// Trusted=true. Trust is monotonic-positive: neither input can revoke a Deny or
// downgrade an Ask — they gate admission only. resolveTrust performs NO writes
// and never errors: a missing/corrupt settings.yaml simply yields TrustNone (the
// workspacetrust reader fails safe).
func resolveTrust(cfg Config) TrustDecision {
	if cfg.TrustProject {
		return TrustDecision{Trusted: true, Source: TrustFlag}
	}
	if cfg.Workspace != "" {
		if workspacetrust.NewWithEnv(trustEnv).IsDeclared(cfg.Workspace) {
			return TrustDecision{Trusted: true, Source: TrustDeclared}
		}
	}
	return TrustDecision{Trusted: false, Source: TrustNone}
}

// narrateTrust logs the trust decision (mirroring the soulMeta narration): an
// Info line for the resolved state, so the why-trusted story is visible in the
// composition log exactly like the soul selection is.
func narrateTrust(d TrustDecision, workspace string) {
	slog.Info("workspace trust",
		"trusted", d.Trusted,
		"source", d.Source.String(),
		"drifted", d.Drifted,
		"workspace", workspace)
}
