// Package hookexec implements port.HookRunner by running a configured shell
// command per lifecycle HookPhase. The HookEvent is serialized to JSON and
// written to the hook process's stdin; the process's exit code carries the
// outcome:
//
//	0          → allow (HookOutcome.Block == false)
//	2          → block (HookOutcome.Block == true); the message is read from
//	             stdout (preferred) or stderr
//	other != 0 → error (the hook itself failed)
//
// A nil or empty hook map means "no hooks configured": every event is allowed.
// Execution honours the caller's context and a per-run timeout.
package hookexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/stacklok/mecatl/internal/governance"
)

// DefaultTimeout bounds a single hook invocation when no timeout is supplied.
const DefaultTimeout = 30 * time.Second

const (
	exitAllow = 0
	exitBlock = 2
)

// Runner implements port.HookRunner over OS process execution. Construct it with
// New. The zero value runs no hooks (allows everything).
type Runner struct {
	hooks   map[governance.HookPhase]string
	timeout time.Duration
	shell   string
}

// Option configures a Runner.
type Option func(*Runner)

// WithTimeout sets the per-invocation timeout. A non-positive value resets to
// DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(r *Runner) {
		if d <= 0 {
			d = DefaultTimeout
		}
		r.timeout = d
	}
}

// WithShell overrides the shell used to interpret hook commands (default
// "/bin/sh"). Each command is run as `<shell> -c <command>`.
func WithShell(shell string) Option {
	return func(r *Runner) {
		if shell != "" {
			r.shell = shell
		}
	}
}

// New constructs a Runner that runs the given phase → shell-command map. A nil or
// empty map yields a Runner that allows every event.
func New(hooks map[governance.HookPhase]string, opts ...Option) *Runner {
	r := &Runner{
		hooks:   hooks,
		timeout: DefaultTimeout,
		shell:   "/bin/sh",
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run executes the hook registered for ev.Phase. With no hook for the phase the
// event is allowed. The HookEvent is delivered as JSON on the hook's stdin.
func (r *Runner) Run(ctx context.Context, ev governance.HookEvent) (governance.HookOutcome, error) {
	cmd, ok := r.hooks[ev.Phase]
	if !ok || strings.TrimSpace(cmd) == "" {
		// No hook configured for this phase: allow.
		return governance.HookOutcome{}, nil
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return governance.HookOutcome{}, fmt.Errorf("hookexec: marshal event: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(runCtx, r.shell, "-c", cmd)
	c.Stdin = bytes.NewReader(payload)
	c.Stdout = &stdout
	c.Stderr = &stderr

	runErr := c.Run()

	// Surface context cancellation / timeout explicitly rather than as a plain
	// exit error, so callers can distinguish an aborted run from a hook verdict.
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return governance.HookOutcome{}, fmt.Errorf("hookexec: %s hook: %w", ev.Phase, ctxErr)
	}

	if runErr == nil {
		// Exit 0 → allow.
		return governance.HookOutcome{Message: trimmed(stdout.String())}, nil
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		switch exitErr.ExitCode() {
		case exitAllow:
			return governance.HookOutcome{Message: trimmed(stdout.String())}, nil
		case exitBlock:
			return governance.HookOutcome{
				Block:   true,
				Message: blockMessage(&stdout, &stderr),
			}, nil
		default:
			return governance.HookOutcome{}, fmt.Errorf(
				"hookexec: %s hook exited %d: %s",
				ev.Phase, exitErr.ExitCode(), blockMessage(&stdout, &stderr))
		}
	}

	// Failure to start the process (e.g. bad shell): treat as an error.
	return governance.HookOutcome{}, fmt.Errorf("hookexec: %s hook: %w", ev.Phase, runErr)
}

// blockMessage prefers stdout, falling back to stderr, for the human-readable
// reason a hook surfaces.
func blockMessage(stdout, stderr *bytes.Buffer) string {
	if m := trimmed(stdout.String()); m != "" {
		return m
	}
	return trimmed(stderr.String())
}

func trimmed(s string) string { return strings.TrimSpace(s) }
