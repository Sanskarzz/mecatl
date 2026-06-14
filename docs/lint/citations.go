// Package lint holds the design-doc anti-drift checks. The core is a PURE
// function (no os import, no disk I/O) so the package itself stays trivially
// testable and gosec never sees a file-inclusion taint; the _test.go file owns
// all filesystem access (statFn/readFn) and is gosec-exempt.
//
// The motivating failure: the engine carve (core moved internal/ -> engine/)
// left design docs citing paths that no longer exist. A backtick span like
// `engine/agent/loop.go:677` is a load-bearing claim that the file is there;
// when it drifts, the doc lies silently. CheckCitations turns that into a CI
// failure. See docs/design/README.md for the citation convention.
package lint

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind classifies a dead citation.
type Kind int

const (
	// MissingFile is a slash-bearing repo-root-relative path that does not exist.
	MissingFile Kind = iota
	// MissingSymbol is a `path` (`Symbol`) form whose Symbol is absent from the file.
	MissingSymbol
)

// String renders the kind for messages.
func (k Kind) String() string {
	switch k {
	case MissingFile:
		return "missing-file"
	case MissingSymbol:
		return "missing-symbol"
	default:
		return "unknown"
	}
}

// Problem is one dead citation found in a design doc. It carries enough to
// point a human at the exact span and (for MissingFile) a basename-rescue
// suggestion when the file looks like it merely moved.
type Problem struct {
	DocName    string // the doc the citation lives in (e.g. "CLOUD-NATIVE.md")
	Citation   string // the raw backtick span text, verbatim (incl. any :NN suffix)
	Path       string // the path with any :NN/:NN-MM/:NN,MM line suffix stripped
	Kind       Kind
	Symbol     string // for MissingSymbol: the symbol that was not found
	Suggestion string // for MissingFile: a basename-rescue hint, or "" if none
}

// Error renders an actionable, single-line message naming the doc and the dead
// citation, so a CI failure tells the maintainer exactly what to fix and where.
func (p Problem) Error() string {
	switch p.Kind {
	case MissingSymbol:
		return fmt.Sprintf("%s: symbol %q not found in %s (citation %s)",
			p.DocName, p.Symbol, p.Path, p.Citation)
	default:
		msg := fmt.Sprintf("%s: cited file %s does not exist (citation %s)",
			p.DocName, p.Path, p.Citation)
		if p.Suggestion != "" {
			msg += "; " + p.Suggestion
		}
		return msg
	}
}

// backtickSpan matches the text inside a pair of backticks. Citations only ever
// live inside inline code spans in these docs, so we never scan free prose.
var backtickSpan = regexp.MustCompile("`([^`]+)`")

// fileCitation matches a known-extension path span, optionally suffixed with a
// line reference (:NN, :NN-MM, or :NN,MM). The leading char excludes '.' so a
// "./rel.go" relative span does not match here (and would be ignored anyway by
// the slash rule, but see TestGrammarRejectsProse). The slash requirement is
// NOT in this regex; it is enforced in code (see fileCitations) so we can
// document the prose/back-reference exemption explicitly.
var fileCitation = regexp.MustCompile(
	`^([A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:go|proto|ya?ml|md))(:[0-9][0-9,\- ]*)?$`)

// symbolName matches the inside of the `Symbol` span in the `path` (`Symbol`)
// pairing: a single identifier (dotted method-on-type allowed, e.g.
// Session.Recover). We never parse arbitrary "the SomeFunc helper" prose into a
// symbol; the pairing is detected structurally (the gap between the two spans
// must be exactly " (" and the span must be followed by ")"), see scanning code.
var symbolName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// CheckCitations scans a single design doc's markdown for dead citations.
//
//   - statFn reports whether a repo-root-relative path exists.
//   - readFn reads a repo-root-relative path's bytes (used only for the
//     (`Symbol`) form, and only after statFn confirms the file exists).
//
// Both are injected so this function imports no os and does no I/O: the test
// supplies map-backed pure fns for fixtures and os-backed fns for the live
// guard. Line numbers are NEVER verified (they are stripped before the stat).
//
// Grammar (two forms, both over backtick spans):
//
//  1. File citation: a backtick span matching fileCitation AND containing at
//     least one '/'. A known-extension span with NO slash is a prose
//     back-reference (e.g. `Save`, `service.go:868`, `task test`) and is
//     IGNORED on purpose: a bare basename is not a repo-root-relative locator,
//     so verifying it would mean guessing, and guessing produces false
//     positives. Repo-root-relative paths (with a slash) ARE verifiable, so
//     those we check strictly: an abbreviated-but-real citation (e.g.
//     `grpcdriver/server.go` for internal/adapter/grpcdriver/server.go) is
//     deliberately flagged so the author expands it, and basename rescue points
//     the way. The ONE escape hatch is the explicit inline ignore marker (see
//     ignoreMarker), for a slash-path that is illustrative and NOT a repo
//     citation (e.g. the skill logical-asset-name example `references/api.md` in
//     DRIVERS.md). The marker is opt-in and self-documenting, so it does not
//     weaken the guard for ordinary citations.
//
//  2. Symbol citation (opt-in): the explicit `path` (`Symbol`) pairing. When a
//     verifiable file citation is immediately followed by a `Symbol` span, the
//     symbol is verified by a word-boundary match over readFn(path). We never
//     infer symbols from anything but this explicit pairing.
//
// rescue, when non-nil, is consulted ONLY for a MissingFile: given a basename it
// returns a one-line suggestion (or "") so the walk that finds a moved file is
// lazy and lives in the caller (the test), keeping this function I/O-free.
func CheckCitations(
	docName, markdown string,
	statFn func(string) bool,
	readFn func(string) ([]byte, error),
	rescue func(basename string) string,
) []Problem {
	var problems []Problem

	// Each match m holds [fullStart, fullEnd, innerStart, innerEnd]; we keep the
	// inner text and the byte offsets so we can inspect the gap between adjacent
	// spans for the `path` (`Symbol`) pairing.
	matches := backtickSpan.FindAllStringSubmatchIndex(markdown, -1)
	spans := make([]span, 0, len(matches))
	for _, m := range matches {
		spans = append(spans, span{text: markdown[m[2]:m[3]], fullStart: m[0], fullEnd: m[1]})
	}

	for i := range spans {
		raw := spans[i].text
		m := fileCitation.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		path := m[1] // line suffix (m[2]) is intentionally discarded, never verified

		// The slash rule, enforced here (not in the regex) so it is documented:
		// a known-extension span with no '/' is a prose/back-reference, ignored.
		if !strings.Contains(path, "/") {
			continue
		}

		// Explicit inline ignore: an illustrative slash-path the author marked
		// as a non-citation (see ignoreMarker). Checked on the line containing
		// the span so the marker stays local to the thing it exempts.
		if hasIgnoreMarker(markdown, spans[i].fullStart) {
			continue
		}

		if !statFn(path) {
			p := Problem{
				DocName:  docName,
				Citation: raw,
				Path:     path,
				Kind:     MissingFile,
			}
			if rescue != nil {
				p.Suggestion = rescue(baseName(path))
			}
			problems = append(problems, p)
			continue // a missing file cannot have its symbol checked
		}

		// Opt-in symbol form: detect the `path` (`Symbol`) pairing STRUCTURALLY.
		// The gap between this span's close backtick and the next span's open
		// backtick must be exactly " (" and the next span must be closed by ")".
		if sym, ok := symbolPairing(markdown, spans, i); ok {
			data, err := readFn(path)
			if err != nil || !symbolPresent(data, sym) {
				problems = append(problems, Problem{
					DocName:  docName,
					Citation: raw + " (`" + sym + "`)",
					Path:     path,
					Kind:     MissingSymbol,
					Symbol:   sym,
				})
			}
		}
	}
	return problems
}

// span is one backtick code span: its inner text and the byte offsets of the
// full `...` (backticks included) in the source markdown.
type span struct {
	text               string
	fullStart, fullEnd int
}

// symbolPairing reports whether the span at index i is followed by a `Symbol`
// span in the exact `path` (`Symbol`) shape, returning the symbol. The pairing
// is STRUCTURAL, not heuristic: the gap between this span's closing backtick and
// the next span's opening backtick must be exactly " (", and the text right
// after the next span must begin with ")". This is why a following second file
// path or an ordinary adjacent code span is never misread as a symbol claim.
func symbolPairing(markdown string, spans []span, i int) (string, bool) {
	if i+1 >= len(spans) {
		return "", false
	}
	cur, next := spans[i], spans[i+1]
	gap := markdown[cur.fullEnd:next.fullStart]
	if gap != " (" {
		return "", false
	}
	after := markdown[next.fullEnd:]
	if !strings.HasPrefix(after, ")") {
		return "", false
	}
	if !symbolName.MatchString(next.text) {
		return "", false
	}
	return next.text, true
}

// symbolPresent reports a word-boundary match of sym in data. \b + QuoteMeta +
// \b: an exact identifier match, never a substring (so "Save" does not match
// "SaveRequest").
func symbolPresent(data []byte, sym string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(sym) + `\b`)
	return re.Match(data)
}

// ignoreMarkerRE matches the inline opt-out for a slash-path that is
// illustrative and not a repo citation. The canonical form is an HTML comment on
// the same line as the span carrying a reason:
//
//	... in the skill namespace (`references/api.md`). <!-- lint:not-a-citation: skill asset name, not a repo file -->
//
// The match is ANCHORED: the token `lint:not-a-citation` must be followed by a
// `:` (the reason form), whitespace, or `-->` (the bare closing form). A trailing
// word is NOT a match, so a typo like `lint:not-a-citations` (an extra letter, or
// any other near-miss substring) does NOT silently suppress the check — it
// fails safe by still flagging the citation, which is the direction we want for
// an escape hatch. The reason is REQUIRED by convention (see docs/design/README.md);
// the bare form still technically matches so a legacy marker keeps working, but
// authors should always write the reason.
var ignoreMarkerRE = regexp.MustCompile(`lint:not-a-citation(:|\s|-->)`)

// hasIgnoreMarker reports whether the markdown line containing the byte offset
// pos carries a well-formed inline ignore marker.
func hasIgnoreMarker(markdown string, pos int) bool {
	lineStart := strings.LastIndexByte(markdown[:pos], '\n') + 1 // 0 if not found
	lineEnd := strings.IndexByte(markdown[pos:], '\n')
	if lineEnd < 0 {
		lineEnd = len(markdown)
	} else {
		lineEnd += pos
	}
	return ignoreMarkerRE.MatchString(markdown[lineStart:lineEnd])
}

// baseName returns the final path segment.
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
