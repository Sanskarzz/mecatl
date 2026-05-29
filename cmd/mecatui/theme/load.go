package theme

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fileTheme is the on-disk JSON shape: a name plus a partial palette. Authors
// only set the slots they want to change; everything else is inherited from the
// merge base (Aztec) via mergePalette. This is what makes a three-line theme
// file ("name" + an accent override) work.
type fileTheme struct {
	Name    string          `json:"name"`
	Palette json.RawMessage `json:"palette"`
}

// LoadDir loads every *.json theme in dir and registers it in r, merging each
// partial palette over the Aztec base so partial themes are complete. Missing
// dirs are not an error (the dir hierarchy is best-effort). Per-file parse
// errors are collected and returned joined, but valid files still register.
func (r *Registry) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read theme dir %q: %w", dir, err)
	}

	var errs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, err := LoadFile(path)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		r.Register(t)
	}
	if len(errs) > 0 {
		return fmt.Errorf("theme dir %q: %s", dir, strings.Join(errs, "; "))
	}
	return nil
}

// LoadFile parses a single theme JSON file, merges its partial palette over the
// Aztec base, and returns the compiled Theme. The name defaults to the file's
// base name (sans .json) when the JSON omits "name". The name is lowercased so
// lookups are case-insensitive at the boundary.
func LoadFile(path string) (Theme, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from a config dir listing, not untrusted input
	if err != nil {
		return Theme{}, fmt.Errorf("read theme %q: %w", path, err)
	}
	t, err := ParseTheme(raw)
	if err != nil {
		return Theme{}, fmt.Errorf("parse theme %q: %w", path, err)
	}
	if t.Name == "" {
		base := filepath.Base(path)
		t = New(strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base))), t.Palette)
	}
	return t, nil
}

// ParseTheme parses theme JSON bytes and returns the compiled Theme with the
// partial palette merged over Aztec. Exposed (and base of LoadFile) so tests can
// exercise the round-trip and merge without touching the filesystem.
func ParseTheme(raw []byte) (Theme, error) {
	var ft fileTheme
	if err := json.Unmarshal(raw, &ft); err != nil {
		return Theme{}, fmt.Errorf("unmarshal theme: %w", err)
	}
	// Decode the palette over a COPY of the Aztec base: only the slots present
	// in the JSON object overwrite the base, so a partial palette is completed.
	merged := aztecPalette
	if len(ft.Palette) > 0 {
		if err := json.Unmarshal(ft.Palette, &merged); err != nil {
			return Theme{}, fmt.Errorf("unmarshal palette: %w", err)
		}
	}
	return New(strings.ToLower(ft.Name), merged), nil
}

// MarshalJSON emits a theme as the on-disk fileTheme shape (name + full
// palette). Used by tests for the round-trip and handy for a "dump current
// theme" affordance.
func (t Theme) MarshalJSON() ([]byte, error) {
	pal, err := json.Marshal(t.Palette)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fileTheme{Name: t.Name, Palette: pal})
}
