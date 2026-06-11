package prompt

import (
	"context"

	"github.com/stacklok/mecatl/engine/tool"
)

// MaxCommandDescriptionRunes caps a command description (in RUNES) so a long
// frontmatter line or body sentence cannot blow out a palette row. It is the
// exported single source of the cap every command surface shares: the
// DirCommandExpander's derived descriptions (maxDescriptionLen aliases it) and
// a remote CommandSource client's defensive re-cap.
const MaxCommandDescriptionRunes = 80

// ValidCommandName reports whether name is a valid command invocation name:
// non-empty and made solely of the invocation grammar's name runes (letters,
// digits, '-', '_', '.'). It is the ONE shared validator: parseCommand accepts
// exactly this set, so a name that fails here can never be invoked as
// "/<name>" — consumers (a palette, a remote-source client) drop violators.
func ValidCommandName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !isNameRune(r) {
			return false
		}
	}
	return true
}

// CommandSource is the consumer-local port a non-filesystem command backend
// implements (the SoulSource precedent: defined here in prompt, next to its
// consumer). It carries NO path/dir/root concept — where a command template
// lives is the implementation's private business.
//
// Lifecycle: LIVE semantics — the source is consulted on EVERY Expand/List
// call (no snapshot), matching the DirCommandExpander's reads-current-files
// discipline, so a palette promise of "the CURRENT commands" holds for remote
// backends too.
type CommandSource interface {
	// ListCommands returns the available commands' metadata only (no bodies),
	// de-duplicated by name and name-sorted. The set MAY change between calls
	// (live semantics). A non-nil error is a genuine backend fault.
	ListCommands(ctx context.Context) ([]Command, error)
	// CommandBody returns the RAW template for name: optional YAML frontmatter
	// is permitted and is stripped by the expander (never by the source), so
	// the body round-trips verbatim. found=false reports an unknown name — a
	// NORMAL outcome (the input passes through unchanged), never an error.
	CommandBody(ctx context.Context, name string) (body string, found bool, err error)
}

// SourceExpander adapts a CommandSource to the CommandExpander/CommandLister
// seams the agent loop and the palette consume, reusing the SAME grammar
// (parseCommand), frontmatter stripping, and placeholder substitution as
// DirCommandExpander — so a template expands byte-identically whichever
// backend serves it. The Workspace argument is ignored (mirrors SoulAssembler:
// the source is not workspace-rooted).
type SourceExpander struct {
	src CommandSource
}

// NewSourceExpander builds a SourceExpander over src.
func NewSourceExpander(src CommandSource) *SourceExpander {
	return &SourceExpander{src: src}
}

// Expand implements CommandExpander: a non-command input passes through
// unchanged; an unknown command name passes through unchanged (found=false is
// normal); a backend fault is returned as an error; otherwise the body is
// frontmatter-stripped and placeholder-substituted exactly like a file-backed
// command.
func (e *SourceExpander) Expand(ctx context.Context, _ tool.Workspace, input string) (string, bool, error) {
	name, args, ok := parseCommand(input)
	if !ok {
		return input, false, nil
	}
	body, found, err := e.src.CommandBody(ctx, name)
	if err != nil {
		return input, false, err
	}
	if !found {
		return input, false, nil
	}
	return substitute(stripFrontmatter(body), args), true, nil
}

// List implements CommandLister as a passthrough to the source's live listing.
func (e *SourceExpander) List(ctx context.Context, _ tool.Workspace) ([]Command, error) {
	return e.src.ListCommands(ctx)
}

// Compile-time assertions that *SourceExpander satisfies both seams.
var (
	_ CommandExpander = (*SourceExpander)(nil)
	_ CommandLister   = (*SourceExpander)(nil)
)
