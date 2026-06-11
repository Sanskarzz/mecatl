package soul

import (
	"github.com/stacklok/mecatl/engine/prompt"
	"github.com/stacklok/mecatl/internal/adapter/skills"
)

// Compile-time assertion that *Store satisfies the prompt-defined SoulSource
// port. Placed here so the soul→prompt edge is documented next to the skills
// reuse (both are deliberate adapter dependencies).
var _ prompt.SoulSource = (*Store)(nil)

// scanForInjection reuses skills.ScanForInjection — the conservative
// role-override / instruction-injection deny-list already maintained for drafted
// skills (internal/adapter/skills/sanitize.go). The soul is user-authored, but it
// re-enters context as identity-shaping text every session, so it shares the same
// load-time injection gate as the skills layer rather than copying the regexes.
// This is a deliberate adapter→adapter import (legal under the layering rule); if
// the coupling ever needs breaking, copy the markers locally as the fallback.
func scanForInjection(s string) (marker string, found bool) {
	return skills.ScanForInjection(s)
}
