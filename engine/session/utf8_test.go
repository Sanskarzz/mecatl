package session

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// invalidUTF8 is the orphaned-lead-byte sequence from issue #402: BSD `cat -t`
// turns a valid em dash (E2 80 94) into E2 4D 2D 5E 40 4D 2D 5E 54 — the E2
// lead byte is retained while its continuation bytes are rendered as ASCII,
// leaving an invalid sequence.
const invalidUTF8 = "\xe2M-^@M-^T"

func TestToValidUTF8(t *testing.T) {
	if got := ToValidUTF8("plain ascii"); got != "plain ascii" {
		t.Fatalf("valid input changed: %q", got)
	}
	if got := ToValidUTF8("valid — em dash"); got != "valid — em dash" {
		t.Fatalf("valid multibyte changed: %q", got)
	}
	got := ToValidUTF8(invalidUTF8)
	if !utf8.ValidString(got) {
		t.Fatalf("repair left invalid UTF-8: %q", got)
	}
	if !strings.ContainsRune(got, '�') {
		t.Fatalf("expected U+FFFD replacement in %q", got)
	}
}

func TestRepairToolResultContent(t *testing.T) {
	r := NewToolResult("c1", "before "+invalidUTF8+" after")
	out := RepairToolResult(r)
	if !utf8.ValidString(out.Content) {
		t.Fatalf("Content still invalid: %q", out.Content)
	}
	if !strings.ContainsRune(out.Content, '�') {
		t.Fatalf("expected U+FFFD in repaired Content: %q", out.Content)
	}
	// Immutable value object: the input is not mutated.
	if utf8.ValidString(r.Content) {
		t.Fatalf("input was mutated: %q", r.Content)
	}
}

func TestRepairToolResultPartsTextualFields(t *testing.T) {
	link := NewResourceLinkBlock("https://x/"+invalidUTF8, invalidUTF8, invalidUTF8, invalidUTF8, "text/plain", 3, []string{"ok", invalidUTF8})
	link.LastModified = invalidUTF8
	text := NewTextBlock("body " + invalidUTF8)
	r := NewToolResultWithParts("c1", invalidUTF8, []Content{text, link})

	out := RepairToolResult(r)
	for i, p := range out.Parts {
		for name, s := range map[string]string{
			"Text": p.Text, "Name": p.Name, "Title": p.Title,
			"Description": p.Description, "URL": p.URL, "LastModified": p.LastModified,
		} {
			if !utf8.ValidString(s) {
				t.Fatalf("part[%d].%s still invalid: %q", i, name, s)
			}
		}
		for j, a := range p.Audience {
			if !utf8.ValidString(a) {
				t.Fatalf("part[%d].Audience[%d] still invalid: %q", i, j, a)
			}
		}
	}
}

func TestRepairToolResultPreservesBinaryAndMIME(t *testing.T) {
	blob := []byte{0xff, 0xfe, 0x00, 0xe2} // arbitrary binary, NOT valid UTF-8
	emb, err := NewEmbeddedResourceBlock("https://x", "application/octet-stream", "", blob, nil)
	if err != nil {
		t.Fatalf("NewEmbeddedResourceBlock: %v", err)
	}
	img := Content{BlockKind: BlockImage, Kind: MediaImage, MIMEType: "image/png", Data: blob}
	r := NewToolResultWithParts("c1", "ok", []Content{emb, img})

	out := RepairToolResult(r)
	if !bytes.Equal(out.Parts[0].Data, blob) {
		t.Fatalf("embedded blob changed: %x", out.Parts[0].Data)
	}
	if !bytes.Equal(out.Parts[1].Data, blob) {
		t.Fatalf("image data changed: %x", out.Parts[1].Data)
	}
	if out.Parts[1].MIMEType != "image/png" {
		t.Fatalf("MIMEType changed: %q", out.Parts[1].MIMEType)
	}
}

func FuzzRepairToolResult(f *testing.F) {
	f.Add("valid — em dash", "name", "title", "desc", "https://x", "2024", "aud")
	f.Add(invalidUTF8, invalidUTF8, invalidUTF8, invalidUTF8, invalidUTF8, invalidUTF8, invalidUTF8)
	f.Add("\xff\xfe", "\xed\xa0\x80", "\xc0\xaf", "\xe2\x82", "\xf0\x9f", "", "\x80")
	f.Fuzz(func(t *testing.T, content, name, title, desc, url, lastmod, aud string) {
		link := NewResourceLinkBlock(url, name, title, desc, "text/plain", 1, []string{aud})
		link.LastModified = lastmod
		// An empty blob fails the embedded-resource exactly-one-of invariant, so
		// only build that block when the fuzzer handed us bytes; the Data
		// byte-identity assertion below keys off whether it exists.
		blob := []byte(content)
		parts := []Content{NewTextBlock(content), link}
		if len(blob) > 0 {
			emb, err := NewEmbeddedResourceBlock(url, "application/octet-stream", "", blob, nil)
			if err != nil {
				t.Fatalf("NewEmbeddedResourceBlock: %v", err)
			}
			parts = append(parts, emb)
		}
		r := NewToolResultWithParts("c1", content, parts)

		out := RepairToolResult(r)

		if !utf8.ValidString(out.Content) {
			t.Fatalf("Content invalid after repair: %q", out.Content)
		}
		for i, p := range out.Parts {
			for fname, s := range map[string]string{
				"Text": p.Text, "Name": p.Name, "Title": p.Title,
				"Description": p.Description, "URL": p.URL, "LastModified": p.LastModified,
			} {
				if !utf8.ValidString(s) {
					t.Fatalf("part[%d].%s invalid after repair: %q", i, fname, s)
				}
			}
			for j, a := range p.Audience {
				if !utf8.ValidString(a) {
					t.Fatalf("part[%d].Audience[%d] invalid after repair: %q", i, j, a)
				}
			}
		}
		// Binary data is byte-identical (never repaired) when the blob block exists.
		if len(blob) > 0 && !bytes.Equal(out.Parts[2].Data, blob) {
			t.Fatalf("binary data changed: got %x want %x", out.Parts[2].Data, blob)
		}
	})
}
