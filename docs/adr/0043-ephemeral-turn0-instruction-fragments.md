# ADR 0043 — Ephemeral turn-0 instruction fragments

- Status: Accepted
- Date: 2026-06-22
- Scope: the agent loop's turn-0 instruction assembly — how the soul / project-instruction (AGENTS.md+CLAUDE.md) / memory-index / user-model fragments produced by the `prompt.InstructionAssembler` chain reach the model.
- Supersedes: the "loaded into the turn-0 conversation as data" framing in [ADR 0009](./0009-tiered-memory.md), [ADR 0011](./0011-soul-and-user-model.md), and [ADR 0012](./0012-compaction.md), and the resume-gate approach landed earlier on this branch.

## Context

The turn-0 instruction fragments — the soul/persona, the discovered project
instructions (AGENTS.md / CLAUDE.md), the memory index, and the user model — were
PERSISTED into `Conversation.Messages` as `RoleUser` messages at the start of a
session, recorded through `RecordUserPromptWithParts(text, parts, instr)` in
`recordPrompt`. This had three compounding problems:

1. **Resume bloat.** Reopen/Interrupt/Recover zero `Counters` (`resetToIdle`), so the
   original `Counters.Turns == 0` injection gate re-fired on EVERY resumed run and
   re-appended soul/AGENTS.md/memory/user-model into the persisted history — observed
   3× soul in a real session. An earlier fix on this branch added a "no genuine user
   turn recorded yet" gate (commit f31bde54) to stop the re-injection, but that left
   the fragments persisted-once and made resume ambiguous: a resumed session that had
   compacted the fragments away would never see them again.

2. **Compaction-pin ambiguity.** Because the fragments were persisted as `RoleUser`
   messages AHEAD of the genuine first user instruction, the compaction pin and the
   verbatim-tail back-snap had to content-identify and skip them
   (`prompt.IsInjectedTurn0Fragment`) so they did not anchor on an injected fragment
   instead of the user's real goal (the "I don't have the original task" bug, fixed in
   6ec96fc8).

3. **Rehydration divergence.** The fragments were NOT event-carried (`EvUserPrompt`
   records the genuine prompt only), so a snapshot-rehydrated session carried them
   while an event-sourced `eventsource.Fold` reconstruction did not — the two
   rehydration paths disagreed on the conversation shape.

## Decision

The turn-0 instruction fragments are EPHEMERAL: assembled ONCE per run and prepended
to `LLMRequest.Messages` on EVERY turn (including resume), and NEVER persisted into
`Conversation.Messages`, event-carried, or snapshotted.

- `recordPrompt` records ONLY the genuine prompt (+ media parts) — it calls
  `RecordUserPromptWithParts(finalText, parts, nil)`. The resume gate is removed.
- `buildRequest` assembles the fragments once per run via `Deps.Instructions.Assemble`
  (cached on the `Run` struct behind a `sync.Once`; fail-soft — an assemble error
  leaves the cache nil and the run proceeds without fragments) and builds a FRESH
  `Messages` slice `[fragments… ] ++ Conversation.Messages` each turn. `Conversation`
  is never mutated. Assembling once per run keeps the message prefix byte-stable
  within the run, preserving the provider prompt cache.
- The genuine prompt is still recorded and still event-carried (`EvUserPrompt`),
  unchanged.
- `isGenuineUserTurn`'s synthesised-summary arm stays LOAD-BEARING (a re-compaction
  must not anchor on a prior summary); its `IsInjectedTurn0Fragment` arm becomes
  DEFENSE-IN-DEPTH — fragments are no longer normally persisted, but the predicate
  stays correct for any legacy history snapshotted before this cutover. The exported
  `prompt.IsInjectedTurn0Fragment` is retained for that defensive arm.

## Consequences

- **Easier:** resume no longer bloats history (no fragments persist, so no growth
  across N resumes); the persisted conversation is clean, so compaction naturally pins
  the genuine first instruction; the fragments are present on every run including
  resume, unconditionally (the resume-gate's "skip on resume" ambiguity is gone); and
  the snapshot + event-sourced rehydration paths CONVERGE fragment-free (a win — they
  now agree on the conversation shape, with no event-schema change).
- **Harder / costs honestly:** the fragments are re-assembled once per run (a small
  per-run cost, not per-turn — the `Run` cache amortises it across the run's turns);
  the per-run cache adds a field + a `sync.Once` to `Run`; and the prompt prefix is
  byte-stable only WITHIN a run (a new run re-assembles, which is correct — a soul or
  memory change between runs should be reflected). The within-run byte-stability that
  the prompt cache relies on is preserved exactly as before.
- **Side improvement — forks no longer carry fragments.** Because the fragments are
  no longer in `Conversation.Messages`, the `subagent fork: true` deep-copy
  (`session.ForkSnapshot` of the parent conversation) no longer duplicates the
  parent's turn-0 fragments into the forked child — the child re-assembles its OWN
  fragments per run (its own soul/memory/project-instruction posture), which is the
  correct behaviour and shrinks the copied history.
- **No API change:** `buildRequest` / `runTurn` / `runLoop` are unexported;
  `RecordUserPromptWithParts` keeps its `instr` parameter (now called with nil — still
  a valid seam); `prompt.IsInjectedTurn0Fragment` stays exported. The engine
  api-compat gate shows no diff. The behaviour change (fragments no longer persisted)
  is noted in `engine/CHANGELOG.md`.

## See also

- [ADR 0012 — compaction](./0012-compaction.md) (the pin + back-snap that this
  cleanup simplifies).
- [ADR 0009 — tiered memory](./0009-tiered-memory.md), [ADR 0011 — soul and user
  model](./0011-soul-and-user-model.md), [ADR 0038 — event-sourced
  rehydration](./0038-event-sourced-rehydration.md) (the rehydration convergence).
- The documentation lifecycle convention in [ADR 0002](./0002-documentation-lifecycle.md).
