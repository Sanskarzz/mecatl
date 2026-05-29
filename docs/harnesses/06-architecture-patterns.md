---
title: "Architecture Patterns for Agent Harnesses"
doc_id: 06-architecture-patterns
layer: practice
captured: 2026-05-18
status: stable
keywords: [agent loop, stop conditions, max_turns, ReAct, Plan-and-Execute, ReWOO, Reflexion, Tree of Thoughts, plan mode, multi-agent, orchestrator-workers, supervisor, swarm, subagent, Task, tool design, system prompt, hooks, permissioning, sandbox, slash commands, skills, progressive disclosure, headless, SDK, CLAUDE.md, AGENTS.md, anti-patterns, tool soup]
answers:
  - "How do I design the core agent loop and its stop conditions?"
  - "When do I use ReAct vs Plan-and-Execute vs Reflexion vs ToT?"
  - "When is multi-agent worth it vs a single agent with subagents?"
  - "How do I design tools, system prompts, hooks, and permissions?"
  - "What are the recurring harness anti-patterns?"
related: [02-twelve-patterns, 07-context-and-mcp, 08-design-considerations]
---

# 06 — Architecture Patterns for Agent Harnesses

> Discipline-wide reference. The patterns here recur across every serious harness
> (Claude Code, Codex, Cursor, Aider, Devin, Cline, Continue, OpenHands). The
> implementations differ; the ideas don't.

## TL;DR

- An agent is a *loop*: model produces messages and tool calls, harness executes
  tools and feeds results back, repeat until a stop condition. Everything else
  is decoration around that loop.
- Pick the **simplest** architecture that works. Anthropic's published guidance
  is unambiguous: start with a single prompt, then a workflow, and only reach
  for agency (a model that drives its own loop) when fixed paths can't capture
  the problem ([Anthropic 2024][bea]).
- Within agency, single-agent is the default. Multi-agent costs about
  **15× more tokens than chat** ([Anthropic 2025a][mar]) and introduces
  decision-conflict bugs that are catastrophic for code-writing agents
  ([Cognition 2025][cog]). Use it for embarrassingly parallel research, not for
  edits to a shared artifact.
- Subagents (one-shot Task delegation with a fresh context) are the safer
  cousin of multi-agent: they preserve isolation without the coordination
  hell.
- Tools, hooks, permissions, and standing-instructions files (CLAUDE.md /
  AGENTS.md) are the four levers the harness gives the operator to *constrain*
  the agent. The agent constrains itself through plan/act mode, slash commands,
  and skills.
- The dominant anti-pattern in 2025/2026 harnesses is **tool soup**: too many
  overlapping tools, schemas the model has to disambiguate, and bloated
  descriptions eating context before the task even starts.

[bea]: https://www.anthropic.com/research/building-effective-agents
[mar]: https://www.anthropic.com/engineering/multi-agent-research-system
[cog]: https://cognition.ai/blog/dont-build-multi-agents

---

## 1. The core agent loop

Every harness reduces to:

```
state := { messages: [system_prompt, initial_user_message], turns: 0 }
loop:
    response := model.complete(state.messages, tools=available_tools)
    state.messages.append(response)
    if response.stop_reason == "end_turn":      break
    if response.stop_reason == "max_tokens":    handle_truncation()
    if state.turns >= max_turns:                break
    for tool_call in response.tool_uses:
        if not permission.allows(tool_call):    inject_denial(); continue
        result := execute(tool_call)
        state.messages.append(tool_result(result))
    state.turns += 1
```

Things to internalize:

- **Stop conditions are load-bearing.** The two Anthropic stop reasons that
  matter are `end_turn` (model decided it's done) and `tool_use` (model wants
  another iteration). If you don't distinguish them, you'll either loop
  forever or stop one step short. Add a `max_turns` cap (Claude Code defaults
  to a high but finite value; LangGraph defaults are 25–50) and a per-call
  timeout.
- **Tool execution is the slow part.** The model returns in seconds; a `bash`
  tool running tests can take minutes. Stream results back as `tool_result`
  blocks paired by `tool_use_id`. Don't drop or rewrite ids — providers reject
  the message.
- **The loop is not the agent.** The same loop powers a coding agent, a
  customer-support agent, and a deep-research agent. Architecture is everything
  *around* this loop: which tools you expose, what's in the system prompt,
  which hooks fire, when you compact, when you spawn a subagent.

Stop-condition mistakes to avoid:

- **Infinite tool-use ping-pong.** Models sometimes loop on a failing tool.
  Track consecutive failures of the same tool and either inject a hard message
  ("this tool has failed 3 times — stop calling it and explain why") or break
  the loop.
- **Silent context overflow.** When `input_tokens` approaches the model limit,
  the next turn will 400. Either compact (see file 07) or refuse to continue
  the loop.
- **No max_turns.** Background-mode agents (Codex Cloud, Devin) without a
  turn cap have produced runaway sessions with five-figure tool-call counts.

---

## 2. Reasoning patterns: ReAct, Plan-and-Execute, Reflexion, ToT

These are inner-loop strategies. They constrain *how* the model uses its
turns, not what tools it has.

**ReAct** (Yao et al., 2022): the model alternates `Thought → Action →
Observation`. This is the default mode of every modern tool-using agent.
Native tool-use APIs (Anthropic, OpenAI) implement ReAct implicitly — the
`thinking` or message text is the Thought, the `tool_use` block is the Action,
the `tool_result` block is the Observation. You almost never need to prompt
"Thought:" / "Action:" explicitly anymore; the model does it natively. Use
ReAct when you need step-by-step grounding (coding, retrieval, debugging) and
when each action's result meaningfully constrains the next.

**Plan-and-Execute** (Wang et al., 2023; LangChain blog 2024): separate a
*planner* call from *executor* calls. The planner emits a list of steps; a
cheaper model (or the same model in a different mode) executes them. Variants:
*ReWOO* (Reasoning WithOut Observation) lets the planner write step
dependencies symbolically (`E1`, `E2 = search(E1.title)`), so the executor
substitutes values without re-calling the LLM ([LangChain][lcpe]).

[lcpe]: https://blog.langchain.com/planning-agents/

When to prefer Plan-and-Execute over ReAct:

- Tasks with predictable, mostly-independent substeps (data ingestion, batch
  refactors, doc generation).
- When the per-step LLM call is the dominant cost and substeps don't need
  observation to choose the next action.
- When you want *human review of the plan* before any side effects — this is
  the cognitive ancestor of Claude Code's Plan Mode.

When ReAct wins: anything where the next action genuinely depends on what the
previous one returned (most coding work).

**Reflexion** (Shinn et al., 2023): after a failed attempt, ask the model to
generate a verbal critique and store it as memory for the next attempt.
Reflexion reported 91% pass@1 on HumanEval, surpassing the GPT-4 SOTA of 80%
at the time. In a harness, this is implemented by adding a *self-critique*
step after test failures: take the test output, ask the model to write down
what went wrong, append that note, retry. Worth it for closed-loop tasks
(does it pass tests yes/no). Pointless when there's no verifier.

**Tree of Thoughts** (Yao et al., 2023): explore multiple reasoning branches,
evaluate each, prune. ToT raised GPT-4 on Game-of-24 from 4% (CoT) to 74%.
**Cost: 10–100× more tokens than CoT.** In a code-writing harness this is
almost never worth it; the cheaper alternative is generate-and-test with a
shell. Use ToT only when the task is search-shaped (planning, puzzles,
synthesis with a tractable evaluator) and a single linear pass under-performs.

Rule of thumb: ReAct by default, Plan-and-Execute when you can split the work,
Reflexion when you have a verifier, ToT essentially never in a coding context.

---

## 3. Plan / Act mode

Claude Code's *Plan Mode* deserves its own section because it has become the
defining UX pattern of 2025-era coding agents ([Ronacher 2025][armin]).

Mechanism:

- The agent is forced into a read-only toolset (Read, Grep, Glob, web search
  — no Write, Edit, Bash).
- The system prompt is augmented with text instructing the model to produce
  a plan, never to mutate state, never to call write tools.
- On user approval ("yes, execute") the harness flips state: full toolset
  restored, plan injected as a steering message, agent enters execute mode.

[armin]: https://lucumr.pocoo.org/2025/12/17/what-is-plan-mode/

Why this works:

1. **Cheap review surface.** A markdown plan is easier to skim than a diff.
   The operator catches misunderstandings before any files change.
2. **Forces decomposition.** The model is told to enumerate steps, so the
   eventual execution has structure to follow.
3. **Side-effect bound.** Read-only tools can't break a working tree. The
   blast radius of a hallucination in plan mode is zero.
4. **Plays well with subagents.** The plan can later be sliced into pieces and
   delegated.

Implementation note: enforce read-only by *hook*, not by prompt. The model
will sometimes try to call `Bash` from plan mode. A `PreToolUse` hook with
`mode=plan` should deny the call and inject the denial as a tool result. Never
trust prompt-only enforcement for security-relevant constraints.

---

## 4. Multi-agent patterns

The decision tree, after the Cognition/Anthropic public split:

1. **Default to single-agent.** "It's hard to overstate just how stupid
   multi-agent setups are without sufficient context-sharing" — Cognition,
   *Don't Build Multi-Agents* ([Cognition 2025][cog]).
2. **Use subagents (one-shot delegation)** for context isolation.
3. **Use a true multi-agent system** only when the work parallelizes naturally
   and information exceeds a single context. This is rare in code-editing,
   common in research and large-corpus analysis.

The taxonomy ([LangGraph 2025][lgg]):

- **Orchestrator-workers (supervisor)**: a central LLM decomposes tasks and
  delegates to workers, then synthesizes. This is Anthropic's research
  system: lead researcher spawns 3–5 parallel subagents for different facets
  of a query, each writes web searches in its own 200K context, returns a
  1–2k-token summary ([Anthropic 2025a][mar]).
- **Swarm**: agents directly hand off to each other without a central node
  via explicit handoff tools. Faster (fewer LLM calls), harder to trace.
- **Hierarchical**: supervisors of supervisors. Useful for very large task
  graphs; mostly an enterprise pattern.

[lgg]: https://github.com/langchain-ai/langgraph-supervisor-py

The **two principles** Cognition articulates that any harness builder should
internalize:

> "Share context, and share full agent traces, not just individual messages."
> "Actions carry implicit decisions, and conflicting decisions carry bad
> results."

In a code-editing harness, multi-agent failure looks like: agent A writes a
helper assuming a particular interface; agent B simultaneously edits that
interface; the supervisor merges and ships something that doesn't compile.

When multi-agent earns its keep (per Anthropic's field report):

- The task is genuinely parallel (search 10 sources, summarize a corpus).
- Subagent outputs are *small* relative to their inputs (heavy compression).
- The task is *high-value enough to justify 15× the token cost*.

Practical guidance:

- Lead agent: most capable model (Opus-class). Subagents: cheaper model
  (Sonnet-class). Anthropic specifically reported this configuration
  outperformed single-agent Opus by 90.2%.
- Spawn 3–5 subagents per delegation, never more. Coordination cost grows
  super-linearly.
- Give each subagent an explicit objective, output format, and tool whitelist.
  Vague delegation produces overlapping work.

---

## 5. The Subagent / Task pattern

This is the *one* multi-agent pattern that's safe to use casually, because
it has a single decisive constraint: **the parent never sees the subagent's
intermediate state, only its final output**.

Claude Code's `Task` tool is the canonical implementation. The parent calls
`Task(prompt="find every use of FooClient and summarize", subagent_type=...)`.
The harness spins up a fresh agent with its own context window, that agent
runs to completion (does its own grepping, reading, reasoning), and returns
a string. From the parent's perspective it was one tool call.

Why this is so much safer than "real" multi-agent:

- **One-shot**: no back-and-forth between parent and child. No coordination
  bugs because there's no coordination.
- **Context budget**: the child's 200K does not pollute the parent's 200K.
  The parent gets a 500-token summary back where a direct read would have
  cost 50,000 tokens.
- **Failure is bounded**: child either returns a result or errors. No
  half-finished side effects on shared files if you scope its tools
  appropriately.

Use-cases that pay off:

- "Search the repo for X and tell me the 5 most relevant files" — saves the
  parent from holding 200 grep results.
- "Read these 30 files and find which one defines Y" — saves the parent from
  loading 30 files.
- "Run the test suite and tell me which tests failed" — saves the parent
  from holding pages of pytest output.

Use-cases that don't pay off:

- Anything where the parent needs the *details* (raw code, exact line
  numbers, error stack). Subagents lose information by design.
- Anything iterative where the parent will need to ask follow-up questions
  about the child's work. Spawn one well-specified subagent or do the work
  inline, never spawn a subagent and then a second to refine the first.

---

## 6. Tool design principles

The single most important piece of writing on this is Anthropic's
"Writing effective tools for AI agents" ([Anthropic 2025b][wt]). The
ideas distill to:

[wt]: https://www.anthropic.com/engineering/writing-tools-for-agents

**Tools are a contract between deterministic systems and non-deterministic
consumers.** The traditional API design lens (predictable consumer, exact
inputs) does not apply. The model will hallucinate parameter names, pass
strings where ints belong, ignore documented constraints, and call your tool
in orders you didn't anticipate.

**Concrete rules:**

1. **Few, high-leverage tools beat many narrow ones.** Don't wrap every REST
   endpoint. Ship the 5 verbs the agent actually needs.
2. **Verb-first names**. `search_issues` not `IssuesSearch`. `read_file`
   not `file`. Names should signal action and namespace.
   `asana_projects_search` is better than a bare `search` when multiple
   tools could plausibly match the model's intent.
3. **Schemas are descriptions.** The model only sees the JSON schema and
   the `description` field. Treat description as onboarding docs for a new
   teammate: when to use, when *not* to use, an example value, common
   pitfalls.
4. **Idempotence where possible.** Tools that can be retried without harm
   recover gracefully from network errors and from the model second-guessing
   itself. Where idempotence isn't possible (writes, deploys), say so loudly
   in the description.
5. **Output shape matters as much as input shape.** Return human-readable
   names alongside opaque IDs. Truncate large outputs with a clear marker
   ("…[7 more results, call with `page=2` to see them]"). Anthropic caps
   tool returns at 25,000 tokens by default in Claude Code; that's a
   reasonable upper bound.
6. **Error messages teach.** "Invalid argument" is worthless. "argument
   `path` was 'src/main.rs' but the working directory is /tmp/wat — use an
   absolute path or call with `cwd=...`" is gold. The agent will read the
   error and fix itself.
7. **Token economics.** Every tool definition costs context. A 500-token
   description × 30 tools = 15K tokens of system overhead per turn. This is
   the case for code execution with MCP ([Anthropic 2025c][cemcp]):
   load tool *definitions* on demand from a filesystem rather than eagerly.
8. **Response-format negotiation.** Optional `response_format: "concise" |
   "detailed"` lets the agent ask for less when it knows it doesn't need
   structure.

[cemcp]: https://www.anthropic.com/engineering/code-execution-with-mcp

The Anthropic team's evaluation methodology is worth copying:

- Build a corpus of realistic tasks.
- Run the agent end-to-end on each.
- Have Claude itself read the transcripts and propose tool-design fixes
  (renaming, parameter splits, description rewrites). This is "Claude as
  auditor" — surprisingly effective.

---

## 7. System prompt design

The system prompt is the most expensive token block in your harness — it
is in every request, and it primes every behavior. Treat it as a designed
artifact, not a junk drawer.

Anatomy of a serious system prompt (loosely modeled on what Claude Code
emits):

- **Role and identity.** One paragraph. "You are an interactive CLI tool
  that helps users with software engineering tasks."
- **Operating environment.** OS, shell, working directory, presence of git,
  whether files are sandboxed. Inject dynamically.
- **Tool inventory summary.** Not the full schemas (those go in the `tools`
  array). A 2-line cheat sheet of when to reach for what.
- **Conventions.** Tone, output format (markdown? plain text? code fences?),
  whether to be concise.
- **Safety rules.** "Don't execute arbitrary code without confirming",
  "don't commit without being asked", etc.
- **Loaded user instructions.** CLAUDE.md / AGENTS.md contents, scoped to
  the project.

Hard-won lessons:

- **Don't tell the model not to do X.** Telling Claude "do not apologize"
  often makes it apologize more — the word "apologize" got primed. Reframe
  positively: "respond directly with the answer."
- **Conditionals belong in code, not prompt.** "If the user asks about
  weather, use the weather tool" is dead weight when you can just *not
  include* the weather tool unless the conversation needs it.
- **Sections > prose.** Use `## headers` and bulleted lists. The model
  parses structure.
- **Environment injection.** Always tell the model what day it is, what OS
  it's on, what the cwd is. Saves an exploratory tool call per turn.
- **CLAUDE.md goes inline, not by reference.** "Read your project conventions
  from CLAUDE.md" is wishful. Concatenate the file into the system prompt
  (with a "<<<project_instructions>>>" delimiter) and cache it.

The system prompt is also the **prime cache target**. Make it stable across
turns; mutations cost cache invalidations (see file 07).

---

## 8. Hooks and event systems

Hooks are deterministic interception points around the agent loop.
Claude Code's model ([Claude Code Hooks][cch]):

[cch]: https://code.claude.com/docs/en/hooks

| Event | When it fires | Use for |
|---|---|---|
| `SessionStart` | Once at session begin | Inject env info, fetch repo state |
| `UserPromptSubmit` | Before model sees user msg | Macro expansion, redaction |
| `PreToolUse` | Before a tool runs | Auth, validation, blocking |
| `PostToolUse` | After a tool succeeds | Linting, formatters, indexing |
| `Notification` | When agent asks for input | UI hooks |
| `Stop` | When the loop ends | Cleanup, summaries |
| `SubagentStop` | When a subagent ends | Result post-processing |

Why hooks beat prompt instructions: **guarantees**. If you tell the model
"always run the formatter after editing a file," it will do it 95% of the
time, which means it fails one time in twenty when you most need it. A
`PostToolUse` hook on Edit/Write that runs `prettier` runs *every time*,
because it's not the model deciding.

What to put in hooks:

- **Linters and formatters** as PostToolUse on file writes.
- **Pre-commit gates** as PreToolUse on `Bash` matching `git commit`.
- **Audit logging** of every tool call (compact JSON to a logfile).
- **Secret scanners** as PreToolUse before any outbound network call.
- **Cost tracking**: meter token usage in PostToolUse.

What *not* to put in hooks:

- Anything that needs the model's reasoning. "Decide whether this edit is
  safe" is a model problem, not a hook problem.
- Anything stateful across many turns — hooks fire in their own subprocess
  with no memory.

---

## 9. Permissioning models

Permissioning is how the harness encodes the user's tolerance for blast
radius. The patterns in production:

**Allowlist with prompt-on-unknown.** Default-deny outside the allowlist;
prompt the user the first time a new tool/command appears. This is Claude
Code's default. The pain point: "approval fatigue" — users click approve
reflexively. Anthropic reports their sandboxing release reduced approval
prompts by 84% ([Truefoundry 2025][tf]).

[tf]: https://www.truefoundry.com/blog/claude-code-sandboxing

**Sandbox-and-allow.** Run the agent inside a restricted environment
(bubblewrap on Linux, Seatbelt on macOS, a container, a VM). Inside the
sandbox, the agent has broad freedom; outside, narrow gates. This is the
dominant 2025/2026 pattern. Cursor, Codex, and Claude Code all ship sandbox
modes; Devin runs each agent in an isolated VM by default.

**Plan-then-confirm.** No tools execute until the user approves a plan.
Slow but high-trust.

**Tiered (read / write / shell).** Read tools always allowed, write tools
prompt once, shell tools prompt every time. Pragmatic for daily use.

What *not* to gate on:

- Read operations on the working tree. The agent needs unfettered Grep/Read
  or you get learned helplessness.
- Tool calls inside subagents. The subagent's outputs are reviewed; gating
  its internal calls multiplies prompts without reducing risk.
- Stdout reads. The agent has to see what it ran.

What *to* gate on:

- Network egress (especially outside the allowed domain set).
- File writes outside the project root.
- `git push`, `git commit --no-verify`, `rm -rf`, anything with `sudo`.
- Any MCP tool that's never been seen before.

Important note on MCP and security: as of 2026, MCP's STDIO transport has a
documented design choice that treats configured commands as trusted and
executes them with no sanitization ([OX Security 2026][ox]). Treat every
MCP server you install like a curl-to-bash script: source it from a registry
you trust, pin versions, and run it inside the same sandbox as the agent.

[ox]: https://www.ox.security/blog/mcp-supply-chain-advisory-rce-vulnerabilities-across-the-ai-ecosystem/

---

## 10. Slash commands and skills

Slash commands and skills are *precomposed prompts*. They're how power users
encode workflows.

**Slash commands** (Claude Code, Cursor): `/review`, `/test`, `/commit`. The
harness expands them into a prompt template, sometimes with parameter
substitution. Implementation is trivial: maintain a directory of markdown
files, when the user sends `/foo` substitute `~/.claude/commands/foo.md` as
the prompt.

**Skills** (Anthropic, October 2025 ([Anthropic Skills][as])): the
generalization. Each skill is a directory containing:

- `SKILL.md` with YAML frontmatter (name, description, when-to-use).
- Optionally: scripts, resources, examples.

[as]: https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills

The frontmatter is loaded into the system prompt always — small, cheap. The
body is loaded only when the agent decides the skill applies. Bundled files
are loaded only when explicitly read. This is **three-level progressive
disclosure**:

1. Index (always in context): name + one-line description.
2. Skill body (loaded when matched): full instructions.
3. Bundled assets (loaded on demand): scripts, templates, examples.

The harness-side requirement is small: a skill directory scanner, a system
prompt section that lists `available_skills`, a convention for when the model
"activates" a skill. Most of the work is on the user side, writing good
SKILL.md files.

Why skills > inline prompts:

- Reusable across sessions.
- Cheap when unused (only the index is loaded).
- Shareable (skill packages on filesystem or registry).

---

## 11. Headless / SDK mode

Every serious harness ships a non-interactive mode:

- `claude -p "fix the failing tests"` exits when done.
- `codex exec` in CI pipes a prompt in, captures stdout.
- The Anthropic Agent SDK exposes the same loop as a library.

Design constraints for headless mode:

- **Default to a higher trust level** because there's no human to approve
  prompts — but compensate with a tighter allowlist and a sandbox.
- **Structured output mode.** Let the caller request JSON; serialize the
  final result, not the model's prose narration.
- **Deterministic exit codes.** 0 = success, non-zero = some recoverable
  state. Don't conflate "task failed" with "agent crashed."
- **Streaming logs to stderr.** Caller can `tee` for observability.

This matters for harness composability: a coding agent invoked from CI, from
a Makefile, from another agent (subagent pattern in process form), or from
a hook. If your harness only works interactively, you've cut off half its
use cases.

---

## 12. CLAUDE.md / AGENTS.md / Cursor rules

The pattern: a markdown file at the project root that the harness loads
into the system prompt for every session in that project. Originated as
CLAUDE.md (Anthropic), generalized as AGENTS.md (open standard adopted by
OpenAI/Cursor/Sourcegraph/Factory, August 2025, donated to the Linux
Foundation in December 2025 ([AGENTS.md][agm])).

[agm]: https://agents.md/

Why it works:

- **It's a file**, so it's in version control. The team's conventions live
  next to the code, not in tool-specific settings.
- **Tool-agnostic.** A repo with a good AGENTS.md works in Claude Code,
  Codex, Cursor, Continue, Aider, etc.
- **Cache-friendly.** Static across turns, fits naturally inside a prompt
  cache breakpoint.

What goes in it:

- Project overview (one paragraph).
- How to build, test, lint, run.
- Coding conventions specific to *this* repo (don't restate "use 2-space
  indent" if it's in `.editorconfig` — the agent reads that already).
- Things that aren't obvious from the code (architectural decisions,
  forbidden patterns, "we use X not Y because Z").
- Pointers to richer docs ("for the API conventions see `docs/api.md`").

What doesn't go in it:

- Anything secret. CLAUDE.md ends up in screenshots, in PRs, in logs.
- Massive content dumps. If it's longer than a page, you're better off
  putting it in `docs/` and pointing the agent at the file.
- Per-session state. CLAUDE.md is timeless; session state goes in scratch
  files.

Layering: most harnesses support `~/.claude/CLAUDE.md` (user-global),
`./CLAUDE.md` (project), and `./<subdir>/CLAUDE.md` (scoped to that subtree).
Loading order matters for overrides — typically narrower beats broader.

---

## 13. Anti-patterns

A non-exhaustive list of footguns that recur in harness designs:

**Tool soup.** 30+ tools, many overlapping (`read_file` and `cat`, `search`
and `grep` and `find_files`). The model wastes turns picking among them and
sometimes calls the wrong one. Curate.

**Over-eager memory.** Persisting "lessons learned" across sessions sounds
smart but quickly becomes noise. The model doesn't need to remember last
month's bug fix — the code already encodes it. Memory is most useful when
the agent literally cannot rediscover the fact (user preferences, secrets).
See file 07 for the deeper take.

**Premature multi-agent.** "I'll have one agent plan, one code, one test."
You've just paid 3× the cost and introduced 3 ways for the system to
desync. Use a single agent with plan mode, or sequence the work with a
workflow.

**No stop conditions.** Loops that only break on `end_turn`. Agents on a
bad path will tool-call forever. Add max_turns, max_tool_calls,
max_consecutive_failures.

**Gating reads.** Asking the user to approve every `Read` call. Reads are
near-free and near-safe; you've taught your user to click "approve" on
autopilot, defeating the gates that matter.

**Schema-only tool descriptions.** A schema with `name: edit_file, params: {
path: string, content: string }` and no description tells the model nothing.
The model will guess at when to use it.

**Trusting the model to enforce constraints.** "Don't write to /etc" in
the system prompt is wishful. The model *will* write to /etc one time in a
thousand. Hooks and sandboxes enforce; prompts merely suggest.

**Context-bloat via tool definitions.** Loading the entire schema of 200
MCP tools into the prompt. Anthropic's code-execution-with-MCP work
([Anthropic 2025c][cemcp]) showed a 98.7% token reduction (150K → 2K) by
loading on demand. If your harness has more than ~30 tools active, that's
the lever to pull.

**Compaction without signal preservation.** Naive `summarize-old-messages`
loses tool IDs, file paths, error context — the exact things the agent
needs to recover from. Compaction needs structure (see file 07).

**Speaking-only enforcement of plan mode.** Trusting the model to "not run
any tools." It will. Make plan mode a hook-enforced toolset.

---

## 14. Reading list

Primary sources (start here):

- Anthropic, *Building Effective Agents* (Dec 2024). The canonical taxonomy
  of workflows and agents.
  <https://www.anthropic.com/research/building-effective-agents>
- Anthropic, *Effective context engineering for AI agents* (Sep 2025). The
  follow-up on what to do with the context window.
  <https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents>
- Anthropic, *Writing effective tools for AI agents* (2025). The definitive
  tool-design piece.
  <https://www.anthropic.com/engineering/writing-tools-for-agents>
- Anthropic, *How we built our multi-agent research system* (Jun 2025). The
  field report from the lead-researcher / subagent architecture.
  <https://www.anthropic.com/engineering/multi-agent-research-system>
- Anthropic, *Code execution with MCP* (Nov 2025). The "load tools on demand
  from a filesystem" pivot.
  <https://www.anthropic.com/engineering/code-execution-with-mcp>
- Cognition AI, *Don't Build Multi-Agents* (Jun 2025). The opposing view.
  <https://cognition.ai/blog/dont-build-multi-agents>
- Lilian Weng, *LLM Powered Autonomous Agents* (Jun 2023). The "agent =
  LLM + memory + planning + tool use" framing.
  <https://lilianweng.github.io/posts/2023-06-23-agent/>

Papers (for theory):

- Yao et al., *ReAct: Synergizing Reasoning and Acting in Language Models*
  (2022). <https://arxiv.org/abs/2210.03629>
- Shinn et al., *Reflexion: Language Agents with Verbal Reinforcement
  Learning* (2023). <https://arxiv.org/abs/2303.11366>
- Yao et al., *Tree of Thoughts: Deliberate Problem Solving with Large
  Language Models* (2023). <https://arxiv.org/abs/2305.10601>
- Wang et al., *Plan-and-Solve Prompting* (2023).
  <https://arxiv.org/abs/2305.04091>
- Xu et al., *ReWOO: Decoupling Reasoning from Observations for Efficient
  Augmented Language Models* (2023). <https://arxiv.org/abs/2305.18323>

Frameworks (for patterns, not necessarily to adopt):

- LangGraph multi-agent docs (supervisor, swarm, hierarchical).
  <https://github.com/langchain-ai/langgraph-supervisor-py>
- LangChain *Plan-and-Execute Agents* blog.
  <https://blog.langchain.com/planning-agents/>

Standards and specs:

- Model Context Protocol specification (current revision).
  <https://modelcontextprotocol.io/specification/2025-11-25>
- AGENTS.md open standard.
  <https://agents.md/>

Practitioner posts worth reading:

- Armin Ronacher, *What Actually Is Claude Code's Plan Mode?* (Dec 2025).
  <https://lucumr.pocoo.org/2025/12/17/what-is-plan-mode/>
- Simon Willison's coverage of every Anthropic agent post (he's the de
  facto archivist). <https://simonwillison.net/tags/agents/>
