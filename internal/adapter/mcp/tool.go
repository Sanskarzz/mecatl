package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// remoteTool adapts a single tool advertised by a remote MCP server into the
// harness's tool.Tool interface. Execute proxies the call back over the live
// MCP session; the Workspace argument is ignored because the tool runs remotely.
//
// It holds the owning *Server (not a *ClientSession) so Execute can re-establish
// a dropped session transparently via Server.withSession (see reconnect.go,
// ADR 0056).
type remoteTool struct {
	spec     tool.ToolSpec
	readOnly bool
	// remoteName is the tool's name on the server, used in the CallTool request
	// (NOT the namespaced spec.Name the model sees).
	remoteName string
	server     *Server
}

// newRemoteTool builds a remoteTool from a server-advertised mcpsdk.Tool.
//
// Namespacing: the model-facing name is mcp__<server>__<toolname>, which cannot
// collide with built-in tool names. ReadOnly defaults to false (conservative,
// so the dispatcher serializes the call) unless the remote advertises
// annotations.readOnlyHint == true. The schema is the remote inputSchema
// marshaled to json.RawMessage.
func newRemoteTool(serverName string, srv *Server, remote *mcpsdk.Tool) (*remoteTool, error) {
	if remote == nil {
		return nil, fmt.Errorf("mcp: server %q advertised a nil tool", serverName)
	}
	if remote.Name == "" {
		return nil, fmt.Errorf("mcp: server %q advertised a tool with no name", serverName)
	}

	schema, err := schemaFor(remote.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("mcp: tool %q on server %q: input schema: %w", remote.Name, serverName, err)
	}

	readOnly := remote.Annotations != nil && remote.Annotations.ReadOnlyHint

	return &remoteTool{
		spec: tool.ToolSpec{
			Name:        namespacedName(serverName, remote.Name),
			Description: remote.Description,
			Schema:      schema,
		},
		readOnly:   readOnly,
		remoteName: remote.Name,
		server:     srv,
	}, nil
}

// namespacedName produces the mcp__<server>__<tool> model-facing name.
func namespacedName(server, toolName string) string {
	return "mcp__" + server + "__" + toolName
}

// schemaFor normalizes the SDK's input schema (which arrives client-side as a
// map[string]any) into a json.RawMessage. A nil schema yields an empty
// object-schema so the model always sees valid JSON.
func schemaFor(in any) (json.RawMessage, error) {
	if in == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	if raw, ok := in.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// Spec returns the model-facing specification (namespaced name, remote
// description, remote input schema).
func (t *remoteTool) Spec() tool.ToolSpec { return t.spec }

// ReadOnly reports the remote read-only hint (defaulting to false).
func (t *remoteTool) ReadOnly() bool { return t.readOnly }

// Execute proxies the call to the remote MCP server over the live session and
// maps the MCP result into a session.ToolResult.
//
// The Workspace is intentionally ignored: MCP tools execute on the remote
// server, not against the local workspace. ctx cancellation is honored by the
// SDK call. A transport/protocol fault is returned as a model-facing tool error
// (IsError) rather than a Go error, so the loop feeds it back to the model for
// self-correction instead of aborting the turn; the Go error return is reserved
// for cases the model genuinely cannot recover from (none here).
func (t *remoteTool) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	// Respect cancellation before doing any work.
	if err := ctx.Err(); err != nil {
		return session.ToolResult{}, err
	}

	args, err := argsFor(in.Args)
	if err != nil {
		return session.NewToolError(in.ID, fmt.Sprintf("invalid tool arguments: %v", err)), nil
	}

	var res *mcpsdk.CallToolResult
	callErr := t.server.withSession(ctx, func(sess *mcpsdk.ClientSession) error {
		var err error
		res, err = sess.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      t.remoteName,
			Arguments: args,
		})
		return err
	})
	if callErr != nil {
		// Propagate context cancellation as a hard Go error so the loop can
		// distinguish an aborted turn from a recoverable tool failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return session.ToolResult{}, ctxErr
		}
		// A connection-drop after the one reconnect attempt (the retry-drop
		// case) OR a reconnect DIAL failure (errReconnectFailed — the server
		// could not be re-established, e.g. a genuinely-down server) is a
		// terminal "server unavailable" — surface the clear message, not the
		// raw transport string ("connection refused" / "context deadline
		// exceeded"). Any other fault is surfaced verbatim.
		if isConnectionDrop(callErr) || errors.Is(callErr, errReconnectFailed) {
			return session.NewToolError(in.ID,
				fmt.Sprintf("mcp call failed: MCP server %q unavailable after reconnect", t.server.Name())), nil
		}
		return session.NewToolError(in.ID, fmt.Sprintf("mcp call failed: %v", callErr)), nil
	}

	content := flattenContent(res.Content)
	if res.IsError {
		return session.NewToolError(in.ID, content), nil
	}
	return session.NewToolResult(in.ID, content), nil
}

// argsFor decodes the model's raw JSON args into the any value the SDK marshals
// back to JSON. Empty/absent args map to nil (no arguments).
func argsFor(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// flattenContent concatenates the textual parts of an MCP result. Text content
// is included verbatim; non-text content (images, audio, embedded resources)
// is summarized by type so the model knows something non-textual came back
// without the adapter inventing an encoding.
func flattenContent(parts []mcpsdk.Content) string {
	var b strings.Builder
	for _, p := range parts {
		switch c := p.(type) {
		case *mcpsdk.TextContent:
			b.WriteString(c.Text)
		case *mcpsdk.ImageContent:
			fmt.Fprintf(&b, "[image content: %s]", c.MIMEType)
		case *mcpsdk.AudioContent:
			fmt.Fprintf(&b, "[audio content: %s]", c.MIMEType)
		case *mcpsdk.ResourceLink:
			fmt.Fprintf(&b, "[resource link: %s]", c.URI)
		case *mcpsdk.EmbeddedResource:
			b.WriteString("[embedded resource]")
		default:
			b.WriteString("[unsupported content]")
		}
	}
	return b.String()
}
