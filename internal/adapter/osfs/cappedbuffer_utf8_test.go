package osfs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCappedBufferCutsOnRuneBoundary is the issue-#402 AC-4 pin: the command
// capture cap must not manufacture invalid UTF-8 out of valid command output.
//
// The cap is a BYTE limit, so a naive cut lands inside a multibyte rune for two
// of every three byte offsets in 3-byte-rune text. That half-rune reaches
// session.ToolResult.Content and then a protobuf string field, which rejects it
// at marshal time — the same codes.Internal that killed the Converse stream,
// except here the producing command's own output was perfectly well-formed and
// the harness introduced the corruption itself.
//
// The downstream RepairToolResult would rewrite it to U+FFFD, so this is not a
// crash today; it is a correctness fix at the source, so the model sees the
// command's real last character instead of a replacement rune the cap invented.
func TestCappedBufferCutsOnRuneBoundary(t *testing.T) {
	// "日" is 3 bytes (E6 97 A5), so a cap that is not a multiple of 3 lands
	// mid-rune. Sweep every offset across several runes to cover all three
	// alignments, including the aligned one that must NOT be trimmed.
	const runes = "日日日日日日"
	for limit := 1; limit <= len(runes); limit++ {
		var b cappedBuffer
		b.cap = limit
		n, err := b.Write([]byte(runes))
		if err != nil {
			t.Fatalf("cap=%d: Write: %v", limit, err)
		}
		if n != len(runes) {
			t.Fatalf("cap=%d: Write reported %d consumed, want %d (a short count stalls the pipe copy)", limit, n, len(runes))
		}
		got := b.String()
		if !utf8.ValidString(got) {
			t.Fatalf("cap=%d: cap produced invalid UTF-8 from valid input: %q", limit, got)
		}
		// It must keep every WHOLE rune that fits, and no more: the trim is for
		// the partial tail only, never a byte of usable output.
		wantRunes := limit / 3
		if n := utf8.RuneCountInString(got); n != wantRunes {
			t.Fatalf("cap=%d: kept %d runes, want %d (got %q)", limit, n, wantRunes, got)
		}
		if !strings.HasPrefix(runes, got) {
			t.Fatalf("cap=%d: output is not a prefix of the input: %q", limit, got)
		}
	}
}

// TestCappedBufferTrimIsCapOnly proves the rune-boundary trim fires ONLY on the
// truncating path. Output that fits must cross byte-for-byte even when it is
// already invalid UTF-8 — the cap's job is bounding, not sanitizing, and the
// semantic repair at the loop's choke point owns the latter.
func TestCappedBufferTrimIsCapOnly(t *testing.T) {
	const bad = "out \xe2M-^@M-^T end" // the orphaned-lead-byte sequence from #402
	var b cappedBuffer
	b.cap = 1024
	if _, err := b.Write([]byte(bad)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := b.String(); got != bad {
		t.Fatalf("under-cap output was altered:\n got %q\nwant %q", got, bad)
	}
}

// TestCappedBufferDiscardsPastCap pins the pre-existing bound: once the cap is
// reached, later writes are discarded but still report full consumption (a short
// count would stall the exec pipe copy).
func TestCappedBufferDiscardsPastCap(t *testing.T) {
	var b cappedBuffer
	b.cap = 4
	for range 3 {
		n, err := b.Write([]byte("abcdefgh"))
		if err != nil || n != 8 {
			t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
		}
	}
	if got := b.String(); got != "abcd" {
		t.Fatalf("buffer = %q, want %q", got, "abcd")
	}
}
