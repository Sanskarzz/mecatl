package session

import "strings"

// ToValidUTF8 returns s with every invalid UTF-8 byte sequence replaced by
// U+FFFD, so the durable event log, the model view, and the gRPC wire all carry
// the same text. An already-valid string is returned unchanged — which is what
// makes the three agree: once repaired here, encoding/json has nothing left to
// substitute and passes the value through untouched.
//
// It is NOT byte-identical to what encoding/json would have produced from the
// RAW string: json substitutes one U+FFFD per invalid BYTE, this one per invalid
// RUN, so "\xe2\x80" becomes two replacement runes there and one here. That only
// matters for a value repaired on one path and not the other — which is exactly
// the divergence the single choke point below exists to prevent.
//
// It exists because protobuf string fields REJECT invalid UTF-8 at marshal
// time (the Converse-stream kill, issue #402): any string that crosses into a
// proto message must be valid UTF-8 first. This is the single shared repair
// primitive consumed by BOTH the loop's effective-payload choke point
// (engine/agent execute, via RepairToolResult) and the protobuf-projection
// backstop (internal/adapter/server mapper). engine/session is the domain
// leaf (stdlib-only), so both layers may import it without violating the
// inward-only dependency rule.
func ToValidUTF8(s string) string {
	return strings.ToValidUTF8(s, "�")
}

// RepairToolResult returns r with every TEXTUAL string field normalized to
// valid UTF-8: Content, and per Parts block the Text/Name/Title/Description/
// URL/LastModified fields plus the Audience list. r is an immutable value
// object, so the repair is a copy, never a mutation.
//
// Byte-exact fields are deliberately untouched:
//   - Data []byte — binary payloads ride proto bytes fields, which carry no
//     UTF-8 rule; they must stay byte-identical.
//   - MIMEType — an IANA machine token, not prose; exactness is the contract, so
//     it is never rewritten. What keeps that safe is the TRANSPORT, not a
//     validator: validateMIME runs only on the inbound media path via NewContent,
//     and the MCP block constructors (NewResourceLinkBlock /
//     NewEmbeddedResourceBlock) do not check it — MCP metadata is simply JSON-
//     decoded before it arrives, which already coerces invalid bytes. A future
//     non-JSON producer of a block MIME type (a sniffed local file, an HTTP
//     Content-Type header) reopens issue #402 on this field.
//
// The repair is applied at the loop's effective-payload choke point (after
// PostToolUse, before the recorder/event/record) so the model view, the audit
// log, the durable log, and the client stream all carry the SAME repaired
// text — see engine/agent execute. It is NOT applied in the NewToolResult*
// constructors: those are also called by replay/test paths with
// already-persisted data, and constructor-side repair would smear the
// single-choke-point invariant across every caller.
func RepairToolResult(r ToolResult) ToolResult {
	r.Content = ToValidUTF8(r.Content)
	if len(r.Parts) == 0 {
		return r
	}
	parts := make([]Content, len(r.Parts))
	for i, p := range r.Parts {
		p.Text = ToValidUTF8(p.Text)
		p.Name = ToValidUTF8(p.Name)
		p.Title = ToValidUTF8(p.Title)
		p.Description = ToValidUTF8(p.Description)
		p.URL = ToValidUTF8(p.URL)
		p.LastModified = ToValidUTF8(p.LastModified)
		if len(p.Audience) > 0 {
			aud := make([]string, len(p.Audience))
			for j, a := range p.Audience {
				aud[j] = ToValidUTF8(a)
			}
			p.Audience = aud
		}
		parts[i] = p
	}
	r.Parts = parts
	return r
}
