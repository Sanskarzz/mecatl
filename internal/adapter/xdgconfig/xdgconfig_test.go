package xdgconfig

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestUserConfigDir pins the three branches the four migrated adapters relied on:
// XDG_CONFIG_HOME set wins; unset falls back to ~/.config; neither yields "".
func TestUserConfigDir(t *testing.T) {
	t.Run("XDG set wins", func(t *testing.T) {
		got := UserConfigDir(ResolveEnv{
			Getenv:      func(k string) string { return map[string]string{"XDG_CONFIG_HOME": "/xdg"}[k] },
			UserHomeDir: func() (string, error) { return "/home/u", nil },
		})
		if got != "/xdg" {
			t.Errorf("XDG_CONFIG_HOME not honoured: got %q, want /xdg", got)
		}
	})

	t.Run("XDG unset falls back to ~/.config", func(t *testing.T) {
		got := UserConfigDir(ResolveEnv{
			Getenv:      func(string) string { return "" },
			UserHomeDir: func() (string, error) { return "/home/u", nil },
		})
		if want := filepath.Join("/home/u", ".config"); got != want {
			t.Errorf("~/.config fallback wrong: got %q, want %q", got, want)
		}
	})

	t.Run("neither resolves to empty", func(t *testing.T) {
		got := UserConfigDir(ResolveEnv{
			Getenv:      func(string) string { return "" },
			UserHomeDir: func() (string, error) { return "", errors.New("no home") },
		})
		if got != "" {
			t.Errorf("no XDG and no home should yield \"\", got %q", got)
		}
	})
}
