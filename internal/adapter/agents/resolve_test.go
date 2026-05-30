package agents

import (
	"errors"
	"path/filepath"
	"testing"
)

func dirs(t *testing.T, sources []AgentSource) []string {
	t.Helper()
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		ds, ok := s.(DirSource)
		if !ok {
			t.Fatalf("resolver produced a non-DirSource: %T", s)
		}
		out = append(out, ds.Dir)
	}
	return out
}

func assertDirs(t *testing.T, got []AgentSource, want []string) {
	t.Helper()
	have := dirs(t, got)
	if len(have) != len(want) {
		t.Fatalf("resolved %d sources, want %d\n got: %v\nwant: %v", len(have), len(want), have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("source[%d] = %q, want %q", i, have[i], want[i])
		}
	}
}

func TestResolveSourcesOptInByDefault(t *testing.T) {
	got := resolveSourcesEnv(ResolveOptions{}, resolveEnv{
		getenv:      func(string) string { return "" },
		userHomeDir: func() (string, error) { return "/home/u", nil },
	})
	if len(got) != 0 {
		t.Fatalf("zero options must resolve no sources, got %d: %v", len(got), got)
	}
}

func TestResolveSourcesConventionalPrecedenceOrder(t *testing.T) {
	const home = "/home/u"
	const ws = "/work/repo"
	got := resolveSourcesEnv(ResolveOptions{
		Explicit:     []string{"/explicit"},
		Conventional: true,
		Workspace:    ws,
	}, resolveEnv{
		getenv:      func(string) string { return "" },
		userHomeDir: func() (string, error) { return home, nil },
	})
	want := []string{
		"/explicit",
		filepath.Join(ws, ".mecatl", "agents"),
		filepath.Join(ws, ".claude", "agents"),
		filepath.Join(home, ".config", "mecatl", "agents"),
		filepath.Join(home, ".claude", "agents"),
	}
	assertDirs(t, got, want)
}

func TestResolveSourcesHonorsXDGConfigHome(t *testing.T) {
	const home = "/home/u"
	const xdg = "/custom/xdg"
	got := resolveSourcesEnv(ResolveOptions{
		Conventional: true,
		Workspace:    "/ws",
	}, resolveEnv{
		getenv: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return xdg
			}
			return ""
		},
		userHomeDir: func() (string, error) { return home, nil },
	})
	resolved := dirs(t, got)
	wantXDG := filepath.Join(xdg, "mecatl", "agents")
	found := false
	for _, d := range resolved {
		if d == wantXDG {
			found = true
		}
		if d == filepath.Join(home, ".config", "mecatl", "agents") {
			t.Errorf("~/.config fallback should be skipped when XDG_CONFIG_HOME is set: %v", resolved)
		}
	}
	if !found {
		t.Errorf("XDG_CONFIG_HOME not honoured: want %q in %v", wantXDG, resolved)
	}
}

func TestResolveSourcesNoHomeSkipsUser(t *testing.T) {
	got := resolveSourcesEnv(ResolveOptions{
		Conventional: true,
		Workspace:    "/ws",
	}, resolveEnv{
		getenv:      func(string) string { return "" },
		userHomeDir: func() (string, error) { return "", errors.New("no home") },
	})
	want := []string{
		filepath.Join("/ws", ".mecatl", "agents"),
		filepath.Join("/ws", ".claude", "agents"),
	}
	assertDirs(t, got, want)
}
