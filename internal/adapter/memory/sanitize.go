package memory

import (
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/prompt"
)

// Compile-time assertion that *Store satisfies the prompt-defined UserModelSource
// port. Placed here next to the skills reuse so the memory→prompt and
// memory→skills edges are documented together (both are deliberate adapter
// dependencies, legal under the layering rule). *Store already satisfies the
// (structurally identical) prompt.MemoryIndexSource via its Index method; this
// names the second consumer for the user-model store instance.
var _ prompt.UserModelSource = (*Store)(nil)

// scanForInjection reuses skills.ScanForInjection — the conservative role-override
// / instruction-injection deny-list already maintained for drafted skills
// (internal/adapter/skills/sanitize.go) and reused by the soul loader
// (internal/adapter/soul/sanitize.go). The user-model write path runs it over a
// candidate VALUE at write time so a transcript-sourced fact cannot launder
// "ignore previous instructions"-style steering into the <user-model> block,
// which re-enters context as operator data every session. This is a deliberate
// adapter→adapter import (legal under the layering rule); if the coupling ever
// needs breaking, copy the markers locally as the fallback (the same note soul
// carries).
func scanForInjection(s string) (marker string, found bool) {
	return skills.ScanForInjection(s)
}
