package ui

import (
	"strconv"
	"strings"
	"testing"
)

// modalPlain renders the permission modal for an ask (expand is the ctrl+t
// details toggle) and strips ANSI so we can assert on substrings, matching the
// existing diff_test.go style.
func modalPlain(ask pendingAsk, expand bool) string {
	r := newTestRenderer()
	return stripANSIstr(r.renderPermissionModal(ask, expand, 80, 24))
}

// TestPermissionModalEditDiff: an Edit ask shows the -/+ diff, not raw JSON.
func TestPermissionModalEditDiff(t *testing.T) {
	plain := modalPlain(pendingAsk{
		AskID:  "ask-1",
		Tool:   "Edit",
		Args:   `{"path":"main.go","old_string":"fmt.Println(\"hi\")","new_string":"fmt.Println(\"hello\")"}`,
		Reason: "Edit requires approval",
	}, false)
	if !strings.Contains(plain, "main.go  -1 +1") {
		t.Errorf("expected diff header, got %q", plain)
	}
	if !strings.Contains(plain, `- fmt.Println("hi")`) || !strings.Contains(plain, `+ fmt.Println("hello")`) {
		t.Errorf("expected -/+ diff lines, got %q", plain)
	}
	if strings.Contains(plain, "old_string") {
		t.Errorf("modal should show diff, not raw JSON args, got %q", plain)
	}
	// Title, reason and buttons must survive.
	if !strings.Contains(plain, "Permission required") || !strings.Contains(plain, "Edit requires approval") {
		t.Errorf("expected title + reason, got %q", plain)
	}
	if !strings.Contains(plain, "[A]llow") || !strings.Contains(plain, "[D]eny") {
		t.Errorf("expected allow/deny buttons, got %q", plain)
	}
}

// TestPermissionModalWriteDiff: a Write ask shows an added/green diff.
func TestPermissionModalWriteDiff(t *testing.T) {
	plain := modalPlain(pendingAsk{
		Tool: "Write",
		Args: `{"path":"note.txt","content":"reviewed"}`,
	}, false)
	if !strings.Contains(plain, "note.txt · 1 line (overwrites if it exists)") {
		t.Errorf("expected non-asserting write header, got %q", plain)
	}
	if strings.Contains(plain, "new file") {
		t.Errorf("write header must not claim 'new file', got %q", plain)
	}
	if !strings.Contains(plain, "+ reviewed") {
		t.Errorf("expected added line, got %q", plain)
	}
	if strings.Contains(plain, `"content"`) {
		t.Errorf("modal should show diff, not raw JSON args, got %q", plain)
	}
}

// TestPermissionModalNonDiffToolFallback: a Bash ask keeps the JSON args.
func TestPermissionModalNonDiffToolFallback(t *testing.T) {
	plain := modalPlain(pendingAsk{
		Tool: "Bash",
		Args: `{"command":"rm -rf /tmp/x"}`,
	}, false)
	if !strings.Contains(plain, "command") || !strings.Contains(plain, "rm -rf /tmp/x") {
		t.Errorf("expected JSON args for Bash, got %q", plain)
	}
}

// TestPermissionModalMalformedEditFallback: malformed Edit args fall back to JSON.
func TestPermissionModalMalformedEditFallback(t *testing.T) {
	// Valid JSON but missing the required path/strings, so renderToolDiff returns
	// ok=false and the caller falls back to pretty JSON.
	plain := modalPlain(pendingAsk{
		Tool: "Edit",
		Args: `{"foo":"bar"}`,
	}, false)
	if !strings.Contains(plain, "foo") || !strings.Contains(plain, "bar") {
		t.Errorf("expected JSON fallback for malformed Edit, got %q", plain)
	}
}

// TestPermissionModalExpandRevealsFullDiff: the cross-confirmed CWE-451 fix.
// Collapsed, a long Write is line-capped and shows a truthful "ctrl+t expand"
// marker; with expand on (ctrl+t at the gate) the full content is revealed, so
// the operator can see every line being authorized before deciding.
func TestPermissionModalExpandRevealsFullDiff(t *testing.T) {
	// Build content with more lines than the diff cap so it must collapse.
	var lines []string
	for i := 0; i < maxDiffLines+8; i++ {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	args := `{"path":"big.txt","content":"` + strings.Join(lines, `\n`) + `"}`
	ask := pendingAsk{Tool: "Write", Args: args}

	collapsed := modalPlain(ask, false)
	if !strings.Contains(collapsed, "ctrl+t expand") {
		t.Errorf("collapsed modal should show the expand marker, got %q", collapsed)
	}
	// The last line is past the cap, so it must be hidden when collapsed.
	if strings.Contains(collapsed, "+ line"+strconv.Itoa(maxDiffLines+7)) {
		t.Errorf("collapsed modal should hide content past the cap, got %q", collapsed)
	}

	expanded := modalPlain(ask, true)
	if strings.Contains(expanded, "ctrl+t expand") {
		t.Errorf("expanded modal should not show the collapse marker, got %q", expanded)
	}
	if !strings.Contains(expanded, "+ line"+strconv.Itoa(maxDiffLines+7)) {
		t.Errorf("expanded modal should reveal the full diff (every line), got %q", expanded)
	}
	if countLines(expanded) <= countLines(collapsed) {
		t.Errorf("expanded modal should be taller than collapsed (more lines visible)")
	}
}

// TestPermissionModalOffersAlways: a main-agent ask (offerAlways) shows three
// buttons — [A]llow / Al[w]ays / [D]eny — plus the muted always-allow caption.
func TestPermissionModalOffersAlways(t *testing.T) {
	plain := modalPlain(pendingAsk{
		Tool:        "Bash",
		Args:        `{"command":"ls"}`,
		offerAlways: true,
	}, false)
	if !strings.Contains(plain, "[A]llow") || !strings.Contains(plain, "Al[w]ays") || !strings.Contains(plain, "[D]eny") {
		t.Errorf("expected three buttons (allow / always / deny), got %q", plain)
	}
	if !strings.Contains(plain, "al[w]ays allows this exact command for the rest of this session") {
		t.Errorf("expected the always-allow caption, got %q", plain)
	}
}

// TestPermissionModalNoAlwaysForChild: a surfaced subagent ask (offerAlways=false)
// shows only the two buttons and no always-allow caption.
func TestPermissionModalNoAlwaysForChild(t *testing.T) {
	plain := modalPlain(pendingAsk{
		Tool:        "Bash",
		Args:        `{"command":"ls"}`,
		offerAlways: false,
	}, false)
	if !strings.Contains(plain, "[A]llow") || !strings.Contains(plain, "[D]eny") {
		t.Errorf("expected allow/deny buttons, got %q", plain)
	}
	if strings.Contains(plain, "Al[w]ays") {
		t.Errorf("a child ask must NOT offer the always button, got %q", plain)
	}
	if strings.Contains(plain, "al[w]ays allows this exact command") {
		t.Errorf("a child ask must NOT show the always-allow caption, got %q", plain)
	}
}

// countLines counts newline-separated lines for the taller-than assertion.
func countLines(s string) int { return strings.Count(s, "\n") + 1 }
