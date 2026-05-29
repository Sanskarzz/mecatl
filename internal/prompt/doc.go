// Package prompt is the domain context for two-layer system-prompt assembly: a
// cache-stable prefix plus a volatile suffix, placed so the LLM adapter can put
// a prompt-cache breakpoint between them. WP1 freezes only the Layered type and
// its render method; discovery behaviour (the <env> block, AGENTS.md/CLAUDE.md)
// lands in WP5.
//
// Allowed imports (ARCHITECTURE.md §3): the standard library only (and other
// domain packages). It MUST NOT import adapter, agent, api, os, the OpenAI SDK,
// or any third-party library.
package prompt
