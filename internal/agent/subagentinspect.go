package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// subagentinspect.go implements the on-demand, PULL subagent-transcript inspection
// tool, the sibling of InspectMember (teaminspect.go). It is a parent-catalog tool the
// parent LLM calls DELIBERATELY to read ONE persisted subagent's transcript by its
// agent id — the value of the 'agentId:' line the Subagent tool result surfaces. It
// does NOT auto-inject any transcript: the pulled transcript enters the parent
// Conversation only as this tool's own ToolResult — exactly like every tool, and the
// parent's explicit choice — so gauntlet #7's "no AUTO-injection" property is preserved.

// inspectSubagentToolName is the catalog name of the subagent-inspection tool.
const inspectSubagentToolName = "InspectSubagent"

// InspectSubagentTool reads one persisted subagent session via a port.SessionStore and
// returns a BOUNDED rendering of its conversation as its ToolResult. It is PULL and
// read-only. The agent_id it takes IS the session id verbatim — no derivation — so the
// id from a Subagent result's 'agentId:' line loads directly.
type InspectSubagentTool struct {
	// store reads subagent sessions by id. Injected by the composition root; the tool
	// consumes the port.SessionStore interface, never a concrete adapter.
	store port.SessionStore
	// requiredPrefix gates which ids this tool will load from the SHARED session store:
	// an agent_id that does not start with it is REJECTED before the store is touched,
	// so the model cannot read team-member ("team-<teamID>-<member>") or service-session
	// transcripts through this tool, bypassing InspectMember's team_id+member framing.
	// It must match SubagentTool's child-session prefix (the default idPrefix+"-", i.e.
	// "subagent-"); a deployment using WithChildSessionPrefix would need a matching
	// option here — noted, not built, until such a deployment exists.
	requiredPrefix string
}

// inspectSubagentArgs is the model-supplied argument payload.
type inspectSubagentArgs struct {
	// AgentID is the subagent's session id verbatim, as seen on the 'agentId:' line of
	// its Subagent tool result.
	AgentID string `json:"agent_id"`
}

// inspectSubagentSchema is the JSON schema the model sees for the tool's arguments.
var inspectSubagentSchema = json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"The subagent's id, exactly as shown on the 'agentId:' line of its Subagent tool result."}},"required":["agent_id"]}`)

// NewInspectSubagentTool constructs the InspectSubagent tool over a session store. store
// must be non-nil; NewInspectSubagentTool panics otherwise (a composition-root
// programming error — the tool has nothing to read without a store).
func NewInspectSubagentTool(store port.SessionStore) tool.Tool {
	if store == nil {
		panic("agent: NewInspectSubagentTool requires a non-nil session store")
	}
	return &InspectSubagentTool{store: store, requiredPrefix: "subagent-"}
}

// Spec returns the model-facing specification for the InspectSubagent tool.
func (*InspectSubagentTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name: inspectSubagentToolName,
		// NOTE: this quotes "~40 messages"; keep that phrase in sync if
		// maxInspectMessages changes (the same sync NOTE InspectMember carries).
		Description: "Read a subagent's transcript by its agent id (the 'agentId:' line on the " +
			"Subagent tool result). Returns a BOUNDED rendering — the last ~40 messages, each " +
			"clamped — not the full raw transcript. Use it to debug a subagent that stopped or " +
			"errored, to verify how it reached its conclusion, or to pull a detail its summary " +
			"omitted. Each call folds that transcript into this conversation and consumes context " +
			"budget, so prefer the subagent's summary when it suffices.",
		Schema: inspectSubagentSchema,
	}
}

// ReadOnly reports that InspectSubagent only READS the store (no workspace mutation),
// so the dispatcher may run it read-parallel.
func (*InspectSubagentTool) ReadOnly() bool { return true }

// Execute loads the subagent's persisted session by its agent id (the id IS the session
// id, verbatim — no derivation) and renders a bounded transcript. An unknown id is a
// model-addressable error (the subagent may not have run yet).
func (t *InspectSubagentTool) Execute(ctx context.Context, call session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args inspectSubagentArgs
	if msg, ok := session.ParseArgs(call, &args); !ok {
		return session.NewToolError(call.ID, "InspectSubagent: "+msg), nil
	}
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		return session.NewToolError(call.ID, "InspectSubagent: 'agent_id' is required and must be non-empty"), nil
	}
	// Prefix gate: only SUBAGENT sessions are inspectable here. The shared store also
	// holds team-member and service sessions; rejecting non-subagent ids BEFORE the load
	// keeps this tool from becoming a verbatim read of the whole store (team member
	// transcripts go through InspectMember's team_id+member framing instead). The same
	// gate guards the planned `resume` path.
	if !strings.HasPrefix(id, t.requiredPrefix) {
		return session.NewToolError(call.ID, fmt.Sprintf(
			"InspectSubagent: agent id %q is not a subagent session; only ids from a Subagent result's 'agentId:' line can be inspected (team member transcripts are read via InspectMember)", id)), nil
	}

	sess, err := t.store.Load(ctx, session.SessionID(id))
	switch {
	case errors.Is(err, port.ErrSessionNotFound) || (err == nil && sess == nil):
		// Genuine not-found: a model-addressable miss the parent can reason about.
		return session.NewToolError(call.ID, fmt.Sprintf(
			"InspectSubagent: no transcript for agent id %q; use the id exactly as shown on the 'agentId:' line of a Subagent result (the subagent may not have run yet)", id)), nil
	case err != nil:
		// A real infrastructure failure (I/O, decode) — surfaced DISTINCTLY so a broken
		// store is not silently misreported as "no transcript". Still a model-addressable
		// error result (never a harness error) so the loop continues.
		return session.NewToolError(call.ID, fmt.Sprintf(
			"InspectSubagent: failed to load agent %q: %v", id, err)), nil
	}
	return session.NewToolResult(call.ID, renderInspectTranscript(
		fmt.Sprintf("Transcript of subagent %q:", id), sess)), nil
}

// Compile-time assertion that InspectSubagentTool satisfies the Tool contract.
var _ tool.Tool = (*InspectSubagentTool)(nil)
