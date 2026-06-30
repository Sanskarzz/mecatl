---
name: mecatl-model-router-config
description: >-
  Designs and writes a mecatl model router configuration (the `models:` subtree of
  ~/.config/mecatl/settings.yaml) tailored to the operator's preferences. Asks
  about priorities (cost vs capability, open vs proprietary, provider, multimodal)
  then searches the latest model benchmarks/pricing and recommends a complete
  alias + slot + router-category taxonomy. Use when setting up or revising model
  routing, picking models per slot, or building a subagent router taxonomy. NOT
  for provider/key wiring, permission config, or non-mecatl harnesses.
---

# mecatl model router config builder

## Prerequisites

- The operator has a mecatl deployment and an API key for at least one provider
  (OpenRouter, Anthropic, or OpenAI). The config targets ONE provider — provider
  is fixed per session, so every alias must resolve to a model id on the same
  provider.
- Web search is available for live benchmark/pricing lookup.

## Workflow

### Step 1 — Elicit preferences (BEFORE any search)

Ask questions **one at a time** in this exact order. Do NOT present the full
list upfront. Wait for the operator's answer before moving to the next question.

Format for each question:

```
───────────────────────────────
✓ <label>: <answer>     ← repeat for each prior answer

→ <current question>?

  ▸ <recommended option> (recommended)
    <other option>

  yes = <recommended> · "back" to redo previous
───────────────────────────────
```

**Q1 — Provider**

```
───────────────────────────────
→ Provider?

  ▸ OpenRouter — aggregates all vendors behind one key (recommended)
    Anthropic direct
    OpenAI direct
    Other (name it)

  yes = OpenRouter · "back" to redo previous
───────────────────────────────
```

**Q2 — Priority axis**

```
───────────────────────────────
✓ Provider: <Q1>

→ Priority axis?

  ▸ balanced — frontier where it matters, cheap elsewhere (recommended)
    cost-tiered — prefer cheap/open models, accept lower ceiling
    capability-first — most capable regardless of cost

  yes = balanced · "back" to redo previous
───────────────────────────────
```

**Q3 — Open vs proprietary**

```
───────────────────────────────
✓ Provider: <Q1>
✓ Priority: <Q2>

→ Open-weights required?

  ▸ proprietary — hosted APIs are fine (recommended)
    open-weights — MIT/Apache only (self-hostable)

  yes = proprietary · "back" to redo previous
───────────────────────────────
```

**Q4 — Multimodal**

```
───────────────────────────────
✓ Provider: <Q1>
✓ Priority: <Q2>
✓ Open-weights: <Q3>

→ Image/vision input needed?

  ▸ text-only — no vision needed (recommended)
    rarely — explicit per-call via model: image
    commonly — bake a vision tier into the router

  yes = text-only · "back" to redo previous
───────────────────────────────
```

**Q5 — Target ceiling (optional)**

```
───────────────────────────────
✓ Provider: <Q1>
✓ Priority: <Q2>
✓ Open-weights: <Q3>
✓ Vision: <Q4>

→ Target coding ceiling? (optional)

  ▸ skip — no specific target (recommended)
    name a model, e.g. "Sonnet 4.6", "Opus 4.8"

  yes = skip · "back" to redo previous
───────────────────────────────
```

**Q6 — Existing config (auto-discovered, not asked blank)**

Do NOT ask. Silently run `cat ~/.config/mecatl/settings.yaml`.

- **Found with `models:` block** — show it fenced, then ask:
  ```
  ───────────────────────────────
  ✓ Provider: <Q1> · ✓ Priority: <Q2> · ✓ Open-weights: <Q3> · ✓ Vision: <Q4> · ✓ Ceiling: <Q5>

  → Found an existing models: config — use as base or start fresh?

    ▸ use as base — revise only what needs changing (recommended)
      start fresh — ignore existing config

    yes = use as base · "back" to redo previous
  ───────────────────────────────
  ```

- **Not found or no `models:` block** — skip silently, proceed to Step 2.

Record all answers. These determine which models are even candidates.

### Step 2 — Search the latest benchmarks & pricing

For each tier the operator needs (heavy/coder/quick, + optional image/specialty),
search for:

- **SWE-bench Verified** (the primary coding benchmark — prefer independent
  Vals.ai scores over vendor self-reports; note both).
- **SWE-bench Pro** (harder real-world repos) and **Terminal-Bench** (agentic
  multi-step shell — closest proxy to a harness loop) where available.
- **LiveCodeBench** / **HumanEval** (pure code-gen) as secondary signals.
- **Pricing** (input + output per 1M tokens) on the operator's chosen provider —
  verify the exact OpenRouter/Anthropic/OpenAI model id.
- **Modality** (text-only vs multimodal) — critical if vision is in scope.
- **License** (MIT/Apache open vs proprietary) if the operator cares.
- **Context window** (1M+ is common in 2026; note anything below).

Cross-check at least two sources per model (vendor model card + independent
leaderboard like Vals.ai / llm-stats / LLMReference). Flag vendor-reported vs
independently-verified scores — they diverge by 2–4 pts regularly.

Present a shortlist table (model | key benchmarks | price | modality | license)
for each tier, then recommend one per tier with rationale tied to the operator's
stated preferences.

### Step 3 — Map to the config schema

Read [`references/config-format.md`](references/config-format.md) for the exact
schema and rules. Then build the config:

1. **Aliases** — the spine. Define `heavy`/`coder`/`quick` (minimum) pointing at
   concrete provider model ids. Add `image` (or another specialty alias) only if
   the operator needs multimodal. Every alias must be on the chosen provider.
2. **`default`** — the session model. Usually the `heavy` alias (the strongest
   reasoning model), unless the operator wants a cheaper default.
3. **Slots** — route the four housekeeping calls (`compaction`/`ask-reviewer`/
   `guardrail`/`router`) to the `quick` alias (they're one-turn, tool-less calls
   that need instruction-following + JSON discipline, not deep coding). Route
   `plan` to `heavy` (the opusplan pattern — plan-mode turns swap to a strong
   reasoning model).
4. **Router categories** — 3 categories minimum (large/medium/small mapped to
   heavy/coder/quick). Add a 4th ONLY for a genuinely distinct model class
   (e.g. `image` for multimodal) — do NOT add a 5th; more categories degrade
   classifier accuracy and widen the steering surface. Write thorough
   `description:` fields — the classifier reads them literally to route.
5. **`default-category`** — usually `medium` (the bulk of delegation work).

### Step 4 — Deliver the config

Emit the complete `models:` YAML block, ready to drop into
`~/.config/mecatl/settings.yaml`. Include inline `# → <concrete-id>` comments on
each category's `model:` line so the operator can see the resolution at a glance.

After the config, include:

- A **changes-from-previous** summary (if revising) — one line per changed alias/
  slot/category, with the rationale.
- A **verification note** — tell the operator to check the mecated log for
  `model slot ACTIVE` / `subagent model router ACTIVE` lines on startup (a missing
  line = that binding failed to resolve and degraded to the session model).
- An **honest gaps note** — call out anything the chosen fleet can't do (e.g.
  "nothing here reaches Opus 4.8's 88.6% SWE-bench Verified; that ceiling requires
  Anthropic"). Don't oversell.

## Guidelines

- **Provider-fixed invariant**: never mix providers across aliases. If the
  operator wants Anthropic models, ALL aliases are `anthropic/*` ids. OpenRouter
  is the common choice because it aggregates many vendors behind one provider.
- **Cost-tiered default**: when the operator says "cost-tiered" or doesn't
  specify, prefer open/cheap models (DeepSeek V4-Flash/Pro, GLM-5.2, Gemini Flash)
  and put the expensive frontier model only where it's load-bearing (`heavy`/
  `plan`/`large`). Housekeeping slots always go on the cheapest credible model.
- **Capability-first default**: when the operator says "capability-first", put
  the strongest available model (Opus 4.8 / GPT-5.5 / Gemini 3 Pro) at `heavy`/
  `default`/`plan` and a strong mid (Sonnet 4.6 / Gemini 3.5 Flash) at `coder`,
  accepting the higher spend. Still route housekeeping to a cheaper tier — there's
  no value in running compaction summaries on Opus.
- **Vision**: if the operator needs images and the `heavy`/`coder` models are
  text-only, add an `image` alias pointing at a multimodal model (Gemini 3.5
  Flash is the strongest cheap multimodal coder in mid-2026). If vision is rare,
  make `image` a category the router can pick OR an alias the operator invokes
  via per-call `model: image` (more reliable — the classifier can't detect
  attached files, only prompt text that mentions them).
- **Category descriptions**: write them thoroughly — they're the classifier's only
  signal. Describe the task profile in concrete terms ("screenshots, UI mockups,
  design specs, diagrams" not just "visual tasks").
- **3–4 categories max**: difficulty tiers (large/medium/small) + at most one
  distinct-class category (image/specialty). More categories hurt classifier
  accuracy and widen the untrusted-prompt steering surface.
- **Cite sources**: when recommending a model, note whether its benchmark is
  vendor-self-reported or independently verified (Vals.ai etc.) — the gap matters.

## Error handling

| Situation | Fix |
|---|---|
| Operator wants a provider you can't find model ids for | Ask for the provider's model directory URL, or fall back to OpenRouter (aggregates most) |
| No model on the chosen provider meets the stated ceiling (e.g. "beat Opus 4.8" on OpenRouter) | Say so honestly. Offer the closest non-Anthropic option + note that the ceiling requires switching providers. |
| Operator asks for >4 router categories | Push back: explain the classifier-accuracy and steering-surface tradeoffs. Suggest an agent-def `model:` pin for the rare task instead of a category. |
| A recommended model id doesn't resolve on the provider | Verify the exact id via the provider's model page before emitting the config. OpenRouter ids are `vendor/model-name`; Anthropic direct ids are `claude-*`. |
| Benchmark data is sparse or stale (>6 months) | Flag it. Prefer Vals.ai (independent, re-runs) over vendor model cards. Note the review date. |

## See also

- [`references/config-format.md`](references/config-format.md) — the complete
  `models:` YAML schema, key rules, and a worked OpenRouter example.

## Interaction example

Step 1 uses `AskUserQuestion` — each question renders as a native UI widget
(radio buttons, descriptions). The operator clicks or types; no "yes"/"back"
shortcuts needed. The five calls happen one at a time:

1. `AskUserQuestion` → Provider → operator picks **OpenRouter**
2. `AskUserQuestion` → Priority → operator picks **capability-first**
3. `AskUserQuestion` → Licensing → operator picks **proprietary**
4. `AskUserQuestion` → Vision → operator picks **rarely**
5. `AskUserQuestion` → Ceiling → operator picks **Opus 4.8 territory**

Then the skill silently runs `cat ~/.config/mecatl/settings.yaml`. If a
`models:` block is found it is shown fenced and a final `AskUserQuestion`
asks **use as base** vs **start fresh**. If not found, Step 2 starts
immediately.

After all answers are recorded the skill moves to Step 2 — searching
benchmarks and pricing for OpenRouter models that can reach Opus 4.8 territory,
with a vision alias for the rare-vision requirement.
