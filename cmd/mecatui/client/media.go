package client

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// Client-side media caps. These DUPLICATE the domain caps in
// internal/session/content.go:29-36 (MaxMediaBytes / MaxPromptMediaBytes /
// MaxPromptMediaParts) so the ui can fail fast — before opening a stream — with a
// clear inline error rather than round-tripping a doomed prompt. They are NOT the
// source of truth: the server re-validates every part via session.ValidateMediaParts
// at the wire→domain boundary, so a drift here only changes WHERE the rejection
// surfaces, never WHETHER an oversize prompt is accepted. The ui layer must not
// import internal/session (the ui→no-internal layering rule), hence the copy; the
// drift sentinel TestClientMediaLimitsMatchDomain pins the numbers to the domain.
const (
	maxMediaBytes       = 10 << 20 // 10 MiB — single inline part (== session.MaxMediaBytes)
	maxPromptMediaBytes = 20 << 20 // 20 MiB — sum of all inline parts (== session.MaxPromptMediaBytes)
	maxPromptMediaParts = 16       // count cap (== session.MaxPromptMediaParts)
)

// sniffLen is how many leading bytes http.DetectContentType inspects; it never
// reads past 512 itself, so capping the slice avoids copying a large file's tail
// into the sniff window.
const sniffLen = 512

// MediaResult is the outcome of expanding a prompt's @-mentions: the built media
// parts to send over the wire, a human-readable descriptor per part (for the
// transcript's "📎 …" placeholder lines), and any inlined text-file bodies (each
// already wrapped in a delimited block) the ui splices into the prompt text.
type MediaResult struct {
	// Parts are the proto Content parts to attach to the Prompt frame.
	Parts []*mecatlv1.Content
	// Descriptors mirror Parts 1:1: one human descriptor per media part, e.g.
	// "image/png (inline)". They feed conversation.addUserWithMedia for the 📎 lines.
	Descriptors []string
	// InlineText holds the delimited bodies of @-mentioned TEXT files (no media
	// kind sniffed), in mention order. The ui appends these to the prompt text so
	// the model sees the file content inline (Gemini-style), since text is not a
	// media part.
	InlineText []string
}

// ExpandMentions reads each @-mentioned path (the UI has already stat-filtered
// these to EXISTING REGULAR FILES; a token that is not a real file stays literal
// prose and never reaches here), sniffs its content type, and routes it to exactly
// one of the three spec outcomes:
//
//   - image/* or audio/* → an inline media Content part (caps-gated and size-capped);
//   - text/*             → inlined into the prompt as a delimited block;
//   - anything else      → a CLEAR ERROR (an explicitly-@'d binary such as a PDF or
//     zip is NOT inlined as raw-byte garbage; the user gets a loud refusal).
//
// It is the SINGLE place proto Content is constructed from a file on the client
// side — the ui passes only resolved path strings and the proto-free Capabilities,
// keeping the ui free of proto + os.
//
// It is LOUD on any failure (unreadable file, an unsupported file type, a media
// kind the server's provider cannot consume, an oversize part, too many parts, or
// too many total bytes): ANY error returns a zero result and that error, and the
// caller must send NOTHING (loud-reject, never silent-drop). It stays defensive —
// it still errors if handed a path it cannot read — but the UI stat-filter is the
// gate that decides attachment-vs-prose. Per-part and aggregate size caps mirror
// the domain (see the const block); the server re-validates regardless.
func ExpandMentions(paths []string, caps Capabilities) (MediaResult, error) {
	var res MediaResult
	total := 0
	for _, p := range paths {
		data, err := os.ReadFile(p) //nolint:gosec // path is a user-typed @-mention resolved against the workspace; reading it is the feature.
		if err != nil {
			return MediaResult{}, fmt.Errorf("read %q: %w", p, err)
		}
		mime := http.DetectContentType(data[:min(sniffLen, len(data))])
		kind := mediaKind(mime)
		switch kind {
		case mecatlv1.Content_KIND_IMAGE:
			if !caps.Image {
				return MediaResult{}, fmt.Errorf("%q is an image (%s) but the server's model does not accept images", p, mime)
			}
		case mecatlv1.Content_KIND_AUDIO:
			if !caps.Audio {
				return MediaResult{}, fmt.Errorf("%q is audio (%s) but the server's model does not accept audio", p, mime)
			}
		default: // KIND_UNSPECIFIED: not media → either text (inline) or unsupported (error).
			if strings.HasPrefix(mime, "text/") {
				res.InlineText = append(res.InlineText, inlineTextBlock(p, data))
				continue
			}
			return MediaResult{}, fmt.Errorf("%q (%s) is not a text, image, or audio file", p, mime)
		}
		if len(data) > maxMediaBytes {
			return MediaResult{}, fmt.Errorf("%q is %d bytes, over the %d-byte per-file limit", p, len(data), maxMediaBytes)
		}
		total += len(data)
		res.Parts = append(res.Parts, &mecatlv1.Content{Kind: kind, MimeType: mime, Data: data})
		res.Descriptors = append(res.Descriptors, mime+" (inline)")
	}
	if len(res.Parts) > maxPromptMediaParts {
		return MediaResult{}, fmt.Errorf("too many media attachments (%d, limit %d)", len(res.Parts), maxPromptMediaParts)
	}
	if total > maxPromptMediaBytes {
		return MediaResult{}, fmt.Errorf("media attachments total %d bytes, over the %d-byte prompt limit", total, maxPromptMediaBytes)
	}
	return res, nil
}

// mediaKind maps a sniffed MIME type to a proto Content kind: image/* → IMAGE,
// audio/* → AUDIO, anything else → KIND_UNSPECIFIED (the caller distinguishes text
// from unsupported). mime may carry a "; charset=…" suffix (http.DetectContentType
// adds one for text), so it is matched by prefix.
func mediaKind(mime string) mecatlv1.Content_Kind {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return mecatlv1.Content_KIND_IMAGE
	case strings.HasPrefix(mime, "audio/"):
		return mecatlv1.Content_KIND_AUDIO
	default:
		return mecatlv1.Content_KIND_UNSPECIFIED
	}
}

// inlineTextBlock wraps a text file's content in a labelled, fenced block so the
// model sees it as an attachment with provenance rather than ambiguous prose
// blended into the prompt. The path is the user-typed mention.
func inlineTextBlock(path string, data []byte) string {
	return fmt.Sprintf("--- %s ---\n%s\n--- end %s ---", path, string(data), path)
}
