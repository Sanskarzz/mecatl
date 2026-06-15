---
title: "Claude Code Recreations and Inspired Harnesses"
doc_id: 04-claude-code-recreations
layer: survey
captured: 2026-05-18
status: stable
keywords: [opencode, crush, claude-code-router, Kode, anon-kode, learn-claude-code, codex, gemini-cli, qwen-code, claw-code, DMCA, source leak, clean-room rewrite, multi-model, AGENTS.md, sandboxing, plugin systems, client/server split, transformers, router pattern]
answers:
  - "Which Claude Code clones, forks, and parallel designs exist and what are they?"
  - "What did every credible recreation KEEP from Claude Code?"
  - "Where did recreations DIVERGE (multi-model, sandbox, plugins, client/server)?"
  - "What does Claude Code do that no recreation has caught up on?"
related: [03-claude-code-architecture, 05-comparative-harnesses, 08-design-considerations]
---

# Claude Code Recreations and Inspired Harnesses

> Captured: 2026-05-18

## Why these exist

Claude Code shipped as a single ~12 MB minified `cli.js` with no source maps, no comments, no readable variable names (renamed to things like `X6`, `K8`, `b6`). It was clearly never meant to be read. But "minification isn't obfuscation," as Martin Alderson noted in his deobfuscation write-up: identifiers like `I`, `Z`, `P9`, `tO` map cleanly back to their original purpose with `grep` plus a model that can do the renaming work for you. Within months of Claude Code's release, the community had a credible reconstruction of its tool definitions, system prompt, and main loop — much of it published as `Piebald-AI/claude-code-system-prompts`, which now tracks the prompt across every Claude Code version ([Piebald-AI/claude-code-system-prompts](https://github.com/Piebald-AI/claude-code-system-prompts)).

The first wave of forks (early 2025) was driven by a single grievance: Claude Code is locked to Anthropic models. `dnakov/anon-kode` (Daniel Nakov) was the most visible — a TypeScript fork that swapped in OpenAI-style endpoints so you could "kode with any LLMs." It got DMCA'd in March 2025 along with 437 forks ([github/dmca 2025-03-10-anthropic.md](https://github.com/github/dmca/blob/master/2025/03/2025-03-10-anthropic.md)). Anthropic's complaint cited the parent `dnakov/claude-code` (the deobfuscated archive), not anon-kode's modifications, but the fork network was killed as a unit.

The DMCA didn't slow the field. It just pushed the next wave away from direct forks toward clean-room rewrites and parallel designs. By mid-2025 a stable cohort had emerged:

- **`sst/opencode`** (now under `anomalyco`) — a TypeScript-and-Go client/server rewrite with provider-agnostic everything. The category leader by stars.
- **`charmbracelet/crush`** — a Go-native, Bubble Tea-powered terminal agent from the Charm team, originally launched as `mods` then rebranded to Crush.
- **`musistudio/claude-code-router`** — not a fork but a *proxy* that intercepts Claude Code's traffic and routes it to cheaper providers. Lets you keep the official client and pay less.
- **`shareAI-lab/Kode`** — a TypeScript fork of anon-kode that survived, now Apache-2.0, "AGENTS.md-compatible", with multi-model collaboration baked in.
- **`shareAI-lab/learn-claude-code`** and **`shareAI-lab/mini-claude-code`** — pedagogical reimplementations that strip the harness to its core.

A separate **parallel-design family** existed independently and got compared constantly:

- **`openai/codex`** — OpenAI's Rust CLI, released April 2025, ~95% Rust by line count, with OS-level sandboxing (Seatbelt/Landlock/seccomp).
- **`google-gemini/gemini-cli`** — Google's TypeScript answer, ReAct-loop based, Apache-2.0.
- **`QwenLM/qwen-code`** — Alibaba's CLI, openly forked from Gemini CLI with Qwen3-Coder-specific tweaks.
- **`paul-gauthier/aider`** — predates Claude Code; git-first, tree-sitter repo maps, no subagents.

Finally, the **March 2026 source map leak** changed everything. Anthropic accidentally published a 59.7 MB source map with v2.1.88 on npm, exposing ~1,884 TypeScript files and ~512,000 lines of unobfuscated source ([InfoQ writeup](https://www.infoq.com/news/2026/04/claude-code-source-leak/)). The archive was on GitHub within hours. `claw-code` (Sigrid Jin, `@sigridjineth`) appeared shortly after as a clean-room Python/Rust rewrite and hit 50,000 stars in two hours — claimed to be the fastest accumulation in GitHub history ([cybernews](https://cybernews.com/tech/claude-code-leak-spawns-fastest-github-repo/)).

What follows is a profile of each project, what they kept from Claude Code, what they replaced, and what's worth stealing.

## Project profiles

### sst/opencode — the category-leading clean-room recreation
- **Repo:** [github.com/sst/opencode](https://github.com/sst/opencode) (canonical mirror at `anomalyco/opencode` after the SST team spun out Anomaly Innovations)
- **Language / runtime:** TypeScript (server + plugins) and Go (CLI/TUI). Bun and Go cohabit. Plugin system is JS/TS.
- **Lineage:** Clean-room. Not a direct fork — built from observation of Claude Code's behavior and from the public Anthropic Agent SDK shape. The architecture diverges significantly: client/server split rather than monolithic CLI.
- **Model support:** ~75+ providers via Models.dev including Claude, GPT, Gemini, DeepSeek, Groq, OpenRouter, AWS Bedrock, Vertex AI, plus full local-model support through Ollama and any OpenAI-compatible endpoint. BYOK is the default; OpenCode also sells subscription tiers ("Go" $10/mo, "Black" $200/mo) but they're optional ([OpenCode docs/models](https://opencode.ai/docs/models/)).
- **Agent loop:** Server-side orchestration in `SessionPrompt.loop()`. The server (`opencode serve`) handles AI interactions and persists session state to SQLite via Drizzle ORM. Multiple clients (TUI, desktop, web, ACP-over-stdio for Zed) connect and observe via a reactive permission system that interrupts the loop and waits on client approval. Compaction is a separate agent with its own system prompt (`PROMPT_COMPACTION`) — invoked when token usage exceeds `context_window - 32,000 (reserved output) - 20,000 (safety buffer)`. Tool output pruning kicks in past 40,000 tokens but the last two turns are protected ([DeepWiki: Context Management](https://deepwiki.com/sst/opencode/2.4-context-management-and-compaction)).
- **Tool set:** Built-in: `bash`, `read`, `write`, `edit` (exact-string-replacement), `glob`, `grep`, `task` (todos), plus web tools. Pluggable via the [Agent Skills](https://agentskills.io) spec (same one Claude/Anthropic released) and MCP (stdio, HTTP, SSE).
- **Notable divergences:**
  - **Client/server architecture** is the biggest break from Claude Code. It enables remote Docker execution, persistent "Workspaces" that survive a laptop close, and editor integration via the Agent Client Protocol (ACP).
  - **Agent modes are first-class.** `build`, `plan`, `general`, `explore` — `plan` denies edit tools except inside `.opencode/plans/*.md`, `explore` is read-only.
  - Uses Vercel's AI SDK as the LLM abstraction, which means almost every provider already has a working adapter.
- **What to steal:**
  1. **Persist sessions to a database, not just a file.** Drizzle + SQLite means resuming, branching, and cross-process inspection just work.
  2. **Reserved-output + safety-buffer compaction math.** Don't wait until you hit the context limit — pre-budget and trigger a summarizer agent.
  3. **Tool output pruning that preserves the last N turns**, not just sliding-window truncation. Older tool blobs lose details; recent ones stay intact.
- **Maturity:** ~150-160K stars by mid-2026 ([Saiyam Pathak Substack](https://saiyampathak.substack.com/p/opencode-just-overtook-claude-code) reports OpenCode passed Claude Code on stars in May 2026 at ~157k vs ~122k). 850+ contributors. Daily commits. MIT licensed. Strongest example in this entire field.

### charmbracelet/crush — the Go-native, terminal-aesthetic agent
- **Repo:** [github.com/charmbracelet/crush](https://github.com/charmbracelet/crush)
- **Language / runtime:** Go, single static binary across macOS, Linux, Windows (PowerShell + WSL), FreeBSD, OpenBSD, NetBSD.
- **Lineage:** Parallel design, not a recreation. Charm has been building TUI libraries (Bubble Tea, Lipgloss, Glamour) for years; Crush is their agent on top of that stack. The author Andrey Petrov and the Charm team built it with awareness of Claude Code but with their own opinions.
- **Model support:** Charm-curated via [Catwalk](https://github.com/charmbracelet/catwalk), their open provider database that auto-updates. Anthropic, OpenAI, Vercel AI Gateway, Gemini, Groq, OpenRouter, Bedrock, Azure, Z.ai, MiniMax, Cerebras, plus any OpenAI-compatible or Anthropic-compatible endpoint. Ollama and LM Studio for local.
- **Agent loop:** Conventional `while(tool_use)` loop in Go. Mid-session model switching is the headline feature: change LLM without losing context. LSP integration (gopls, rust-analyzer, pyright) feeds symbol tables and diagnostics directly into the agent's view of the codebase.
- **Tool set:** `view`, `ls`, `grep`, `edit`, `bash`, plus MCP servers (stdio, HTTP, SSE). Per-tool permission allowlists in `crush.json`. `--yolo` skips all prompts.
- **Notable divergences:**
  - **LSP-as-tool.** The agent literally calls `gopls` like a human would — it's not just for syntax highlighting. This is the cleanest answer to "how does the agent understand the codebase" that I've seen.
  - **Shell-style value expansion** in config: `"$(cat /path/to/token)"`, `${VAR:?error}`. They commit hard to bash semantics even on Windows.
  - **Builtin `crush-config` skill** — you can literally tell Crush to configure itself.
  - Respects the Agent Skills spec, reads `~/.claude/skills/` as a fallback. Same data, no lock-in.
- **What to steal:**
  1. **LSP integration as a first-class tool, not an editor concern.** This is genuinely missing from most other agents and is doing real work.
  2. **External, community-curated model registry** (Catwalk). Crush ships with a snapshot, then pulls updates from one repo. Means the agent always knows about new models without a release.
  3. **Same config file, multiple discovery paths.** Reading `.claude/skills`, `.cursor/skills`, `.crush/skills`, `.agents/skills` is unglamorous interop that lowers adoption friction.
- **Maturity:** Active, single-binary release each ~week, FSL-1.1-MIT license (becomes MIT after two years). Charm team is full-time. Stars in the tens of thousands.

### musistudio/claude-code-router — the proxy, not a fork
- **Repo:** [github.com/musistudio/claude-code-router](https://github.com/musistudio/claude-code-router)
- **Language / runtime:** TypeScript / Node.js. Monorepo with `cli`, `server`, `shared`, `ui` packages.
- **Lineage:** Not a fork at all. It's an `ANTHROPIC_BASE_URL`-targeted proxy that intercepts Claude Code's outbound API calls and routes them to other providers, with request/response transformers per provider.
- **Model support:** OpenRouter, DeepSeek, Ollama, Gemini (via official API or experimental `gemini-cli` transformer), Volcengine, SiliconFlow, ModelScope, DashScope, AIHubMix. Multi-route: `default`, `background`, `think`, `longContext` (>60k tokens), `webSearch`, `image`.
- **Agent loop:** N/A — the agent loop is Claude Code's. CCR sits between Claude Code and the provider, translating message shapes. The `Anthropic` transformer is the no-op passthrough.
- **Tool set:** N/A — CCR doesn't have tools. But the *transformers* are pluggable: `deepseek`, `gemini`, `openrouter`, `groq`, `maxtoken`, `tooluse`, `reasoning`, `sampling`, `enhancetool`, `cleancache`, `vertex-gemini`. You can register custom transformers as `.js` files.
- **Notable divergences:**
  - **Routing is conditional on the request.** Big context → bigger model. Background tasks → local Ollama. The `longContextThreshold: 60000` is a sensible default.
  - **`<CCR-SUBAGENT-MODEL>provider,model</CCR-SUBAGENT-MODEL>`** prefix in a subagent's prompt overrides the model for that subagent. Slick way to retrofit per-subagent model selection onto Claude Code.
  - GitHub Actions integration is first-class: run Claude Code in CI but pay DeepSeek prices.
- **What to steal:**
  1. **The router pattern itself.** If you're building a harness, you don't always need to own the UI — sometimes you just want to redirect a popular client's traffic.
  2. **Per-task routing rules** (background, think, longContext, webSearch). Not every step needs the smartest, most expensive model. This is real money saved.
  3. **Transformers as plug-in JavaScript files.** Loading user-authored transformers via `config.transformers[].path` is a clean way to keep provider-specific quirks out of the core.
- **Maturity:** ~26k+ stars by Feb 2026, very active, MIT licensed. Sponsored by Z.ai (GLM Coding Plan). A pragmatic project run by one developer plus the community.

### shareAI-lab/Kode — anon-kode's successor with multi-model collaboration
- **Repo:** [github.com/shareAI-lab/Kode](https://github.com/shareAI-lab/Kode)
- **Language / runtime:** TypeScript / Bun, npm-distributed as `@shareai-lab/kode`. Native binaries for Windows since Dec 2025.
- **Lineage:** Acknowledged successor to dnakov's anon-kode (the README says "Some code from @dnakov's anonkode"). Adds UI ideas from gemini-cli. Apache-2.0 license.
- **Model support:** Any OpenAI- or Anthropic-compatible endpoint. Multi-model is the headline feature: configure a "main", "task" (subagent), "compact" (compaction), and "quick" model independently. Switch with `Option+M`.
- **Agent loop:** Inherits the Claude Code loop structure. Adds:
  - **`AskExpertModel`** tool — temporarily consult a different model mid-conversation, isolated from main context.
  - **`TaskTool`** ("Architect") for subagent spawning that accepts a model parameter.
  - **`@ask-<model>`** and **`@run-agent-<agent>`** mentions in the prompt for ad-hoc routing.
- **Tool set:** Read, Write, Edit, Grep, Bash, Task (subagent), AskExpertModel, plus MCP (stdio + SSE) and Agent Skills. AGENTS.md-compatible, but also reads `.claude/`, `CLAUDE.md`.
- **Notable divergences:**
  - **YOLO mode is the default.** This is genuinely unusual — most clones default to safe. The README explicitly says use `--safe` for serious work.
  - **Linux bwrap sandbox** when in safe mode, network disabled by default, `KODE_SYSTEM_SANDBOX=required` for fail-closed.
  - **YAML-shareable model profiles** with `kode models export/import` — the team-config sharing problem solved.
- **What to steal:**
  1. **Four model pointers** (main/task/compact/quick) is the right shape. Most tools either pick one or expose 20 dropdowns. Four-with-defaults is the sweet spot.
  2. **`AskExpertModel` as a tool, not a config switch.** The agent itself can decide to consult a smarter model on a hard subproblem, then come back. Nicely model-driven.
  3. **YAML model-config export/import.** Teams need this and almost nobody ships it.
- **Maturity:** Apache-2.0, active. Has its own sister SDK (`shareAI-lab/Kode-agent-sdk`) for embedding agent capabilities in non-CLI runtimes (browser extensions, backends). Trending in Trendshift.

### shareAI-lab/learn-claude-code — the pedagogical recreation
- **Repo:** [github.com/shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)
- **Language / runtime:** Python. 12 sessions (`s01` through `s12`) plus an `s_full` capstone.
- **Lineage:** Educational. Not a production product. The author's thesis is the most important: "Agency comes from the model. An agent product = model + harness. This repo teaches you how to build the vehicle."
- **Model support:** Anthropic API directly (each session is `client.messages.create`).
- **Agent loop:** The cleanest reference implementation you'll find. From the README:

  ```python
  def agent_loop(messages):
      while True:
          response = client.messages.create(
              model=MODEL, system=SYSTEM,
              messages=messages, tools=TOOLS,
          )
          messages.append({"role": "assistant",
                           "content": response.content})
          if response.stop_reason != "tool_use":
              return
          results = []
          for block in response.content:
              if block.type == "tool_use":
                  output = TOOL_HANDLERS[block.name](**block.input)
                  results.append({
                      "type": "tool_result",
                      "tool_use_id": block.id,
                      "content": output,
                  })
          messages.append({"role": "user", "content": results})
  ```

  Every session layers on one mechanism without changing this loop.
- **Tool set:** Progressively built. `s01`: just `bash`. `s02`: read/write/edit/grep. `s03`: + `TodoWrite`. `s04`: + `Task` (subagents). `s05`: + `Skill` loading. `s07`: + persistent task graph. `s09`: + agent team mailboxes. `s12`: + git worktree isolation.
- **Notable divergences:**
  - **It's a tutorial, not a tool.** Use it to understand the design space, not as a daily driver.
  - Explicitly opinionated against "prompt plumbing": "Drag-and-drop workflow builders, no-code 'AI agent' platforms — they all share the same delusion: that wiring together LLM API calls with if-else branches constitutes 'building an agent.' It doesn't."
- **What to steal:**
  1. **The 12-mechanism breakdown.** Loop → tools → planning → subagents → skill loading → compaction → tasks → background → teams → protocols → autonomy → worktree isolation. That's a checklist for any harness.
  2. **The mantra "the model is 80%, code is 20%"** — visible in the sibling repo `mini-claude-code`. A useful corrective when you're tempted to add another control flow.
  3. **Worktree isolation for parallel subagents (s12).** Each subagent gets its own git worktree, bound to a task ID. No cross-interference.
- **Maturity:** MIT, actively maintained. Trendshift-listed. Pairs with the production `shareAI-lab/Kode` CLI as the "ship it" companion.

### openai/codex — the parallel design from OpenAI
- **Repo:** [github.com/openai/codex](https://github.com/openai/codex)
- **Language / runtime:** Rust (~95%), distributed via npm (`@openai/codex`) and Homebrew. Single static binary per platform.
- **Lineage:** Parallel design. Released April 2025. Apache-2.0. Originally a TypeScript implementation, fully rewritten in Rust by mid-2025 for performance and sandboxing.
- **Model support:** OpenAI (ChatGPT plan login OR API key). Recent versions (GPT-5.2-Codex, Dec 2025) introduce context compaction for long-horizon work. Codex is OpenAI's own model in the loop — not designed to be model-agnostic, though it works as both an MCP client and MCP server, which lets you compose it with other agents.
- **Agent loop:** Standard ReAct-style: gather history → send to LLM with tools → execute tool calls → loop. The Rust rewrite is built around a strongly-typed protocol crate (`codex-rs/protocol`) with a core engine in `codex-rs/core`.
- **Tool set:** File I/O, shell (`apply_patch`-style edits), MCP integration. Codex pioneered the `AGENTS.md` convention that the entire field has now standardized on (Kode, OpenCode, Crush all support it).
- **Notable divergences:**
  - **Three sandbox modes:** `read-only` (default), `workspace-write` (writes to workspace, no network), `danger-full-access`.
  - **OS-level sandboxing**: Apple Seatbelt on macOS, Bubblewrap + Landlock + Seccomp on Linux. The Linux helper is its own binary, `codex-linux-sandbox`. This is the most rigorous sandbox in the field.
  - **`.git` and `.codex` are read-only in workspace-write** by default, even though the rest of the cwd is writable.
  - **Both client and server for MCP** — `codex mcp-server` exposes Codex as a tool to other agents. None of the Claude Code recreations do this yet.
- **What to steal:**
  1. **OS-level sandboxing.** Seatbelt/Landlock/Seccomp is heavier engineering than a Docker container but fewer moving parts at runtime and faster startup. The right answer for a CLI.
  2. **`AGENTS.md` convention.** Now portable across Codex, Kode, OpenCode, Crush, Gemini CLI. If you're building a new harness, support it.
  3. **`codex sandbox <platform> <cmd>` debug subcommand.** Test your sandbox config without running the agent. This is the kind of operability detail other projects skip.
- **Maturity:** ~72-76K stars by early 2026, 700+ releases, daily commits, Apache-2.0. The single most stable parallel design.

### google-gemini/gemini-cli — Google's answer, ReAct-loop-first
- **Repo:** [github.com/google-gemini/gemini-cli](https://github.com/google-gemini/gemini-cli)
- **Language / runtime:** TypeScript / Node.js. Apache-2.0.
- **Lineage:** Parallel design. Released mid-2025.
- **Model support:** Gemini family only (Gemini 3 with 1M context window) but with three auth flows: Google OAuth (free tier 60 RPM / 1000/day), Gemini API key, Vertex AI.
- **Agent loop:** Two-package split — `packages/cli` (frontend) and `packages/core` (backend). Core constructs prompts, manages history, dispatches tools. Explicitly described as "ReAct" ([Cloud docs](https://cloud.google.com/gemini/docs/codeassist/gemini-cli)).
- **Tool set:** File operations, shell, web fetch + built-in Google Search grounding, MCP integration (`@github`, `@slack`, `@database` etc. via mentions). Conversation checkpointing for resuming sessions. `GEMINI.md` context files (their `CLAUDE.md` equivalent).
- **Notable divergences:**
  - **Google Search grounding is built-in**, not bolted on. The agent can cite real-time information from search without an MCP server.
  - **Multi-agent Hub-and-Spoke** architecture introduced in late 2025: main session is the Hub, sub-agents are Spokes ([blog: subagents tutorial](https://medium.com/google-cloud/mastering-gemini-cli-subagents-part-1-a4666091c154)).
  - **Trusted folders policy** controls execution by folder, more granular than global allowlists.
  - **JSON output format** (`--output-format json`) and **stream-JSON** (`--output-format stream-json`) for headless / CI use.
- **What to steal:**
  1. **`--output-format stream-json`.** Newline-delimited JSON events. Easy to consume from a parent process. Most other CLIs don't expose their event stream nicely.
  2. **Trusted-folders policy.** Different security postures per directory is much more useful than one global allowlist.
  3. **Conversation checkpointing**, especially explicit save/resume. Most agents have implicit history; Gemini CLI makes it a command.
- **Maturity:** Apache-2.0, active. Has its own GitHub Action (`google-github-actions/run-gemini-cli`). Backed by a full Google team.

### QwenLM/qwen-code — Gemini CLI fork tuned for Qwen3-Coder
- **Repo:** [github.com/QwenLM/qwen-code](https://github.com/QwenLM/qwen-code)
- **Language / runtime:** TypeScript, Apache-2.0. Installed as `@qwen-code/qwen-code`.
- **Lineage:** Direct fork of `google-gemini/gemini-cli` with parser enhancements and tool support specifically for Qwen3-Coder (480B-parameter MoE, 35B active). Released July 2025.
- **Model support:** Qwen3-Coder default via Alibaba Cloud Model Studio, but also any OpenAI-compatible endpoint, plus Anthropic and Google.
- **Agent loop:** Inherits Gemini CLI's ReAct loop more or less unchanged. Modifications are concentrated in the parser (Qwen-specific tool-call format quirks) and tool support layer.
- **Tool set:** Inherits Gemini CLI's: file ops, shell, web fetch, MCP.
- **Notable divergences:** Mostly minimal. The interesting thing about Qwen-code is what it *demonstrates*: that the Gemini CLI architecture is general enough to host a different model family with localized parser changes. It's evidence that the agent loop is portable across model providers.
- **What to steal:** The lesson, not the code. Build your parser layer cleanly enough that swapping models doesn't require touching the loop.
- **Maturity:** Active, Apache-2.0, backed by Alibaba Cloud's Qwen team. Mid-tier stars by mid-2026.

### claw-code (Sigrid Jin / instructkr) — post-leak clean-room rewrite
- **Repo:** Originally [github.com/AI-App/InstructKr.Claw-Code](https://github.com/AI-App/InstructKr.Claw-Code), with parity work at `ultraworkers/claw-code-parity` (Rust port). The original repo has been moving between organizations.
- **Language / runtime:** Python first, now being rewritten in Rust ("using oh-my-codex").
- **Lineage:** Clean-room rewrite. Sigrid Jin (`@sigridjineth`), reportedly the most active Claude Code user worldwide (>25B tokens processed, per WSJ), wrote claw-code by *reconstructing* the harness patterns from observable behavior and from public analyses of the deobfuscated source — explicitly not redistributing Anthropic's source. The repo hit 50K stars in two hours after publication in April 2026, per multiple reports.
- **Model support:** Any provider via the harness — the design point is that the harness is what's interesting, the model is a parameter.
- **Agent loop:** Replicates the Claude Code patterns: query engine, permission-gated tool system (19+ tools), memory management, multi-agent orchestration. Not a verbatim port — patterns only.
- **Tool set:** Mirrors Claude Code's: ~19 tools covering read/write/edit/glob/grep/bash/task/web/skill etc. Permission gating per tool.
- **Notable divergences:**
  - **Legal posture.** Explicit clean-room. The same defense Wine and ReactOS use. Whether it holds against a determined Anthropic plaintiff is undecided; nobody has tested it yet.
  - **Rust port already in flight**, suggesting the same trajectory as Codex (TS → Rust).
- **What to steal:**
  1. **The clean-room discipline itself.** Read the deobfuscated source if you want, but don't copy it. Reimplement from your understanding, in a different language ideally.
  2. **160K stars in weeks** says there's enormous appetite for a Claude-Code-shaped agent that's actually open. If you build one, the market is there.
- **Maturity:** Hard to assess. The repo has moved between orgs; the parity-Rust fork is active; the original "fastest GitHub repo in history" claim is repeated in many places but accurate stargazer counts are noisy because the repo keeps being re-archived. Treat as influential but unstable as a codebase.

### Honorable mentions and historical comparisons

- **`paul-gauthier/aider`** (predates Claude Code by years): git-first, no subagents, tree-sitter repo map. Per recent benchmarks ([morphllm](https://www.morphllm.com/comparisons/morph-vs-aider-diff)), Aider uses ~4.2x fewer tokens than Claude Code per equivalent task because it doesn't load whole files into context — it loads structure first, code on demand. Not a Claude Code recreation; a credible competitor with different tradeoffs.
- **`smol-ai/smol-developer`** (mid-2023): the spiritual precursor to all of this. A few hundred lines of Python that gave an agent a tool, a loop, and a project description. Historically interesting because it was the first widely-shared "what if a model just wrote the whole repo?" experiment. Now thoroughly obsoleted, but worth reading for the shape.
- **`dnakov/anon-kode`** (DMCA'd March 2025): the original Claude Code fork. Lives on via `shareAI-lab/Kode` which incorporated its code with attribution. Has secondary forks like `WilliamAGH/anon-kode` that survive but are not actively developed.

## Cross-project comparison table

| Project | Lang | Multi-model | Local models | Plan mode | Subagents | Hooks | MCP | Sandboxing |
|---------|------|-------------|--------------|-----------|-----------|-------|-----|------------|
| sst/opencode | TS + Go | Yes (75+) | Ollama, any OpenAI-compat | Yes (`plan` agent) | Yes (`general`, `explore`) | Plugin lifecycle (25+ hooks) | Yes (stdio/HTTP/SSE) | Permission system + remote Docker exec |
| charmbracelet/crush | Go | Yes (Catwalk-curated) | Ollama, LM Studio | No (use allowlists) | No native | Preliminary | Yes (stdio/HTTP/SSE) | Permission allowlists; no OS sandbox |
| musistudio/claude-code-router | TS | Yes (proxy-level) | Ollama | Inherits Claude Code's | Inherits Claude Code's | Inherits Claude Code's | Inherits Claude Code's | Inherits Claude Code's |
| shareAI-lab/Kode | TS | Yes (4 pointers + AskExpertModel) | Any OpenAI-compat | Yes | Yes (Task tool) | Plugin marketplace | Yes (stdio/SSE) | bwrap (Linux), safe mode |
| shareAI-lab/learn-claude-code | Python | Anthropic only (it's a tutorial) | No | s03 (TodoWrite) | s04, s09 | Minimal event stream | No | No |
| openai/codex | Rust | OpenAI only | No (cloud-only) | No explicit | Yes | Yes | Yes (client + server) | Seatbelt / Landlock / Seccomp (best in class) |
| google-gemini/gemini-cli | TS | Gemini only | No | No | Yes (Hub-and-Spoke) | Custom commands | Yes | Trusted-folders policy |
| QwenLM/qwen-code | TS | Yes (any OpenAI-compat) | No (model-side) | Inherits Gemini CLI | Inherits Gemini CLI | Inherits Gemini CLI | Inherits Gemini CLI | Inherits Gemini CLI |
| claw-code | Python → Rust | Yes (provider-agnostic by design) | Yes | Yes (claimed) | Yes | Claimed | Claimed | Claimed |

## Patterns that survived the recreation

Looking across these projects, a small set of Claude Code design choices show up in every credible recreation. These are the load-bearing ideas:

1. **One loop, many tools.** The `while(tool_use)` loop with stop_reason inspection is universal. Nobody got cute and replaced it with a state machine or DAG. learn-claude-code's mantra `bash is all you need` is half a joke and half a serious architectural claim: agency is the model, the loop is trivial, complexity belongs in the tools.

2. **A small, opinionated tool set as the core.** Read, Write, Edit (exact-string-replacement), Bash, Glob, Grep, Task/TodoWrite. Maybe WebFetch. That's it. Every project lands within one or two tools of this list. The exact-string-replacement edit tool is universal — nobody uses unified diffs as the primary edit mechanism (despite Aider's success with them, the Claude-Code-shaped agents all picked exact replacement).

3. **TodoWrite or equivalent.** Every recreation has a planning tool. Sometimes it's called Task, sometimes Todo, sometimes Plan. The pattern of "write down the plan as a tool call, then execute it" survived intact. Per Claude Code's own system prompt (~2,037 tokens just for TodoWrite per Piebald-AI's analysis), the model is heavily trained to use this. The recreations don't fight that — they expose the same surface.

4. **Subagents with fresh context.** When a task is too big or too noisy, spawn a child agent with empty `messages[]`. OpenCode's `general`, Kode's TaskTool, Gemini CLI's Hub-and-Spoke, learn-claude-code's s04 — all the same idea.

5. **`AGENTS.md` / `CLAUDE.md` discovery from CWD up to repo root.** This started as `CLAUDE.md` in Claude Code, was generalized as `AGENTS.md` by Codex, and is now read by Kode, OpenCode, Crush, Gemini CLI (`GEMINI.md`), and qwen-code. The convention won.

6. **MCP everywhere.** Anthropic's protocol is now the cross-project standard. Every credible Claude Code recreation supports stdio MCP, most support HTTP and SSE. This is one of Anthropic's most successful pieces of standards work — the recreations didn't roll their own.

7. **Per-tool permissions, not blanket allow/deny.** All projects expose tool allowlists (`allowed_tools` in Crush, `permissions` in OpenCode, `--safe` in Kode). The dial isn't on/off — it's per-tool.

8. **Compaction as a separate agent call.** When context fills up, summarize via a *different* LLM call with a `PROMPT_COMPACTION`-style system prompt, then replace history with the summary. OpenCode, Kode (`compact` pointer), learn-claude-code (s06) all do this. Nobody tries to fit compaction into the main loop.

## Patterns that diverged

A few places where the recreations consistently picked something different from Claude Code:

1. **Multi-model is the default.** Claude Code is Anthropic-only by design (commercial reality). Every recreation supports multiple providers, and most expose `main`/`task`/`compact`/`quick` pointers so different parts of the agent run different models. This is the single biggest divergence and the single biggest reason these projects exist.

2. **OS-level sandboxing is contested.** Codex went hard with Seatbelt/Landlock/Seccomp. Kode uses `bwrap` opt-in. Crush and OpenCode rely on permission prompts. Nobody has settled this — the engineering cost of OS-level sandboxing is real, and the alternative (ask the user before each bash command) works well enough for most users.

3. **Plugin systems instead of monolithic tools.** Claude Code historically baked tools into the binary. OpenCode has a plugin lifecycle with ~25 hooks. Crush has skills + MCP. CCR has transformers. Kode has plugin marketplaces. The recreations all bet that extensibility is the killer feature and exposed it more aggressively than Claude Code did.

4. **Client/server split.** OpenCode pioneered it (server holds state, multiple clients connect). Crush is monolithic. Codex is monolithic. Gemini CLI has a soft split (cli + core packages, but same process). The market hasn't decided, but the trend is toward split as remote-execution and multi-frontend (TUI / desktop / web / editor-via-ACP) becomes table stakes.

5. **`AGENTS.md` over `CLAUDE.md`.** Open projects standardized on `AGENTS.md` (Codex's convention) for portability. Claude Code itself still uses `CLAUDE.md`. Most recreations read both as a kindness.

6. **Hooks are uneven.** Claude Code has rich pre/post-tool-use hooks. OpenCode has them. Crush has "preliminary support." Codex has them. Gemini CLI doesn't really. learn-claude-code intentionally omits them as out-of-scope for teaching. The shape of "what is a hook" still varies a lot.

## What's missing in the recreations

Things Claude Code does that none of the recreations have caught up on:

1. **Background long-running tasks ("AutoDream").** The Claude Code source leak (April 2026) exposed references to a feature called "AutoDream" — background or scheduled autonomous operation, plus a "Coordinator" mode for multi-agent orchestration and an "Ultra" subscription plan. None of these are in any recreation yet. Closest is `claw0` (shareAI-lab's sibling project) with heartbeat/cron, but it's a teaching repo.

2. **Coordinator mode.** Multi-agent orchestration at the harness level (not just spawning a subagent and waiting). The leaked code references this; no recreation has fully implemented it.

3. **Tool descriptions tuned to the model.** Claude Code's bash tool description is 1,558 tokens and includes specific behavior around git commits and pre-commit hooks; TodoWrite is 2,037. These are model-specific. The recreations use generic descriptions. Hard to fix unless you co-train the model.

4. **Statusline, magic docs, security review, agent creation as first-class subsystems.** Per Piebald-AI's prompt collection, Claude Code has dedicated prompts for each. Recreations re-implement statusline and a couple others, but not the whole set.

5. **Same-process IDE integration via ACP.** OpenCode has ACP, but Claude Code's tight integration with Anthropic's web UI / mobile app is harder to replicate for a community project.

6. **The training side.** As learn-claude-code emphasizes, the model is doing 80% of the work, and Anthropic trains Claude specifically to be an agent. No recreation can compete with that — they can only swap in the best general-purpose model and hope it generalizes. Codex sidesteps this by using a model OpenAI also tunes for agentic use.

## Reading list

- [Piebald-AI/claude-code-system-prompts](https://github.com/Piebald-AI/claude-code-system-prompts) — the canonical extracted system prompts, updated per Claude Code version. Required reading.
- [github/dmca 2025-03-10-anthropic.md](https://github.com/github/dmca/blob/master/2025/03/2025-03-10-anthropic.md) — the original DMCA against anon-kode and 437 forks.
- [Martin Alderson: Minification isn't obfuscation](https://martinalderson.com/posts/minification-isnt-obfuscation-claude-code-proves-it/) — the deobfuscation methodology.
- [InfoQ: Anthropic Accidentally Exposes Claude Code Source via npm Source Map File](https://www.infoq.com/news/2026/04/claude-code-source-leak/) — the April 2026 leak that triggered claw-code.
- [DEV: We Reverse-Engineered 12 Versions of Claude Code](https://dev.to/kolkov/we-reverse-engineered-12-versions-of-claude-code-then-it-leaked-its-own-source-code-pij) — twelve-version diff of the deobfuscated source.
- [Cybernews: Leaked Claude Code source spawns fastest growing repository in GitHub's history](https://cybernews.com/tech/claude-code-leak-spawns-fastest-github-repo/) — the claw-code phenomenon.
- [DeepWiki: sst/opencode](https://deepwiki.com/sst/opencode) — accessible deep-dive into OpenCode internals; the agent system and context management pages are particularly good.
- [shareAI-lab/learn-claude-code README](https://github.com/shareAI-lab/learn-claude-code) — the most useful single-document framing of "agency from the model, harness from the engineer" that exists.
- [Saiyam Pathak: OpenCode Just Overtook Claude Code on GitHub Stars](https://saiyampathak.substack.com/p/opencode-just-overtook-claude-code) — market context for May 2026.
- [Jonathan Fulton: Inside the Agent Harness: How Codex and Claude Code Actually Work](https://medium.com/jonathans-musings/inside-the-agent-harness-how-codex-and-claude-code-actually-work-63593e26c176) — best side-by-side of the parallel-design pair.
- [VILA-Lab/Dive-into-Claude-Code](https://github.com/VILA-Lab/Dive-into-Claude-Code) — a systematic academic-style analysis.
- [Anthropic engineering: making Claude Code more secure and autonomous](https://www.anthropic.com/engineering/claude-code-sandboxing) — Anthropic's own write-up of the sandboxing design.

---

**Previous:** [03 · Claude Code Architecture](./03-claude-code-architecture.md) · [↑ Index](./INDEX.md) · **Next:** [05 · Comparative Harnesses](./05-comparative-harnesses.md)
