// Package hashutil is the shared, stdlib-only content-fingerprint primitive for
// the adapter layer. It exists so the two INDEPENDENT drift mechanisms in this
// codebase — the soul drift baseline (internal/adapter/soul + internal/app/soulguard)
// and the workspace-trust identity-anchor hash (internal/adapter/workspacetrust) —
// compute their fingerprints with EXACTLY the same discipline: lowercase-hex
// SHA-256.
//
// # What this is, and what it is NOT
//
// This is ONLY the shared cryptographic primitive (SHA256Hex). It is deliberately
// the SOLE thing the two subsystems share (Workspace-Trust MUST-FIX 4). The soul
// drift baseline (a soul-only sidecar <soul>.sha256, re-blessed by --approve-soul)
// and the trust identity anchor (a soul ⊕ agent ⊕ command ⊕ skill fold stored in
// the trust.yaml registry, re-blessed by re-answering the prompt) remain PARALLEL:
// different surface, different store, different re-bless gesture. We are NOT
// generalising soulguard into a trust subsystem and NOT merging the two anchors.
//
// # Layering
//
// hashutil is an adapter LEAF with only stdlib imports (crypto/sha256 +
// encoding/hex). Adapters and composition MAY import it; no domain package
// (session, prompt, governance, tool) ever does.
package hashutil

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256Hex returns the lowercase-hex SHA-256 of b. This is the exact discipline
// soul.LoadWithMeta and the workspace-trust identity anchor both use, so a hash
// computed here is byte-identical to one computed inline with
// hex.EncodeToString(sha256.Sum256(b)[:]).
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
