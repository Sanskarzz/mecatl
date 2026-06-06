package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

// stateEnv builds a ResolveEnv whose XDG_STATE_HOME points at dir (and whose home
// resolution fails), so the diaglog path resolves under a temp state base — never
// the developer's real ~/.local/state. Fully offline.
func stateEnv(dir string) xdgconfig.ResolveEnv {
	return xdgconfig.ResolveEnv{
		Getenv: func(k string) string {
			if k == "XDG_STATE_HOME" {
				return dir
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return "", errors.New("no home") },
		ReadFile:    os.ReadFile,
	}
}

// homeEnv builds a ResolveEnv with NO XDG_STATE_HOME but a home dir, so the path
// falls back to <home>/.local/state — the documented fallback.
func homeEnv(home string) xdgconfig.ResolveEnv {
	return xdgconfig.ResolveEnv{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return home, nil },
		ReadFile:    os.ReadFile,
	}
}

func TestResolveDiagLogPathUsesXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	got := resolveDiagLogPath(stateEnv(dir))
	want := filepath.Join(dir, "mecatl", "mecatui.log")
	if got != want {
		t.Fatalf("resolveDiagLogPath under XDG_STATE_HOME = %q, want %q", got, want)
	}
}

func TestResolveDiagLogPathFallsBackToHomeLocalState(t *testing.T) {
	home := t.TempDir()
	got := resolveDiagLogPath(homeEnv(home))
	want := filepath.Join(home, ".local", "state", "mecatl", "mecatui.log")
	if got != want {
		t.Fatalf("resolveDiagLogPath fallback = %q, want %q", got, want)
	}
}

func TestResolveDiagLogPathEmptyWhenUnresolvable(t *testing.T) {
	env := xdgconfig.ResolveEnv{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "", errors.New("no home") },
		ReadFile:    os.ReadFile,
	}
	if got := resolveDiagLogPath(env); got != "" {
		t.Fatalf("resolveDiagLogPath with no state base = %q, want empty", got)
	}
}

func TestOpenDiagLogWriterQuietDiscards(t *testing.T) {
	// --quiet must yield io.Discard regardless of a resolvable state base, and open
	// no file (the dir/file must NOT be created).
	dir := t.TempDir()
	w, closer, toFile := openDiagLogWriter(stateEnv(dir), true)
	defer func() { _ = closer.Close() }()
	if w != io.Discard {
		t.Fatalf("--quiet writer = %T, want io.Discard", w)
	}
	if toFile {
		t.Fatal("--quiet must not open a file")
	}
	if _, err := os.Stat(filepath.Join(dir, "mecatl")); !os.IsNotExist(err) {
		t.Fatalf("--quiet must not create the state dir; stat err = %v", err)
	}
}

func TestOpenDiagLogWriterOpensFileForAppend(t *testing.T) {
	dir := t.TempDir()
	w, closer, toFile := openDiagLogWriter(stateEnv(dir), false)
	t.Cleanup(func() { _ = closer.Close() })
	if !toFile {
		t.Fatal("a resolvable state base (not quiet) must open a file")
	}
	if w == io.Discard {
		t.Fatal("non-quiet with a resolvable state base must NOT discard")
	}
	// The dir must be created 0700 and the file writable.
	path := filepath.Join(dir, "mecatl", "mecatui.log")
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write to diag log: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back diag log: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("diag log content = %q, want %q", string(data), "hello\n")
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir perm = %o, want 0700", perm)
	}
	// The log FILE itself must be 0600 (operator diagnostics may carry prompt
	// text/paths — never group/world readable). A regression to 0644 would otherwise
	// stay green, so pin it explicitly.
	lfi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat diag log file: %v", err)
	}
	if perm := lfi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("diag log file perm = %o, want 0600", perm)
	}
}

func TestOpenDiagLogWriterDiscardsWhenUnresolvable(t *testing.T) {
	env := xdgconfig.ResolveEnv{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "", errors.New("no home") },
		ReadFile:    os.ReadFile,
	}
	w, closer, toFile := openDiagLogWriter(env, false)
	defer func() { _ = closer.Close() }()
	if w != io.Discard || toFile {
		t.Fatalf("no resolvable state base must discard (w=%T toFile=%v)", w, toFile)
	}
}
