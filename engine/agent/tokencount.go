package agent

import "github.com/stacklok/mecatl/engine/session"

// TokenCounter estimates how many model tokens a piece of text or a slice of
// conversation messages occupies. It is a seam (ARCHITECTURE.md §8, gauntlet
// #12): the loop and the compaction cascade consume it to decide when to compact
// and which segments to drop, while the concrete tokenizer (a dependency-free
// heuristic by default, an optional tiktoken-backed adapter in production) is
// injected from the composition root.
//
// Implementations MUST be deterministic and SHOULD be cheap: Count/CountMessages
// run on every turn. A counter is never required to be exact — the loop only
// needs the estimate to be in the right ballpark — but it must be stable, since
// an unstable estimate would make compaction non-reproducible.
type TokenCounter interface {
	// Count returns the estimated token count of a single string.
	Count(text string) int
	// CountMessages returns the estimated token count of a conversation slice,
	// summing each message's text/reasoning/tool bodies plus the per-message and
	// per-role framing overhead the provider adds on the wire.
	CountMessages(msgs []session.Message) int
}

// charsPerToken is the bytes→tokens ratio the heuristic counter uses. English
// prose and code both sit near ~4 characters per token for the common
// byte-pair-encoding tokenizers, so this is a serviceable offline estimate.
const charsPerToken = 4

// perMessageOverhead is the fixed token cost the heuristic attributes to every
// message for the role tag and message framing the provider wraps each turn in
// (OpenAI documents ~3–4 framing tokens per message). Accounting for it keeps the
// estimate from undercounting many-small-message histories.
const perMessageOverhead = 4

// perToolCallOverhead is the fixed token cost the heuristic attributes to each
// tool call for its id and JSON envelope, on top of the name and argument bytes.
const perToolCallOverhead = 4

// HeuristicTokenCounter is the default, dependency-free TokenCounter. It divides
// byte length by charsPerToken and adds a small fixed overhead per message and
// per tool call so a history of many short messages is not undercounted. It
// performs no network or tokenizer-table lookup, so it is fully deterministic and
// testable offline. It replaces the inline 4-chars/token estimate the loop used
// before the TokenCounter seam existed.
type HeuristicTokenCounter struct {
	// CharsPerToken overrides charsPerToken when > 0.
	CharsPerToken int
}

// Count implements TokenCounter for a single string.
func (h HeuristicTokenCounter) Count(text string) int {
	cpt := charsPerToken
	if h.CharsPerToken > 0 {
		cpt = h.CharsPerToken
	}
	return len(text) / cpt
}

// CountMessages implements TokenCounter for a conversation slice, summing the
// text/reasoning/tool bodies (divided by the chars-per-token ratio) plus the
// fixed per-message and per-tool-call framing overhead.
func (h HeuristicTokenCounter) CountMessages(msgs []session.Message) int {
	cpt := charsPerToken
	if h.CharsPerToken > 0 {
		cpt = h.CharsPerToken
	}
	total := 0
	for _, m := range msgs {
		total += perMessageOverhead
		total += (len(m.Text) + len(m.Reasoning)) / cpt
		for _, c := range m.ToolCalls {
			total += perToolCallOverhead
			total += (len(c.Name) + len(c.Args)) / cpt
		}
		if m.ToolResult != nil {
			total += len(m.ToolResult.Content) / cpt
		}
		// Media parts are not free: a multimodal message carries image/audio bytes
		// that the provider bills. Count each part's inline byte length (URL-sourced
		// parts contribute only their reference length) so a multimodal message is
		// not undercounted and the compaction budget is not silently blown.
		for _, p := range m.Parts {
			total += (len(p.Data) + len(p.URL) + len(p.MIMEType)) / cpt
		}
	}
	return total
}

// Compile-time assertion that the default counter satisfies the seam.
var _ TokenCounter = HeuristicTokenCounter{}
