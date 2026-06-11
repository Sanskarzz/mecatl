package sourceconformance

import (
	"context"
	"reflect"
	"testing"

	"github.com/stacklok/mecatl/engine/prompt"
)

// FixtureCommand is one canonical fixture slash command: invocation metadata
// plus the RAW template body (optional YAML frontmatter INCLUDED — stripping
// is the expander's job, never the source's, so the body must round-trip
// verbatim).
type FixtureCommand struct {
	Name        string
	Description string
	Body        string
}

// CommandFixture is the canonical command set RunCommandSource asserts
// against, sorted by Name. It covers a frontmatter-less positional template,
// a punctuation-bearing name (the full invocation grammar), and a
// frontmatter-bearing template with $ARGUMENTS — the raw body keeps its
// frontmatter so the round-trip subtest proves the source never strips.
var CommandFixture = []FixtureCommand{
	{
		Name:        "fix",
		Description: "Fix a reported bug.",
		Body:        "Fix the bug in $1 and add a regression test.",
	},
	{
		Name:        "pr.merge-2",
		Description: "Merge a pull request (grammar: letters, digits, '-', '_', '.').",
		Body:        "Merge PR $1 once checks pass.",
	},
	{
		Name:        "review",
		Description: "Run a structured review.",
		Body:        "---\ndescription: Run a structured review.\n---\nReview $ARGUMENTS for correctness first, style second.",
	},
}

// RunCommandSource executes the shared CommandSource conformance table
// against the source produced by newSource. newSource must return a fresh
// source serving EXACTLY the canonical CommandFixture each call.
//
// NOTE on lifecycle: the PORT is LIVE-semantics (the set MAY change between
// calls); the fixture backend is FIXED, so the stability subtest pins only
// "a fixed backend lists stably", not snapshot semantics.
func RunCommandSource(t *testing.T, newSource func(t *testing.T) prompt.CommandSource) {
	t.Helper()
	ctx := context.Background()

	t.Run("list matches fixture", func(t *testing.T) {
		src := newSource(t)
		cmds, err := src.ListCommands(ctx)
		if err != nil {
			t.Fatalf("ListCommands: %v", err)
		}
		if len(cmds) != len(CommandFixture) {
			t.Fatalf("ListCommands returned %d commands, want %d: %+v", len(cmds), len(CommandFixture), cmds)
		}
		seen := map[string]bool{}
		for i, c := range cmds {
			if seen[c.Name] {
				t.Errorf("duplicate command name %q (names must be unique)", c.Name)
			}
			seen[c.Name] = true
			if i > 0 && cmds[i-1].Name >= c.Name {
				t.Errorf("ListCommands not sorted by name: %q before %q", cmds[i-1].Name, c.Name)
			}
			if !prompt.ValidCommandName(c.Name) {
				t.Errorf("command name %q violates the invocation grammar (it could never be invoked)", c.Name)
			}
			if want := fixtureCommand(t, c.Name).Description; c.Description != want {
				t.Errorf("command %q Description = %q, want %q", c.Name, c.Description, want)
			}
		}
	})

	t.Run("body round trip raw", func(t *testing.T) {
		src := newSource(t)
		for _, f := range CommandFixture {
			body, found, err := src.CommandBody(ctx, f.Name)
			if err != nil {
				t.Fatalf("CommandBody(%q): %v", f.Name, err)
			}
			if !found {
				t.Fatalf("CommandBody(%q) found = false, want true", f.Name)
			}
			if body != f.Body {
				t.Errorf("CommandBody(%q) = %q, want the RAW template (frontmatter intact) %q", f.Name, body, f.Body)
			}
		}
	})

	t.Run("unknown name is normal", func(t *testing.T) {
		src := newSource(t)
		body, found, err := src.CommandBody(ctx, "no-such-command")
		if err != nil {
			t.Errorf("CommandBody(unknown) err = %v, want nil (unknown is NORMAL, never an error)", err)
		}
		if found || body != "" {
			t.Errorf("CommandBody(unknown) = (%q, %v), want (\"\", false)", body, found)
		}
	})

	t.Run("list stable over fixed backend", func(t *testing.T) {
		src := newSource(t)
		first, err := src.ListCommands(ctx)
		if err != nil {
			t.Fatalf("ListCommands #1: %v", err)
		}
		second, err := src.ListCommands(ctx)
		if err != nil {
			t.Fatalf("ListCommands #2: %v", err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("a FIXED backend must list stably across calls:\n#1 %+v\n#2 %+v", first, second)
		}
	})
}

// fixtureCommand returns the fixture entry for name.
func fixtureCommand(t *testing.T, name string) FixtureCommand {
	t.Helper()
	for _, f := range CommandFixture {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no fixture command %q", name)
	return FixtureCommand{}
}

// NewCommandFixtureSource returns the in-memory REFERENCE prompt.CommandSource
// serving exactly the canonical CommandFixture. It lives here (not in a
// _test.go file) because the driver conformance fixtures mount it behind a
// wire server; it is also the suite's self-test subject.
func NewCommandFixtureSource() prompt.CommandSource {
	return commandFixtureSource{}
}

type commandFixtureSource struct{}

func (commandFixtureSource) ListCommands(_ context.Context) ([]prompt.Command, error) {
	out := make([]prompt.Command, 0, len(CommandFixture))
	for _, f := range CommandFixture {
		out = append(out, prompt.Command{Name: f.Name, Description: f.Description})
	}
	return out, nil
}

func (commandFixtureSource) CommandBody(_ context.Context, name string) (string, bool, error) {
	for _, f := range CommandFixture {
		if f.Name == name {
			return f.Body, true, nil
		}
	}
	return "", false, nil
}
