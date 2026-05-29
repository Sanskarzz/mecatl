package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// config is the resolved CLI/env configuration for mecatui.
type config struct {
	server     string
	workspace  string
	mode       string
	theme      string
	themeDir   string
	authToken  string
	useTLS     bool
	tlsCA      string
	insecure   bool
	listThemes bool
}

// defaultServer mirrors mecated's default loopback gRPC listen address.
const defaultServer = "127.0.0.1:8080"

// parseFlags parses argv into a config, applying env fallbacks. The workspace is
// resolved to an absolute path (the server requires absolute). args excludes the
// program name.
func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("mecatui", flag.ContinueOnError)
	fs.StringVar(&cfg.server, "server", defaultServer, "mecated gRPC address (host:port)")
	fs.StringVar(&cfg.workspace, "workspace", "", "absolute workspace root for the session (default: cwd)")
	fs.StringVar(&cfg.mode, "mode", "default", "permission mode: default | plan | accept-edits")
	fs.StringVar(&cfg.theme, "theme", "", "theme name (default: aztec)")
	fs.StringVar(&cfg.themeDir, "theme-dir", "", "extra directory of *.json themes to load")
	fs.StringVar(&cfg.authToken, "auth-token", "", "bearer token (or MECATL_AUTH_TOKEN)")
	fs.BoolVar(&cfg.useTLS, "tls", false, "use TLS transport")
	fs.StringVar(&cfg.tlsCA, "tls-ca", "", "PEM CA bundle for server verification")
	fs.BoolVar(&cfg.insecure, "insecure", false, "skip TLS verification (testing only)")
	fs.BoolVar(&cfg.listThemes, "list-themes", false, "list available themes and exit")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if cfg.authToken == "" {
		cfg.authToken = os.Getenv("MECATL_AUTH_TOKEN")
	}
	if cfg.theme == "" {
		cfg.theme = os.Getenv("MECATUI_THEME")
	}

	if !cfg.listThemes {
		ws, err := resolveWorkspace(cfg.workspace)
		if err != nil {
			return config{}, err
		}
		cfg.workspace = ws
	}
	return cfg, nil
}

// resolveWorkspace defaults an empty workspace to the cwd and makes it absolute.
func resolveWorkspace(ws string) (string, error) {
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd as workspace: %w", err)
		}
		return cwd, nil
	}
	if !filepath.IsAbs(ws) {
		abs, err := filepath.Abs(ws)
		if err != nil {
			return "", fmt.Errorf("resolve workspace %q: %w", ws, err)
		}
		return abs, nil
	}
	return ws, nil
}

// validate checks invariants the server also enforces, failing fast client-side.
func (c config) validate() error {
	if c.listThemes {
		return nil
	}
	if c.workspace == "" {
		return errors.New("workspace is required")
	}
	if !filepath.IsAbs(c.workspace) {
		return fmt.Errorf("workspace must be absolute: %q", c.workspace)
	}
	switch c.mode {
	case "default", "plan", "accept-edits":
	default:
		return fmt.Errorf("invalid --mode %q (want default|plan|accept-edits)", c.mode)
	}
	return nil
}
