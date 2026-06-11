package acp

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/engine/session"
)

// pngData is a few inline bytes used as image content; the validators check the
// mime, not the byte signature, so any non-empty payload is fine for these tests.
var pngB64 = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake-image-bytes"))

// asMethodErr unwraps the *MethodError a loud reject returns, failing if err is
// nil or not a MethodError.
func asMethodErr(t *testing.T, err error) *MethodError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a loud error, got nil")
	}
	var me *MethodError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MethodError, got %T: %v", err, err)
	}
	if me.Code != codeInvalidParams {
		t.Fatalf("expected codeInvalidParams (%d), got %d", codeInvalidParams, me.Code)
	}
	return me
}

func TestBuildPromptContentTextAndImage(t *testing.T) {
	text, parts, err := buildPromptContent([]contentBlock{
		{Type: "text", Text: "look at this"},
		{Type: "image", MimeType: "image/png", Data: pngB64},
	})
	if err != nil {
		t.Fatalf("buildPromptContent: %v", err)
	}
	if text != "look at this" {
		t.Fatalf("text = %q, want %q", text, "look at this")
	}
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	p := parts[0]
	if p.Kind != session.MediaImage || p.MIMEType != "image/png" {
		t.Fatalf("part = %+v, want image/png image", p)
	}
	if len(p.Data) == 0 || p.URL != "" {
		t.Fatalf("part should be inline-data (Data set, URL empty), got %+v", p)
	}
}

func TestBuildPromptContentImageURL(t *testing.T) {
	_, parts, err := buildPromptContent([]contentBlock{
		{Type: "image", MimeType: "image/png", URI: "https://example.com/cat.png"},
	})
	if err != nil {
		t.Fatalf("buildPromptContent: %v", err)
	}
	if len(parts) != 1 || parts[0].URL != "https://example.com/cat.png" || len(parts[0].Data) != 0 {
		t.Fatalf("want one URL-sourced image part, got %+v", parts)
	}
}

func TestBuildPromptContentInlineTextResourceFlattens(t *testing.T) {
	text, parts, err := buildPromptContent([]contentBlock{
		{Type: "text", Text: "header"},
		{Type: "resource", Resource: &resourceContents{URI: "file:///a.txt", MimeType: "text/plain", Text: "embedded body"}},
	})
	if err != nil {
		t.Fatalf("buildPromptContent: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("inline-text resource must NOT produce a Part, got %d", len(parts))
	}
	if text != "header\nembedded body" {
		t.Fatalf("text = %q, want the resource text flattened in", text)
	}
}

func TestBuildPromptContentBlobResourceBecomesImage(t *testing.T) {
	_, parts, err := buildPromptContent([]contentBlock{
		{Type: "resource", Resource: &resourceContents{URI: "file:///a.png", MimeType: "image/png", Blob: pngB64}},
	})
	if err != nil {
		t.Fatalf("buildPromptContent: %v", err)
	}
	if len(parts) != 1 || parts[0].Kind != session.MediaImage {
		t.Fatalf("want one image part from the blob resource, got %+v", parts)
	}
}

func TestBuildPromptContentResourceLinkRejected(t *testing.T) {
	_, _, err := buildPromptContent([]contentBlock{
		{Type: "resource_link", URI: "https://example.com/spec.md"},
	})
	me := asMethodErr(t, err)
	if !strings.Contains(me.Message, "resource_link") {
		t.Fatalf("error should mention resource_link, got %q", me.Message)
	}
}

func TestBuildPromptContentUnknownTypeRejected(t *testing.T) {
	_, _, err := buildPromptContent([]contentBlock{
		{Type: "video", Text: ""},
	})
	me := asMethodErr(t, err)
	if !strings.Contains(me.Message, "unsupported content type") {
		t.Fatalf("error should mention unsupported content type, got %q", me.Message)
	}
}

func TestBuildPromptContentSSRFURLRejected(t *testing.T) {
	// An internal/metadata URL must be rejected by the reused ValidateMediaURL —
	// proving the ACP boundary goes through the same SSRF backstop as gRPC/HTTP.
	_, _, err := buildPromptContent([]contentBlock{
		{Type: "image", MimeType: "image/png", URI: "https://169.254.169.254/latest/meta-data/"},
	})
	asMethodErr(t, err) // any loud codeInvalidParams is correct; the cause is the SSRF guard
}

func TestBuildPromptContentBadBase64Rejected(t *testing.T) {
	_, _, err := buildPromptContent([]contentBlock{
		{Type: "image", MimeType: "image/png", Data: "!!!not base64!!!"},
	})
	me := asMethodErr(t, err)
	if !strings.Contains(me.Message, "base64") {
		t.Fatalf("error should mention base64, got %q", me.Message)
	}
}

func TestBuildPromptContentResourceNoContentsRejected(t *testing.T) {
	_, _, err := buildPromptContent([]contentBlock{
		{Type: "resource", Resource: &resourceContents{URI: "file:///x", MimeType: "image/png"}},
	})
	asMethodErr(t, err)
}

func TestBuildPromptContentResourceBothTextAndBlobRejected(t *testing.T) {
	// A resource carrying BOTH text and blob is ambiguous (ACP is text XOR blob);
	// it must be rejected loudly, not have one silently dropped.
	_, _, err := buildPromptContent([]contentBlock{
		{Type: "resource", Resource: &resourceContents{
			URI: "file:///a", MimeType: "image/png", Text: "body", Blob: base64.StdEncoding.EncodeToString([]byte("x")),
		}},
	})
	me := asMethodErr(t, err)
	if !strings.Contains(me.Message, "both text and blob") {
		t.Fatalf("error should mention both text and blob, got %q", me.Message)
	}
}

func TestBuildPromptContentAudioParsed(t *testing.T) {
	// Audio parses into a MediaAudio part regardless of provider support; the
	// provider gate (rejectUnsupportedMedia) is a separate, later step.
	_, parts, err := buildPromptContent([]contentBlock{
		{Type: "audio", MimeType: "audio/wav", Data: base64.StdEncoding.EncodeToString([]byte("RIFFfake"))},
	})
	if err != nil {
		t.Fatalf("buildPromptContent: %v", err)
	}
	if len(parts) != 1 || parts[0].Kind != session.MediaAudio || parts[0].MIMEType != "audio/wav" {
		t.Fatalf("want one audio/wav part, got %+v", parts)
	}
}
