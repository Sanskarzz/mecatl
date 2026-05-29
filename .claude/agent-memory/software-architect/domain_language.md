---
name: domain-language
description: Canonical ubiquitous-language names for the mecatl harness domain — use these, not Manager/Helper/Util mechanics names.
metadata:
  type: project
---

Ubiquitous language for mecatl. Code names must match these; mechanics-names (Manager/Handler/Service/Util) are rejected.

- **Session** — aggregate root for one agent run. Holds Conversation, config, stop-condition counters, permission mode.
- **Conversation** — ordered list of Messages (the model-visible history).
- **Turn** — one model call + the tool invocations it triggered.
- **Message** — system / user / assistant / tool-result entry in the Conversation.
- **ToolCall** — model's request to run a tool (name + args + id).
- **ToolResult** — outcome of a ToolCall (paired by id).
- **PermissionDecision** — deny / ask / allow outcome for a ToolCall across merged scopes.
- **Hook / HookEvent** — deterministic lifecycle interception (PreToolUse, PostToolUse, SessionStart, etc.).
- **Usage** — token/cost/cache accounting for a Turn (value object).
- **Event** — the streamed typed event emitted by the loop and surfaced over the API.

Ports (interfaces, named for what they do): LLMProvider/Completer, Tool/ToolExecutor, PermissionPolicy, HookRunner, FileSystem, Clock, SessionStore. See [[project-mecatl]] and [[design-decisions]].
