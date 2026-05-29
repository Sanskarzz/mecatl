package governance

import "strings"

// SplitCommands splits a compound Bash command line into its individual simple
// commands, breaking on the shell operators "&&", "||", ";", "|", a single
// unquoted "&" (background), and newlines, while respecting single and double
// quotes (operators inside quotes are literal).
//
// This is the compound-command awareness the permission layer relies on: a line
// such as `git status && rm -rf /` must be evaluated as TWO commands, so that a
// deny on `rm` blocks the whole compound even though `git status` would be
// allowed. The returned slice contains the trimmed segments with empty segments
// dropped; an empty or whitespace-only input yields nil.
//
// Newlines and a single "&" are separators too: `ls\nrm -rf build` and
// `ls & rm -rf build` smuggle a second command past a gate that only knows the
// classic operators. Command/process substitution and subshell grouping
// (`$(...)`, backticks, `<(...)`, and `(`/`{` grouping) are NOT decomposed here;
// callers must treat any segment for which HasSubstitutionOrGrouping reports
// true as fail-safe (not read-only; escalate to at least Ask), since the inner
// command cannot be soundly extracted without a full shell parser.
func SplitCommands(cmd string) []string {
	var (
		out      []string
		buf      strings.Builder
		inSingle bool
		inDouble bool
	)
	runes := []rune(cmd)
	flush := func() {
		seg := strings.TrimSpace(buf.String())
		if seg != "" {
			out = append(out, seg)
		}
		buf.Reset()
	}
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
			buf.WriteRune(c)
			continue
		case inDouble:
			if c == '"' {
				inDouble = false
			}
			buf.WriteRune(c)
			continue
		case c == '\'':
			inSingle = true
			buf.WriteRune(c)
			continue
		case c == '"':
			inDouble = true
			buf.WriteRune(c)
			continue
		}

		// Unquoted: look for the two-rune operators "&&" and "||" first, then
		// the single-rune separators ";", "|", "&" and newlines. A lone "&" is a
		// background separator and must split too, so a denied inner command
		// cannot ride on `ls & rm -rf build`.
		if (c == '&' || c == '|') && i+1 < len(runes) && runes[i+1] == c {
			flush()
			i++ // consume the second operator rune
			continue
		}
		if c == ';' || c == '|' || c == '&' || c == '\n' || c == '\r' {
			flush()
			continue
		}
		buf.WriteRune(c)
	}
	flush()
	return out
}

// HasSubstitutionOrGrouping reports whether a Bash segment contains command or
// process substitution, or subshell/group-command grouping, in unquoted text:
// `$(...)`, a backtick, `<(...)`/`>(...)`, an opening "(" or "{" used as
// grouping. These constructs can hide an arbitrary inner command that the
// operator-based splitter does not decompose; the permission layer treats any
// such segment as fail-safe (not read-only, and escalated to at least Ask) so a
// denied/destructive inner command cannot be silently matched as one literal
// allowed token.
//
// The scan is quote-aware: a "(" inside single or double quotes is literal data
// and does not count. A "$" immediately before "(" or "{" (command substitution
// or "${...}" expansion) is flagged conservatively.
func HasSubstitutionOrGrouping(seg string) bool {
	runes := []rune(seg)
	var inSingle, inDouble bool
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '"' {
				inDouble = false
				continue
			}
			// Command substitution and backticks stay active inside double quotes.
			if hasSubstitutionTrigger(c, next) {
				return true
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case hasSubstitutionTrigger(c, next) || c == '(' || c == '{':
			// Unquoted "(" / "{" is subshell or group-command grouping. "{" is
			// only a group command as a standalone token, but treating any
			// unquoted occurrence as grouping is the conservative, fail-safe
			// choice.
			return true
		}
	}
	return false
}

// hasSubstitutionTrigger reports whether c (with following rune next) opens a
// command or process substitution: a backtick, "$(", "${", "<(" or ">(".
func hasSubstitutionTrigger(c, next rune) bool {
	switch c {
	case '`':
		return true
	case '$':
		return next == '(' || next == '{'
	case '<', '>':
		return next == '('
	}
	return false
}

// wrapperFlagsTakeValue records, per stripped wrapper, the long/short flags that
// consume a following argument (so we skip the value too, not just the flag).
// Conservative: unknown flags that look like options are skipped as valueless.
var wrapperFlagsTakeValue = map[string]map[string]bool{
	"timeout": {"-s": true, "--signal": true, "-k": true, "--kill-after": true},
	"nice":    {"-n": true, "--adjustment": true},
	"env":     {"-u": true, "--unset": true, "-C": true, "--chdir": true},
	"stdbuf":  {"-i": true, "--input": true, "-o": true, "--output": true, "-e": true, "--error": true},
	"ionice":  {"-c": true, "--class": true, "-n": true, "--classdata": true, "-p": true, "--pid": true},
	"time":    {},
}

// wrapperLeadingPositionals records how many mandatory non-flag positional
// arguments a wrapper consumes before the wrapped command begins. `timeout`
// takes a required DURATION (`timeout 5 rm x`); the rest take none.
var wrapperLeadingPositionals = map[string]int{
	"timeout": 1,
}

// strippableWrappers is the CLOSED, audited set of process-wrapper commands that
// Canonicalize peels off before permission matching. It deliberately contains
// only transparent wrappers that do not change which program ultimately runs.
//
// It MUST NOT grow to include re-entrant launchers such as `docker exec`, `npx`,
// `devbox run` or `sudo`: those select or virtualize a different execution
// environment and stripping them would silently open a permission backdoor
// (doc 08 §10).
var strippableWrappers = map[string]bool{
	"timeout": true,
	"time":    true,
	"nice":    true,
	"env":     true,
	"stdbuf":  true,
	"ionice":  true,
}

// Canonicalize strips leading transparent process wrappers from a single Bash
// command so that permission matching sees the real program being run. Only the
// closed strippableWrappers set is removed (`timeout`, `time`, `nice`, `env`,
// `stdbuf`, `ionice`), together with their leading option flags and any values
// those flags consume.
//
// Re-entrant launchers are intentionally left intact: `docker exec foo rm x`,
// `npx ...`, `devbox run ...` and `sudo rm x` are returned unchanged, because
// stripping them would defeat the permission boundary.
//
// Canonicalize operates on a SINGLE simple command; callers that may receive a
// compound line should SplitCommands first.
func Canonicalize(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	for {
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			return trimmed
		}
		head := fields[0]
		if !strippableWrappers[head] {
			return trimmed
		}
		// Walk past the wrapper's own flags (and any values they consume), then
		// past its mandatory leading positionals (e.g. timeout's DURATION), to
		// find where the wrapped command begins.
		valueFlags := wrapperFlagsTakeValue[head]
		positionals := wrapperLeadingPositionals[head]
		i := 1
		for i < len(fields) {
			f := fields[i]
			if !strings.HasPrefix(f, "-") {
				if positionals > 0 {
					positionals--
					i++
					continue
				}
				break // first non-flag token after positionals is the program
			}
			// `--key=value` carries its own value; consume just this token.
			if strings.Contains(f, "=") {
				i++
				continue
			}
			i++
			if valueFlags[f] {
				i++ // also consume the separate value argument
			}
		}
		if i >= len(fields) {
			// Nothing left after the wrapper (e.g. bare `env`): leave as-is.
			return trimmed
		}
		trimmed = strings.Join(fields[i:], " ")
		// Loop to peel further nested wrappers (e.g. `timeout 5 nice rm x`).
	}
}

// readOnlyVerbs are first-token commands whose canonical use does not mutate the
// workspace. `git` is handled specially (only read-only subcommands qualify).
//
// Deliberately EXCLUDED are general-purpose interpreters whose program argument
// can execute arbitrary commands or write arbitrary files with no shell-level
// redirection (which the top-level `>`/writeIndicators checks would catch):
//   - awk: `awk 'BEGIN{system("…")}'`, `print | "cmd"`.
//   - sed: `sed -i` (in-place edit), the `w`/`W` script commands.
//
// These cannot be soundly classified read-only without parsing their language,
// so they are not listed and resolve to Ask (and are hard-denied under plan
// mode). Verbs that are read-only in normal use but mutate via a finite,
// well-defined set of flags (find, sort) ARE listed, and are guarded by
// verbArgsMutate below.
var readOnlyVerbs = map[string]bool{
	"ls": true, "cat": true, "grep": true, "egrep": true, "fgrep": true,
	"find": true, "rg": true, "head": true, "tail": true, "pwd": true,
	"echo": true, "wc": true, "stat": true, "file": true, "which": true,
	"whoami": true, "date": true, "tree": true, "diff": true, "sort": true,
	"uniq": true, "cut": true, "true": true,
}

// readOnlyGitSubcommands are the `git` subcommands that only inspect state.
//
// `config` is deliberately EXCLUDED: `git config` writes .git/config and can
// persist values that execute shell on later (allow-listed) git operations —
// e.g. an alias whose value starts with `!`, or core.pager/core.sshCommand/
// core.fsmonitor. Its only read-only forms (--get/--list) are not worth that
// bypass surface, so all `git config` resolves to Ask.
var readOnlyGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "branch": true,
	"remote": true, "ls-files": true, "rev-parse": true, "blame": true,
	"describe": true, "tag": true, "shortlog": true,
}

// verbArgsMutate reports whether a read-only verb's own arguments request a file
// write or command execution that the top-level redirection/writeIndicators
// checks miss. It guards verbs (find, sort) that are read-only in normal use but
// can mutate through a finite set of flags/primaries with no shell redirection.
func verbArgsMutate(verb string, args []string) bool {
	switch verb {
	case "find":
		// Primaries that delete, execute, or write files.
		for _, a := range args {
			switch a {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir",
				"-fprintf", "-fprint", "-fprint0", "-fls":
				return true
			}
		}
	case "sort":
		// -o/--output writes results to a named file (no shell redirection).
		for _, a := range args {
			if a == "-o" || a == "--output" ||
				strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "--output=") {
				return true
			}
		}
	}
	return false
}

// writeIndicators are tokens that, anywhere in a command, mark it as mutating
// regardless of the leading verb (output redirection and destructive verbs).
var writeIndicators = map[string]bool{
	"rm": true, "mv": true, "cp": true, "mkdir": true, "rmdir": true,
	"touch": true, "tee": true, "dd": true, "chmod": true, "chown": true,
	"ln": true, "truncate": true, "install": true,
}

// ReadOnlyBash reports whether a Bash command line is read-only, i.e. safe to
// run under plan mode. It is a conservative heuristic: it returns true only when
// EVERY simple command in a (possibly compound) line is recognised read-only,
// and false the moment it sees output redirection, a destructive verb, or an
// unrecognised command. Wrappers are canonicalized away before classification.
//
// Examples that are read-only: `ls`, `cat f`, `grep x f`, `git status`,
// `git log`, `git diff`. Examples that are NOT: anything containing `>`, `>>`,
// `rm`, `mv`, `mkdir`, `git commit`, or an unknown command.
func ReadOnlyBash(cmd string) bool {
	parts := SplitCommands(cmd)
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if !simpleReadOnly(p) {
			return false
		}
	}
	return true
}

// simpleReadOnly classifies a single simple command (no shell operators).
func simpleReadOnly(cmd string) bool {
	// Command/process substitution or subshell grouping can hide an arbitrary
	// (possibly destructive) inner command the splitter does not decompose; fail
	// safe and treat the whole segment as not read-only.
	if HasSubstitutionOrGrouping(cmd) {
		return false
	}
	canon := Canonicalize(cmd)
	// Any output redirection makes it a write.
	if strings.ContainsAny(canon, ">") {
		return false
	}
	fields := strings.Fields(canon)
	if len(fields) == 0 {
		return false
	}
	// A destructive verb anywhere in the line disqualifies it.
	for _, f := range fields {
		if writeIndicators[f] {
			return false
		}
	}
	verb := fields[0]
	if verb == "git" {
		if len(fields) < 2 {
			return false
		}
		return readOnlyGitSubcommands[fields[1]]
	}
	if !readOnlyVerbs[verb] {
		return false
	}
	// A read-only verb can still mutate through its own arguments (e.g.
	// `find -delete`, `sort -o`); reject those.
	return !verbArgsMutate(verb, fields[1:])
}
