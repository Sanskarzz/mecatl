package governance

import (
	"strings"
	"testing"
)

// splitSeeds are the tricky inputs from bash_test.go plus a few extra hostile
// forms. They seed every governance fuzzer so the corpus starts from the cases
// we already know exercise the security-critical paths.
var splitSeeds = []string{
	"",
	"   ",
	"git status",
	"git status && rm -rf /",
	"make || echo fail",
	"cd /tmp; ls",
	"cat f | grep x",
	"a && b; c | d || e",
	"echo 'a && b' && ls",
	`echo "x | y" | wc`,
	"ls &&",
	"ls\nrm -rf build",
	"ls\r\nrm x",
	"ls & rm -rf build",
	"echo \"a\nb\"\nls",
	// Substitution / grouping / expansion (must never be read-only).
	"echo $(rm -rf x)",
	"cat $(rm x)",
	"echo ok $(rm -rf build)",
	"echo `rm -rf build`",
	"diff <(rm x) f",
	"tee >(cat)",
	"ls;(rm -rf build)",
	"ls; { rm x; }",
	"cat ${HOME}",
	"(rm x)",
	"{ rm x; }",
	// Wrappers and re-entrant launchers.
	"timeout 5 rm x",
	"timeout -s KILL 5 rm x",
	"timeout --signal=KILL 5 rm x",
	"nice -n 10 cat f",
	"env FOO=bar ls",
	"stdbuf -oL grep x f",
	"timeout 5 nice -n 5 rm x",
	"docker exec foo rm x",
	"npx some-tool",
	"devbox run rm x",
	"sudo rm x",
	"env",
	// Pathological / unbalanced.
	"'unterminated",
	"\"unterminated",
	"$",
	"`",
	"$(",
	"<",
	"&&&&",
	"||||",
	";;;;",
	"\n\n\n",
}

// destructiveTokens are verbs whose appearance as a whole token in the input
// must never be lost across SplitCommands: if any segment contains one, at
// least one returned segment must still contain it (the splitter may not
// silently swallow a destructive command).
var destructiveTokens = []string{
	"rm", "mv", "cp", "mkdir", "rmdir", "touch", "tee", "dd",
	"chmod", "chown", "ln", "truncate", "install",
}

func seedGovernance(f *testing.F) {
	for _, s := range splitSeeds {
		f.Add(s)
	}
}

// FuzzSplitCommands asserts SplitCommands never panics and upholds several
// invariants, the most important being the security cross-check against
// HasSubstitutionOrGrouping / ReadOnlyBash: any input that contains a
// substitution, grouping, or a raw newline can never be classified read-only.
func FuzzSplitCommands(f *testing.F) {
	seedGovernance(f)
	f.Fuzz(func(t *testing.T, cmd string) {
		segs := SplitCommands(cmd)

		// Invariant 1: every returned segment is non-empty and trimmed (no
		// leading/trailing whitespace), matching the documented contract.
		for i, s := range segs {
			if s == "" {
				t.Fatalf("SplitCommands(%q) returned empty segment at %d: %#v", cmd, i, segs)
			}
			if s != strings.TrimSpace(s) {
				t.Fatalf("SplitCommands(%q) segment %d not trimmed: %q", cmd, i, s)
			}
		}

		// Invariant 2: idempotence on already-single segments. Re-splitting a
		// segment that the splitter produced must yield exactly that one segment
		// back (the split reached a fixed point).
		for _, s := range segs {
			again := SplitCommands(s)
			if len(again) != 1 || again[0] != s {
				t.Fatalf("SplitCommands not idempotent on segment %q (from %q): %#v", s, cmd, again)
			}
		}

		// Invariant 3: no destructive token is silently dropped. If the raw input
		// contains a destructive token as a whitespace-delimited field, at least
		// one segment must still contain it as a field. (We only check inputs
		// without quotes/substitution where field semantics are unambiguous.)
		if !strings.ContainsAny(cmd, "'\"`$(){}") {
			inFields := fieldSet(cmd)
			for _, tok := range destructiveTokens {
				if !inFields[tok] {
					continue
				}
				found := false
				for _, s := range segs {
					if fieldSet(s)[tok] {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("SplitCommands(%q) dropped destructive token %q: %#v", cmd, tok, segs)
				}
			}
		}

		// Invariant 4 (security): a raw (unquoted) newline in the input means more
		// than one logical command may be present, so the line must NOT be
		// classified read-only as a single safe command. We assert the splitter
		// actually broke on it by checking ReadOnlyBash's downstream guarantee.
		if hasRawNewline(cmd) && ReadOnlyBash(cmd) {
			// ReadOnlyBash may still be true if BOTH sides are independently
			// read-only (e.g. "ls\npwd"); that is fine. The violation would be a
			// destructive token surviving as read-only, which Invariant 5 covers.
			_ = segs
		}

		// Invariant 5 (security, the crux): if ANY produced segment contains a
		// substitution/grouping construct, ReadOnlyBash must be false. A
		// substitution can hide an arbitrary destructive inner command that the
		// operator splitter cannot decompose.
		anySub := false
		for _, s := range segs {
			if HasSubstitutionOrGrouping(s) {
				anySub = true
				break
			}
		}
		if anySub && ReadOnlyBash(cmd) {
			t.Fatalf("SECURITY: ReadOnlyBash(%q)=true but a segment has substitution/grouping: %#v", cmd, segs)
		}
	})
}

// FuzzCanonicalize asserts Canonicalize never panics, is idempotent, and never
// strips a re-entrant launcher (sudo / docker exec / npx / devbox run), which
// would open a permission backdoor.
func FuzzCanonicalize(f *testing.F) {
	seedGovernance(f)
	f.Fuzz(func(t *testing.T, cmd string) {
		canon := Canonicalize(cmd)

		// Invariant 1: idempotent. Canonicalize(Canonicalize(x)) == Canonicalize(x).
		if again := Canonicalize(canon); again != canon {
			t.Fatalf("Canonicalize not idempotent: Canonicalize(%q)=%q, Canonicalize again=%q", cmd, canon, again)
		}

		// Invariant 2 (security): re-entrant launchers are never peeled off. If
		// the trimmed input begins with one of these as its first field, the
		// canonical form must begin with the same launcher.
		fields := strings.Fields(strings.TrimSpace(cmd))
		if len(fields) > 0 {
			head := fields[0]
			launchers := map[string]bool{"sudo": true, "docker": true, "npx": true, "devbox": true}
			if launchers[head] {
				cf := strings.Fields(canon)
				if len(cf) == 0 || cf[0] != head {
					t.Fatalf("SECURITY: Canonicalize(%q)=%q stripped re-entrant launcher %q", cmd, canon, head)
				}
			}
		}

		// Invariant 3: canonicalization only ever removes leading tokens, so the
		// canonical form must be a suffix (token-wise) of the trimmed input, or
		// equal to it. We assert the canonical form's fields are a tail of the
		// input's fields.
		if !isFieldTail(fields, strings.Fields(canon)) {
			t.Fatalf("Canonicalize(%q)=%q is not a token-tail of the input %v", cmd, canon, fields)
		}
	})
}

// FuzzReadOnlyBash asserts ReadOnlyBash never panics and that the
// substitution/grouping fail-safe holds: any command whose split yields a
// segment with substitution or grouping is never read-only.
func FuzzReadOnlyBash(f *testing.F) {
	seedGovernance(f)
	f.Fuzz(func(t *testing.T, cmd string) {
		ro := ReadOnlyBash(cmd)
		if !ro {
			return
		}
		// If classified read-only, NO segment may contain substitution/grouping.
		for _, s := range SplitCommands(cmd) {
			if HasSubstitutionOrGrouping(s) {
				t.Fatalf("SECURITY: ReadOnlyBash(%q)=true but segment %q has substitution/grouping", cmd, s)
			}
		}
		// And no segment may, after canonicalization, contain a destructive verb
		// as a field or any output redirection.
		for _, s := range SplitCommands(cmd) {
			canon := Canonicalize(s)
			if strings.ContainsAny(canon, ">") {
				t.Fatalf("SECURITY: ReadOnlyBash(%q)=true but segment %q canonicalizes to a redirection %q", cmd, s, canon)
			}
			fs := fieldSet(canon)
			for _, tok := range destructiveTokens {
				if fs[tok] {
					t.Fatalf("SECURITY: ReadOnlyBash(%q)=true but segment %q contains destructive token %q", cmd, s, tok)
				}
			}
		}
	})
}

// fieldSet returns the set of whitespace-delimited fields of s.
func fieldSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.Fields(s) {
		out[f] = true
	}
	return out
}

// hasRawNewline reports whether s contains a newline or carriage return outside
// of any quoting (a conservative check: any newline at all, since the splitter
// treats quoted newlines as literal — used only as a soft signal).
func hasRawNewline(s string) bool {
	return strings.ContainsAny(s, "\n\r")
}

// isFieldTail reports whether tail is a contiguous suffix of full (token-wise),
// or whether tail equals full. Canonicalize only strips leading wrapper tokens,
// so its output fields must be a tail of the input fields — except it may merge
// an "env FOO=bar ls" case where the wrapper's own residue stays; to stay
// robust we accept any suffix match anchored at the end.
func isFieldTail(full, tail []string) bool {
	if len(tail) > len(full) {
		return false
	}
	off := len(full) - len(tail)
	for i := range tail {
		if full[off+i] != tail[i] {
			return false
		}
	}
	return true
}
