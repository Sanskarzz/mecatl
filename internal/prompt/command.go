package prompt

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/stacklok/mecatl/internal/tool"
)

// CommandExpander rewrites a raw user input into the prompt the model sees. If
// the input is a command invocation (e.g. "/review foo.go"), it expands the
// matching template; otherwise it returns the input unchanged (expanded=false).
//
// It is the seam that makes slash commands / templated prompts pluggable: the
// agent loop consumes this interface in recordPrompt instead of using the raw
// user text directly, so a richer expander can be wired at the composition root
// without touching the loop. The default implementation, NoopExpander, returns
// the input unchanged so behaviour is identical when no commands are configured.
type CommandExpander interface {
	// Expand inspects input. When input is a command invocation it loads the
	// matching template from ws, substitutes its placeholders, and returns the
	// rendered body with expanded=true. When input is not a command, or the named
	// command does not exist, it returns input unchanged with expanded=false.
	//
	// A non-nil error is returned only for a genuine read fault discovering or
	// reading a command file (not for "not a command" or "unknown command", which
	// are normal, non-error outcomes that must not abort the run).
	Expand(ctx context.Context, ws tool.Workspace, input string) (string, bool, error)
}

// NoopExpander is the default CommandExpander. It performs no expansion and
// returns every input unchanged (expanded=false). The zero value is ready to
// use; it is the default in agent.Deps so command expansion is OFF unless a
// DirCommandExpander (or another adapter) is explicitly wired in.
type NoopExpander struct{}

// Expand implements CommandExpander by returning input unchanged.
func (NoopExpander) Expand(_ context.Context, _ tool.Workspace, input string) (string, bool, error) {
	return input, false, nil
}

// Compile-time assertion that NoopExpander satisfies the interface.
var _ CommandExpander = NoopExpander{}

// defaultCommandDirs are the workspace-relative directories DirCommandExpander
// searches, in order, for a command's <name>.md file. ".mecatl/commands/" is the
// native location; ".claude/commands/" is accepted for familiarity. The first
// directory that contains a matching file wins.
var defaultCommandDirs = []string{".mecatl/commands", ".claude/commands"}

// DirCommandExpander discovers command templates as <name>.md files under one or
// more workspace-relative directories (default ".mecatl/commands/" and
// ".claude/commands/"), read through the tool.Workspace FS port (never os, so
// the type stays infra-free / domain-pure).
//
// Invocation grammar: an input is a command iff, after trimming leading spaces,
// it begins with "/" followed by a non-empty command name made of letters,
// digits, '-', '_', or '.'. The remainder (after the name) is split on
// whitespace into positional arguments.
//
// Template substitution, applied to the command body:
//   - "$ARGUMENTS" → all arguments joined by a single space (empty if none).
//   - "$1", "$2", … → the corresponding positional argument (1-based); a
//     reference past the end of the argument list expands to the empty string.
//   - Any other "$"-prefixed token is left intact, so bodies may contain literal
//     shell-style variables the model is meant to see.
//
// Frontmatter: an optional leading YAML frontmatter block (delimited by a "---"
// line at the very start and a closing "---" line) is stripped before
// substitution, so only the body template is returned. The frontmatter is
// metadata (e.g. a description) and never reaches the model.
//
// Unknown command (no matching file in any directory) or non-command input
// returns the original input unchanged with expanded=false and no error, so a
// mistyped or unconfigured command never aborts the run.
type DirCommandExpander struct {
	dirs []string
}

// NewDirCommandExpander constructs a DirCommandExpander. Each dir is a
// workspace-relative directory searched in order for "<name>.md"; a blank dir is
// ignored. When no non-blank dir is given it falls back to the defaults
// (".mecatl/commands/" then ".claude/commands/").
func NewDirCommandExpander(dirs ...string) *DirCommandExpander {
	cleaned := make([]string, 0, len(dirs))
	for _, d := range dirs {
		d = strings.TrimRight(strings.TrimSpace(d), "/")
		if d != "" {
			cleaned = append(cleaned, d)
		}
	}
	if len(cleaned) == 0 {
		cleaned = append(cleaned, defaultCommandDirs...)
	}
	return &DirCommandExpander{dirs: cleaned}
}

// Expand implements CommandExpander. See the type doc for the grammar and
// substitution rules.
func (e *DirCommandExpander) Expand(ctx context.Context, ws tool.Workspace, input string) (string, bool, error) {
	name, args, ok := parseCommand(input)
	if !ok {
		return input, false, nil
	}

	for _, dir := range e.dirs {
		path := dir + "/" + name + ".md"
		data, err := ws.Read(ctx, path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return input, false, err
		}
		body := stripFrontmatter(string(data))
		return substitute(body, args), true, nil
	}

	// No matching command file in any directory: not an error, leave unchanged.
	return input, false, nil
}

// Compile-time assertion that DirCommandExpander satisfies the interface.
var _ CommandExpander = (*DirCommandExpander)(nil)

// parseCommand reports whether input is a command invocation and, if so, returns
// the command name and the positional arguments. An input is a command iff it
// begins (after leading whitespace) with "/" immediately followed by a non-empty
// run of name characters (letters, digits, '-', '_', '.'). The remainder is
// split on whitespace into args.
func parseCommand(input string) (name string, args []string, ok bool) {
	s := strings.TrimLeft(input, " \t")
	if !strings.HasPrefix(s, "/") {
		return "", nil, false
	}
	s = s[1:]
	// Find the end of the command name.
	end := len(s)
	for i, r := range s {
		if !isNameRune(r) {
			end = i
			break
		}
	}
	name = s[:end]
	if name == "" {
		return "", nil, false
	}
	args = strings.Fields(s[end:])
	return name, args, true
}

// isNameRune reports whether r is allowed in a command name.
func isNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_' || r == '.':
		return true
	default:
		return false
	}
}

// stripFrontmatter removes a leading YAML frontmatter block from body. A block
// is present only when the very first line is exactly "---"; it ends at the next
// line that is exactly "---". The content between (the metadata) and the
// delimiters are removed and the remaining body is returned with a single
// leading newline trimmed. If there is no opening delimiter, or no closing
// delimiter, body is returned unchanged.
func stripFrontmatter(body string) string {
	if !strings.HasPrefix(body, "---\n") && body != "---" && !strings.HasPrefix(body, "---\r\n") {
		return body
	}
	// Normalise the search to line-by-line so we tolerate CRLF.
	lines := strings.Split(body, "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return body
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			rest := strings.Join(lines[i+1:], "\n")
			return strings.TrimPrefix(rest, "\n")
		}
	}
	// No closing delimiter: not valid frontmatter, leave body untouched.
	return body
}

// substitute applies the placeholder rules ($ARGUMENTS, $1, $2, …) to body,
// leaving any other "$"-prefixed token intact.
func substitute(body string, args []string) string {
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); {
		if body[i] != '$' {
			b.WriteByte(body[i])
			i++
			continue
		}
		// At a '$': try to match a known placeholder.
		rest := body[i+1:]
		if strings.HasPrefix(rest, "ARGUMENTS") {
			b.WriteString(strings.Join(args, " "))
			i += 1 + len("ARGUMENTS")
			continue
		}
		// Positional: $ followed by one or more digits (1-based).
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j > 0 {
			n := atoi(rest[:j])
			if n >= 1 && n <= len(args) {
				b.WriteString(args[n-1])
			}
			// Out-of-range positional expands to empty (placeholder consumed).
			i += 1 + j
			continue
		}
		// Not a recognised placeholder: emit the '$' literally and move on.
		b.WriteByte('$')
		i++
	}
	return b.String()
}

// atoi parses a run of ASCII digits into an int. The input is guaranteed by the
// caller to contain only '0'–'9' and to be non-empty.
func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}
