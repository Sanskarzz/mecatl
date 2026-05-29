package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/stacklok/mecatl/internal/adapter/toolkit"
)

// SkillFileName is the conventional file every skill directory contains. A skill
// lives at <dir>/<name>/SKILL.md, mirroring the Agent Skills layout.
const SkillFileName = "SKILL.md"

// DefaultDir is the conventional skills directory, relative to the workspace. It
// is NOT applied automatically — skills are opt-in, so discovery only runs when
// an explicit directory is configured. It is exposed so the composition root can
// surface the convention (e.g. in flag help text).
const DefaultDir = ".mecatl/skills"

// maxDescriptionBytes caps a skill's one-line description. The description is the
// ALWAYS-IN-CONTEXT metadata (it lives in the Skill tool's Spec().Description, on
// every request), so an unbounded one would inflate every prompt and break the
// byte-stable prompt-prefix caching the OpenAI adapter relies on. A skill
// description is a single line; 800 bytes is generous for that. Discover truncates
// (rune-safe, with an ellipsis) and records a non-fatal warning when it trims.
const maxDescriptionBytes = 800

// SkipError records one diagnostic from discovery: either a skill that could not
// be loaded (a fatal SKIP — the skill is excluded), or a non-fatal WARNING about
// a skill that WAS kept (e.g. its description or body was truncated). In both
// cases Discover keeps scanning rather than aborting, and returns the collected
// diagnostics so the composition root can surface them. It is never returned as
// Discover's fatal error.
type SkipError struct {
	// Path is the SKILL.md (or directory) the problem was found at.
	Path string
	// Reason is a short, human-readable description of the problem.
	Reason string
}

func (e SkipError) Error() string {
	return fmt.Sprintf("skipped skill at %q: %s", e.Path, e.Reason)
}

// frontmatter is the parsed YAML header of a SKILL.md file. Only name and
// description are part of the always-in-context metadata; any other keys are
// ignored so the format can grow without breaking discovery.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Discover scans dir for skills laid out as <dir>/<name>/SKILL.md, parses each
// one's YAML frontmatter and markdown body, and returns the valid skills sorted
// by name for deterministic output.
//
// It is forgiving by design: a missing dir yields no skills and no error (skills
// are opt-in); a malformed or frontmatter-less SKILL.md is SKIPPED and reported
// via the returned []SkipError rather than aborting the scan. Discover returns a
// non-nil error only for a genuine I/O fault reading the directory itself.
//
// A skill whose frontmatter `name` disagrees with its directory name is accepted
// using the FRONTMATTER name (the frontmatter is the source of truth for the
// activation key); duplicate effective names are resolved by keeping the first in
// sorted-path order and skipping the rest (reported as a SkipError).
func Discover(dir string) ([]Skill, []SkipError, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Opt-in: an absent skills dir is simply "no skills", not an error.
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("skills: read dir %q: %w", dir, err)
	}

	// Sort directory entries first so both the keep-first dedup and the final
	// output are deterministic regardless of filesystem ordering.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var (
		out   []Skill
		skips []SkipError
		seen  = map[string]string{} // effective name -> path that claimed it
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), SkillFileName)
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				// A subdirectory without a SKILL.md is not a skill; silently ignore.
				continue
			}
			skips = append(skips, SkipError{Path: path, Reason: fmt.Sprintf("cannot read: %v", rerr)})
			continue
		}
		sk, perr, notes := parseSkill(raw, path)
		if perr != "" {
			skips = append(skips, SkipError{Path: path, Reason: perr})
			continue
		}
		// Non-fatal warnings (e.g. truncation): the skill is kept, but the author
		// gets a signal via the returned diagnostics.
		for _, n := range notes {
			skips = append(skips, SkipError{Path: path, Reason: n})
		}
		if prev, dup := seen[sk.Name]; dup {
			skips = append(skips, SkipError{
				Path:   path,
				Reason: fmt.Sprintf("duplicate skill name %q (already defined at %q)", sk.Name, prev),
			})
			continue
		}
		seen[sk.Name] = path
		out = append(out, sk)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, skips, nil
}

// parseSkill splits raw into YAML frontmatter and a markdown body and validates
// the required header fields. It returns:
//   - a fatal reason string (with a zero Skill) on any structural problem, so the
//     caller records a SkipError and EXCLUDES the skill; reason is "" on success.
//   - a slice of non-fatal warning notes for a skill that IS kept (e.g. its
//     description was truncated to the always-in-context cap, or its body exceeds
//     the activation output cap), so the author gets a signal.
//
// The description is capped HERE (at parse time) to maxDescriptionBytes because it
// lives in the always-in-context tool spec; the body is NOT trimmed here (the
// Skill tool truncates it on activation against the shared toolkit cap), but an
// oversized body is flagged so the author knows it will be truncated.
func parseSkill(raw []byte, path string) (Skill, string, []string) {
	fmText, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return Skill{}, "missing YAML frontmatter (expected a leading '---' delimited block)", nil
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return Skill{}, fmt.Sprintf("malformed YAML frontmatter: %v", err), nil
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		return Skill{}, "frontmatter is missing a non-empty \"name\"", nil
	}
	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return Skill{}, "frontmatter is missing a non-empty \"description\"", nil
	}

	var notes []string
	if len(desc) > maxDescriptionBytes {
		notes = append(notes, fmt.Sprintf(
			"description is %d bytes; truncated to the always-in-context cap of %d bytes (a skill description should be a single line)",
			len(desc), maxDescriptionBytes))
		desc = truncateRunes(desc, maxDescriptionBytes)
	}

	trimmedBody := strings.TrimSpace(body)
	if len(trimmedBody) > toolkit.MaxOutputBytes {
		notes = append(notes, fmt.Sprintf(
			"body is %d bytes; it will be truncated to %d bytes when the skill is activated",
			len(trimmedBody), toolkit.MaxOutputBytes))
	}

	return Skill{
		Name:        name,
		Description: desc,
		Body:        trimmedBody,
		Path:        path,
	}, "", notes
}

// truncateRunes trims s to at most maxBytes on a rune boundary and appends a
// single-character ellipsis. It is used for the always-in-context description cap;
// the body has its own truncation (toolkit.Truncate) at activation time.
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	cut := maxBytes - len(ellipsis)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// continuation byte 0b10xxxxxx). It mirrors toolkit's internal helper, kept local
// so this package does not need to export one from toolkit.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// splitFrontmatter separates a leading YAML frontmatter block, delimited by a
// line containing only "---" at the very start and a matching closing "---"
// line, from the markdown body that follows. It returns the frontmatter text
// (without the delimiters), the body, and whether a well-formed frontmatter
// block was found. A leading UTF-8 BOM is tolerated.
func splitFrontmatter(s string) (fm, body string, ok bool) {
	s = strings.TrimPrefix(s, "\ufeff")
	// Normalise CRLF so the delimiter match is line-ending agnostic.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") && s != "---" {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "---\n")
	// Find the closing delimiter: a line that is exactly "---".
	idx := indexClosingDelim(rest)
	if idx < 0 {
		return "", "", false
	}
	fm = rest[:idx]
	// Skip past the closing "---" line (and its trailing newline, if any).
	after := rest[idx:]
	after = strings.TrimPrefix(after, "---")
	after = strings.TrimPrefix(after, "\n")
	return fm, after, true
}

// indexClosingDelim returns the byte offset, within s, of the start of the first
// line that is exactly "---" (the closing frontmatter delimiter), or -1 if none.
func indexClosingDelim(s string) int {
	offset := 0
	for _, line := range strings.SplitAfter(s, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if trimmed == "---" {
			return offset
		}
		offset += len(line)
	}
	return -1
}
