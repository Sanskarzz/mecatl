// Package gitenv builds the git-neutralizing process environment shared by the
// forker (its own fork-time git invocations) and the sandboxed team-member command
// runner. It is a stdlib-only leaf: it imports nothing from the rest of the
// codebase so both the forker (which deliberately does NOT import osfs) and the
// composition root can depend on it without creating an adapter→adapter edge.
//
// The threat it addresses: a git worktree (the read-only-member isolation default)
// SHARES the base repo's `.git/config` and `.git/hooks`. An untrusted base repo can
// therefore drive code execution via several git config keys and via repo hooks
// (notably the forker's OWN `git worktree add` fires the base's post-checkout hook at
// fork time, BEFORE the sandboxed member runner ever exists). Both code paths must run
// git with the SAME neutralizing environment; keeping that single set here is what
// stops them from drifting.
//
// What this NEUTRALIZES (fixed-key env overrides + scrubbing): system+global config are
// ignored (GIT_CONFIG_NOSYSTEM, GIT_CONFIG_GLOBAL=/dev/null); every inherited GIT_*/PAGER
// variable is scrubbed (so GIT_EXTERNAL_DIFF, GIT_SSH_COMMAND, etc. cannot leak in); and
// precedence-winning env-injected config forces core.hooksPath=/dev/null (kills ALL repo
// hooks, including the fork-time post-checkout), core.pager=cat, core.fsmonitor=false, and
// an empty diff.external.
//
// RESIDUAL — what a fixed-key env override structurally CANNOT cover: git driver configs
// whose driver NAME is attacker-chosen in a tracked `.gitattributes`. Specifically
// filter.<drv>.smudge (fires at worktree checkout, i.e. fork time) and
// diff.<drv>.textconv (fires on `git show` / `git log -p`); also alias.<name>=!sh, but
// only if the member invokes that alias by name. Because the driver name is arbitrary,
// no fixed GIT_CONFIG_KEY_n can pin it to an inert value. These are reachable only when
// the shared `.git` belongs to an UNTRUSTED repo; for a TRUSTED repo (the operator's own)
// this execution is equivalent to the operator running git themselves, which is
// acceptable. The planned robust mitigation is to gate read-only-member shell on
// workspace trust (untrusted ⇒ no subagent shell) — a TRACKED FOLLOW-UP, not yet
// implemented.
package gitenv

import (
	"strconv"
	"strings"
)

// configPairs is the set of repo-local git config keys that can drive code
// execution, each force-overridden to an inert value. Env-injected config
// (GIT_CONFIG_COUNT + GIT_CONFIG_KEY_n/VALUE_n) takes PRECEDENCE over the shared
// `.git/config`, so these win over whatever an untrusted base repo set:
//
//   - core.hooksPath=/dev/null — no repo hook (post-checkout, etc.) ever fires.
//   - core.pager=cat            — no pager process is spawned.
//   - core.fsmonitor=false      — no fsmonitor hook/program is launched.
//   - diff.external=(empty)     — no external diff driver is invoked.
var configPairs = [][2]string{
	{"core.hooksPath", "/dev/null"},
	{"core.pager", "cat"},
	{"core.fsmonitor", "false"},
	{"diff.external", ""},
}

// NeutralizingVars returns the git-neutralizing environment entries ("KEY=VALUE")
// to APPEND to a scrubbed base environment. It combines the plain neutralizing
// variables (GIT_CONFIG_NOSYSTEM, GIT_CONFIG_GLOBAL=/dev/null, GIT_PAGER/PAGER=cat)
// with the env-injected git config (GIT_CONFIG_COUNT + the KEY/VALUE pairs above),
// keeping GIT_CONFIG_COUNT in lock-step with len(configPairs) so a future pair can
// never silently fall out of the injected config.
func NeutralizingVars() []string {
	out := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(configPairs)),
	}
	for i, kv := range configPairs {
		idx := strconv.Itoa(i)
		out = append(out,
			"GIT_CONFIG_KEY_"+idx+"="+kv[0],
			"GIT_CONFIG_VALUE_"+idx+"="+kv[1],
		)
	}
	return out
}

// Scrub returns a process environment derived from base (typically os.Environ())
// with every inherited git danger DROPPED, then the neutralizing set APPENDED. It
// removes:
//
//   - every inherited GIT_* variable — including the dangerous
//     GIT_EXTERNAL_DIFF, GIT_SSH_COMMAND, GIT_ALTERNATE_OBJECT_DIRECTORIES and
//     GIT_PROXY_COMMAND that a plain append could not unset — and any stale
//     GIT_CONFIG_* the parent already had (so our injected config is the only one);
//   - PAGER / LESS — pager programs git would otherwise launch.
//
// All other inherited variables (PATH, HOME, …) are kept so git still functions.
// Appending NeutralizingVars() last means our values win on a duplicate key.
func Scrub(base []string) []string {
	out := make([]string, 0, len(base)+len(NeutralizingVars()))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if strings.HasPrefix(key, "GIT_") || key == "PAGER" || key == "LESS" {
			continue // dropped; reinstated (where wanted) by NeutralizingVars
		}
		out = append(out, kv)
	}
	return append(out, NeutralizingVars()...)
}
