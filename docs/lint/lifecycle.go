package lint

import (
	"fmt"
	"regexp"
)

// The ADR-shape gate (docs/adr/0003-consolidate-design-records-as-adrs.md).
//
// Every Architecture Decision Record under docs/adr/ must be self-describing: a
// `- Status:` line and a `- Date:` line in its header. This replaces the earlier
// design-record lifecycle gate (ADR 0002's banner + no-Status rule), retired when
// the design records were consolidated into the ADR scheme (ADR 0003). The
// guarantee is the same in spirit: a reader or agent learns an ADR's status and
// date without reading the body.

// ADRExempt reports whether a docs/adr/ basename is exempt from the shape gate:
// the index (README.md) and the blank template (template.md).
func ADRExempt(docName string) bool {
	switch docName {
	case "README.md", "template.md":
		return true
	default:
		return false
	}
}

// adrStatusLine matches the `- Status:` header field; adrDateLine the `- Date:`
// field (each at line start, an optional space after the dash, case-insensitive).
var (
	adrStatusLine = regexp.MustCompile(`(?m)^- ?[Ss]tatus:`)
	adrDateLine   = regexp.MustCompile(`(?m)^- ?[Dd]ate:`)
)

// ADRProblem is one missing-header-field violation in an ADR.
type ADRProblem struct {
	DocName string
	Missing string // "Status" or "Date"
}

// Error renders an actionable, single-line message.
func (p ADRProblem) Error() string {
	return fmt.Sprintf("%s: ADR is missing a `- %s:` header line (see docs/adr/template.md)",
		p.DocName, p.Missing)
}

// CheckADRShape applies the ADR-shape gate to one ADR file. It is pure (no I/O);
// the _test.go file owns disk access. Exempt files return nil.
func CheckADRShape(docName, content string) []ADRProblem {
	if ADRExempt(docName) {
		return nil
	}
	var problems []ADRProblem
	if !adrStatusLine.MatchString(content) {
		problems = append(problems, ADRProblem{DocName: docName, Missing: "Status"})
	}
	if !adrDateLine.MatchString(content) {
		problems = append(problems, ADRProblem{DocName: docName, Missing: "Date"})
	}
	return problems
}
