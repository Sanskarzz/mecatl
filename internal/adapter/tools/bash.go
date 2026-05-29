package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stacklok/mecatl/internal/session"
	"github.com/stacklok/mecatl/internal/tool"
)

// bashDescription is the model-facing documentation for the Bash tool.
const bashDescription = `Run a shell command in the workspace root and return its combined output and exit code.

When to use:
- To run builds, tests, linters, git, and other CLI tooling.
- For operations no dedicated tool covers.

When NOT to use:
- To read a file (use Read), search contents (use Grep), or find files by name
  (use Glob). Those tools give cleaner, line-numbered, capped output.

Behavior:
- The command runs with the workspace root as its working directory.
- Standard output and standard error are captured together and returned along
  with the process exit code. A non-zero exit code is reported, not hidden.

Arguments:
- command    (required): the shell command line to run.
- timeout_ms (optional): cancel the command after this many milliseconds.

Example:
  {"command": "go test ./...", "timeout_ms": 120000}

Limits:
- Output is truncated to ~25000 bytes; redirect to a file and Read it in pages
  if you need more.
- Whether a given command is permitted is decided by the harness, not this tool.`

// BashTool runs a shell command via an injected tool.CommandRunner. It is
// statically classified as non-read-only: deciding whether a specific command is
// read-only is governance's job, not this tool's.
//
// The runner is injected at construction (NewBashTool) rather than taken from
// the Workspace, because command execution is optional: a deployment with no
// shell simply never constructs a BashTool. The frozen Tool.Execute signature
// still receives a Workspace, but the Bash tool ignores it.
type BashTool struct {
	runner tool.CommandRunner
}

// NewBashTool constructs the Bash tool bound to runner, which performs the actual
// command execution (locally via osfs.NewCommandRunner, in a remote environment,
// or refusing with tool.ErrNoShell). The composition root registers the returned
// tool ONLY when a runner is configured; without one, the catalog has no Bash and
// the agent runs shell-less. runner must be non-nil.
func NewBashTool(runner tool.CommandRunner) tool.Tool {
	if runner == nil {
		panic("tools: NewBashTool requires a non-nil CommandRunner")
	}
	return BashTool{runner: runner}
}

// Compile-time assertion that BashTool implements tool.Tool.
var _ tool.Tool = BashTool{}

// bashArgs is the JSON argument shape for the Bash tool.
type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

// Spec returns the model-facing specification of the Bash tool.
func (BashTool) Spec() tool.ToolSpec {
	return tool.ToolSpec{
		Name:        "Bash",
		Description: bashDescription,
		Schema: schema(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command line to run in the workspace root."},
    "timeout_ms": {"type": "integer", "description": "Optional timeout in milliseconds."}
  },
  "required": ["command"]
}`),
	}
}

// ReadOnly reports that Bash is statically treated as mutating.
func (BashTool) ReadOnly() bool { return false }

// Execute runs the command, honoring an optional timeout, and returns combined
// output with the exit code.
func (bt BashTool) Execute(ctx context.Context, in session.ToolCall, _ tool.Workspace) (session.ToolResult, error) {
	var args bashArgs
	if msg, ok := parseArgs(in, &args); !ok {
		return session.NewToolError(in.ID, msg), nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return session.NewToolError(in.ID, "the \"command\" argument is required"), nil
	}
	if args.TimeoutMS < 0 {
		return session.NewToolError(in.ID, "\"timeout_ms\" must be non-negative"), nil
	}

	if args.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	res, err := bt.runner.Run(ctx, args.Command)
	if err != nil {
		// Surface command-execution failures (no shell, timeout, cancellation)
		// to the model so it can adapt, rather than aborting the harness.
		return session.NewToolError(in.ID, fmt.Sprintf("command failed to run: %v", err)), nil
	}

	var b strings.Builder
	if res.Stdout != "" {
		b.WriteString(res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if res.Stderr != "" {
		b.WriteString(res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	fmt.Fprintf(&b, "[exit code: %d]", res.ExitCode)

	out := truncateBytes(b.String())
	if res.ExitCode != 0 {
		return session.NewToolError(in.ID, out), nil
	}
	return session.NewToolResult(in.ID, out), nil
}
