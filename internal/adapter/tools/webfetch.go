package tools

import (
	"context"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// webFetchDescription is the model-facing documentation for the WebFetch stub.
const webFetchDescription = `Fetch a URL and return its contents. NOT IMPLEMENTED in v1.

This tool is a stub: it makes no network calls and always returns an error
saying web fetching is unavailable. It exists so the tool catalog has its final
shape; a real implementation will land in a later version. Do not rely on it.

Arguments:
- url (required): the URL that would be fetched.`

// WebFetchTool is a deliberate stub: it performs no network I/O and always
// returns a "not implemented" tool error. It is classified read-only so that,
// once implemented, it slots into the read-parallel dispatch path.
type WebFetchTool struct{}

// Compile-time assertion that WebFetchTool implements tool.Tool.
var _ tool.Tool = WebFetchTool{}

// webFetchArgs is the JSON argument shape for the WebFetch tool. It is defined
// now to lock in the tool's contract; the stub Execute does not yet parse it,
// so it is unused until the real implementation lands.
//
//nolint:unused // intentional placeholder for the not-yet-implemented WebFetch stub
type webFetchArgs struct {
	URL string `json:"url"`
}

// Spec returns the model-facing specification of the WebFetch stub.
func (WebFetchTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name:        "WebFetch",
		Description: webFetchDescription,
		Schema: schema(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "URL to fetch (not implemented in v1)."}
  },
  "required": ["url"]
}`),
	}
}

// ReadOnly reports that WebFetch is a read-only operation.
func (WebFetchTool) ReadOnly() bool { return true }

// Execute always returns a not-implemented tool error; it makes no network call.
func (WebFetchTool) Execute(_ context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	return session.NewToolError(in.ID, "WebFetch is not implemented in v1; web fetching is unavailable"), nil
}
