// Package welcome renders the first-run splash for the mecatui zero-state: a
// faithful mascot (half-block on any truecolor terminal, a zero-dependency kitty
// Unicode-placeholder high-res path on capable terminals), a gradient "mecatl"
// wordmark, and an info block. It imports ONLY the theme package, the charm
// libraries, and the standard library — never client or ui — so it slots beneath
// the ui package without violating the inward-only convention (ui imports it).
//
// The splash is the un-framed BODY; the caller (ui.help.go) wraps it in the
// shared centerCard treatment. It shows on the empty zero-state and vanishes the
// instant the first conversation block is appended (the existing isEmpty guard in
// ui.View).
package welcome

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
	"sync"
)

// mascotPNG is the embedded mascot. go:embed cannot reach the repo-root
// assets/mecatito.png (a parent dir), so a COPY lives at assets/mecatito.png in
// this package. The repo-root assets/mecatito.png is CANONICAL; keep this copy in
// sync (it is byte-identical, 1254×1254 RGBA).
//
//go:embed assets/mecatito.png
var mascotPNG []byte

var (
	mascotOnce sync.Once
	mascotImg  image.Image
	mascotErr  error
)

// DecodeMascot decodes the embedded mascot PNG exactly once and caches the
// result. The zero-state re-renders on every frame, so decoding the 1254×1254
// image per frame would be wasteful; sync.Once makes it a one-time cost. Both the
// decoded image and any decode error are memoised, so a corrupt embed fails the
// same way every call (and lets callers fall back cleanly).
func DecodeMascot() (image.Image, error) {
	mascotOnce.Do(func() {
		mascotImg, mascotErr = png.Decode(bytes.NewReader(mascotPNG))
	})
	return mascotImg, mascotErr
}

// rawMascotPNG returns the embedded PNG bytes, used by the kitty transmit path
// (which sends the full-resolution PNG to the terminal, letting the terminal
// scale it into the placement cell area).
func rawMascotPNG() []byte { return mascotPNG }
