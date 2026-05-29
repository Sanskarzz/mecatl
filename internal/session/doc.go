// Package session is the Agent Session bounded context: the run lifecycle,
// conversation, turns, the stop conditions, and the domain-owned event taxonomy.
//
// It holds the Session aggregate root plus the shared value objects
// (ToolCall, ToolResult, Usage, Message) that Governance and Tooling exchange,
// and the single Event taxonomy shared by the loop and the API.
//
// Allowed imports (ARCHITECTURE.md §3): the standard library only
// (and other domain packages). This package MUST NOT import adapter, agent,
// api, os, the OpenAI SDK, or any third-party library.
package session
