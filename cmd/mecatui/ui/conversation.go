package ui

import "github.com/stacklok/mecatl/cmd/mecatui/client"

// maxSubagentTrace caps how many child-tool chips a subagent block retains for
// the expanded trace, mirroring the line-cap idiom used elsewhere (e.g.
// maxToolResultLines). Older chips are dropped once the cap is reached so a long
// investigation never unbounds the card.
const maxSubagentTrace = 12

// subToolChip is one entry in a subagent block's redacted child-tool trace: the
// child tool's NAME and whether it errored. It deliberately holds no args or
// result content — only the metadata forwarded by subagent.tool.
type subToolChip struct {
	name    string
	isError bool
}

// blockKind classifies a scrollback block so the renderer knows how to style it.
type blockKind int

const (
	blockUser      blockKind = iota // a user prompt
	blockAssistant                  // streamed assistant markdown (+ optional reasoning summary)
	blockTool                       // a tool call (+ its resolved result)
	blockNotice                     // compaction / muted info
	blockTurnStat                   // muted per-turn usage + elapsed stat line
	blockError                      // an error notice
	blockHook                       // a structured hook notice (phase + decision)
)

// block is one entry in the conversation scrollback. Assistant blocks accumulate
// streamed deltas in raw (re-rendered through glamour each delta); tool blocks
// carry their call args and, once resolved, their result. Keeping the raw text
// on the block (not a pre-rendered string) lets a theme/width change re-render
// the whole history correctly.
type block struct {
	kind blockKind

	raw string // user text, assistant markdown buffer, or notice text

	// Reasoning is an ATTRIBUTE of the assistant block, not a sibling: a turn's
	// reasoning-summary deltas and answer-text deltas can interleave on the wire
	// (separate SSE events), so all of a turn's reasoning accumulates here and
	// renders as one dim, collapsed header above the merged answer. reasoning is
	// the accumulated summary text; reasoningStreaming is true while reasoning is
	// still arriving and the answer text has not started (drives the live
	// "reasoning…" affordance).
	reasoning          string
	reasoningStreaming bool

	// Tool-block fields.
	toolID      string
	toolName    string
	toolArgs    string
	resolved    bool
	resultBody  string
	resultError bool

	// Subagent fields (attached to a Task tool block): the REDACTED,
	// metadata-only projection of the Task's child run. They never carry child
	// content. subagent is true once a subagent.start has been attributed to this
	// block; subGoal is the card title; subTrace is a capped trace of child tool
	// chips; subToolCount is the running/final child tool count; subUsage,
	// subStop, and subDurationMs are the resolved end stats (subDone gates them).
	subagent      bool
	subGoal       string
	subTrace      []subToolChip
	subToolCount  int
	subUsage      client.Usage
	subStop       string
	subDurationMs int64
	subDone       bool

	// Hook-block fields (blockHook): the structured phase/tool/decision used to
	// render a hook notice distinctly from a compaction notice and colour a
	// blocked hook.
	hookPhase    string
	hookTool     string
	hookDecision string // "info" | "blocked" | "modified"
}

// conversation is the ordered scrollback. It owns block creation/mutation so the
// model never pokes blocks directly; render.go turns it into the viewport string.
type conversation struct {
	blocks []block
}

// addUser appends a user-prompt block.
func (c *conversation) addUser(text string) {
	c.blocks = append(c.blocks, block{kind: blockUser, raw: text})
}

// startAssistant opens a fresh, empty assistant block to accumulate deltas into.
// Called on turn.start so each turn is its own markdown block.
func (c *conversation) startAssistant() {
	c.blocks = append(c.blocks, block{kind: blockAssistant})
}

// appendAssistant appends streamed text to the current assistant block, opening
// one if the last block isn't an (unfinished) assistant block — defensive against
// a delta arriving before turn.start. The first answer text of a turn ends the
// "reasoning…" live affordance (the reasoning summary, if any, freezes into its
// static collapsed header).
func (c *conversation) appendAssistant(text string) {
	if b := c.currentAssistant(); b != nil {
		b.raw += text
		b.reasoningStreaming = false
		return
	}
	c.blocks = append(c.blocks, block{kind: blockAssistant, raw: text})
}

// appendReasoning accumulates streamed reasoning-summary text into the current
// turn's assistant block. Reasoning is an attribute of that block (not a
// reordered sibling) precisely because reasoning and answer deltas can interleave
// within one turn — folding it in means a single reasoning region always renders,
// above the merged answer, with a truthful line count. A reasoning delta arriving
// before any assistant block (e.g. a delta racing turn.start) opens one rather
// than dropping the text.
func (c *conversation) appendReasoning(text string) {
	b := c.currentAssistant()
	if b == nil {
		c.blocks = append(c.blocks, block{kind: blockAssistant})
		b = &c.blocks[len(c.blocks)-1]
	}
	b.reasoning += text
	// Reasoning is still "live" only while the answer text has not started.
	if b.raw == "" {
		b.reasoningStreaming = true
	}
}

// endReasoningStream clears the live "reasoning…" affordance on the current
// assistant block (called on turn.end), so a turn that streamed reasoning but no
// answer text settles into the static collapsed header.
func (c *conversation) endReasoningStream() {
	if b := c.currentAssistant(); b != nil {
		b.reasoningStreaming = false
	}
}

// currentAssistant returns the trailing assistant block (the one being streamed
// into this turn) or nil when the last block is not an assistant block.
func (c *conversation) currentAssistant() *block {
	if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blockAssistant {
		return &c.blocks[n-1]
	}
	return nil
}

// addTurnStat appends a muted per-turn usage/elapsed stat line.
func (c *conversation) addTurnStat(text string) {
	c.blocks = append(c.blocks, block{kind: blockTurnStat, raw: text})
}

// addTool appends a running tool-call block.
func (c *conversation) addTool(id, name, args string) {
	c.blocks = append(c.blocks, block{
		kind:     blockTool,
		toolID:   id,
		toolName: name,
		toolArgs: args,
	})
}

// resolveTool marks the tool block matching callID (its toolID) as resolved with
// its result. Matching is by id only — never by tool name — mirroring the
// tool_call.id ⇄ tool_result.call_id contract. Returns false if no match (the
// caller can then render an orphan result notice).
func (c *conversation) resolveTool(callID, body string, isErr bool) bool {
	for i := len(c.blocks) - 1; i >= 0; i-- {
		b := &c.blocks[i]
		if b.kind == blockTool && b.toolID == callID && !b.resolved {
			b.resolved = true
			b.resultBody = body
			b.resultError = isErr
			return true
		}
	}
	return false
}

// subagentBlock returns the unresolved Task tool block whose toolID matches
// parentCallID, or nil if none. Matching is by id only — the SAME contract as
// resolveTool — so a subagent.* event is attributed to its originating Task card
// even with several Task cards interleaved. It scans from the end so the most
// recent matching call wins.
func (c *conversation) subagentBlock(parentCallID string) *block {
	for i := len(c.blocks) - 1; i >= 0; i-- {
		b := &c.blocks[i]
		if b.kind == blockTool && b.toolID == parentCallID {
			return b
		}
	}
	return nil
}

// setSubagentStart marks the Task block matching parentCallID as a subagent and
// records its goal title. Returns false when no matching block exists.
func (c *conversation) setSubagentStart(parentCallID, goal string) bool {
	b := c.subagentBlock(parentCallID)
	if b == nil {
		return false
	}
	b.subagent = true
	b.subGoal = goal
	return true
}

// addSubagentTool appends a child-tool chip (name + error) to the matching Task
// block's trace and bumps its running tool count. The trace is capped at
// maxSubagentTrace (oldest chips dropped); the count is the authoritative running
// total carried by the event, not len(trace). Returns false when no match.
func (c *conversation) addSubagentTool(parentCallID, toolName string, isError bool, toolCount int) bool {
	b := c.subagentBlock(parentCallID)
	if b == nil {
		return false
	}
	b.subagent = true
	b.subToolCount = toolCount
	b.subTrace = append(b.subTrace, subToolChip{name: toolName, isError: isError})
	if len(b.subTrace) > maxSubagentTrace {
		b.subTrace = b.subTrace[len(b.subTrace)-maxSubagentTrace:]
	}
	return true
}

// setSubagentEnd records the resolved end stats (usage, final tool count, stop,
// duration) on the matching Task block. Returns false when no match.
func (c *conversation) setSubagentEnd(parentCallID string, usage client.Usage, toolCount int, stop string, durationMs int64) bool {
	b := c.subagentBlock(parentCallID)
	if b == nil {
		return false
	}
	b.subagent = true
	b.subDone = true
	b.subUsage = usage
	b.subToolCount = toolCount
	b.subStop = stop
	b.subDurationMs = durationMs
	return true
}

// addNotice appends a muted info block (compaction / permission verb).
func (c *conversation) addNotice(text string) {
	c.blocks = append(c.blocks, block{kind: blockNotice, raw: text})
}

// addHook appends a structured hook-notice block carrying the lifecycle phase,
// the related tool (per-tool phases), and the decision. The renderer styles it
// distinctly from a plain notice — a hook glyph + phase, with the outcome
// coloured (blocked stands out from a benign info/modified notice).
func (c *conversation) addHook(text, phase, tool, decision string) {
	c.blocks = append(c.blocks, block{
		kind:         blockHook,
		raw:          text,
		hookPhase:    phase,
		hookTool:     tool,
		hookDecision: decision,
	})
}

// addError appends an error block.
func (c *conversation) addError(text string) {
	c.blocks = append(c.blocks, block{kind: blockError, raw: text})
}
