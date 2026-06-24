// Command mecak8s is the storage-free, Kubernetes-native mecatl agent binary
// (ADR 0048). See flags.go for the configuration surface and serve.go for the
// shutdown contract. This file is the thin entry point: parse flags → build the
// diagnostics sink → app.Build → serve → os.Exit.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/stacklok/mecatl/internal/adapter/redisstore"
	"github.com/stacklok/mecatl/internal/adapter/slogdiag"
	"github.com/stacklok/mecatl/internal/app"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mecak8s exited with error", "err", err)
		os.Exit(1)
	}
}

// run is the testable entry point: parse flags, install the slog default +
// Diagnostics sink, build the engine/service via app.Build, and serve until a
// signal drives the bounded shutdown. It returns nil on a clean shutdown
// (exit 0) — the honest contract is that in-flight runs are CANCELLED, not
// drained (see serve.go's doc comment).
func run() error {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// slog.SetDefault stays for the daemon: this is the DELIBERATE, PERMANENT
	// third-party-slog bridge — a server's operational output belongs on
	// stderr/journald. cmd/ mains are the only layer allowed to call
	// slog.SetDefault; all of internal/ flows through the injected
	// port.Diagnostics (ban-guarded). Mirrors cmd/mecated.
	slog.SetDefault(logger)
	diag := slogdiag.NewFromLogger(logger)

	ctx, stop := signalCtx()
	defer stop()

	built, err := app.Build(ctx, appConfig(cfg, diag))
	if err != nil {
		return err
	}
	defer built.Close()

	// The Redis store app.Build wired (nil when --redis-url is empty). It is
	// threaded into serve ONLY for the /readyz ping; the Service already holds
	// it as the session store. Recover it by type-asserting the built Service's
	// store — but the Service does not expose its store, and serve only needs
	// the Ping capability. Rather than widen server.Service, re-open a PING-
	// ONLY handle here when --redis-url is set: it shares the broker, not the
	// store's command streams, and a second client is cheap (go-redis pools).
	// A nil redisStore means /readyz is drain-gated only.
	var redisStore *redisstore.Store
	if cfg.redisURL != "" {
		// app.Build already pinged and would have failed startup on an
		// unreachable broker, so this re-open is expected to succeed; a race
		// where the broker drops between Build and here surfaces as a not-ready
		// /readyz (the ping fails), which is exactly the desired behaviour.
		st, rerr := redisstore.New(cfg.redisURL)
		if rerr != nil {
			return fmt.Errorf("redis readyz probe: %w", rerr)
		}
		redisStore = st
		defer func() { _ = redisStore.Close() }()
	}

	return serve(ctx, cfg, built.Service, redisStore)
}
