package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/internal/adapter/gitenv"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// SummarySchemaVersion is the current Summary.SchemaVersion value. It is bumped only
// on a BREAKING change to the contract (a field removed or its meaning changed);
// additive fields do NOT bump it. Pipeline 2 keys compatibility off this.
const SummarySchemaVersion = 1

// Summary is the STABLE machine-readable result of a single mecatequi run. Pipeline 2
// consumes it; the contract is ADDITIVE-ONLY thereafter (new fields may be appended,
// existing fields never change meaning or JSON name). It is emitted as a single JSON
// object to --out-summary.
//
// HONESTY INVARIANT: StopReason and NonEmptyDiff always reflect the ACTUAL terminal
// EvResult and the ACTUAL working-tree git diff — never hardcoded optimism. A run that
// the model ended with no_progress / budget / structured_output reports that reason,
// and a run that touched no files reports NonEmptyDiff=false.
//
// Exit-code mapping (see exitCode): end_turn -> 0; the CLEAN non-completions
// (no_progress, budget, max_turns, max_tool_calls, max_consecutive_failures,
// structured_output) -> 0 (the reason is honest in StopReason); error -> 1;
// cancelled -> 1; a SETUP failure (bad flags, missing prompt, Build/CreateSession
// error, not-a-git-repo, write failure) -> 2.
//
// IMPORTANT for consumers: exit code 1 alone does NOT distinguish an error terminal
// from a cancelled one (a model failure, a no-approver cancel-on-ask, and a timeout all
// map to 1). The consumer MUST read StopReason (and Error) to tell them apart. Likewise
// exit 0 is NOT "task accomplished": no_progress / budget / max_* all exit 0 — read
// StopReason and NonEmptyDiff to judge whether real work landed.
type Summary struct {
	// SchemaVersion is the contract version (SummarySchemaVersion). It is the FIRST
	// field deliberately — a consumer reads it before anything else. Bumped only on a
	// breaking change.
	SchemaVersion int `json:"schema_version"`
	// SessionID is the id of the session the run drove.
	SessionID string `json:"session_id"`
	// StopReason is the terminal session.StopReason value verbatim. It is a CLOSED
	// enum (a string on the wire, no proto enum):
	//
	//	end_turn                  — the model finished (clean, exit 0)
	//	no_progress               — silent/empty turns exhausted the nudge budget (clean, exit 0)
	//	budget                    — the token ceiling tripped (clean, exit 0)
	//	max_turns                 — the turn limit tripped (clean, exit 0)
	//	max_tool_calls            — the tool-call limit tripped (clean, exit 0)
	//	max_consecutive_failures  — too many tool failures in a row (clean, exit 0)
	//	structured_output         — output-schema retries exhausted (clean, exit 0)
	//	error                     — the run failed (exit 1; Error is set)
	//	cancelled                 — the run was cancelled, incl. the no-approver
	//	                            cancel-on-ask and the --timeout path (exit 1)
	//
	// "" (the empty string) is also possible: NO terminal EvResult was observed (the
	// stream closed without one). It is TREATED AS FAILURE (exit 1) — a run that never
	// reported a terminal did not finish.
	StopReason string `json:"stop_reason"`
	// NonEmptyDiff reports whether the run left an uncommitted working-tree change.
	NonEmptyDiff bool `json:"non_empty_diff"`
	// DiffBytes is the size in bytes of the computed working-tree patch (`git diff
	// HEAD`). It lets a consumer size-gate the change without stat'ing the patch file.
	// 0 when the diff is empty or no diff was computed.
	DiffBytes int `json:"diff_bytes"`
	// Usage is the cumulative token accounting for the run (from the terminal
	// EvResult).
	Usage SummaryUsage `json:"usage"`
	// Error carries the failure detail; non-empty ONLY when StopReason == "error".
	Error string `json:"error,omitempty"`
	// FinalText is the terminal assistant text (the run's deliverable prose).
	FinalText string `json:"final_text"`
}

// SummaryUsage is the token accounting carried in a Summary. It mirrors
// session.Usage's fields plus the TotalTokens proxy, all as stable JSON names.
type SummaryUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// runOutcome is the result of run(): the Summary, the captured event stream (the
// durable-log source), and NoApprover — set when the run was cancelled because the MAIN
// engine asked for a permission approval and mecatequi has no approver attached. The
// caller uses NoApprover to print the actionable "re-run with --posture …" guidance and
// to distinguish a no-approver cancel from an ordinary cancel in the operator message
// (the StopReason is "cancelled" in both, honestly).
type runOutcome struct {
	Summary    Summary
	Events     []session.Event
	NoApprover bool
}

// run drives ONE prompt to completion against svc and returns the outcome. It is the
// testable core: it takes a *server.Service (NOT an *app.Built or a Config it builds
// internally) so a test can construct a Service over a SCRIPTED mockllm and exercise
// the terminal-result extraction faithfully.
//
// It creates a session, starts the run, and ranges run.Events() ONCE — writing the
// human-readable render of each event to w AND capturing the event for the durable
// log. We capture from the channel rather than wiring Config.EventLog DELIBERATELY:
// EventLog.Append is RELAY-ONLY (it fires in internal/adapter/server's grpc.go/http.go
// relays), and the embedded in-process drive has NO relay — so wiring Config.EventLog
// would produce an EMPTY log. mecatequi IS the relay here. The events from
// run.Events() are already redacted (the stream is the redaction boundary), so we
// inherit that and add no redaction.
//
// CANCEL-ON-ASK (the headless main-ask trap): mecatequi has no approver and the run ctx
// is not interactive, so a MAIN-engine permission ask (e.g. posture=strict asking on a
// mutate) would park AWAITING forever and the range loop would block. Any
// EvPermissionAsk reaching this loop IS a parent-own ask — in headless mode a CHILD ask
// is auto-denied and never surfaced (engine/agent dispatch.go), so there is no
// ambiguity. On the first such ask we Cancel() the run and KEEP RANGING until the
// channel closes (drain-to-close, never a bare break — that would leak the loop
// goroutine). The terminal is then "cancelled"; NoApprover records the cause so the
// caller can explain it.
//
// The returned error is non-nil ONLY on a SETUP failure (CreateSession failing); a
// model/run terminal of error/cancelled is reported in the Summary (StopReason) with a
// nil error, so the caller maps it via exitCode rather than treating it as a setup
// failure.
//
// workspace is the session root passed to CreateSession (the default profile requires
// a non-empty workspace). The architect's sketch took only (ctx, svc, prompt, w); the
// workspace is threaded explicitly because the Service does not expose its configured
// default and CreateSession needs a concrete root — this keeps run self-contained for
// the adversarial tests (which pass a memfs root like "/ws").
func run(ctx context.Context, svc *server.Service, workspace, prompt string, w io.Writer) (runOutcome, error) {
	sess, err := svc.CreateSession(ctx, workspace, session.ModeDefault, session.Limits{})
	if err != nil {
		return runOutcome{}, fmt.Errorf("create session: %w", err)
	}

	r, err := svc.StartRunContent(ctx, sess.ID, prompt, nil)
	if err != nil {
		return runOutcome{Summary: Summary{SchemaVersion: SummarySchemaVersion, SessionID: string(sess.ID)}}, fmt.Errorf("start run: %w", err)
	}

	var (
		captured   []session.Event
		result     *session.ResultPayload
		noApprover bool
	)
	for ev := range r.Events() {
		// Capture EVERY observed event for the durable log (inherits the stream's
		// redaction), then render it for the human log.
		captured = append(captured, ev)
		_, _ = fmt.Fprintln(w, formatEvent(ev))

		// A MAIN-engine permission ask with no approver attached: cancel the run so it
		// cannot park AWAITING forever, then keep draining until the channel closes.
		// Cancel is idempotent, so guarding on the first ask only is belt-and-braces.
		if ev.Type == session.EvPermissionAsk && !noApprover {
			noApprover = true
			r.Cancel()
		}

		if ev.Type == session.EvResult && ev.Result != nil {
			// Copy out of the loop variable: the channel reuses no backing storage,
			// but holding the pointer past the range is clearer with a local copy.
			rp := *ev.Result
			result = &rp
		}
	}

	// Persist + unregister the run, matching the service-level test pattern, so the
	// session reaches its terminal snapshot and the run is released.
	svc.Persist(ctx, sess.ID)
	svc.FinishRun(sess.ID, r)

	sum := Summary{SchemaVersion: SummarySchemaVersion, SessionID: string(sess.ID)}
	if result != nil {
		sum.StopReason = string(result.Stop)
		sum.FinalText = result.Text
		sum.Error = result.Error
		sum.Usage = SummaryUsage{
			InputTokens:      result.Usage.InputTokens,
			OutputTokens:     result.Usage.OutputTokens,
			CacheReadTokens:  result.Usage.CacheReadTokens,
			CacheWriteTokens: result.Usage.CacheWriteTokens,
			TotalTokens:      result.Usage.TotalTokens(),
		}
	}
	return runOutcome{Summary: sum, Events: captured, NoApprover: noApprover}, nil
}

// exitCode maps a Summary's terminal stop reason to a process exit code.
//
//	end_turn                      -> 0  (clean completion)
//	no_progress / budget /
//	max_turns / max_tool_calls /
//	max_consecutive_failures /
//	structured_output             -> 0  (CLEAN terminals; the reason is honest)
//	error                         -> 1
//	cancelled                     -> 1
//	(anything else, incl. empty)  -> 1  (fail-safe: an unrecognised/absent terminal)
//
// SETUP failures (bad flags, missing prompt, Build/CreateSession error,
// not-a-git-repo, write failure) exit 2 and are decided in main, not here.
func exitCode(s Summary) int {
	switch session.StopReason(s.StopReason) {
	case session.StopEndTurn,
		session.StopNoProgress,
		session.StopBudget,
		session.StopMaxTurns,
		session.StopMaxToolCalls,
		session.StopMaxConsecutiveFailures,
		session.StopStructuredOutput:
		return 0
	default:
		// StopError, StopCancelled, StopNone (no terminal observed), and any
		// unknown reason all fail.
		return 1
	}
}

// writeDurableLog writes the captured events to path as JSONL — one json.Marshal of a
// session.Event per line. The events are already redacted (inherited from the stream),
// so no redaction is added here. An empty path is a no-op (the log is disabled).
func writeDurableLog(w io.Writer, events []session.Event) error {
	enc := json.NewEncoder(w)
	for i := range events {
		if err := enc.Encode(events[i]); err != nil {
			return fmt.Errorf("encode event %d: %w", i, err)
		}
	}
	return nil
}

// validateWorkspaceRepo asserts the workspace is a git work tree AND is its TOP LEVEL
// (not a subdirectory). A subdir would let gitDiffPatch silently diff an enclosing
// repo — reporting changes from the wrong root, the wrong forge artifact. It resolves
// both the requested workspace and `git rev-parse --show-toplevel` to absolute,
// symlink-evaluated paths before comparing, so a symlinked or relative workspace still
// matches its own top level. A non-repo or a subdir is a setup error (the caller maps
// it to exit 2). The git env is scrubbed (gitenv.Scrub) like gitDiffPatch.
func validateWorkspaceRepo(ctx context.Context, workspace string) error {
	env := gitenv.Scrub(os.Environ())

	top := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	top.Dir = workspace
	top.Env = env
	out, err := top.Output()
	if err != nil {
		return fmt.Errorf("workspace %q is not a git repository (needed to compute the run's diff); run mecatequi against a repo root", workspace)
	}
	toplevel := strings.TrimSpace(string(out))

	// Compare absolute, symlink-resolved paths so a relative or symlinked --workspace
	// still matches its own top level. EvalSymlinks failures fall back to the abs path
	// (best-effort; the comparison just gets stricter, never falsely passes).
	wantAbs := resolvePath(workspace)
	gotAbs := resolvePath(toplevel)
	if wantAbs != gotAbs {
		return fmt.Errorf("workspace %q is a SUBDIRECTORY of the repository rooted at %q; point --workspace at the repository top level so the diff covers the whole run", workspace, toplevel)
	}
	return nil
}

// resolvePath returns p as an absolute, symlink-evaluated path, falling back to the
// abs path (then p itself) if resolution fails — best-effort canonicalisation for the
// top-level comparison.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if eval, err := filepath.EvalSymlinks(abs); err == nil {
		return eval
	}
	return abs
}

// gitDiffPatch computes the working-tree diff of the workspace as a unified patch. It
// returns the patch bytes, whether the diff is NON-EMPTY, and an error. The git
// environment is SCRUBBED via gitenv.Scrub(os.Environ()) so no inherited GIT_* danger
// (GIT_EXTERNAL_DIFF, GIT_SSH_COMMAND, a stale GIT_CONFIG_*) influences the diff.
//
// It runs `git diff HEAD` so both tracked-file edits and staged changes are captured
// relative to the last commit; untracked files are reported via a follow-up
// status-porcelain check folded into the non-empty signal (a brand-new file the run
// created should count as a change even though `git diff` alone would miss it).
//
// A non-git workspace is a SETUP error (the caller maps it to exit 2): mecatequi's
// contract is to report the diff a run produced, which requires a git repo.
func gitDiffPatch(ctx context.Context, workspace string) (patch []byte, nonEmpty bool, err error) {
	env := gitenv.Scrub(os.Environ())

	// Verify it is a work tree first, so a non-repo is a clear, actionable error
	// rather than a confusing empty diff.
	check := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	check.Dir = workspace
	check.Env = env
	if out, cerr := check.CombinedOutput(); cerr != nil {
		return nil, false, fmt.Errorf("workspace %q is not a git repository: %v: %s", workspace, cerr, strings.TrimSpace(string(out)))
	}

	// --no-ext-diff disables ANY external diff driver: belt (the scrubbed env already
	// neutralises GIT_EXTERNAL_DIFF and forces diff.external empty) and braces (an
	// empty diff.external would otherwise make git try to exec "" — "cannot run : No
	// such file or directory" — the moment there is a real tracked-file change to
	// render). It is also the correct hardening: never run a repo-named diff driver.
	diff := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff", "HEAD")
	diff.Dir = workspace
	diff.Env = env
	out, derr := diff.Output()
	if derr != nil {
		return nil, false, fmt.Errorf("git diff HEAD in %q: %w", workspace, derr)
	}
	patch = out
	nonEmpty = len(strings.TrimSpace(string(out))) > 0

	// Fold untracked files into the non-empty signal: a run that created a NEW file
	// (not yet added) is a real change `git diff HEAD` would not show.
	if !nonEmpty {
		status := exec.CommandContext(ctx, "git", "status", "--porcelain")
		status.Dir = workspace
		status.Env = env
		if sout, serr := status.Output(); serr == nil {
			nonEmpty = len(strings.TrimSpace(string(sout))) > 0
		}
	}
	return patch, nonEmpty, nil
}

// formatEvent renders one event as a single human-readable log line, mirroring
// cmd/mecademo's render so an operator reading the human log sees the familiar shape
// (turn -> tool.call -> result). It is intentionally terse; the durable JSONL log is
// the complete record.
func formatEvent(ev session.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%03d] turn=%d %-14s", ev.Seq, ev.Turn, ev.Type)
	switch ev.Type {
	case session.EvMessageDelta:
		fmt.Fprintf(&b, " text=%q", ev.Text)
	case session.EvToolCall:
		if ev.ToolCall != nil {
			fmt.Fprintf(&b, " tool=%s args=%s", ev.ToolCall.Name, string(ev.ToolCall.Args))
		}
	case session.EvToolResult:
		if ev.ToolResult != nil {
			fmt.Fprintf(&b, " error=%t result=%q", ev.ToolResult.IsError, oneLine(ev.ToolResult.Content))
		}
	case session.EvPermissionAsk:
		if ev.Ask != nil {
			fmt.Fprintf(&b, " ASK tool=%s reason=%q", ev.Ask.Tool, ev.Ask.Reason)
		}
	case session.EvResult:
		if ev.Result != nil {
			fmt.Fprintf(&b, " stop=%s text=%q", ev.Result.Stop, oneLine(ev.Result.Text))
			if ev.Result.Error != "" {
				fmt.Fprintf(&b, " error=%q", ev.Result.Error)
			}
			u := ev.Result.Usage
			fmt.Fprintf(&b, " usage(in=%d out=%d total=%d)", u.InputTokens, u.OutputTokens, u.TotalTokens())
		}
	}
	return b.String()
}

// oneLine collapses newlines so a multi-line value prints on one log row.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", " / ")
}
