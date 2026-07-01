package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stacklok/mecatl/cmd/mecatui/keymap"
	"github.com/stacklok/mecatl/cmd/mecatui/ui"
	"github.com/stacklok/mecatl/internal/adapter/xdgconfig"
)

type operatorSettings struct {
	Keymap map[string]string `yaml:"keymap"`
}

// readOperatorKeymap reads the operator-tier settings.yaml and returns keymap overrides
// as action -> []chords. Missing file returns nil, nil.
func readOperatorKeymap() (map[string][]string, error) {
	cfgBase := xdgconfig.UserConfigDir(xdgconfig.OSEnv)
	if cfgBase == "" {
		return nil, nil
	}
	path := filepath.Join(cfgBase, "mecatl", "settings.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s operatorSettings
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(s.Keymap) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(s.Keymap))
	for action, val := range s.Keymap {
		parts := make([]string, 0, 1)
		for _, p := range strings.Split(val, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			out[action] = parts
		}
	}
	return out, nil
}

// mergeKeymaps returns a new map with b overlaying a (b wins on conflicts).
func mergeKeymaps(a, b map[string][]string) map[string][]string {
	if a == nil && b == nil {
		return nil
	}
	out := make(map[string][]string)
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// applyKeyOverridesToDeps parses and validates CLI/YAML keymap overrides and applies them to deps.
// Lives in package main to avoid adding imports to main.go; this file imports keymap.
func applyKeyOverridesToDeps(cfg config, deps *ui.Deps) error {
	// YAML (operator-tier) first, then CLI overlays and wins
	yamlMap, err := readOperatorKeymap()
	if err != nil {
		return err
	}
	cliMap := keyOverridesFromConfig(cfg)
	merged := mergeKeymaps(yamlMap, cliMap)
	if os.Getenv("MECATUI_DEBUG_KEYMAP") == "1" {
		fmt.Fprintf(os.Stderr, "mecatui keymap (YAML): %v\n", yamlMap)
		fmt.Fprintf(os.Stderr, "mecatui keymap (CLI): %v\n", cliMap)
		fmt.Fprintf(os.Stderr, "mecatui keymap (merged): %v\n", merged)
	}
	if merged == nil {
		return nil
	}
	res, err := keymap.Parse(merged)
	if err != nil {
		return fmt.Errorf("keymap: %w", err)
	}
	if err := keymap.Validate(res); err != nil {
		return fmt.Errorf("keymap: %w", err)
	}
	deps.KeyOverrides = res.ByAction
	return nil
}
