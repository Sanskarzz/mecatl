---
title: "Agent-Primitives Evaluation — mecatl vs the Field (June 2026)"
doc_id: 09-agent-primitives-evaluation
layer: evaluation
captured: 2026-06-09
status: stable (point-in-time evaluation; the roadmap below has largely SHIPPED — see Status)
keywords: [delegation, subagent, teams, parallel, background agents, per-child cancel, inspect, resume, token budget, permission posture, field comparison, Claude Code, opencode, Gemini CLI, Codex, Amp, Goose, Cursor, Copilot, Factory, Crush]
---

> **Status (2026-06-10).** Tiers 1–4 of the roadmap below shipped as commits
> `3b982bb..e4cb72b`; the Tier-5 headline (background delegation + per-child cancel) shipped as
> the bg-arc `2d0bb4f..0bdcc21` (design: `docs/design/BACKGROUND-SUBAGENTS.md`). The remaining
> items are tracked as GitHub issues #28–#40. This document is preserved as the evaluation that
> drove that work and as the field-comparison reference the issues cite.

# Agent Primitives Evaluation — mecatl vs the field (2026-06-09)

Method: scout of our primitives + extraction of the in-repo harness corpus (dated 2026-05-18),
then fresh web research on Claude Code, opencode, Gemini CLI, Codex CLI, Amp, Goose, Cursor,
Copilot/AgentHQ, Factory, Crush, plus three deep-dive reviews of our own code (mechanics/bugs,
model-facing prompts, TUI UX). Seven sonnet agents total.

## Verdict

mecatl's delegation primitives are **competitive with, and in several dimensions ahead of, every
harness surveyed**. No critical or major bugs were found in the delegation code. The weakest
surface is the **model-facing prompt language** (one outright contradiction, a high-impact lead-
briefing gap), and the largest **strategic gaps vs the field** are background/async delegation,
child continuation/inspection (Subagent), and per-agent memory.

## Where we are AHEAD of the field

| Capability | mecatl | Best comparable |
|---|---|---|
| Structured child output w/ validation retry | `output_schema` → synthetic SubmitResult → bounded retry → StopStructuredOutput | Goose sub-recipe `response.json_schema` (no retry loop); Codex CSV-batch only ("future enhancement"); Claude Code has NOTHING |
| Per-call tighten-only limits (turns/tools/timeout/tokens) | All four on the Subagent call | Claude Code: frontmatter-only maxTurns; Gemini: def-level only; Amp/Cursor/Crush: none |
| Loop-level token budget inherited by all children | Deps.MaxRunTokens + per-call override | Nobody else has a token budget at all (Copilot: 59-min timeout) |
| Child permission nuance | 4-step model: substitution analysis → isolation auto-approve → surface-to-human → honest auto-deny | Claude Code background: blanket auto-deny; Gemini: yolo default, no surfacing; opencode: inheritance broken (issue #12566) |
| Redaction by structure | drainChildObserved single chokepoint, gauntlet #7 | Amp leaks via stream-json (by design); most others undocumented |
| Resilient team deliverable | 3-tier (synthesis → ledger fallback → honest floor) + findings ledger | Claude Code teams: organic synthesis, no fallback |
| Worktree isolation default for read-only children | Always-on for Subagent/team-RO | Claude Code: opt-in `isolation: worktree`; opencode/Gemini: none |
| Judge-joined parallel fan-out | Parallel join=judge with preserved winner fork | Cursor /best-of-n (human adjudicates); nobody has LLM-judge built in |
| Structural nesting prevention | callSiteExcluded at catalog build | Crush does same; opencode had recursion regressions (#18100, 20 levels deep) |

## Field capabilities we LACK (gap analysis)

1. **Background / async delegation** — biggest architectural gap. Claude Code: `run_in_background`,
   Ctrl+B, agent view (separate supervisor process), notification on completion. opencode: experimental
   background subagents + `task_status` polling. Cursor: cloud agents. mecatl: every delegation blocks
   the parent turn.
2. **Child continuation + inspection for Subagent** — Claude Code returns agentId and `SendMessage`
   resumes a stopped subagent; opencode Task has `task_id` resume; Codex threads have `codex-reply`.
   We emit the `agentId:` trailer but there is no InspectSubagent and no resume (both already named
   deferred items — the field has now validated them).
3. **Fork (context-inheriting) subagents** — Claude Code v2.1.117+: forks inherit the parent
   transcript (cheap via prompt-cache reuse). All our children are fresh-context.
4. **Per-agent persistent memory** — Claude Code subagent frontmatter `memory: user|project|local`
   with MEMORY.md injection at startup; Factory's Knowledge Droid. Our agent defs have no memory field.
5. **Workflow orchestration layer** — Claude Code dynamic workflows (model-authored JS scripts,
   16 concurrent / 1000 total agents, phases, resumable, saveable as commands). Different league of
   primitive; noted as the field's direction for "comprehensive" work.
6. **User-steerable teammates** — Claude Code in-process mode: Shift+Down to cycle, type to message
   a teammate, plan-approval gate (teammate works in plan mode until lead approves). Our members are
   only model-steerable (SendMessage between members); the human can only watch the overlay.
7. **Config-level child permission axis** — Amp's `context: "subagent"` rule scope and `delegate`
   action (external approver binary, stdin JSON / exit code). *Addressed (issue #32):* the
   `permissions: subagent:` block + `governance.Audience` now give user-configurable child-scoped
   allow/ask/deny (trust-gated, escape-bounded); the `delegate` external-approver action remains
   unbuilt.
8. **Headless auto-review** — Codex `approvals_reviewer = "auto_review"`: a reviewer agent
   adjudicates boundary-crossing asks instead of auto-deny (~200x fewer human stops, circuit breaker
   after 3 consecutive denials). Directly applicable to our step-4 headless auto-deny.
9. **Cheap-model default for children** — Crush auto-routes sub-agents to `smallModel`; Anthropic's
   own field report (lead Opus + Sonnet workers beat single Opus by 90.2%). We default children to
   the parent's model; a def or per-call override is required to cheapen them.
10. **OS-level child sandboxing** — Codex Seatbelt/bwrap+Landlock+seccomp. Our worktree isolates the
    FS layout, not the process (we already acknowledge this: `-exec`/`-toolexec` rejection; untrusted-
    repo shell trust-gating still deferred).

## Bugs / code findings (eval agent: no critical or major findings)

Two scout leads REFUTED on verification:
- `turnsUsed` undercount (teamsupervisor.go:865) — turns are captured before Reopen; no undercount.
- `runBranchesFirst` wedge (parallel.go:678) — `done` is buffered to len(tasks), every goroutine
  defers the send; no deadlock, event bracketing correct.

Confirmed (all minor/nit):
- **MINOR** teamsupervisor.go:882 — member `Reopen()` error silently discarded; a store-failure
  Reopen is indistinguishable from a cancelled member. Add a Warn when `stop != StopCancelled`.
- **NIT** subagent.go:799 — `childPosture.role = t.idPrefix` ⇒ all concurrent Subagent children log
  `agent=subagent`; team/parallel use per-member/per-branch labels. Use the child session id.
- **NIT** teamsupervisor.go:1360/1401 — `"You have claimed task …"` header line is not in
  `framingHeader`, so it is not stripped inside fences (low practical risk: task ids are harness-
  incremented).
- **NIT** — no test asserts `turnsUsed` on the cancelled-member path.

Noted as done well: register-before-emit askRegistry; drainChildObserved bulkhead; cross-attempt
token accumulation in driveChild; neutraliseFraming defense-in-depth; unconditional cleanupAll defer.

## Prompt-surface findings (ranked by impact)

1. **HIGH — Lead briefing** (`renderTurnPrompt`, teamsupervisor.go:1338): "Decompose the goal into
   tasks with AddTask, delegate them, and when teammates report back you will be asked to produce the
   final consolidated report." Problems: "delegate them" is vague (there is no delegate tool — members
   ClaimTask); synthesis timing opaque; **the lead is never told to RecordFinding its own conclusions**
   (the synthesis reads the ledger; lead-only knowledge not in the ledger can be lost). Likely a root
   cause of weak/empty syntheses. Proposed: "(1) create tasks with AddTask — teammates will claim
   them; (2) monitor with ListTasks; (3) coordinate via SendMessage; (4) RecordFinding any conclusions
   YOU reach. When all tasks complete you will receive a synthesis prompt."
2. **HIGH — Team tool description** (teamtool.go:202): no template for what a good LEAD role briefing
   contains; parents write vague lead roles ("coordinate the team"). Add a lead-role recipe + a member-
   role recipe to the description, and surface the team_id→InspectMember link here too.
3. **MED-HIGH — Subagent description** (subagent.go:521): "read-only investigation" undersells the
   build/test shell (models won't route `go test` diagnosis there); no deliverable contract ("the
   subagent's FINAL MESSAGE is its deliverable"); `prompt` param should tell the model to specify the
   expected output format; no when-NOT-to-use vs Parallel/Team.
4. **MED — SubmitResult description contradiction** (structuredoutput.go:52): "Call this exactly
   once" contradicts the validation-retry behavior — its own Execute error says "call SubmitResult
   again". May inhibit corrections. One-line fix.
5. **MED — Judge prompt** (forkjudge.go:160): no evaluation rubric; doesn't tell the judge it sees
   summaries only (not the actual forks); "one sentence" rationale too thin; JSON-only instruction
   could be firmer.
6. **MED — Synthesis header** (teamsupervisor.go:1022): add one line on what a good report looks like
   ("directly answers the goal, presents key findings, actionable without having seen the team's
   work") to reduce non-deliverable syntheses that burn the fallback.
7. **LOW-MED** — `goal` schema description uses trust-model jargon ("trusted top-level instruction");
   `description` param undersold ("logs/UX only" — models skip it); `output_schema` lacks when-to-use;
   InspectMember should disclose its bounds (last ~40 msgs) and a positive trigger.

Done well: RecordFinding description (best in codebase), CompleteTask ordering constraint, fence
explanations, both nudge texts, agencyDelta, explorerReferencesInstruction.

## TUI/UX findings (ranked)

1. **MED — Allow-always is a dead proto path** (client/stream.go:126): TUI sends legacy `allow bool`
   only; `APPROVAL_VERDICT_ALLOW_ALWAYS` exists end-to-end on proto+server but no button. Repetitive
   child Bash asks must be approved one by one.
2. **MED — Child asks unattributed** (permission.go:39, dispatch.go:470): modal shows "subagent
   requests approval to run Bash: …" but not WHICH child (goal/hash) — unattributable with N
   concurrent children. Include the child goal label in the surfaced Reason.
3. **MED — No cancel-one-child**: esc cancels the whole run; the ctrl+a overlay is read-only.
   Needs a per-child cancel affordance (and a server seam for it).
4. **MED — Task descriptions never rendered** (team.go:572): `teamTask.desc` is stored; the Tasks
   sub-view shows only id/state/assignee — the comment claims a focus surface that doesn't exist.
5. **LOW-MED — No transcript depth for Subagent/Parallel**: chip trace capped at 12; teams have
   InspectMember, subagents have nothing (ties to the InspectSubagent gap).
6. **LOW** — no completion notice when a team ends (footer segment just vanishes); branch durationMs
   stored but never rendered; parallel run-level stop not in group focus; cache tokens per child not
   shown; inline subagent card lacks the live current-tool signal (overlay-only).

Done well: gauntlet-#7 honesty notes in the focus panes; preferredAgentsTab precedence; single
stop-label vocabulary; sanitized+unmissable permission modal; graceful footer width-shedding.

## Recommended roadmap (prioritized)

**Tier 1 — prompt-only fixes (no mechanics, hours):**
1. Fix SubmitResult "exactly once" contradiction.
2. Rewrite the lead briefing (RecordFinding for lead, claim-don't-delegate, synthesis timing).
3. Add lead/member role templates to the Team description; de-jargon `goal`.
4. Rewrite Subagent description: deliverable contract, build/test capability, output-format guidance,
   when-NOT-to-use.
5. Judge prompt rubric + summaries-only caveat.
6. Synthesis-header report-quality line.

**Tier 2 — small code fixes:**
7. Per-child diagnostics role (child session id, not idPrefix); Warn on unexpected member Reopen
   error; add "you have claimed task" to framingHeader; turnsUsed assertion in the cancelled test.

**Tier 3 — UX (medium):**
8. Wire ALLOW_ALWAYS into the TUI approval modal.
9. Attribute child asks (goal label in Reason).
10. Render task descriptions; branch duration; parallel stop; team-done transient notice.
11. Per-child cancel (needs a server seam).

**Tier 4 — mechanics (the two deferred items the field has validated):**
12. InspectSubagent (mirror InspectMember; the agentId trailer already pre-positions it).
13. Subagent continuation/resume (task_id-style or SendMessage-style; Claude Code, opencode, and
    Codex all converged on this).
14. Team-wide token budget (already the named next item).

**Tier 5 — strategic bets (bigger design work, in rough value order):**
15. Background subagents + completion notification (the field's clearest direction).
16. Headless auto-review: an LLM reviewer adjudicates step-4 asks instead of blanket auto-deny
    (Codex pattern; with a deny circuit-breaker).
17. Config-level child permission axis (Amp `context: subagent`) — make childPosture user-tunable.
    *Addressed (issue #32):* shipped as the `permissions: subagent:` config block + rule audiences.
18. Per-agent memory (`memory:` frontmatter field, MEMORY.md injection).
19. Fork/context-inheriting subagents (prompt-cache-cheap).
20. Cheap-model-by-default child routing (Crush smallModel pattern) — at minimum a config default.

