package client

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// readFixturePNG loads the checked-in 1×1 PNG fixture; http.DetectContentType
// sniffs it as image/png (asserted by the image test below).
func readFixturePNG(t *testing.T) []byte {
	t.Helper()
	return readFixture(t, "pixel.png")
}

// readFixtureWAV loads the checked-in minimal WAV fixture; http.DetectContentType
// sniffs a RIFF/WAVE header as "audio/wave".
func readFixtureWAV(t *testing.T) []byte {
	t.Helper()
	return readFixture(t, "clip.wav")
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// TestExpandMentionsImagePart asserts an @-mentioned image file, with an
// image-capable server, becomes one inline image Content part: kind IMAGE, the
// sniffed image/png MIME, the exact file bytes, a "(inline)" descriptor, and no
// inlined text.
func TestExpandMentionsImagePart(t *testing.T) {
	png := readFixturePNG(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, png, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{Image: true})
	if err != nil {
		t.Fatalf("ExpandMentions: %v", err)
	}
	if len(res.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(res.Parts))
	}
	p := res.Parts[0]
	if p.GetKind() != mecatlv1.Content_KIND_IMAGE {
		t.Errorf("kind = %v, want KIND_IMAGE", p.GetKind())
	}
	if p.GetMimeType() != "image/png" {
		t.Errorf("mime = %q, want image/png", p.GetMimeType())
	}
	if !bytes.Equal(p.GetData(), png) {
		t.Errorf("data mismatch: got %d bytes, want %d", len(p.GetData()), len(png))
	}
	if len(res.Descriptors) != 1 || res.Descriptors[0] != "image/png (inline)" {
		t.Errorf("descriptors = %v, want [image/png (inline)]", res.Descriptors)
	}
	if len(res.InlineText) != 0 {
		t.Errorf("inline text = %v, want none", res.InlineText)
	}
}

// TestExpandMentionsImageCapGated asserts an image mention is REFUSED (loud error,
// zero parts) when the server's provider does not accept images.
func TestExpandMentionsImageCapGated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, readFixturePNG(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{Image: false})
	if err == nil {
		t.Fatalf("want error for image with Image:false, got parts=%v", res.Parts)
	}
	if !strings.Contains(err.Error(), "does not accept images") {
		t.Errorf("error = %q, want it to explain the image cap", err)
	}
	if len(res.Parts) != 0 {
		t.Errorf("parts = %d, want 0 on refusal", len(res.Parts))
	}
}

// TestExpandMentionsOversizeRefused asserts a single file over the per-file cap is
// refused with no part produced. The bytes start with the PNG signature so they
// sniff as image/png (taking the media branch where the cap is enforced).
func TestExpandMentionsOversizeRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.png")
	big := make([]byte, maxMediaBytes+1)
	copy(big, readFixturePNG(t)) // PNG signature → sniffed as image/png
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{Image: true})
	if err == nil {
		t.Fatalf("want oversize error, got parts=%v", res.Parts)
	}
	if !strings.Contains(err.Error(), "per-file limit") {
		t.Errorf("error = %q, want a per-file-limit message", err)
	}
	if len(res.Parts) != 0 {
		t.Errorf("parts = %d, want 0", len(res.Parts))
	}
}

// TestExpandMentionsTextInlined asserts a non-media file is inlined as a delimited
// text block (no media part, no descriptor) — caps are irrelevant for text.
func TestExpandMentionsTextInlined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	body := "hello from a text file"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{}) // no media caps
	if err != nil {
		t.Fatalf("ExpandMentions: %v", err)
	}
	if len(res.Parts) != 0 {
		t.Fatalf("parts = %d, want 0 for a text file", len(res.Parts))
	}
	if len(res.InlineText) != 1 {
		t.Fatalf("inline text blocks = %d, want 1", len(res.InlineText))
	}
	block := res.InlineText[0]
	if !strings.Contains(block, body) {
		t.Errorf("inline block missing body: %q", block)
	}
	if !strings.Contains(block, path) {
		t.Errorf("inline block missing path provenance: %q", block)
	}
}

// TestExpandMentionsTooManyParts asserts the per-prompt part-count cap rejects a
// flood of small image mentions.
func TestExpandMentionsTooManyParts(t *testing.T) {
	dir := t.TempDir()
	png := readFixturePNG(t)
	paths := make([]string, maxPromptMediaParts+1)
	for i := range paths {
		p := filepath.Join(dir, "img"+string(rune('a'+i))+".png")
		if err := os.WriteFile(p, png, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		paths[i] = p
	}

	if _, err := ExpandMentions(paths, Capabilities{Image: true}); err == nil {
		t.Fatal("want too-many-parts error")
	} else if !strings.Contains(err.Error(), "too many media attachments") {
		t.Errorf("error = %q, want a too-many-attachments message", err)
	}
}

// TestExpandMentionsTotalBytes asserts the aggregate-bytes cap rejects several
// parts that are each under the per-file cap but together exceed the prompt cap.
// Each part is half the per-file cap, so 3 of them (15 MiB) exceed the 20 MiB
// prompt cap... actually 2.x do; use enough to cross 20 MiB while each stays under
// the 10 MiB per-file cap.
func TestExpandMentionsTotalBytes(t *testing.T) {
	dir := t.TempDir()
	png := readFixturePNG(t)
	// Each ~8 MiB (< 10 MiB per-file cap); three total ~24 MiB > 20 MiB prompt cap.
	part := make([]byte, 8<<20)
	copy(part, png)
	var paths []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, "big"+string(rune('a'+i))+".png")
		if err := os.WriteFile(p, part, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		paths = append(paths, p)
	}

	if _, err := ExpandMentions(paths, Capabilities{Image: true}); err == nil {
		t.Fatal("want total-bytes error")
	} else if !strings.Contains(err.Error(), "prompt limit") {
		t.Errorf("error = %q, want a prompt-limit message", err)
	}
}

// TestExpandMentionsUnreadable asserts a missing file is a loud error (never a
// silent skip).
func TestExpandMentionsUnreadable(t *testing.T) {
	if _, err := ExpandMentions([]string{filepath.Join(t.TempDir(), "nope.png")}, Capabilities{Image: true}); err == nil {
		t.Fatal("want read error for a missing file")
	}
}

// TestExpandMentionsAudioPart asserts an @-mentioned audio file, with an
// audio-capable server, becomes one inline audio Content part: kind AUDIO, the
// sniffed audio/wave MIME, the exact file bytes, a "(inline)" descriptor, no text.
func TestExpandMentionsAudioPart(t *testing.T) {
	wav := readFixtureWAV(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.wav")
	if err := os.WriteFile(path, wav, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{Audio: true})
	if err != nil {
		t.Fatalf("ExpandMentions: %v", err)
	}
	if len(res.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(res.Parts))
	}
	p := res.Parts[0]
	if p.GetKind() != mecatlv1.Content_KIND_AUDIO {
		t.Errorf("kind = %v, want KIND_AUDIO", p.GetKind())
	}
	if p.GetMimeType() != "audio/wave" {
		t.Errorf("mime = %q, want audio/wave", p.GetMimeType())
	}
	if !bytes.Equal(p.GetData(), wav) {
		t.Errorf("data mismatch: got %d bytes, want %d", len(p.GetData()), len(wav))
	}
	if len(res.Descriptors) != 1 || res.Descriptors[0] != "audio/wave (inline)" {
		t.Errorf("descriptors = %v, want [audio/wave (inline)]", res.Descriptors)
	}
	if len(res.InlineText) != 0 {
		t.Errorf("inline text = %v, want none", res.InlineText)
	}
}

// TestExpandMentionsAudioCapGated asserts an audio mention is REFUSED (loud error,
// zero parts) when the server's provider does not accept audio.
func TestExpandMentionsAudioCapGated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.wav")
	if err := os.WriteFile(path, readFixtureWAV(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{Audio: false})
	if err == nil {
		t.Fatalf("want error for audio with Audio:false, got parts=%v", res.Parts)
	}
	if !strings.Contains(err.Error(), "does not accept audio") {
		t.Errorf("error = %q, want it to explain the audio cap", err)
	}
	if len(res.Parts) != 0 {
		t.Errorf("parts = %d, want 0 on refusal", len(res.Parts))
	}
}

// TestExpandMentionsUnsupportedFileRefused asserts an existing-but-unsupported file
// (a PDF — sniffs as application/pdf, neither text nor media) is a LOUD error and is
// NOT inlined as raw-byte garbage. The UI stat-filter passes a real file here, so
// ExpandMentions is the one that must reject the unsupported type.
func TestExpandMentionsUnsupportedFileRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(path, readFixture(t, "doc.pdf"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ExpandMentions([]string{path}, Capabilities{Image: true, Audio: true})
	if err == nil {
		t.Fatalf("want unsupported-type error, got parts=%v inline=%v", res.Parts, res.InlineText)
	}
	if !strings.Contains(err.Error(), "not a text, image, or audio file") {
		t.Errorf("error = %q, want the unsupported-type message", err)
	}
	if len(res.Parts) != 0 || len(res.InlineText) != 0 {
		t.Errorf("unsupported file leaked: parts=%v inline=%v", res.Parts, res.InlineText)
	}
}

// TestClientMediaLimitsMatchDomain is the drift sentinel: the client-side fail-
// fast caps MUST equal the domain caps in internal/session/content.go. They are
// duplicated (the ui→no-internal layering forbids importing session), so this
// pins the numbers; if the domain caps change, update both and this test.
func TestClientMediaLimitsMatchDomain(t *testing.T) {
	if maxMediaBytes != 10<<20 {
		t.Errorf("maxMediaBytes = %d, want 10<<20 (session.MaxMediaBytes)", maxMediaBytes)
	}
	if maxPromptMediaBytes != 20<<20 {
		t.Errorf("maxPromptMediaBytes = %d, want 20<<20 (session.MaxPromptMediaBytes)", maxPromptMediaBytes)
	}
	if maxPromptMediaParts != 16 {
		t.Errorf("maxPromptMediaParts = %d, want 16 (session.MaxPromptMediaParts)", maxPromptMediaParts)
	}
}
