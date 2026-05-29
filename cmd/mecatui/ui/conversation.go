package ui

// blockKind classifies a scrollback block so the renderer knows how to style it.
type blockKind int

const (
	blockUser      blockKind = iota // a user prompt
	blockAssistant                  // streamed assistant markdown
	blockTool                       // a tool call (+ its resolved result)
	blockNotice                     // hook / compaction / muted info
	blockError                      // an error notice
)

// block is one entry in the conversation scrollback. Assistant blocks accumulate
// streamed deltas in raw (re-rendered through glamour each delta); tool blocks
// carry their call args and, once resolved, their result. Keeping the raw text
// on the block (not a pre-rendered string) lets a theme/width change re-render
// the whole history correctly.
type block struct {
	kind blockKind

	raw string // user text, assistant markdown buffer, or notice text

	// Tool-block fields.
	toolID      string
	toolName    string
	toolArgs    string
	resolved    bool
	resultBody  string
	resultError bool
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
// a delta arriving before turn.start.
func (c *conversation) appendAssistant(text string) {
	if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == blockAssistant {
		c.blocks[n-1].raw += text
		return
	}
	c.blocks = append(c.blocks, block{kind: blockAssistant, raw: text})
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

// addNotice appends a muted info block (hook / compaction).
func (c *conversation) addNotice(text string) {
	c.blocks = append(c.blocks, block{kind: blockNotice, raw: text})
}

// addError appends an error block.
func (c *conversation) addError(text string) {
	c.blocks = append(c.blocks, block{kind: blockError, raw: text})
}
