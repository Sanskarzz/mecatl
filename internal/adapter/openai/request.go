package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// buildParams translates a provider-neutral LLMRequest into a
// responses.ResponseNewParams for a stateless, client-owned Responses call.
//
// Mapping:
//   - System (Layered) -> Instructions (StablePrefix + VolatileSuffix rendered).
//   - Tools ([]tool.ToolSpec) -> function tools carrying their JSON schemas.
//   - Messages ([]session.Message) -> the input item array (message,
//     reasoning, function_call, function_call_output items).
//   - Model -> Model (a plain string for compatible-endpoint friendliness).
//   - Store:false + Include reasoning.encrypted_content so reasoning survives
//     across turns statelessly.
func buildParams(req port.LLMRequest) (responses.ResponseNewParams, error) {
	tools, err := buildTools(req.Tools)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	items, err := buildInput(req.Messages)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}

	params := responses.ResponseNewParams{
		Model: req.Model,
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
		Tools: tools,
		Store: oai.Bool(false),
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
		},
	}
	if instr := req.System.Render(); instr != "" {
		params.Instructions = oai.String(instr)
	}
	return params, nil
}

// buildTools converts tool specs into function ToolUnionParams. Each spec's
// Schema is the JSON schema object for the tool's arguments; it is unmarshalled
// into the map[string]any the SDK expects. An empty schema is sent as an empty
// object so the tool is always well-formed.
func buildTools(specs []tool.ToolSpec) ([]responses.ToolUnionParam, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]responses.ToolUnionParam, 0, len(specs))
	for _, s := range specs {
		var params map[string]any
		if len(s.Schema) > 0 {
			if err := json.Unmarshal(s.Schema, &params); err != nil {
				return nil, fmt.Errorf("tool %q: invalid JSON schema: %w", s.Name, err)
			}
		} else {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		fn := responses.FunctionToolParam{
			Name:       s.Name,
			Parameters: params,
			Strict:     oai.Bool(true),
		}
		if s.Description != "" {
			fn.Description = oai.String(s.Description)
		}
		out = append(out, responses.ToolUnionParam{OfFunction: &fn})
	}
	return out, nil
}

// buildInput converts the conversation history into Responses input items.
//
// Per the reasoning-preservation rule the ordering within an assistant turn is:
// reasoning item -> function_call item(s) -> (then tool results as
// function_call_output items in subsequent tool messages). User/system text
// become message items; tool messages become function_call_output items keyed by
// call_id.
func buildInput(msgs []session.Message) (responses.ResponseInputParam, error) {
	items := make(responses.ResponseInputParam, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case session.RoleSystem:
			items = append(items, responses.ResponseInputItemParamOfMessage(
				m.Text, responses.EasyInputMessageRoleSystem))
		case session.RoleUser:
			// Text-only fast path: keep the EXACT simple-string message form so the
			// byte-stable prompt prefix and every existing fixture are unchanged.
			if len(m.Parts) == 0 {
				items = append(items, responses.ResponseInputItemParamOfMessage(
					m.Text, responses.EasyInputMessageRoleUser))
				break
			}
			// Multimodal: build a content-list message (input_text + per-part media).
			content, perr := userContentList(m)
			if perr != nil {
				return nil, perr
			}
			items = append(items, responses.ResponseInputItemParamOfMessage(
				content, responses.EasyInputMessageRoleUser))
		case session.RoleAssistant:
			items = append(items, assistantItems(m)...)
		case session.RoleTool:
			if m.ToolResult != nil {
				items = append(items, responses.ResponseInputItemParamOfFunctionCallOutput(
					string(m.ToolResult.CallID), m.ToolResult.Content))
			}
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	return items, nil
}

// userContentList builds the Responses input-message content list for a
// multimodal user message: an input_text part (only when Text is non-empty)
// followed by one part per media Part. An image Part becomes an input_image
// whose image_url is the part's URL or a base64 data URL of its inline bytes
// (the Responses API accepts both in the same field). An audio Part is an honest
// hard error: the Responses input-message content union has no audio member
// (openai-go v3.37.0), and the provider declares Audio:false, so a surface
// adapter rejects audio upstream — this is the belt-and-suspenders guard for a
// part that slips through.
func userContentList(m session.Message) (responses.ResponseInputMessageContentListParam, error) {
	content := responses.ResponseInputMessageContentListParam{}
	if m.Text != "" {
		content = append(content, responses.ResponseInputContentUnionParam{
			OfInputText: &responses.ResponseInputTextParam{Text: m.Text},
		})
	}
	for _, p := range m.Parts {
		switch p.Kind {
		case session.MediaImage:
			url := p.URL
			if url == "" {
				url = dataURL(p.MIMEType, p.Data)
			}
			content = append(content, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{ImageURL: oai.String(url)},
			})
		case session.MediaAudio:
			return nil, fmt.Errorf("openai: audio input not supported by Responses API")
		default:
			return nil, fmt.Errorf("openai: unsupported media kind %q", p.Kind)
		}
	}
	return content, nil
}

// dataURL renders inline media bytes as an RFC 2397 base64 data URL
// ("data:<mime>;base64,<...>"), the form the Responses API accepts in an
// input_image image_url field.
func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// assistantItems expands an assistant message into its ordered input items:
// the reasoning item (carrying encrypted_content verbatim) first, then a
// function_call item per requested tool call, then a text message item if the
// assistant produced visible text. This ordering matches the brief's multi-turn
// rule: prior reasoning -> function_call(s).
func assistantItems(m session.Message) []responses.ResponseInputItemUnionParam {
	out := make([]responses.ResponseInputItemUnionParam, 0, len(m.ToolCalls)+2)
	if m.Reasoning != "" {
		reasoning := responses.ResponseReasoningItemParam{
			Summary:          []responses.ResponseReasoningItemSummaryParam{},
			EncryptedContent: oai.String(m.Reasoning),
		}
		out = append(out, responses.ResponseInputItemUnionParam{OfReasoning: &reasoning})
	}
	for _, call := range m.ToolCalls {
		out = append(out, responses.ResponseInputItemParamOfFunctionCall(
			string(call.Args), string(call.ID), call.Name))
	}
	if m.Text != "" {
		out = append(out, responses.ResponseInputItemParamOfMessage(
			m.Text, responses.EasyInputMessageRoleAssistant))
	}
	return out
}
