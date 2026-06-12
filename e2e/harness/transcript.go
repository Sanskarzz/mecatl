//go:build e2e

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Transcript is the per-scenario JSONL artifact: one line per prompt/event/
// approval/meta record, each carrying a wall-clock timestamp, the record kind,
// the Go type of the payload, and the payload itself. It is the SELF-DIAGNOSING
// failure report: replaying it answers "what did the model do" without re-paying
// for the run.
type Transcript struct {
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
	path string
}

// NewTranscript opens <artifactsDir>/<scenario>/transcript-<ts>.jsonl. When
// artifactsDir is "" (remote target) it falls back to
// <repo>/.scratch/e2e-artifacts so remote runs still produce artifacts.
func NewTranscript(artifactsDir, scenario string) (*Transcript, error) {
	if artifactsDir == "" {
		repo, err := RepoRoot()
		if err != nil {
			return nil, err
		}
		artifactsDir = filepath.Join(repo, ".scratch", "e2e-artifacts")
	}
	dir := filepath.Join(artifactsDir, sanitizeName(scenario))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "transcript-"+time.Now().Format("150405.000")+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Transcript{f: f, enc: json.NewEncoder(f), path: path}, nil
}

// Path returns the transcript file path.
func (t *Transcript) Path() string { return t.path }

// entry is one JSONL line.
type entry struct {
	TS   string `json:"ts"`
	Kind string `json:"kind"`
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Record appends one line. Marshal failures degrade to a stringified payload —
// the artifact must never be the thing that fails.
func (t *Transcript) Record(kind string, payload any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := entry{
		TS:   time.Now().Format(time.RFC3339Nano),
		Kind: kind,
		Type: typeName(payload),
		Data: payload,
	}
	if err := t.enc.Encode(e); err != nil {
		e.Data = fmt.Sprintf("%+v (marshal error: %v)", payload, err)
		_ = t.enc.Encode(e)
	}
}

// Close flushes and closes the file.
func (t *Transcript) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.f.Close()
}

func typeName(v any) string {
	if v == nil {
		return ""
	}
	return reflect.TypeOf(v).String()
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// Classify produces the failure-report triage line: a best-effort
// network-vs-model-vs-harness classification from the collected run.
func Classify(res *RunResult, err error) string {
	switch {
	case err != nil && (strings.Contains(err.Error(), "connection") ||
		strings.Contains(err.Error(), "Unavailable") ||
		strings.Contains(err.Error(), "deadline")):
		return "likely NETWORK/TRANSPORT: " + err.Error()
	case err != nil:
		return "likely HARNESS (pre-stream failure): " + err.Error()
	case res == nil:
		return "HARNESS: no run result collected"
	case res.TimedOut:
		return "TIMEOUT: the run exceeded the scenario budget (model stall, idle stream, or a parked ask — check the ask ledger and the mecated log)"
	case res.StreamErr != nil:
		return "likely NETWORK/TRANSPORT (stream error): " + res.StreamErr.Error()
	case res.Result == nil:
		return "HARNESS/SERVER: stream closed without a terminal result event"
	case res.Result.Error != "":
		return "PROVIDER/MODEL (run failed): " + res.Result.Error
	case len(res.Denied) > 0:
		return "HARNESS PERMISSION GAP: the model asked for " + strings.Join(res.Denied, ", ") + " and was auto-denied — either steer the prompt away or add the tool to ApproveTools"
	default:
		return "MODEL BEHAVIOUR: the run completed (stop=" + res.Result.Stop + ") but the asserted events/side effects were not observed — see the transcript"
	}
}

// Summary renders the compact failure-report body attached to a failing spec.
// logTail is the server's combined stdout+stderr tail (Target.LogTail).
func Summary(res *RunResult, err error, logTail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "classification: %s\n", Classify(res, err))
	if res != nil {
		fmt.Fprintf(&b, "transcript:     %s\n", res.TranscriptPath)
		fmt.Fprintf(&b, "session:        %s\n", res.SessionID)
		fmt.Fprintf(&b, "resolved model: %s/%s\n", res.Resolved.ProviderID, res.Resolved.ModelID)
		fmt.Fprintf(&b, "stop:           %s\n", res.Stop())
		u := res.Usage()
		fmt.Fprintf(&b, "usage:          in=%d out=%d cache_read=%d\n", u.InputTokens, u.OutputTokens, u.CacheReadTokens)
		if len(res.Asks) > 0 {
			fmt.Fprintf(&b, "asks:           approved=%v denied=%v\n", res.Approved, res.Denied)
		}
	}
	if logTail != "" {
		fmt.Fprintf(&b, "--- mecated log tail ---\n%s\n", logTail)
	}
	return b.String()
}
