package hashutil

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestSHA256HexMatchesStdlib pins the shared primitive to the EXACT discipline both
// drift subsystems relied on inline before extraction: lowercase-hex SHA-256. If a
// future edit changed the encoding (e.g. base64, uppercase), every stored soul
// sidecar and trust anchor would silently mismatch — this catches that.
func TestSHA256HexMatchesStdlib(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("You are a project persona."),
		[]byte("a\x00b\nc"),
	}
	for _, in := range cases {
		sum := sha256.Sum256(in)
		want := hex.EncodeToString(sum[:])
		if got := SHA256Hex(in); got != want {
			t.Fatalf("SHA256Hex(%q) = %q, want %q (must be lowercase-hex sha256)", in, got, want)
		}
	}
}

// TestSHA256HexDeterministic asserts the same bytes always hash the same and
// different bytes hash differently (the minimum a drift fingerprint must guarantee).
func TestSHA256HexDeterministic(t *testing.T) {
	a := SHA256Hex([]byte("same"))
	if a != SHA256Hex([]byte("same")) {
		t.Fatal("SHA256Hex not deterministic for identical input")
	}
	if a == SHA256Hex([]byte("different")) {
		t.Fatal("SHA256Hex collided on distinct inputs")
	}
}
