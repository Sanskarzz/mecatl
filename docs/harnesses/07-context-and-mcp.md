---
title: "Context Engineering and MCP"
doc_id: 07-context-and-mcp
layer: practice
captured: 2026-05-18
status: stable
volatile: "Contains pricing and token economics that drift; re-check vendor pages before relying on numbers."
keywords: [context rot, token economics, pricing, prompt caching, cache breakpoints, TTL, cache hit rate, compaction, tool-result clearing, sliding window, hierarchical compaction, agentic discovery, repo map, embeddings, memory tiers, MCP, JSON-RPC, stdio, streamable HTTP, tools resources prompts, sampling, elicitation, roots, code execution with MCP, tool descriptions, tool result shaping, error handling, observability]
answers:
  - "How do I manage the context window as a finite, degrading resource?"
  - "How does prompt caching work on Anthropic vs OpenAI, and how do I lay out breakpoints?"
  - "Which compaction strategy do I use and what must I preserve?"
  - "How does MCP work and what are the server best practices?"
  - "How do I write tool descriptions, shape tool results, and handle errors so the model recovers?"
related: [06-architecture-patterns, 03-claude-code-architecture, 08-design-considerations]
---

# 07 — Context Engineering and MCP

> Companion to file 06. Where 06 covers the *shape* of the agent, this file
> covers what flows through it: tokens, memory, tools, and the protocol
> that's eaten the integration world.

## TL;DR

- The context window is a **finite, degrading resource**. Context rot is real:
  models get measurably worse at retrieval and reasoning as inputs grow, even
  on inputs that fit ([Chroma 2025][rot]). Treat tokens like dollars.
- Today (May 2026) you are paying roughly **$3/$15 per Mtok** for Sonnet,
  **$5/$25 for Opus 4.7**, **$1/$5 for Haiku 4.5** ([Anthropic Pricing][acp]).
  OpenAI's frontier line is roughly comparable. Prompt-cache hits are 10% of
  list input price on Anthropic; OpenAI's automatic prefix cache is 50% off.
- Anthropic prompt caching is **explicit**: place `cache_control:
  {"type":"ephemeral"}` on up to 4 breakpoints; 5-minute TTL default, 1-hour
  optional at 2× write cost; reads are 0.1× base. OpenAI's caching is
  **automatic** above 1024 tokens of common prefix.
- The three Anthropic-blessed context strategies are **compaction**, **tool-
  result clearing**, and **memory tools** ([Anthropic 2025d][cce]). Pick the
  smallest one that works.
- For tools and integrations, MCP is the standard. **Don't expose every API
  as a tool**: small focused servers, on-demand loading
  ([Anthropic 2025c][cemcp]), and the "code execution with MCP" pattern is
  the path forward.
- The output shape of a tool matters as much as its input shape. Truncate,
  paginate, and teach the model to recover from errors.

[rot]: https://research.trychroma.com/context-rot
[acp]: https://platform.claude.com/docs/en/about-claude/pricing
[cce]: https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents
[cemcp]: https://www.anthropic.com/engineering/code-execution-with-mcp

---

## 1. Context as a finite resource

The most useful frame for context engineering, from Anthropic:

> "Context is a finite resource with diminishing marginal returns. Find the
> smallest set of high-signal tokens that maximize the likelihood of your
> desired outcome." ([Anthropic 2025d][cce])

Two consequences:

1. **Size ≠ quality.** A 1M-token Sonnet context is technically available, but
   model accuracy drops as input grows. Chroma's *Context Rot* paper (2025)
   tested 18 frontier models including GPT-4.1, Claude 4, Gemini 2.5, and
   Qwen3 — every one degraded on simple retrieval as input length grew. The
   degradation was worse on tasks needing multi-hop reasoning and worse when
   the question and the answer had low lexical overlap (i.e., real
   questions, not needle-in-a-haystack tests) ([Chroma 2025][rot]).

2. **There's an attention budget.** Every token you put in is a token competing
   for the model's attention with every other token. A 50K-token CLAUDE.md
   doesn't mean the model "reads it carefully"; it means most of it dilutes
   whatever signal you needed it to provide.

The harness's job is to keep the context **small, signal-dense, and stable**.
Small for performance and cost. Signal-dense because the model attends to what
it sees. Stable because instability invalidates the prompt cache (more below).

---

## 2. Token economics (May 2026)

Current per-Mtok prices, frontier models:

| Provider | Model | Input | Output | Cache read | Cache write (5m) |
|---|---|---|---|---|---|
| Anthropic | Opus 4.7 | $5 | $25 | $0.50 (0.1×) | $6.25 (1.25×) |
| Anthropic | Sonnet 4.6 | $3 | $15 | $0.30 | $3.75 |
| Anthropic | Haiku 4.5 | $1 | $5 | $0.10 | $1.25 |
| OpenAI | GPT-5 (frontier) | ~$3 | ~$15 | ~50% off | n/a (automatic) |
| Google | Gemini 2.5 Pro | ~$2.50 | ~$10 | varies | varies |

(Caveats: prices move; OpenAI's exact pricing depends on tier; Gemini's
varies by context window. Numbers above are correct as of May 2026 from
vendor pricing pages.)

Operational implications:

- **Output is the killer.** Output tokens cost 5× input on Anthropic. A model
  that writes a 10K-token explanation costs the same as one that reads 50K
  of input. Lean on terseness conventions in the system prompt.
- **A turn isn't one input.** The full conversation is re-sent every turn.
  Without prompt caching, a 50-turn session with a 20K system prompt and 5K
  growing context per turn costs roughly: 50 × (20K + 5K × 25) = ~7.25M
  input tokens. *With* prompt caching: the system prompt is cached, so you
  pay the 0.1× cache-read rate after turn 1. Same conversation: ~770K
  effective full-price tokens. Ten times cheaper. Prompt caching isn't an
  optimization, it's table stakes.
- **Subagent isolation pays twice.** A subagent that does 50K of grep
  results internally and returns 1K to the parent saves 49K from the parent's
  every subsequent turn. Over a long session this compounds. (See file 06,
  section 5.)

---

## 3. Prompt caching

The vendor-specific details matter. Get them wrong and you save nothing.

### Anthropic (explicit, breakpoint-based)

Mechanism ([Anthropic Prompt Caching docs][pc]):

[pc]: https://platform.claude.com/docs/en/build-with-claude/prompt-caching

- You mark up to **4 cache breakpoints** per request with
  `"cache_control": {"type":"ephemeral"}`. Anything *before* a breakpoint is
  a candidate for caching.
- Breakpoints must appear in canonical order: `tools` → `system` → `messages`.
- Default TTL is **5 minutes**. Set `"ttl": "1h"` for a 1-hour cache at 2×
  write cost. 1h breakpoints must precede 5m breakpoints in the same request.
- Cache writes cost **1.25×** base input (5m) or **2×** base input (1h).
- Cache reads cost **0.1×** base input. Reads are free of charge against the
  breakpoint itself; you pay only for cached tokens read.
- Minimum cacheable prefix: **1,024 tokens** for Sonnet/Opus 4 series,
  **4,096 tokens** for Haiku 4.5 and Opus 4.5+. Shorter prompts simply don't
  cache (no error).
- Cache is **keyed by exact prefix match**. Any change before the breakpoint
  invalidates the cache. Changes inside the `tools` array invalidate
  everything downstream. Changes to model parameters that affect generation
  (e.g., `tool_choice` differences, certain feature flags) invalidate too.

Automatic mode (newer): set a single top-level `cache_control` and the API
places the breakpoint on the last cacheable block, moving it forward as the
conversation grows. Simpler but not available on Bedrock.

Cache hit measurement: the response `usage` field includes
`cache_read_input_tokens` and `cache_creation_input_tokens`. Sum the three
(`cache_read + cache_creation + input_tokens`) to recover total input.

### OpenAI (automatic, prefix-based)

Mechanism ([OpenAI prompt caching][oac]):

[oac]: https://openai.com/index/api-prompt-caching/

- **Automatic.** No code changes; the API detects matching prefixes from
  recent requests.
- Applies to prompts ≥ **1,024 tokens**; the cache grows in 128-token
  increments.
- **50% discount** on cached input tokens. No surcharge for cache writes.
- TTL is implicit (caches are evicted by load; expect minutes, not hours).

The result: OpenAI is simpler, Anthropic is more controllable. With
Anthropic you can deliberately cache static skill bodies, tool definitions,
and system prompts at independent breakpoints with different TTLs. With
OpenAI you write a stable prefix and trust the platform.

### Practical cache architecture

A good cache layout for a coding agent:

```
[breakpoint: tools]          ← 1h cache. Tool defs are stable across the session.
  (tool definitions)
[breakpoint: system]         ← 1h cache. System prompt + CLAUDE.md.
  (system prompt)
  (CLAUDE.md inline)
[breakpoint: history-stable] ← 5m cache. Older conversation turns.
  (message turns 1..N-5)
  (message turns N-5..N-1)   ← uncached; rotating window
  (current user message)     ← uncached
```

Cache-hit rate is the metric you actually want to track. Anthropic recommends
"refresh before TTL"; for long-running sessions, set up a periodic ping
request that hits the cache and resets the TTL clock.

When to **invalidate deliberately**:

- New session (obvious).
- After a long pause where TTL almost certainly expired.
- After a code change that touched the system prompt content.

When to **avoid invalidation**:

- Don't reformat the system prompt for cosmetic reasons mid-session.
- Don't add timestamps to the system prompt — they invalidate caches every
  request. Inject them into the most recent user turn instead.

---

## 4. Compaction strategies

Sooner or later every long agent session reaches the limit. The strategies
in production:

**Summarize-and-trim.** When token count exceeds a threshold (Claude Code
triggers around ~80–85% of model limit), ask the model to summarize older
turns into a compact "what we did, what we learned, what's pending" block,
then drop the originals. The summary slot replaces the originals. This is
Anthropic's `/compact` command and now their server-side compaction feature
([Anthropic Compaction docs][cmp]).

[cmp]: https://platform.claude.com/docs/en/build-with-claude/compaction

What to preserve:

- The current plan (what we're trying to do).
- Decisions made (what approach we chose and why).
- Unresolved questions / known errors.
- File paths the agent has touched.
- Tool IDs only if there are outstanding tool calls (otherwise drop).

What to drop:

- Raw file contents (the agent can re-read).
- Verbose grep output.
- Stack traces older than ~5 turns.
- Anything the agent has already acted on and confirmed.

**Sliding window.** Keep the most recent N turns verbatim plus the system
prompt. Cheap, lossy, no model call required. Works for chat-style sessions
where state is mostly recent; fails for coding agents that need to remember
the test failure from 30 turns ago.

**Hierarchical compaction.** Summaries of summaries. Recent N turns full,
preceding M turns as a single summary, preceding L turns as a summary of
summaries. Good for very long agents (hours+) at the cost of resolution.

**Tool-result clearing.** The lightest-touch version. Replace large tool
result bodies in older turns with a placeholder: `<<tool_result_truncated id=X
size=42KB>>`. The model retains that the tool was called and the call
returned, but the content is gone. Anthropic shipped this as a platform
feature in late 2025 ([Anthropic 2025d][cce]).

The right thing to compact first is **tool outputs**, not user-or-assistant
text. Tool outputs are the biggest and least signal-dense per token.

**When to trigger:** don't wait for the model to 400 on context overflow.
Trigger compaction at 70–80% of model limit so there's headroom for the
summarization call itself. The summarization call costs tokens.

**Signal preservation as a pattern:** before compacting, dump current state
to disk (a `scratch.md`, a `plan.md`). After compacting, re-inject *just the
file paths* into context. The model can re-load on demand.

---

## 5. Agentic context discovery vs. pre-built indices

A foundational question in any coding agent: **how does the model find the
right code?**

Three approaches, with tradeoffs:

**Let the model grep.** Tools: `grep`, `find`, `read_file`. The model
explores like a human would. Pros: no index to maintain, always
authoritative, surfaces the precise lines the model needs. Cons: many tool
calls per discovery, each one round-trips the LLM. This is the dominant
pattern in 2025/2026 — Claude Code, Cursor, Codex all default to it.
The bet: tool calls are cheaper than the cost of stale indices.

**Pre-built repo map.** A skeleton view of the codebase (file tree, function
signatures, top-of-file docstrings) generated at session start and pinned
into context. Aider pioneered this. Pros: the model has a global view from
turn one. Cons: stale the moment files change; consumes 5–20K tokens of
context permanently; large repos blow the budget.

**Embeddings / vector search.** Pre-compute embeddings of every file or
function, retrieve top-K on demand. Pros: scales to huge codebases. Cons:
embedding-based retrieval is semantically lossy on code (it's better on
prose); maintaining the index across edits is non-trivial; precision is
worse than a grep on a known string.

The **empirical winner in coding agents** has been "let the model grep,"
augmented with a *cached file tree* (cheap to maintain, gives the model a
map without consuming much context). Anthropic's coding team has been
unusually vocal that they tried RAG and embeddings and found greppable
exploration won at typical repo sizes (~hundreds of MB) ([Anthropic 2025d][cce]).

The right default: ship a fast grep tool, ship a fast read tool, ship a
fast file-tree summary tool. Add embeddings only when you can prove the
agent is wasting turns on bad searches and the repo is too big for
exhaustive search.

---

## 6. Memory systems

A common request: "make the agent remember things across sessions." Tread
carefully — this is one of the easiest places to over-engineer.

The three memory tiers:

**In-context.** What's in the prompt right now. Free and instant. Bounded
by the window.

**File-based.** Memory stored as files on disk that the agent can read and
write. CLAUDE.md, scratch files, NOTES.md, project conventions, etc. This
is what Anthropic recommends for most "memory" use cases — the file system
*is* the memory, and the agent already knows how to use it.

**Vector store / database.** A separate retrieval system the agent queries.
Required when memory exceeds what's enumerable, or when retrieval needs
to be semantic.

**The anti-pattern:** saving facts the file system already knows. "Remember
that `Auth` is in `src/auth.py`" is worthless — the next session's first
grep will find it. "Remember that user prefers TypeScript with strict mode"
might be worth saving, because it's *not* discoverable from the codebase
without inferring.

A good rule: a memory entry is worth saving if and only if (a) the agent
cannot rediscover it from the file system in a few tool calls, and (b) it
will be needed across sessions. By this filter, useful memory tends to be:

- User-specific preferences.
- Stable secrets/credentials (use a real secret manager, not the model's
  memory).
- Cross-cutting facts about external systems the agent integrates with.
- Long-running plans that span sessions.

Things to *not* store in memory:

- "Lessons learned" from a single session. Most are wrong out of context.
- Anything the model can re-derive in <5 tool calls.
- Anything the team's docs already document.

Implementation: Anthropic's memory pattern is a **memory tool** — `recall(key)`,
`remember(key, value)` — backed by a flat JSON or markdown file. Keep it
small. Show the agent its index at session start (a list of keys), let it
read values on demand.

---

## 7. MCP overview

The Model Context Protocol (Anthropic, Nov 2024; spec revision 2025-11-25)
is the JSON-RPC standard for connecting agents to tools, resources, and
prompts ([MCP spec][mcp]).

[mcp]: https://modelcontextprotocol.io/specification/2025-11-25

Architecture:

- **Host**: the LLM application (Claude Code, Cursor, ChatGPT desktop, etc.).
- **Client**: one connector instance inside the host, per server.
- **Server**: a process exposing tools/resources/prompts to the client.

Wire format: JSON-RPC 2.0 over one of:

- **stdio**: server is a subprocess of the host; messages go over stdin/stdout.
  Default for local servers (most common).
- **Streamable HTTP** (formerly HTTP+SSE): server is an HTTP service;
  bidirectional via SSE for server-initiated messages.

Three server primitives:

- **Tools**: model-callable functions. Arguments validated by JSON schema,
  return content (text, images, embedded resources).
- **Resources**: addressable read-only data the host can attach to context.
  Files, query results, schema introspection. Identified by URI.
- **Prompts**: parameterized prompt templates the host can offer to the user
  (e.g., a slash-command implementation backed by the server).

Three client primitives (server-initiated):

- **Sampling**: server asks the host to make an LLM completion. Lets servers
  do agentic reasoning of their own.
- **Roots**: server asks "what filesystem URIs am I allowed to access?"
- **Elicitation**: server asks the host to prompt the user for input.

Capability negotiation happens at handshake time — both sides declare what
they support, and use is restricted accordingly.

Why MCP won: before it, every coding agent had to write a bespoke integration
for every external system. With MCP, you ship a small adapter once, and every
MCP-aware host can use it. The MCP registry as of mid-2026 lists thousands
of servers.

---

## 8. MCP best practices

After 18 months of production, the lessons:

**Small, focused servers.** One server per service, not one server per
company. A `github` server, a `slack` server, a `linear` server. Avoid
"do-everything" servers; they bloat the tool inventory and confuse the
model. Anthropic's own `writing-tools-for-agents` guidance applies here.

**Limit the tool count per server.** 5–15 tools per server is a sweet
spot. More than that and the model picks badly. If you have 50 endpoints
to expose, group them under fewer, higher-level tools and use parameters
to disambiguate. Or use the **code-execution-with-MCP** approach below.

**Clear, specific tool descriptions.** This is the bridge to "writing tools
for agents" — the rules from file 06 §6 apply doubly to MCP tools, because
they're shared across many agents. Write descriptions assuming the model
has never seen your service before.

**Stateless tools when possible.** Each tool call should be self-contained;
the server should not rely on session state the model can't see. If state
is necessary, expose it as a resource the model can read.

**Auth: OAuth 2.1 with PKCE for HTTP servers.** The current spec mandates
this for remote servers. For local stdio servers, the security model relies
on the user choosing to install the server — but see the security caveat
below.

**Security boundary: the harness, not the protocol.** MCP's STDIO transport
treats configured commands as trusted and executes them with no sanitization
([OX Security 2026][ox]). This is by design; Anthropic confirmed the
behavior. Practical implication: treat every MCP server you install as
arbitrary code you'd otherwise `curl | bash`. Pin versions, source from
registries you trust, sandbox the agent. Never let user-controlled input
flow into MCP server configuration.

[ox]: https://www.ox.security/blog/mcp-supply-chain-advisory-rce-vulnerabilities-across-the-ai-ecosystem/

**Code execution with MCP** ([Anthropic 2025c][cemcp]) is the emerging
high-volume pattern: instead of registering 200 tool schemas with the model,
expose servers as TypeScript modules in a sandboxed filesystem and let the
model write code that imports them. Tool *discovery* happens by listing
directories; tool *use* happens by writing a script. Anthropic's reported
result: 150K tokens → 2K tokens for a Google-Drive-to-Salesforce workflow
(98.7% reduction). The tradeoff is real: you've turned the problem from
"protect against bad tool calls" into "protect against arbitrary code
execution," which means you need a real sandbox.

---

## 9. Tool descriptions that actually work

The model sees: tool name, parameter schema, parameter descriptions, the
top-level tool description. That's it.

Worked example. Don't ship:

```json
{
  "name": "get_file",
  "description": "Get a file.",
  "parameters": {"path": "string"}
}
```

Ship:

```json
{
  "name": "read_file",
  "description":
    "Read the contents of a text file from the project's working directory.\n\
     Use this when you need to inspect code, configuration, or documentation.\n\
     For binary files (images, executables), use `inspect_binary` instead.\n\
     Returns the file contents as a string with line numbers prefixed.\n\
     Maximum file size: 1 MB; larger files are truncated with a marker.",
  "parameters": {
    "path": {
      "type": "string",
      "description":
        "Absolute path to the file, or relative to the working directory. \
         Example: 'src/lib/auth.ts' or '/home/user/project/README.md'."
    },
    "offset": {
      "type": "integer",
      "description": "Optional: line number to start reading from (1-indexed).",
      "default": 1
    },
    "limit": {
      "type": "integer",
      "description":
        "Optional: number of lines to read. Default 2000. For large files, \
         iterate by setting offset and limit.",
      "default": 2000
    }
  }
}
```

What's load-bearing in the second version:

- The description **says when to use this and when not to** (and points to
  the right tool for the not-this case).
- Parameter descriptions include **examples** of valid values.
- The description discloses **limits and edge cases** (1 MB, truncation,
  line-number prefixing). The model can plan around them.
- The parameter name `path` is unambiguous; not `file` or `target`.
- Optional parameters have defaults so the model isn't forced to guess them.

Test your descriptions by reading them as a new hire would. If you can't
figure out when to use the tool from the description alone, the model can't
either.

---

## 10. Tool result shaping

The shape of what comes back is half the battle.

**Truncate, with markers.** A 100K-byte grep output is useless. Truncate to
~25K tokens and append a clear continuation hint:

```
[... 312 more matches truncated; refine the query or paginate with offset=]
```

**Paginate predictably.** Cursor-based pagination > offset-based when
results may change between calls. Either way, signal "more available" in
the output, not just an empty next page.

**Human-readable identifiers alongside opaque ones.** Don't return
`{"user_id": "u_8f3a..."}` alone. Return `{"user_id": "u_8f3a...", "name":
"alice@example.com"}`. The model uses the name to reason; the ID to act.

**Filter at the source.** If a list endpoint returns 100 items and the
model needs 5, accept a `limit` parameter and a `filter`. Don't make the
agent paginate through hundreds of items it'll ignore.

**Encourage follow-up calls when appropriate.** "I returned the first 5
results out of 47. Refine your query or set `page=2` for the next batch."
Tells the model both what it has and what's available.

**Token-count the output.** Anthropic caps tool returns at 25K tokens by
default. Your tool should do its own check and fail fast if the result will
be huge — "this query would return 800K of data; please narrow it."

**Structured > raw when the model needs to act on fields.** Return JSON,
not free text, when the model will extract values. Return Markdown or plain
text when the model will surface it to the user.

**The "what would I send back if I had to pay $0.25 per Mtok of model
attention for it" mental check** is a useful heuristic. Every token of
tool output is a token the model has to read on this turn and every
subsequent turn until compaction.

---

## 11. Error handling

The biggest single-turn lift in agent reliability is **error messages that
teach the model how to recover**.

Pattern: every error includes (a) what failed, (b) why, (c) what to try
instead.

Bad:

```
Error: 400 Bad Request
```

Better:

```
Error: argument `path` was 'src/main.rs' but the working directory is /tmp/wat.
Try an absolute path, or call `set_cwd(...)` first.
```

Best:

```
Error: file not found at 'src/main.rs'.
Working directory is /tmp/wat. Files starting with 'src/' exist at:
  /home/user/project/src/main.rs
Did you mean to call read_file('/home/user/project/src/main.rs')?
```

The last form is expensive to generate (it had to search for plausible
matches) but pays off enormously: the model corrects in one turn instead of
five.

**Don't suppress errors.** Returning a fake-success result so the agent
doesn't get confused is the worst possible outcome — the agent proceeds on
a false premise and the failure surfaces later, far from its source. Errors
should be loud and informative.

**Limit error retry loops.** If the same tool returns the same error three
times in a row, escalate: inject a system message telling the agent to
stop, summarize the problem to the user, and ask for guidance. Otherwise
you'll burn tokens loop-debugging.

---

## 12. Observability

You cannot operate a serious harness without observability. Minimum
viable instrumentation:

- **Per-tool-call log:** timestamp, tool name, arguments (redacted where
  needed), result size, latency, success/failure.
- **Per-turn token accounting:** `input_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens`, `output_tokens`. Compute the dollar cost.
  Roll up to per-session totals.
- **Cache hit rate**: `cache_read / (cache_read + cache_creation +
  input_tokens)`. Should be > 0.7 for a well-tuned coding agent.
- **Stop-reason distribution**: how many turns end with `end_turn`,
  `tool_use`, `max_tokens`, `pause_turn`. A high `max_tokens` rate means
  your max-output is wrong.
- **Turn count distribution**: histogram of conversations by turn count.
  Watch for runaways.

For diagnostics:

- **Replay logs.** Capture full request/response payloads (JSON, gzipped)
  so you can replay a session in dev with a flag flip. Indispensable for
  debugging non-deterministic agent behavior.
- **Distributed tracing.** If your harness orchestrates subagents, MCP
  servers, and hook subprocesses, wire OpenTelemetry spans through them.
  Anthropic's multi-agent research piece explicitly calls out tracing as
  the difference between "we know what happened" and "we don't."
- **Eval suite.** A small set of canonical tasks the agent runs nightly
  with a fixed model. Track success rate, token usage, turn count over
  time. This is how you detect regressions when models update or you change
  the system prompt.

For governance:

- **Audit log** of every tool call that touched the outside world (writes,
  network calls, shell). Append-only, retention sufficient for incident
  response.
- **Cost cap** per session, surfaced to the user.

---

## 13. Reading list

Context engineering:

- Anthropic, *Effective context engineering for AI agents* (Sep 2025).
  <https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents>
- Anthropic, *Context engineering: memory, compaction, and tool clearing*
  (cookbook).
  <https://platform.claude.com/cookbook/tool-use-context-engineering-context-engineering-tools>
- Chroma Research, *Context Rot: How Increasing Input Tokens Impacts LLM
  Performance* (2025). <https://research.trychroma.com/context-rot>

Prompt caching:

- Anthropic, *Prompt Caching* docs.
  <https://platform.claude.com/docs/en/build-with-claude/prompt-caching>
- OpenAI, *Prompt Caching in the API*.
  <https://openai.com/index/api-prompt-caching/>

Tools and MCP:

- Anthropic, *Writing effective tools for AI agents*.
  <https://www.anthropic.com/engineering/writing-tools-for-agents>
- Anthropic, *Code execution with MCP* (Nov 2025).
  <https://www.anthropic.com/engineering/code-execution-with-mcp>
- MCP specification, current revision.
  <https://modelcontextprotocol.io/specification/2025-11-25>
- MCP Quickstart and SDK docs.
  <https://modelcontextprotocol.io/>

Security:

- OX Security, *MCP STDIO Command Injection: Vulnerability Advisory*.
  <https://www.ox.security/blog/mcp-supply-chain-advisory-rce-vulnerabilities-across-the-ai-ecosystem/>
- Cursor, *Implementing a secure sandbox for local agents*.
  <https://cursor.com/blog/agent-sandboxing>
- Anthropic, *Sandboxing* (Claude Code docs).
  <https://code.claude.com/docs/en/sandboxing>

Memory:

- Anthropic, *Multi-agent research system* (Memory tool usage).
  <https://www.anthropic.com/engineering/multi-agent-research-system>
- Lilian Weng, *LLM Powered Autonomous Agents* (foundational memory framing).
  <https://lilianweng.github.io/posts/2023-06-23-agent/>

Pricing (for current numbers, always re-check):

- <https://platform.claude.com/docs/en/about-claude/pricing>
- <https://openai.com/api/pricing/>
- <https://ai.google.dev/pricing>
