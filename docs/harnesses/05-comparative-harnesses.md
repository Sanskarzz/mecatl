---
title: "Comparative Survey — Other Major Coding Agent Harnesses"
doc_id: 05-comparative-harnesses
layer: survey
captured: 2026-05-18
status: stable
keywords: [Aider, repo map, PageRank, tree-sitter, OpenHands, CodeAct, Jupyter kernel, event log, Cline, plan/act, Roo Code, modes, Continue, YAML blocks, Codex CLI, App Server, JSON-RPC, Gemini CLI, SWE-agent, ACI, linter-gated edits, Cursor, Fast Apply, speculative edits, Windsurf, Memories, Devin, Goose, recipes, Copilot Coding Agent, Augment, context engine, Zed, ACP, edit format, context strategy, sandboxing tiers]
answers:
  - "How do the major non-Claude-Code coding agents work (Aider, Cursor, Cline, Codex, OpenHands, Devin, Goose, etc.)?"
  - "What edit formats exist (SEARCH/REPLACE, unified diff, structured + linter gate, fast-apply)?"
  - "What context-discovery strategies exist (agentic grep, symbol-graph repo map, embeddings)?"
  - "What is each harness's single best idea worth stealing?"
related: [04-claude-code-recreations, 06-architecture-patterns, 08-design-considerations]
---

# Comparative Survey: Other Major Coding Agent Harnesses

> Captured: 2026-05-18

## TL;DR

The coding-agent landscape in 2026 has settled into four overlapping shapes. **Terminal CLIs** (Aider, Codex CLI, Gemini CLI, Goose) optimize for a low-friction local loop, BYO-key model flexibility, and tight git integration. **IDE-embedded agents** (Cline, Roo Code, Continue, Cursor Composer, Windsurf Cascade, Zed Agent) live inside the editor process and lean on indexed codebase context, fast-apply edit models, and diff-review UIs. **Sandboxed remote agents** (OpenHands, Devin, GitHub Copilot Coding Agent) trade local latency for clean isolation, parallelism, and async PR-style workflows. **Research harnesses** (SWE-agent) optimize for benchmark performance and have given the field its sharpest ideas about Agent-Computer Interfaces.

The throughlines worth taking seriously: edit format is the most consequential design decision (SEARCH/REPLACE vs unified diff vs fast-apply-by-rewrite vs structured tool call), context strategy is splitting between *agentic discovery* (let the model grep) and *pre-built semantic indexes* (Augment, Cursor, Windsurf), and "plan then act" has converged as a near-universal pattern. The single biggest architectural divergence is *who owns state*: a stateful long-running process (Codex App Server, OpenHands Conversation) versus a stateless request loop (Aider, Cline).

## Taxonomy

A useful framework for slotting any harness:

- **Surface**: CLI / TUI / IDE extension / cloud web UI / GitHub-native (PR-as-interface).
- **Execution locus**: in-process on the user's machine / Docker sandbox on the user's machine / remote VM or container in the cloud.
- **Interaction model**:
  - *Chat-driven* (Aider, Cline, Continue): user sends turn, agent responds, loop.
  - *Plan-then-execute* (Cline plan/act, Devin, Windsurf, Cursor 2.0): explicit two-mode split, optionally with separate models per mode.
  - *Async PR* (Copilot Coding Agent, Devin in async mode): assign work, get a pull request back later.
  - *Inline edit* (Cursor Tab, Continue Edit, Copilot inline): non-agentic single-shot transformations alongside the agent.
- **Model strategy**: BYO-key multi-vendor (Aider, Cline, Continue, Roo, Goose, OpenHands), single-vendor (Codex CLI, Gemini CLI, Copilot Coding Agent), single-vendor with a proprietary frontier model (Cursor Composer, Windsurf, Devin, Augment), multi-model router (Roo Code per-mode, Goose role-based).
- **Edit format**: whole-file rewrite, unified diff, SEARCH/REPLACE block, structured tool call with line ranges, fast-apply (model emits a sketch and a separate fast model materializes the edit), or shell-mediated (`sed`, `patch`, `str_replace_editor`).
- **Context strategy**: agentic file discovery (grep/find as a tool), pre-built embedding index, tree-sitter symbol graph + ranking (Aider), live IDE state (open editor, cursor, recent edits), or runtime "memory" layer.

These axes are not orthogonal — a sandboxed remote agent almost always pairs with shell-mediated editing — but they capture the meaningful design space.

## Per-project profiles

### Aider

- **Surface / locus**: Local terminal CLI; runs in-process on user's machine, operates directly on the user's git working tree.
- **Model strategy**: Aggressively BYO-key. Supports every major provider via LiteLLM. Defaults track the strongest available model per family (Claude, GPT, Gemini, DeepSeek, local Ollama). Picks an edit format per model based on empirical reliability.
- **Agent loop**: Chat-driven. User sends a message; Aider builds a prompt with the repo map plus files explicitly `/add`-ed; LLM returns edits in the chosen format; Aider applies and commits to git. No internal multi-turn planner — the user drives. `architect` mode splits planner LLM from editor LLM.
- **Distinctive idea(s)**:
  1. **The repo map** — tree-sitter parses the whole repo into `def` and `ref` tags, assembled into a multigraph (files as nodes, references as edges), then ranked with *personalized PageRank* where edge weights are boosted by chat-mentioned identifiers (10x), real-looking names (10x), references from already-open files (50x), and downweighted for private or oversaturated names (0.1x). Top-ranked symbols are rendered via `grep_ast.TreeContext` (scoped signatures, elided bodies); a binary search packs as many as fit the token budget. Aider's single biggest unique contribution.
  2. **Atomic git commits per turn**, with AI-authored commits identified in the message — makes diff review and `git revert` first-class.
- **Tool design**: Aider largely does *not* expose tools to the model. The LLM emits edits in textual edit formats and Aider parses/applies them outside the model loop. Deliberate: structured tool calls were less reliable than well-formatted plain text in Aider's own benchmarks circa 2024-2025.
- **Context strategy**: Repo map plus explicit file inclusion. No embeddings, no agentic search.
- **Permissioning / sandboxing**: None — runs as the user. Confirms before shell commands. Relies on git for safety.
- **Multi-agent / subagents**: Architect/editor split only.
- **What to steal**:
  1. Tree-sitter + PageRank repo map — the best non-embedding way to surface a codebase to a model. No API key dependency, no indexing latency.
  2. Edit-format-per-model selection driven by an empirical benchmark suite.
  3. Atomic-commit-per-turn as the unit of work.
- **Tradeoffs / weaknesses**: No long-horizon autonomy, no sandboxing, weak at tasks requiring code execution. Most cognitive load lives in the user's head.

Aider's edit format menu (representative):

```
file.py
<<<<<<< SEARCH
def old_implementation():
    return None
=======
def new_implementation():
    return 42
>>>>>>> REPLACE
```

vs. `udiff` (used to combat GPT-4-Turbo "laziness"):

```diff
--- file.py
+++ file.py
@@ ... @@
-def old_implementation():
-    return None
+def new_implementation():
+    return 42
```

Aider's benchmarks showed `udiff` raising GPT-4-Turbo's pass rate from ~20% to ~61% by reducing elision-with-comments behavior.

### OpenHands (formerly OpenDevin)

- **Surface / locus**: Web UI, headless CLI, SDK (V1, Nov 2025). Local Docker sandbox by default; cloud workspaces optional.
- **Model strategy**: Model-agnostic via LiteLLM. CodeActAgent is the default. Optional security analyzer between agent and execution.
- **Agent loop**: Event-stream. Agent emits an `Action` (typed Pydantic — `CmdRunAction`, `FileEditAction`, `BrowseURLAction`...). `Workspace` executes, returns an `Observation`. Both append to an immutable `EventLog`. Only mutable state is `ConversationState`, which itself mutates by appending events. Deterministic replay, pause/resume, debugging come for free.
- **Distinctive idea(s)**:
  1. **CodeAct**: instead of N bespoke tools with JSON schemas, give the model `bash`, a Python interpreter, and a browser DSL. The thesis: LLMs are better at code than structured function calls.
  2. **Action/Observation as Pydantic types with an append-only log** — trivially replayable and inspectable.
  3. **Opt-in sandboxing** — swap `LocalWorkspace` for `DockerWorkspace` without changing agent code.
- **Tool design**: Three primary tools: `execute_bash`, `execute_ipython_cell`, `browse`, plus `str_replace_editor`. Each session gets a Docker container with SSH, a **long-lived Jupyter kernel** (state survives across tool calls), and a BrowserGym instance.
- **Context strategy**: Agentic discovery via shell. No pre-built index. Pluggable `Condenser` handles compaction.
- **Permissioning / sandboxing**: Docker by default. Network egress controllable per session. Optional pre-execution security analyzer.
- **Multi-agent / subagents**: V1 supports agent-to-agent delegation. Less mature than Goose recipes.
- **What to steal**:
  1. Event-log architecture. Modeling state as `apply(event) -> state` makes replay, telemetry, debugging free.
  2. **Persistent Jupyter kernel** across tool calls collapses dozens of would-be tool calls into one.
  3. Sandbox-as-choice, not ceremony.
- **Tradeoffs / weaknesses**: Heavy. Docker per session has real overhead. `bash`-first stance can blow up context with stdout. Less polished UX than commercial alternatives.

### Cline

- **Surface / locus**: VS Code extension (plus JetBrains, Zed, Cursor, Windsurf, Neovim, preview CLI as of 2026). Extension host process; webview React UI sidebar.
- **Model strategy**: BYO-key, very wide. **Per-mode model selection** — strong reasoner for Plan, fast/cheap for Act.
- **Agent loop**: **Plan/Act split.** In Plan mode, Cline can read/search/discuss but *cannot* write or execute. User toggles to Act; Cline retains plan-mode context and starts modifying/running. Tools are XML-tagged invocations parsed from streaming text. Multi-process: extension host + webview, gRPC between them.
- **Distinctive idea(s)**:
  1. **Plan mode as a hard tool-permission boundary**, not just a system-prompt instruction.
  2. **Per-mode model**: cheap fast model for execution, expensive reasoner for planning.
  3. **`replace_in_file` (SEARCH/REPLACE) + `write_to_file` (full rewrite)** with explicit guidance on when to use each.
- **Tool design**: Small, curated: `read_file`, `write_to_file`, `replace_in_file`, `execute_command`, `search_files`, `list_files`, `browser_action`, plus MCP. Each call requires user approval unless auto-approved per tool.
- **Context strategy**: Agentic via `search_files` (ripgrep), `list_files`, `read_file`. No embedding index. Injects active editor, open tabs, terminal state.
- **Permissioning / sandboxing**: Per-tool, per-action user approval. Auto-approve allowlists. No sandbox.
- **Multi-agent / subagents**: Not built-in beyond plan/act.
- **What to steal**:
  1. Plan/Act as *enforced* tool gating, not prompt etiquette.
  2. Two-model split — cheap model can't plan, expensive model doesn't thrash on edits.
  3. The `replace_in_file` + `write_to_file` doctrine — clean, learnable rules.
- **Tradeoffs / weaknesses**: `replace_in_file` is famously a sharp edge — SEARCH blocks that don't exactly match cause cascading retries. No sandbox.

### Roo Code

- **Surface / locus**: VS Code extension. Fork of Cline.
- **Model strategy**: BYO-key, multi-vendor, per-mode model selection.
- **Agent loop**: Cline's backbone, with **five built-in modes** (Code, Architect, Ask, Debug, Custom) plus community Mode Gallery. Each mode has its own role-definition prompt, tool allowlist, and optional file allowlist.
- **Distinctive idea(s)**:
  1. **Modes as scoped personas with tool/file allowlists.** A "security reviewer" mode can read but not write; an "architect" can plan but not run commands. Generalizes Cline's binary plan/act into N typed roles.
  2. **`.clinerules-[mode]`** files for mode-specific instructions.
  3. Diff-based editing claimed ~30% token savings vs whole-file.
- **Tool design**: Cline's tools; modes constrain which are allowed.
- **What to steal**: Mode = (prompt + tool allowlist + file allowlist + model choice). The cleanest open-source formulation of "agent persona."
- **Tradeoffs / weaknesses**: Fork divergence from Cline. Configuration sprawl risk.

### Continue

- **Surface / locus**: VS Code and JetBrains extensions. CLI shipped 2025.
- **Model strategy**: BYO-key, every major provider. Strong opinion that autocomplete, chat, and agent each use a different model.
- **Agent loop**: Four modes — **Agent** (multi-step), **Chat** (Q&A), **Edit** (inline transform), **Autocomplete**. Tri-process: `core` (TS, agent logic), `extension` (TS or Kotlin), `gui` (React/Redux). Message-passing between them.
- **Distinctive idea(s)**:
  1. **YAML "blocks" and Continue Hub.** Models, rules, prompts, assistants are all YAML blocks referenced by `owner/name` slugs and composed at runtime. The closest the OSS ecosystem has to a package manager for agent config.
  2. **Source-controlled rules**: `.continue/rules` is committed; Hub rules are pinned. Rules can be *CI-enforceable*.
  3. **AGENTS.md/CLAUDE.md auto-discovery** — workspace-root agent files auto-convert to rules.
- **Tool design**: Standard tool set, MCP integration, plus `create_rule_block` — the agent can codify "remember to do X" as a persistent rule.
- **Context strategy**: Mixed — semantic index + agentic search + IDE state.
- **Permissioning / sandboxing**: Per-tool approval. No sandbox.
- **What to steal**:
  1. Config-as-content: shareable blocks with semver-ish slugs eliminate prompt sprawl.
  2. `create_rule_block` — let the agent write its own rules.
  3. CI-enforceable AI rules — "no `console.log`" can also be a lint check.
- **Tradeoffs / weaknesses**: Agent mode historically weaker than dedicated competitors; Continue's strength is the platform.

### Codex CLI

- **Surface / locus**: Local terminal. Rust TUI (`codex-tui`), headless (`codex-exec`), JSON-RPC App Server (`codex-app-server`) for IDE integration. ~95% Rust by early 2026.
- **Model strategy**: Single-vendor (OpenAI). Default GPT-5.4, 272K context, configurable to 1M.
- **Agent loop**: Stateless request-response against the Responses API, but with a **long-lived App Server process** ("Codex core") owning conversation threads. SSE streaming. Aggressive prompt-cache utilization keeps growth roughly linear, not quadratic. Automatic compaction at thresholds.
- **Distinctive idea(s)**:
  1. **App Server with JSON-RPC** — runtime split from any specific client. The same `codex-core` powers TUI, IDE integrations, headless CI, and `codex-mcp-server` (Codex as MCP server exposed to *other* agents).
  2. **Layered sandbox + approval policy.** Sandbox = capability (Seatbelt on macOS; Landlock + seccomp + namespaces on Linux). Approval = when to confirm. Orthogonal. Presets like `--sandbox workspace-write --ask-for-approval on-request` make the matrix tractable.
  3. **OS-native sandboxing as a first-class concern** — `codex debug seatbelt` / `codex debug landlock` test commands through the sandbox without invoking the model.
- **Tool design**: Small shell-first: `apply_patch` (constrained text-edit format), sandboxed shell, MCP. 2026 MCP surface includes resource reads, output schemas, elicitations, file uploads.
- **Context strategy**: Agentic via shell. Huge context window.
- **Permissioning / sandboxing**: Best-in-class among OSS CLIs. Seatbelt/Landlock/seccomp/bwrap all wired up. Network egress policy-controlled.
- **Multi-agent / subagents**: Codex can act as an MCP server.
- **What to steal**:
  1. **Split runtime from interface via JSON-RPC.** Right factoring for any harness expected to grow IDE plugins, CI runners, and CLI.
  2. **Two-axis safety**: sandbox (capability) and approval (gate). Don't conflate them.
  3. OS sandboxing is achievable today. There's no reason a local agent should run with full user privileges by default.
- **Tradeoffs / weaknesses**: Single-vendor. Rust monorepo raises contribution friction.

### Gemini CLI

- **Surface / locus**: Local terminal. TypeScript, Apache 2.0.
- **Model strategy**: Single-vendor (Google). Leans hard on Gemini's 1M+ context.
- **Agent loop**: Explicit **ReAct loop** with tools and MCP. ~12 lifecycle hook points (session start, before plan, before tool, after tool...).
- **Distinctive idea(s)**:
  1. **Hooks at every lifecycle event.** JSON-configured shell scripts run synchronously. The most flexible OSS extensibility surface for a terminal agent: lint before commit, deny dangerous tools, transform prompts, post telemetry.
  2. **Aggressive use of long context** — "context strategy" can be brute-force.
- **Tool design**: Built-in file ops, shell, web fetch, Google Search grounding. MCP for the rest. Tool-name conflicts across MCP servers resolved by auto-prefixing.
- **Context strategy**: Long context + agentic discovery. Less repo-mapping intelligence than Aider.
- **Permissioning / sandboxing**: Tool approvals only. No OS-level sandbox.
- **Multi-agent / subagents**: Experimental agent framework, late 2025.
- **What to steal**:
  1. **Lifecycle hook system** — more general than rules or subagents.
  2. Google Search as a first-class grounding tool with citations.
- **Tradeoffs / weaknesses**: Single-vendor. Less mature sandboxing than Codex.

### SWE-agent

- **Surface / locus**: Research harness. Headless. Designed for SWE-bench.
- **Model strategy**: Multi-vendor.
- **Agent loop**: Single-agent ReAct-ish loop, history processors for context, demonstrations baked into the prompt for ACI usage.
- **Distinctive idea(s)**: **The Agent-Computer Interface (ACI) concept itself.** SWE-agent's enduring contribution: interfaces designed for humans (raw `bash`, raw `cat`, raw editors) are bad interfaces for LLMs. Replacing `cat` with a paginated viewer and `vim` with a constrained edit command produced double-digit SWE-bench gains.
- **Tool design**:
  - `open`, `goto`, `scroll_up`, `scroll_down`, `find_file`, `search_file`, `search_dir`, `edit`.
  - **Window-limited file viewer (~100 lines), persistent line numbers, "current position" tracked across turns.**
  - `edit` takes a line range + replacement. **A linter runs immediately on every edit; invalid edits are *discarded* with the linter error returned to the model.** Massive reliability win.
  - Search returns a *summary* (file → match counts), not raw `grep`.
- **Context strategy**: No index. Agentic discovery via constrained tools. History processors compact the stream.
- **Permissioning / sandboxing**: Docker per task.
- **Multi-agent / subagents**: SWE-agent Multi (follow-up paper) explores teams; v1 is single-agent.
- **What to steal**:
  1. **Reject the human interface.** A paginated viewer with persistent cursor beats `cat`. A line-range `edit` beats `sed`. Summarizing search beats raw `grep`.
  2. **Linter-gated edits** with error returned to model — enormous reliability win, near-zero implementation cost.
- **Tradeoffs / weaknesses**: Built for benchmarks; rough UX for interactive work.

### Cursor (Composer / Agent)

- **Surface / locus**: Closed-source IDE (VS Code fork). Composer runs in-editor and against remote runners.
- **Model strategy**: Multi-model (Claude, GPT, others) plus **Composer**, a proprietary MoE model RL-trained for coding agent workflows. Reportedly 4x faster than peers, sub-30-second turns.
- **Agent loop**: Cursor 2.0 (Nov 2025) is agent-first — the primary interface is multiple agents, not files. Parallel agents via git worktrees or remote VMs. Plan-then-execute is baked into Composer's training, not just the prompt.
- **Distinctive idea(s)**:
  1. **Fast Apply** — a small specialized model takes a coarse edit description and rewrites the file at ~1000 tok/s using a **speculative-decoding variant** ("speculative edits"). Big model thinks; small model executes. For files under ~400 lines, full rewrite beats diff because apply is so fast.
  2. **Agent parallelism via worktrees** — no inter-agent file conflicts.
  3. **Native browser tool** for DOM-level verification.
  4. **Composer as RL-tuned-for-agents** — most harnesses use stock models; Cursor trained one for the loop.
- **Tool design**: Codebase-wide semantic search as a tool. Custom edit tools. Browser. Run.
- **Context strategy**: Live semantic embedding index + IDE state + agentic search.
- **Permissioning / sandboxing**: Remote runners sandboxed; local agents largely not.
- **Multi-agent / subagents**: Parallel agents is the 2.0 headline.
- **What to steal**:
  1. **Planner/applier split** with a fast specialized apply model — the most underrated edit-format insight. Diffs lose to "describe coarsely, rewrite at speed" once you have a fast-apply model.
  2. **Worktree-per-agent** for lock-free parallelism.
  3. Train the model for the harness, not just prompt it.
- **Tradeoffs / weaknesses**: Closed. Composer is Cursor-only. IDE-fork lock-in.

### Windsurf (Cascade)

- **Surface / locus**: Closed IDE (VS Code fork). Built by Codeium, acquired by Cognition in Dec 2025.
- **Model strategy**: Multi-vendor (Claude, GPT, Codeium's own).
- **Agent loop**: Cascade is **continuously aware** — tracks edits, terminal output, clipboard, conversation in real time. Mode-less. A specialized **planning agent** continuously refines the long-term plan while the executor handles short-term actions.
- **Distinctive idea(s)**:
  1. **Fast Context** — proprietary index giving codebase-level awareness without manual file tagging.
  2. **Memories** — learned profile of the developer's patterns, builds over ~48 hours, persists.
  3. **Continuous planning agent** — not a one-shot plan, but one refined as observations arrive.
- **Tool design**: File ops, shell, integrated linter feedback, checkpoint/rollback in UI.
- **Context strategy**: Fast Context index + real-time user activity + Memories.
- **Permissioning / sandboxing**: Per-action approval. No OS sandbox.
- **Multi-agent / subagents**: Planner/executor split.
- **What to steal**:
  1. **Continuously-running planner** distinct from executor — most "plan/act" bakes the plan once.
  2. Treat *user signals* (clipboard, terminal, scroll) as first-class context.
  3. Persisted "Memories" beyond a session.
- **Tradeoffs / weaknesses**: Closed. Index ships codebase to Codeium servers.

### Devin (Cognition)

- **Surface / locus**: Closed cloud product. Agent-native cloud IDE: shell + editor + browser + planner UI. Slack-summonable. PR-output workflow.
- **Model strategy**: Closed; frontier models plus heavy proprietary scaffolding.
- **Agent loop**: Three modes — **planning**, **standard**, **edit**. Planning mode forbids modifications; the job is information-gathering with strict skeptic instructions ("every claim must be backed by file-level evidence with line numbers"). Long-horizon tasks can run for hours.
- **Distinctive idea(s)**:
  1. **Full sandboxed cloud laptop** — shell + VS Code-style editor + Chrome + memory layer, isolated per session.
  2. **Memory and replay** — vectorized codebase snapshots plus a full event timeline of every command, file diff, browser tab. Memory persists across sessions.
  3. **Architectural brain** — explicit planning stage produces a step-by-step development path before execution.
  4. **Skeptic prompt design** — leaked prompt emphasizes "if you don't know, say so" and requires file:line citations.
- **Tool design**: Shell, editor with diff view, browser. Higher-level abstractions like "look up in DeepWiki."
- **Context strategy**: Per-repo memory layer with vectorized snapshots; full event replay.
- **Permissioning / sandboxing**: Full cloud VM isolation. (Notable: Devin has shipped prompt-injection vulns where ports got exposed to the public internet — sandboxing is necessary, not sufficient.)
- **Multi-agent / subagents**: Implied by architecture, not user-visible.
- **What to steal**:
  1. **Skeptic-by-prompt** — cheap, behavior-changing.
  2. **Replayable event timeline as a product surface**, not just internal debugging.
  3. Treat the agent's workspace as a *durable artifact*, not ephemeral per-turn state.
- **Tradeoffs / weaknesses**: Closed. Expensive. Prompt-injection surface is huge (browser + shell + memory amplify).

### Goose

- **Surface / locus**: Local desktop app and CLI. Apache 2.0, donated to Linux Foundation's AAIF in Dec 2025.
- **Model strategy**: 15+ providers via unified interface. **Role-based model assignment** (planning/execution/review models can differ).
- **Agent loop**: Three layers: interface → Rust agent core → extensions (MCP servers). Core is small; everything else is an extension.
- **Distinctive idea(s)**:
  1. **Extensions = MCP servers**, full stop. No built-in tools beyond what MCP provides.
  2. **Recipes** — reusable YAML workflow definitions bundling instructions, extensions, params, provider settings, retry logic, response schemas.
  3. **Subagents** — recipes can spawn subagents (prompts or sub-recipes) in isolated contexts.
  4. **Scheduled tasks** — recipes runnable on cron.
- **Tool design**: Everything is MCP. Core dispatches.
- **Context strategy**: Up to extensions; core imposes nothing.
- **Permissioning / sandboxing**: Per-extension tool approval. No OS sandbox by default.
- **Multi-agent / subagents**: First-class. Recipes call sub-recipes in isolation.
- **What to steal**:
  1. **Recipe as composable unit** = (instructions + extensions + params + provider + retry + response schema). Better than "system prompt" alone for reuse.
  2. **Sub-recipe as subagent** with isolated context, formalized in YAML.
  3. **Pure MCP-native core** — strong principled stance, raises bootstrapping cost.
- **Tradeoffs / weaknesses**: Minimal core means rougher cold-start UX. Inherits MCP's auth/security warts.

### GitHub Copilot Coding Agent

- **Surface / locus**: GitHub-native (issues, PRs). GA Sep 2025. Runs in a GitHub Actions runner spun up on demand.
- **Model strategy**: Multi-model (Claude, GPT, Gemini) via "AgentHQ" routing.
- **Agent loop**: **Async**. Assign an issue; agent spins up a sandboxed Actions runner, plans, executes, opens a draft PR.
- **Distinctive idea(s)**:
  1. **PR is the interface.** No chat surface; unit of work is a PR draft.
  2. **Actions runner as sandbox** — inherits the entire GitHub Actions ecosystem (caching, secrets, matrix, containers).
  3. **Spaces** — named persistent grounding context bundling repos, issues, docs, instructions.
- **Tool design**: Workspace-aware tools inside the runner. Inherits Actions environment.
- **Permissioning / sandboxing**: Strong. Ephemeral runner; GitHub's existing secret model.
- **What to steal**:
  1. **Async-by-PR**: long tasks belong in a queue, not a chat.
  2. Use the platform's existing CI runners as your sandbox.
  3. Spaces as a named, persistent context bundle.
- **Tradeoffs / weaknesses**: GitHub lock-in. Async-only isn't always the right shape.

### Augment Code

- **Surface / locus**: VS Code extension plus MCP server.
- **Model strategy**: Multi-model.
- **Agent loop**: Tool-using agent. Augment's bet is on context, not the loop.
- **Distinctive idea(s)**: **Context Engine.** Semantic search engine over the entire codebase, cross-repo and history. Reports 30-80% quality improvements on other coding agents when plugged in via MCP (Feb 2026). Thesis: indexes win.
- **What to steal**: Exposing your context strategy *as an MCP server* lets other agents consume it. If your differentiator is context, sell it as an MCP.
- **Tradeoffs / weaknesses**: Index lives somewhere — privacy and cost questions.

### Zed Agent

- **Surface / locus**: Native (Rust) GPU-accelerated editor. Agent panel.
- **Model strategy**: Multi-model. Hosted defaults: claude-sonnet-4-5 for agent work, gpt-5-nano for "fast" tasks.
- **Agent loop**: Built around **Agent Communication Protocol (ACP)** — Zed does not run the agent; it hosts *external* agent servers (Claude Code, Codex, Gemini CLI, custom) over stdio JSON-RPC. Zed manages threads and editor integration.
- **Distinctive idea(s)**:
  1. **ACP — the editor isn't the agent.** Out-of-process by design. JetBrains is collaborating on ACP, so the same agent server works in both.
  2. **Two-tier model defaults** — agent model vs "fast" model for commit messages and thread summaries — exposed to the user.
- **What to steal**: **Agent Client Protocol.** If you're building an editor or host, don't bake the agent in. Speak a protocol; let users bring their own. Cleanest separation-of-concerns in the field.
- **Tradeoffs / weaknesses**: Smaller user base. Protocol maturing.

## Comparison matrix

| Project | Surface | Locus | Edit format | Context | Sandbox | Subagents | Open? |
|---|---|---|---|---|---|---|---|
| Aider | CLI | Local | SEARCH/REPLACE, udiff, whole-file (per-model) | Tree-sitter + PageRank repo map | None (git) | Architect/editor split | Apache 2 |
| OpenHands | Web/CLI/SDK | Local Docker | `str_replace_editor` + bash + Python (CodeAct) | Agentic + Condenser | Docker per session | Multi-agent V1 | MIT |
| Cline | IDE | Local | `replace_in_file` (SEARCH/REPLACE) + `write_to_file` | Agentic (ripgrep) | None (per-tool approval) | Plan/Act mode split | Apache 2 |
| Roo Code | IDE | Local | Same as Cline + diff | Agentic | Per-mode tool allowlist | Modes (not concurrent) | Apache 2 |
| Continue | IDE + CLI | Local | Diff + tool call | Semantic index + agentic | Per-tool approval | Composable assistants | Apache 2 |
| Codex CLI | CLI | Local | `apply_patch` text format | Agentic + long context | Seatbelt / Landlock / seccomp | Codex as MCP server | Apache 2 |
| Gemini CLI | CLI | Local | Tool calls | Agentic + huge context | Approval-based | Experimental | Apache 2 |
| SWE-agent | Research CLI | Docker | Line-range `edit` with linter gate | Agentic via custom viewer | Docker | Multi (research) | MIT |
| Cursor | IDE | Local + remote | Fast Apply (rewrite via specialized model + spec-decode) | Semantic index + IDE state | Remote runner | Parallel via worktrees | Closed |
| Windsurf | IDE | Local | Tool call + linter | Fast Context index + real-time + Memories | None | Continuous planner | Closed |
| Devin | Cloud | Remote VM | Editor diff | Vectorized memory + replay | Full VM | Implicit | Closed |
| Goose | Desktop/CLI | Local | MCP tool (no built-ins) | Per-extension | Approval | Recipes + sub-recipes | Apache 2 |
| Copilot Coding Agent | GitHub PR | Actions runner | Tool call | Spaces + repo index | Actions runner | Sub-agents | Closed |
| Augment | IDE + MCP | Local | Tool call | Cross-repo semantic index | Approval | N/A | Closed |
| Zed Agent | Editor | Out-of-process | Provided by ACP agent | Provided by ACP agent | Approval | Multiple connections | GPL + others |

## Themes

**Edit format is a first-class design decision.** Four serious options:

1. **SEARCH/REPLACE blocks** (Aider, Cline, Roo) — model emits literal pre/post state. Reliable when SEARCH matches exactly; brittle when it doesn't.
2. **Unified diff** (Aider udiff) — terser, but model must count line numbers. Wins for stronger models.
3. **Structured edit tool with line ranges + linter gate** (SWE-agent, OpenHands `str_replace_editor`). Strongest reliability story.
4. **Fast-apply** (Cursor) — model emits coarse English; separate small model materializes the diff via speculative decoding at ~1000 tok/s. Requires owning a model.

Match the format to the model: cheap/local models do best with whole-file, frontier models do best with structured tools or fast-apply.

**Repo map / context discovery is the second big lever.** Three camps:

- *Agentic discovery* (Cline, Codex CLI, Gemini CLI, OpenHands, SWE-agent): model uses ripgrep/find/ls. Zero indexing cost, but burns tokens.
- *Symbol graph + ranking* (Aider): cheap, no embedding, surprisingly powerful. The most under-borrowed idea in the field.
- *Semantic embedding index* (Cursor, Windsurf, Augment, Continue): expensive to build but wins on huge codebases. Augment's whole bet.

**Plan/act has converged as a near-universal pattern.** Cline as enforced tool gating, Cursor 2.0 baked into Composer's training, Windsurf as continuous planner, Devin as top-level mode. Throughline: separate the model that thinks about *what* from the model that does it.

**Sandboxing splits into three tiers.** None (most IDE extensions) → per-process OS sandboxing (Codex's Seatbelt/Landlock is the gold standard) → full container/VM (OpenHands, Devin, Copilot Coding Agent). The middle tier — OS-level sandboxing of a local agent — is barely explored outside Codex. No good reason for that.

**Out-of-process agents via protocol** are the clean answer to "should the editor own the agent?" Zed's ACP, Codex's App Server JSON-RPC, and Goose's MCP-everything stance all converge: agent runtime is a separate process speaking a stable protocol; surfaces are clients.

**Persistent Jupyter kernel (OpenHands CodeAct)** deserves more borrowing. A long-lived Python REPL across tool calls collapses dozens of read-inspect-transform-save tool calls into one stream where variables persist. Almost no other harness does this.

**Linter-gated edits are nearly free reliability.** SWE-agent's "discard edit if linter fails, return error to model" is cheap and a measurable benchmark win. Astonishing how few production harnesses do this.

**Hooks as a lifecycle extensibility primitive** (Gemini CLI) are more general than rules, more flexible than subagents.

**Memory beyond a session** (Windsurf Memories, Devin memory layer) is still mostly closed-source. The OSS ecosystem throws everything away per session.

## What Claude Code does that these don't (and vice versa)

**Where Claude Code is ahead:**

- *Skill packaging.* Claude Code Skills (CLAUDE.md, .claude/rules/, .claude/skills/) plus the SKILL.md spec is a more disciplined model for "what should the agent know in this directory" than Continue's blocks or Goose's recipes — closer to the OS-level "right thing in the right place" intuition. Augment's AGENTS.md is converging on this; nobody else is quite there.
- *Subagent discipline.* Claude Code's subagent model has a clean isolation story (subagent gets a fresh context window and a defined toolbox) that beats both Roo's modes (sequential, not concurrent) and Goose's sub-recipes (heavier).
- *Tool ergonomics for an LLM.* Claude Code's tool descriptions and TUI conventions are some of the most LM-friendly in the field, partly because they're co-designed with the model.
- *Hooks system* — comparable to Gemini CLI's, more mature.

**Where others are ahead:**

- *Aider's repo map* — Claude Code's "let the model grep" approach is fine, but a tree-sitter symbol graph with PageRank-ranked summaries is strictly more efficient for first-touch context. Worth adopting as an optional layer.
- *Cursor's Fast Apply* — Claude Code's edit format is fine, but a planner-then-fast-applier split would cut tokens and latency dramatically. This requires owning or fine-tuning a small model, which Anthropic *could* do.
- *Codex CLI's OS-level sandboxing* — Claude Code is dangerously open by default. Seatbelt + Landlock would be net-positive with almost no UX cost.
- *OpenHands CodeAct's persistent Jupyter kernel* — Claude Code's tool calls are stateless; a persistent Python REPL would change the kinds of tasks the model finds easy.
- *Zed's ACP* — Claude Code is currently both runtime and TUI; splitting them via a clean protocol would let third-party clients flourish.
- *Cursor's parallel-agents-via-worktrees* — Claude Code doesn't have a good story for running N agents in parallel without conflicts.
- *Async-by-PR (Copilot Coding Agent)* — Claude Code's chat-loop assumption isn't always right; long tasks belong in a queue with PRs as output.
- *Continue Hub / Goose Recipes* — a registry of shareable skills/agents with semver-ish slugs is a community-scaling primitive Claude Code mostly lacks.

## Reading list

If you read only five things:

1. **SWE-agent NeurIPS paper** (arXiv:2405.15793) — the foundational ACI paper.
2. **OpenHands SDK paper** (arXiv:2511.03690) — V1 architecture, action/observation/event-log.
3. **Aider "Building a better repository map with tree sitter"** (Oct 2023) — the PageRank-repo-map writeup.
4. **OpenAI "Unrolling the Codex agent loop"** (Jan 2026) and **"Unlocking the Codex harness"** — the App Server / runtime-split pattern.
5. **Cursor "Editing Files at 1000 Tokens per Second"** + **"Composer: Building a fast frontier model with RL"** — Fast Apply and the train-the-model-for-the-harness thesis.

Honourable mentions: Cline's annotated system-prompt blog series, Goose's subagents docs, Codex's sandbox docs (Seatbelt/Landlock writeup), EliFuzz/awesome-system-prompts (leaked Devin/Cursor/Windsurf prompts), the JetBrains × Zed ACP announcement, Pragmatic Engineer's "How do AI software engineering agents work?"

---

Primary sources (one anchor per project):

- Aider: [repo map docs](https://aider.chat/docs/repomap.html), [tree-sitter writeup](https://aider.chat/2023/10/22/repomap.html), [unified diff writeup](https://aider.chat/docs/unified-diffs.html)
- OpenHands: [SDK paper arXiv:2511.03690](https://arxiv.org/abs/2511.03690), [ICLR 2025 paper arXiv:2407.16741](https://arxiv.org/abs/2407.16741)
- Cline: [Plan & Act docs](https://docs.cline.bot/core-workflows/plan-and-act), [DeepWiki architecture](https://deepwiki.com/cline/cline/1.3-architecture-overview)
- Roo Code: [GitHub](https://github.com/RooCodeInc/Roo-Code)
- Continue: [rules docs](https://docs.continue.dev/customize/deep-dives/rules), [YAML blocks](https://deepwiki.com/continuedev/continue/5.2-yaml-blocks-and-composition)
- Codex CLI: [agent loop writeup](https://openai.com/index/unrolling-the-codex-agent-loop/), [App Server writeup](https://openai.com/index/unlocking-the-codex-harness/), [sandbox docs](https://developers.openai.com/codex/concepts/sandboxing)
- Gemini CLI: [GitHub](https://github.com/google-gemini/gemini-cli), [hooks on The New Stack](https://thenewstack.io/gemini-cli-gets-its-hooks-into-the-agentic-development-loop/)
- SWE-agent: [NeurIPS paper arXiv:2405.15793](https://arxiv.org/abs/2405.15793), [ACI docs](https://swe-agent.com/latest/background/)
- Cursor: [Composer blog](https://cursor.com/blog/composer), [Instant Apply blog](https://cursor.com/blog/instant-apply), [Fireworks speculative-edits](https://fireworks.ai/blog/cursor)
- Windsurf: [Cascade docs](https://docs.windsurf.com/windsurf/cascade/cascade)
- Devin: [Cognition launch post](https://cognition.ai/blog/introducing-devin), [leaked system prompt](https://github.com/EliFuzz/awesome-system-prompts/blob/main/leaks/devin/archived/2025-08-09_prompt_system.md)
- Goose: [subagents docs](https://goose-docs.ai/docs/guides/subagents/), [extension deep dive](https://dev.to/lymah/deep-dive-into-gooses-extension-system-and-model-context-protocol-mcp-3ehl)
- Copilot Coding Agent: [GA discussion](https://github.com/orgs/community/discussions/159068), [agent mode announcement](https://code.visualstudio.com/blogs/2025/02/24/introducing-copilot-agent-mode)
- Augment: [Context Engine MCP launch](https://www.augmentcode.com/blog/context-engine-mcp-now-live)
- Zed: [Agent Panel docs](https://zed.dev/docs/ai/agent-panel), [JetBrains × Zed ACP announcement](https://blog.jetbrains.com/ai/2025/10/jetbrains-zed-open-interoperability-for-ai-coding-agents-in-your-ide/)

---

**Previous:** [04 · Claude Code Recreations](./04-claude-code-recreations.md) · [↑ Index](./INDEX.md) · **Next:** [06 · Architecture Patterns](./06-architecture-patterns.md)
