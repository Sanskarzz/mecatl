package gitenv_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/gitenv"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			t.Fatalf("malformed env entry without '=': %q", kv)
		}
		m[kv[:i]] = kv[i+1:]
	}
	return m
}

// TestNeutralizingVarsCountMatchesPairs is the load-bearing invariant: GIT_CONFIG_COUNT
// must equal the number of GIT_CONFIG_KEY_n/VALUE_n pairs, or git silently ignores the
// trailing injected config keys and a neutralization vector falls through.
func TestNeutralizingVarsCountMatchesPairs(t *testing.T) {
	m := envMap(t, gitenv.NeutralizingVars())

	count, err := strconv.Atoi(m["GIT_CONFIG_COUNT"])
	if err != nil {
		t.Fatalf("GIT_CONFIG_COUNT not an int: %v", err)
	}
	for i := 0; i < count; i++ {
		idx := strconv.Itoa(i)
		if _, ok := m["GIT_CONFIG_KEY_"+idx]; !ok {
			t.Errorf("GIT_CONFIG_COUNT=%d but GIT_CONFIG_KEY_%d missing", count, i)
		}
		if _, ok := m["GIT_CONFIG_VALUE_"+idx]; !ok {
			t.Errorf("GIT_CONFIG_COUNT=%d but GIT_CONFIG_VALUE_%d missing", count, i)
		}
	}
	// No EXTRA pair beyond the declared count.
	if _, ok := m["GIT_CONFIG_KEY_"+strconv.Itoa(count)]; ok {
		t.Errorf("found GIT_CONFIG_KEY_%d beyond GIT_CONFIG_COUNT=%d", count, count)
	}

	// The neutralizing config must include the four code-exec vectors.
	wantKeys := map[string]string{
		"core.hooksPath": "/dev/null",
		"core.pager":     "cat",
		"core.fsmonitor": "false",
		"diff.external":  "",
	}
	got := map[string]string{}
	for i := 0; i < count; i++ {
		got[m["GIT_CONFIG_KEY_"+strconv.Itoa(i)]] = m["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
	}
	for k, v := range wantKeys {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("injected config %s = %q (present=%v), want %q", k, gv, ok, v)
		}
	}

	// Plain neutralizing vars.
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_PAGER":           "cat",
		"PAGER":               "cat",
	} {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
}

// TestScrubDropsInheritedGitDanger asserts Scrub removes inherited GIT_*/PAGER/LESS
// (which a plain append could not) while keeping non-git vars, and that the
// neutralizing set overrides any survivor on a duplicate key (appended last).
func TestScrubDropsInheritedGitDanger(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/x",
		"GIT_EXTERNAL_DIFF=/bin/evil",
		"GIT_SSH_COMMAND=ssh -o ProxyCommand=evil",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/elsewhere",
		"GIT_PROXY_COMMAND=/bin/evil",
		"GIT_PAGER=less", // inherited; must be overridden to cat by NeutralizingVars
		"PAGER=less",
		"LESS=-R",
	}
	m := envMap(t, gitenv.Scrub(base))

	// Non-git survives.
	if m["PATH"] != "/usr/bin" || m["HOME"] != "/home/x" {
		t.Errorf("Scrub dropped non-git env: PATH=%q HOME=%q", m["PATH"], m["HOME"])
	}
	// Inherited danger gone (no inherited copy remains; only the neutralized values).
	for _, k := range []string{"GIT_EXTERNAL_DIFF", "GIT_SSH_COMMAND", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PROXY_COMMAND", "LESS"} {
		if _, ok := m[k]; ok {
			t.Errorf("inherited %s survived Scrub: %q", k, m[k])
		}
	}
	// Pager forced to cat (the neutralizing append overrides the inherited "less").
	if m["GIT_PAGER"] != "cat" || m["PAGER"] != "cat" {
		t.Errorf("Scrub did not force pager to cat: GIT_PAGER=%q PAGER=%q", m["GIT_PAGER"], m["PAGER"])
	}
}
