package lint

import (
	"fmt"
	"regexp"
	"strings"
)

// The documentation-lifecycle gate (docs/adr/0002-documentation-lifecycle.md).
//
// A design doc under docs/design/ is a FROZEN decision record: it captures the
// rationale at a point in time. It must NOT carry mutable status (that lives only
// in PRODUCTION-READINESS.md) and it MUST declare its lifecycle up front with a
// banner. Before this gate, status headers rotted silently (CLOUD-NATIVE frozen at
// "Phase 0" after Phase 3c shipped, MEMORY-TIERING "DESIGN ONLY" after the tier-0
// index shipped); making the rotting idiom a build failure turns the discipline
// from aspirational into structural.

// LifecycleExempt reports whether a design-doc basename is exempt from the
// lifecycle gate: the index (README.md), the status tracker itself
// (PRODUCTION-READINESS.md — the ONE place status is allowed), and the living
// dense-reference companion (IMPLEMENTATION-NOTES.md, which tracks per-subsystem
// shipped/deferred state by design — see ADR 0002 "Not done here").
func LifecycleExempt(docName string) bool {
	switch docName {
	case "README.md", "PRODUCTION-READINESS.md", "IMPLEMENTATION-NOTES.md":
		return true
	default:
		return false
	}
}

// lifecycleBanner matches the bold lead token of a lifecycle banner: one of
// "**Design record.**", "**Historical.**", "**Research note.**".
var lifecycleBanner = regexp.MustCompile(`\*\*(Design record|Historical|Research note)\.\*\*`)

// statusLine matches a line whose FIRST word (after an optional blockquote marker
// and optional bold markers) is "Status" immediately followed by a colon — the
// mutable status idiom (`Status:`, `> Status:`, `**Status:**`, `**STATUS:`). It
// deliberately does NOT match the banner's "shipped/deferred state:" (that line
// starts with "Current behaviour:") or prose like "status lives in the tracker"
// (no colon directly after the word).
var statusLine = regexp.MustCompile(`^[ \t]*>?[ \t]*\*{0,2}[Ss]tatus\b[ \t]*\*{0,2}[ \t]*:`)

// LifecycleKind classifies a lifecycle problem.
type LifecycleKind int

const (
	// MissingBanner is reported when a design doc declares no lifecycle banner in
	// its preamble.
	MissingBanner LifecycleKind = iota
	// StatusInDesignDoc is reported when a mutable "Status:" line appears in a
	// frozen design doc.
	StatusInDesignDoc
)

// LifecycleProblem is a single lifecycle-gate violation in a design doc.
type LifecycleProblem struct {
	DocName string
	Kind    LifecycleKind
	Line    int    // 1-based; meaningful for StatusInDesignDoc
	Text    string // the offending line, trimmed; for StatusInDesignDoc
}

// Error renders an actionable, single-line message.
func (p LifecycleProblem) Error() string {
	switch p.Kind {
	case StatusInDesignDoc:
		return fmt.Sprintf("%s:%d: status line in a frozen design doc — move status to "+
			"PRODUCTION-READINESS.md (ADR 0002): %q", p.DocName, p.Line, p.Text)
	default:
		return fmt.Sprintf("%s: no lifecycle banner — add one of \"**Design record.**\" / "+
			"\"**Historical.**\" / \"**Research note.**\" as the first content after the H1 "+
			"(see docs/design/README.md)", p.DocName)
	}
}

// CheckDesignDocLifecycle applies the lifecycle gate to one design doc. It is
// pure (no I/O); the _test.go file owns disk access. Exempt docs return nil —
// callers should skip them via LifecycleExempt, but this is also defensive.
func CheckDesignDocLifecycle(docName, content string) []LifecycleProblem {
	if LifecycleExempt(docName) {
		return nil
	}

	var problems []LifecycleProblem
	lines := strings.Split(content, "\n")

	// (1) Banner present in the preamble — before the first "## " section heading.
	bannerFound := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			break // past the preamble
		}
		if lifecycleBanner.MatchString(ln) {
			bannerFound = true
			break
		}
	}
	if !bannerFound {
		problems = append(problems, LifecycleProblem{DocName: docName, Kind: MissingBanner})
	}

	// (2) No "Status:" line, outside fenced code blocks (a code example may
	// legitimately contain a `Status:` field; the prose must not).
	inFence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if statusLine.MatchString(ln) {
			problems = append(problems, LifecycleProblem{
				DocName: docName,
				Kind:    StatusInDesignDoc,
				Line:    i + 1,
				Text:    strings.TrimSpace(ln),
			})
		}
	}

	return problems
}
