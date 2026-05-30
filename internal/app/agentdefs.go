package app

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/hookexec"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/tools"
	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/governance"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/prompt"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// callSiteExcluded is the set of tool names a scoped agent-def catalog NEVER
// contains, regardless of the def's allowlist. Task/Fork enforce the no-nesting
// guard (a child must not recurse or fan out further); ToolSearch is excluded
// because child engines run with progressive disclosure OFF and a def must not
// silently gain a hydration tool it never listed (critique M1). The exclusion is
// applied AFTER the allowlist so a def cannot re-add any of these.
var callSiteExcluded = map[string]struct{}{
	"Task":              {},
	"Fork":              {},
	tool.ToolSearchName: {}, // "ToolSearch"
}

// builtinModelAliases are the Claude-Code-style model aliases recognised even
// when the operator configures no ModelAliases map. They map to the SENTINEL
// "inherit" semantics by default (so a real `.claude/agents` file saying
// `model: sonnet` resolves to a known, non-failing outcome) UNLESS the operator
// overrides the alias in Config.ModelAliases with a concrete id. Keeping them as
// known aliases (rather than unknown) is what turns "model: sonnet" into a clean
// inherit instead of a warning. resolveModel treats a known alias whose target is
// empty/"inherit" as inherit.
var builtinModelAliases = map[string]string{
	"inherit": "",
	"sonnet":  "",
	"opus":    "",
	"haiku":   "",
}

// resolveModel resolves a def's model selector to a concrete provider model id,
// in the composition layer only (the domain/agent never sees an alias). The
// precedence is: def.Model (if set and not "inherit") > global SubagentModel >
// parent cfg.Model. Aliases are resolved against the operator's ModelAliases map
// first, then the built-in CC aliases. An unknown alias is non-fatal: it WARNS
// and falls back to inherit (the parent model), so a shared .claude/agents file
// naming a model mecatl doesn't know never breaks startup (critique M4).
func resolveModel(cfg Config, def agents.AgentDef) string {
	parent := cfg.Model
	pick := func(model string) string {
		if model == "" {
			return parent
		}
		return model
	}

	sel := strings.TrimSpace(def.Model)
	if sel == "" || sel == "inherit" {
		// def doesn't pin a model: apply the global override, else inherit.
		return pick(resolveAlias(cfg, def, strings.TrimSpace(cfg.SubagentModel)))
	}
	return pick(resolveAlias(cfg, def, sel))
}

// resolveAlias maps sel through the operator aliases then the built-in aliases,
// returning a concrete id (or "" meaning inherit). A non-alias, non-empty sel is
// treated as a literal model id. An unrecognised alias-looking value warns and
// returns "" (inherit).
func resolveAlias(cfg Config, def agents.AgentDef, sel string) string {
	if sel == "" {
		return ""
	}
	if id, ok := cfg.ModelAliases[sel]; ok {
		return strings.TrimSpace(id)
	}
	if id, ok := builtinModelAliases[sel]; ok {
		return id // may be "" => inherit
	}
	// Not a known alias. Heuristic: a value containing a separator looks like a
	// concrete model id (e.g. "gpt-4o", "claude-sonnet-4.5"); treat it literally.
	// A bare unknown token is most likely a mistyped alias — warn and inherit.
	if strings.ContainsAny(sel, "-./:") || strings.Contains(sel, " ") {
		return sel
	}
	slog.Warn("agent def references an unknown model alias; inheriting parent model",
		"agent", def.Name, "model", sel, "path", def.Path)
	return ""
}

// resolvePermissionMode maps a def's frontmatter permissionMode string to a
// domain session.PermissionMode, in the composition layer (the domain never sees
// the raw string). "default"/empty/unknown => "" (the caller's default — for a team
// member that means the team-wide WithTeamMode). "plan" and "acceptEdits" map to
// their domain constants. An unrecognised value warns and falls back to the default.
//
// Note (critique B2): plan mode hard-denies mutations (existing invariant), so a
// plan member is effectively read-only even if Mutating. acceptEdits is meaningful
// only for a Mutating member (a read-only member has no mutating tools to accept).
func resolvePermissionMode(def agents.AgentDef) session.PermissionMode {
	switch strings.TrimSpace(def.PermissionMode) {
	case "", "default":
		return "" // caller's default
	case string(session.ModePlan):
		return session.ModePlan
	case string(session.ModeAccept):
		return session.ModeAccept
	default:
		slog.Warn("agent def references an unknown permissionMode; using the default",
			"agent", def.Name, "mode", def.PermissionMode, "path", def.Path)
		return ""
	}
}

// scopeDiag is one resolution-time diagnostic about a def's catalog scoping. Kind
// distinguishes the cases the critique (M5) requires be DISTINCT so an operator
// can tell a typo from a forbidden tool.
type scopeDiag struct {
	tool   string
	reason string
}

// scopedToolNames computes a def's effective, read-only tool NAME set for a
// Task-routed child (allowMutating == false), as a pure set operation over the
// AVAILABLE base tools. It is the read-only shim over scopedToolNamesMode; see that
// for the full algorithm. Task children are unconditionally read-only so
// Task.ReadOnly() stays honestly true.
func scopedToolNames(def agents.AgentDef, available map[string]tool.Tool) ([]string, []scopeDiag) {
	return scopedToolNamesMode(def, available, false)
}

// scopedToolNamesMode computes a def's effective tool NAME set as a pure set
// operation over the AVAILABLE base tools:
//
//  1. start from def.Tools if non-empty, else every available base tool name;
//  2. subtract def.DisallowedTools;
//  3. drop the always-excluded set (Task/Fork/ToolSearch) — the no-nesting /
//     no-disclosure guard, applied AFTER the allowlist so a def cannot re-add them;
//  4. drop any name not in the available base set (DISTINCT "unknown tool"
//     diagnostic — a typo or an MCP/skills tool this Tier-1 call site can't see);
//  5. when allowMutating is false, drop any mutating (non-read-only) tool with a
//     DISTINCT "not permitted (read-only)" diagnostic. When allowMutating is true
//     (a Mutating team member, which runs in an isolated fork) a def MAY keep
//     Edit/Write/Bash, so mutating tools survive.
//
// available maps an available base tool name to its tool.Tool (used to read
// ReadOnly()). It returns the kept names (sorted) and the diagnostics. The caller
// (teams) still appends MemberTools AFTER this — coordination tools bypass the
// allowlist and this filter entirely.
func scopedToolNamesMode(def agents.AgentDef, available map[string]tool.Tool, allowMutating bool) ([]string, []scopeDiag) {
	disallowed := make(map[string]struct{}, len(def.DisallowedTools))
	for _, d := range def.DisallowedTools {
		disallowed[d] = struct{}{}
	}

	// Step 1: the requested set.
	var requested []string
	if len(def.Tools) > 0 {
		requested = def.Tools
	} else {
		for name := range available {
			requested = append(requested, name)
		}
	}
	sort.Strings(requested)

	var (
		kept  []string
		diags []scopeDiag
		seen  = map[string]struct{}{}
	)
	for _, name := range requested {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, no := disallowed[name]; no {
			continue // step 2: explicitly disallowed, silently honoured.
		}
		if _, excluded := callSiteExcluded[name]; excluded {
			diags = append(diags, scopeDiag{name, "excluded at this call site (no nesting/disclosure); dropped"})
			continue
		}
		t, ok := available[name]
		if !ok {
			// Step 3: not in the base set. DISTINCT from "forbidden": this is an
			// unknown name (typo) OR an MCP/skills/repo-map tool that Tier-1 scoped
			// catalogs cannot see (documented v1 limitation).
			diags = append(diags, scopeDiag{name, "unknown tool (not in the core base set; MCP/skills tools are not scopable in Tier 1); dropped"})
			continue
		}
		if !allowMutating && !t.ReadOnly() {
			// Step 5: read-only call site (Task, or a base-sharing team member); a
			// workspace-mutating tool is dropped, full stop.
			diags = append(diags, scopeDiag{name, "tool is workspace-mutating but this call site is read-only; dropped"})
			continue
		}
		kept = append(kept, name)
	}
	return kept, diags
}

// baseTaskTools returns the AVAILABLE base toolset a Task-def catalog is scoped
// over: the core read-only/explorer tools plus Bash-if-configured (mirroring what
// buildChildEngine/buildMemberEngine register). Bash is included so a def that
// allow-lists it gets a DISTINCT "mutating; dropped" diagnostic rather than a
// misleading "unknown tool" — it exists but is forbidden for read-only Task.
func baseTaskTools(cfg Config) map[string]tool.Tool {
	out := map[string]tool.Tool{}
	for _, t := range tools.All() { // Read, Edit, Write, Grep, Glob, WebFetch
		out[t.Spec().Name] = t
	}
	if runner := buildCommandRunner(cfg); runner != nil {
		bt := tools.NewBashTool(runner)
		out[bt.Spec().Name] = bt
	}
	return out
}

// knownHookPhases is the governance hook-phase taxonomy a def's `hooks:` map may
// scope. A def hook keyed on a phase outside this set is dropped with a
// composition-time diagnostic (the catalog-free parser cannot validate phases, so
// it is done here, against the domain taxonomy).
var knownHookPhases = map[governance.HookPhase]struct{}{
	governance.PhaseSessionStart:     {},
	governance.PhaseUserPromptSubmit: {},
	governance.PhasePreToolUse:       {},
	governance.PhasePostToolUse:      {},
	governance.PhaseStop:             {},
	governance.PhaseSubagentStop:     {},
	governance.PhaseTeammateIdle:     {},
	governance.PhaseTaskCreated:      {},
	governance.PhaseTaskCompleted:    {},
}

// skillIndex is a name → body lookup over the active skills, built once at
// composition time so each def's `skills:` preload is a cheap map read. It is the
// SAME discovered-skill set the Skill tool serves (operator-controlled content), so
// preloading a skill body into a def's prompt stays inside the skill trust boundary.
type skillIndex map[string]string

// resolveSkillIndex discovers the active skills (explicit + conventional, exactly
// as registerSkills does) and indexes them by name → body. It is forgiving: any
// discovery fault yields an empty index (a def's skills preload then no-ops with a
// diagnostic) rather than failing the build. Returns nil when skills are disabled.
func resolveSkillIndex(ctx context.Context, cfg Config) skillIndex {
	sources := skills.ResolveSources(skills.ResolveOptions{
		Explicit:     cfg.SkillsDirs,
		Conventional: cfg.SkillsConventional,
		Workspace:    cfg.Workspace,
	})
	if len(sources) == 0 {
		return nil
	}
	discovered, _, err := skills.NewMultiSource(sources...).Skills(ctx)
	if err != nil || len(discovered) == 0 {
		return nil
	}
	idx := make(skillIndex, len(discovered))
	for _, s := range discovered {
		idx[s.Name] = s.Body
	}
	return idx
}

// preloadedSkillBodies resolves a def's `skills:` names against the index, returning
// the matched bodies in def order. An unknown name is a non-fatal diagnostic (logged
// by the caller via the returned missing list), never a failure — mirroring the
// forgiving tool/model resolution. A nil index (skills disabled) makes every name
// "missing".
func preloadedSkillBodies(def agents.AgentDef, idx skillIndex) (bodies []string, missing []string) {
	for _, name := range def.Skills {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if body, ok := idx[name]; ok && strings.TrimSpace(body) != "" {
			bodies = append(bodies, "Skill ("+name+"):\n\n"+strings.TrimSpace(body))
		} else {
			missing = append(missing, name)
		}
	}
	return bodies, missing
}

// defMCPTools resolves a def's mcpServers into the tools to add to its engine's
// catalog, the tool NAMES (so the read-only backstop can exempt them), and a Close
// that tears down any INLINE managers this def connected (nil when the def opened no
// inline server). It is the SINGLE place both call sites (Task path and team-member
// path) resolve per-agent MCP, so reference/inline semantics cannot drift.
//
//   - REFERENCE entries (URL empty) take the named server's tools out of mainMgr —
//     NO new connection, so they contribute nothing to Close. An unknown reference is
//     a clear diagnostic and is skipped.
//   - INLINE entries connect a SCOPED mcp.NewManager for this def alone; their tools
//     are added and their manager's Close is aggregated into the returned Close.
//
// It is forgiving end-to-end (the skills/teams philosophy): an unreachable inline
// server or an unknown reference is logged and skipped, never fatal — the def is
// still built with whatever MCP tools did resolve.
func defMCPTools(ctx context.Context, def agents.AgentDef, mainMgr *mcp.Manager) (mcpTools []tool.Tool, names []string, closeFn func() error) {
	if len(def.MCPServers) == 0 {
		return nil, nil, nil
	}

	var inlineConfigs []mcp.ServerConfig
	for _, entry := range def.MCPServers {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		if entry.IsReference() {
			refTools, ok := mainServerTools(mainMgr, name)
			if !ok {
				slog.Warn("agent def references an unknown MCP server; not scoped (no such configured server)",
					"agent", def.Name, "server", name, "path", def.Path)
				continue
			}
			mcpTools = append(mcpTools, refTools...)
			slog.Info("agent def scopes a referenced MCP server",
				"agent", def.Name, "server", name, "tools", len(refTools), "path", def.Path)
			continue
		}
		inlineConfigs = append(inlineConfigs, mcp.ServerConfig{
			Name:    name,
			URL:     strings.TrimSpace(entry.URL),
			Headers: entry.Headers,
		})
	}

	if len(inlineConfigs) > 0 {
		onError := func(sc mcp.ServerConfig, err error) {
			slog.Warn("agent def inline MCP server unreachable; skipping",
				"agent", def.Name, "server", sc.Name, "url", sc.URL, "err", err)
		}
		mgr, err := mcp.NewManager(ctx, inlineConfigs, onError)
		if err != nil {
			slog.Warn("agent def inline MCP managers all failed; none scoped",
				"agent", def.Name, "err", err)
		}
		if mgr != nil {
			inlineTools := mgr.Tools()
			mcpTools = append(mcpTools, inlineTools...)
			closeFn = mgr.Close
			slog.Info("agent def scopes inline MCP servers",
				"agent", def.Name, "servers", len(mgr.Servers()), "tools", len(inlineTools), "path", def.Path)
		}
	}

	for _, t := range mcpTools {
		names = append(names, t.Spec().Name)
	}
	return mcpTools, names, closeFn
}

// mainServerTools returns the named main server's tools and true on a hit, or nil,
// false when mainMgr is nil or has no such server. It is the reference-resolution
// primitive defMCPTools uses.
func mainServerTools(mainMgr *mcp.Manager, name string) ([]tool.Tool, bool) {
	if mainMgr == nil {
		return nil, false
	}
	for _, s := range mainMgr.Servers() {
		if s.Name() == name {
			return s.Tools(), true
		}
	}
	return nil, false
}

// defHookRunner builds the HookRunner scoped to a def's engine from its `hooks:`
// map. A def with no (valid) hooks returns the shared default runner (an inert
// hookexec.New(nil)), so behaviour is unchanged for defs that scope no hooks.
// Unknown phases are dropped with a diagnostic. The runner uses cfg.Shell when set
// so a def hook runs under the same interpreter as the main session's hooks.
func defHookRunner(cfg Config, def agents.AgentDef, fallback port.HookRunner) port.HookRunner {
	if len(def.Hooks) == 0 {
		return fallback
	}
	hooks := make(map[governance.HookPhase]string, len(def.Hooks))
	for rawPhase, cmd := range def.Hooks {
		phase := governance.HookPhase(rawPhase)
		if _, ok := knownHookPhases[phase]; !ok {
			slog.Warn("agent def references an unknown hook phase; ignored",
				"agent", def.Name, "phase", rawPhase, "path", def.Path)
			continue
		}
		hooks[phase] = cmd
	}
	if len(hooks) == 0 {
		return fallback
	}
	var opts []hookexec.Option
	if cfg.Shell != "" {
		opts = append(opts, hookexec.WithShell(cfg.Shell))
	}
	slog.Info("agent def scopes lifecycle hooks", "agent", def.Name, "phases", len(hooks), "path", def.Path)
	return hookexec.New(hooks, opts...)
}

// buildAgentTaskEngines turns the registry into the per-def, read-only child
// engines + metadata the Task tool routes over. Each engine gets:
//   - a SCOPED catalog = (def.Tools allowlist ∩ available base tools) minus
//     def.DisallowedTools, read-only-only, never Task/Fork/ToolSearch;
//   - a resolved model (def.Model > SubagentModel > parent);
//   - the def.Body composed into the system prompt as the Role (composition-layer
//     only; no prompt.Config domain change — critique M2/M3);
//   - an allow-all policy and progressive disclosure OFF (tiny catalog).
//
// A def whose entire allowlist is stripped (e.g. a pure-mutating Task def) still
// gets an engine with an empty-but-valid catalog; the diagnostics explain why,
// and the model still receives a clear "no tools" inventory. Returns nil/empty
// when the registry is empty so Task behaves exactly as before.
// The skillIdx preloads each def's `skills:` bodies into its prompt; defaultHooks
// is the inert fallback HookRunner a def with no scoped `hooks:` adopts (so the
// default Task engine behaviour is unchanged).
func buildAgentTaskEngines(ctx context.Context, cfg Config, provider port.LLMProvider, reg *agents.Registry, skillIdx skillIndex, defaultHooks port.HookRunner, mainMgr *mcp.Manager) (map[string]*agent.Engine, []agent.AgentMeta, func() error) {
	if reg == nil || reg.Len() == 0 {
		return nil, nil, nil
	}
	base := baseTaskTools(cfg)

	engines := make(map[string]*agent.Engine, reg.Len())
	meta := make([]agent.AgentMeta, 0, reg.Len())
	var closeFn func() error

	for _, def := range reg.List() {
		names, diags := scopedToolNames(def, base)
		for _, d := range diags {
			slog.Warn("agent def tool scoping",
				"agent", def.Name, "tool", d.tool, "reason", d.reason, "path", def.Path)
		}

		cat := tool.NewCatalog()
		for _, name := range names {
			cat.MustRegister(base[name])
		}

		// Per-agent MCP: a def's mcpServers add the referenced/inline servers' tools to
		// THIS def's catalog (not the main conversation's). The inline managers' Close is
		// aggregated into closeFn → Built.Close (process-lifetime engines, torn down on
		// shutdown). MCP tool names are NOT relevant to a Task def's read-only backstop
		// (Task defs are not team members), so the names return is ignored here.
		mcpTools, _, mcpClose := defMCPTools(ctx, def, mainMgr)
		for _, mt := range mcpTools {
			if err := cat.Register(mt); err != nil {
				slog.Warn("agent def MCP tool registration failed; skipped",
					"agent", def.Name, "tool", mt.Spec().Name, "err", err)
			}
		}
		closeFn = composeCloseErr(mcpClose, closeFn)

		model := resolveModel(cfg, def)

		bodies, missing := preloadedSkillBodies(def, skillIdx)
		for _, name := range missing {
			slog.Warn("agent def references an unknown skill; not preloaded",
				"agent", def.Name, "skill", name, "path", def.Path)
		}
		hooks := defHookRunner(cfg, def, defaultHooks)

		// ProgressiveTools deliberately OFF: child catalogs are tiny and a ToolSearch
		// tool would not be in the def allowlist. newChildEngineWithHooks leaves it at
		// its zero value (off), matching the original explicit omission.
		engines[def.Name] = newChildEngineWithHooks(provider, cat, model, agentPromptConfig(cfg, def, model, bodies...), hooks)
		meta = append(meta, agent.AgentMeta{Name: def.Name, Description: def.Description})

		slog.Info("agent def engine built",
			"agent", def.Name, "tools", strings.Join(names, ","), "model", model,
			"preloaded_skills", len(bodies), "path", def.Path)
	}

	// meta in registry (name-sorted) order for a byte-stable Task spec.
	sort.Slice(meta, func(i, j int) bool { return meta[i].Name < meta[j].Name })
	return engines, meta, closeFn
}

// composeCloseErr chains two optional error-returning close funcs into one (first
// then second, both always run, first non-nil error returned), or nil when both are
// nil. It is the app-layer analogue of agent.composeCleanup, used to aggregate the
// per-def inline MCP managers' Close into one chain.
func composeCloseErr(first, second func() error) func() error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return func() error {
			err1 := first()
			err2 := second()
			if err1 != nil {
				return err1
			}
			return err2
		}
	}
}

// composeClose adapts an error-returning close (the aggregated inline MCP teardown)
// and a plain func() (the main MCP close) into ONE func() that runs both — MCP-def
// teardown first, then the main manager. It is how buildCatalog folds the Task-def
// inline managers into the single mcpClose that feeds Built.Close.
func composeClose(errClose func() error, plainClose func()) func() {
	return func() {
		if errClose != nil {
			if err := errClose(); err != nil {
				slog.Warn("agent def inline MCP close", "err", err)
			}
		}
		if plainClose != nil {
			plainClose()
		}
	}
}

// agentPromptConfig is promptConfig with the def's body composed into the Role
// (Option C from the critique: compose, do not replace, and keep the domain
// prompt.Config untouched). The default role line is preserved and the def body
// is appended after it, so the specialist's playbook rides in the cache-stable
// StablePrefix while the standard mecatl framing remains. The Env model is set to
// the caller's ALREADY-RESOLVED model id (threaded in, not re-resolved): resolving
// it a second time here would re-run resolveModel and log the unknown-alias warning
// a second time per def. The caller resolves the model ONCE and passes it.
func agentPromptConfig(cfg Config, def agents.AgentDef, resolvedModel string, skillBodies ...string) prompt.Config {
	pc := promptConfig(cfg)
	pc.Env.Model = resolvedModel
	var parts []string
	if body := strings.TrimSpace(def.Body); body != "" {
		parts = append(parts, "Agent definition ("+def.Name+"):\n\n"+body)
	}
	// PRELOADED skills (def.Skills): inject each matched skill body so the specialist
	// starts with those playbooks in context (Claude-Code-style skill preloading).
	// They ride in the cache-stable StablePrefix alongside the def body.
	parts = append(parts, skillBodies...)
	if len(parts) > 0 {
		pc.Role = prompt.DefaultRole() + "\n\n" + strings.Join(parts, "\n\n")
	}
	return pc
}

// agentSnapshot projects the resolved registry into the proto AgentInfo list the
// server's ListAgents RPC returns. The model is RESOLVED (alias → concrete id,
// "" meaning inherit) and the tools field is the def's EFFECTIVE read-only Task
// scope (the same allowlist∩base, minus mutating/excluded, that Task children
// get) so the snapshot reflects what the model can actually route to — not the
// raw frontmatter. It is a pure projection: name-sorted (registry order), no I/O,
// nil-safe (an empty/nil registry yields an empty slice, never nil-as-error).
func agentSnapshot(cfg Config, reg *agents.Registry) []*mecatlv1.AgentInfo {
	if reg == nil || reg.Len() == 0 {
		return nil
	}
	base := baseTaskTools(cfg)
	out := make([]*mecatlv1.AgentInfo, 0, reg.Len())
	for _, def := range reg.List() {
		names, _ := scopedToolNames(def, base)
		out = append(out, &mecatlv1.AgentInfo{
			Name:           def.Name,
			Description:    def.Description,
			Model:          resolveModel(cfg, def),
			Tools:          names,
			PermissionMode: strings.TrimSpace(def.PermissionMode),
			Color:          def.Color,
		})
	}
	return out
}

// resolveAgentRegistry resolves the agent-definition registry from cfg (explicit
// dirs + conventional locations when enabled). It is forgiving: discovery
// diagnostics are logged and a hard fault yields an empty registry rather than
// failing the build. Strict opt-in — zero sources means an empty registry.
func resolveAgentRegistry(ctx context.Context, cfg Config) *agents.Registry {
	sources := agents.ResolveSources(agents.ResolveOptions{
		Explicit:     cfg.AgentsDirs,
		Conventional: cfg.AgentsConventional,
		Workspace:    cfg.Workspace,
	})
	if len(sources) == 0 {
		slog.Info("agent definitions DISABLED (no agents dirs configured)")
		return agents.NewRegistry(nil)
	}
	reg, skips, err := agents.ResolveRegistry(ctx, agents.NewMultiSource(sources...))
	for _, s := range skips {
		slog.Warn("agent def skipped", "path", s.Path, "reason", s.Reason)
	}
	if err != nil {
		slog.Warn("resolving agent definitions failed; none registered",
			"dirs", strings.Join(cfg.AgentsDirs, ","), "conventional", cfg.AgentsConventional, "err", err)
		return agents.NewRegistry(nil)
	}
	if reg.Len() == 0 {
		slog.Info("agent definitions DISABLED (no valid <name>.md found in any source)")
	} else {
		names := make([]string, 0, reg.Len())
		for _, d := range reg.List() {
			names = append(names, d.Name)
		}
		slog.Info("agent definitions ENABLED",
			"count", reg.Len(), "agents", strings.Join(names, ","),
			"conventional", cfg.AgentsConventional)
	}
	return reg
}
