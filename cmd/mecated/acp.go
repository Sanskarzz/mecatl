package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/stacklok/mecatl/internal/adapter/acp"
	"github.com/stacklok/mecatl/internal/adapter/server"
)

// serveACP runs the Agent Client Protocol stdio loop: it speaks JSON-RPC 2.0 to
// the ACP editor that spawned mecated, over stdin (inbound) and stdout
// (outbound), reusing the shared *server.Service that app.Build assembled. It
// blocks until the editor closes its input stream (clean EOF) or ctx is
// cancelled (SIGINT/SIGTERM), then returns. It is the stdio alternative to the
// gRPC/HTTP serve() path; the engine, tools, permission policy, store, MCP, and
// skills are all the same wiring.
//
// stdout MUST carry only JSON-RPC frames — slog is configured to stderr in run()
// before this is called, so log lines never corrupt the protocol stream.
func serveACP(ctx context.Context, svc *server.Service) error {
	slog.Info("serving Agent Client Protocol over stdio (JSON-RPC 2.0); TCP/HTTP listeners skipped")
	agent := acp.NewAgent(svc)
	conn := acp.NewConn(os.Stdin, os.Stdout, agent.Handle)
	return agent.Serve(ctx, conn)
}
