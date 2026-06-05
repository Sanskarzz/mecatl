package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// DialConfig is the connection-time configuration: address, optional bearer
// token, and TLS posture. It mirrors mecated's trust model — loopback is
// unauthenticated plaintext by default; non-loopback may need a token and/or
// TLS/mTLS.
type DialConfig struct {
	Server    string // host:port, e.g. 127.0.0.1:8080
	AuthToken string // optional bearer; sent as "authorization: Bearer <tok>"
	UseTLS    bool   // enable transport TLS
	TLSCAFile string // optional custom CA bundle for server verification
	Insecure  bool   // skip TLS verification (testing only; with UseTLS)
}

// Client is a connected mecated gRPC client: the dialled conn plus the generated
// service stub. Close it on shutdown.
type Client struct {
	conn *grpc.ClientConn
	svc  mecatlv1.HarnessServiceClient
}

// Dial connects to mecated per cfg. It uses grpc.NewClient (not the deprecated
// grpc.Dial), attaches a per-RPC bearer credential when a token is set, and
// configures transport credentials (plaintext for loopback by default, TLS/mTLS
// when requested). The connection is lazy; the first RPC (CreateSession)
// surfaces a connect error.
func Dial(cfg DialConfig) (*Client, error) {
	var opts []grpc.DialOption

	loopback := isLoopbackHost(cfg.Server)

	// Refuse to leak a bearer token in cleartext to a non-loopback server. The
	// per-RPC credential's RequireTransportSecurity() also blocks this at send
	// time, but a hard pre-dial guard gives the operator a clear, actionable
	// error instead of an opaque RPC failure later.
	if cfg.AuthToken != "" && !cfg.UseTLS && !loopback {
		return nil, fmt.Errorf(
			"refusing to send auth token in cleartext to non-loopback %q: use --tls", cfg.Server)
	}

	if cfg.UseTLS {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // Insecure is opt-in and documented testing-only
		if cfg.Insecure {
			tlsCfg.InsecureSkipVerify = true
		} else if cfg.TLSCAFile != "" {
			pool, err := loadCAPool(cfg.TLSCAFile)
			if err != nil {
				return nil, err
			}
			tlsCfg.RootCAs = pool
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if cfg.AuthToken != "" {
		// The credential requires transport security UNLESS the target is
		// loopback (the documented plaintext single-user default). For any
		// non-loopback target it demands TLS even when --tls is unset, so the
		// token can never ride a cleartext wire to a remote host.
		creds := bearerCreds{token: cfg.AuthToken, allowInsecure: loopback}
		opts = append(opts, grpc.WithPerRPCCredentials(creds))
	}

	conn, err := grpc.NewClient(cfg.Server, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %q: %w", cfg.Server, err)
	}
	return &Client{conn: conn, svc: mecatlv1.NewHarnessServiceClient(conn)}, nil
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CreateSession allocates a server-side session against an absolute workspace and
// returns its id together with the server's advertised Capabilities. mode is the
// proto PermissionMode (see ModeFromString). sel is the optional, proto-free model
// selection (its zero value ⇒ no provider_id/model_id set ⇒ the server's default).
// This is the SINGLE proto-build point for the model selection: the ui passes a
// plain ModelSelection and never sees the proto request. The Capabilities are the
// proto-free mirror of the create response's ServerCapabilities; an older server
// that omits the field yields the all-false zero value (see capabilitiesFrom).
func (c *Client) CreateSession(ctx context.Context, workspace string, mode mecatlv1.PermissionMode, sel ModelSelection) (string, Capabilities, error) {
	resp, err := c.svc.CreateSession(ctx, &mecatlv1.CreateSessionRequest{
		Workspace:  workspace,
		Mode:       mode,
		ProviderId: sel.ProviderID,
		ModelId:    sel.ModelID,
	})
	if err != nil {
		return "", Capabilities{}, fmt.Errorf("create session: %w", err)
	}
	return resp.GetSessionId(), capabilitiesFrom(resp.GetCapabilities()), nil
}

// OpenConverse opens a fresh bidi Converse stream and wraps it in a Stream
// (serialised Sends + a Recver for the reader goroutine). Each user prompt opens
// one stream — matching the "one run per Converse" model. The stream's lifetime
// is bound to ctx; cancelling ctx aborts the run.
func (c *Client) OpenConverse(ctx context.Context) (*Stream, error) {
	bidi, err := c.svc.Converse(ctx)
	if err != nil {
		return nil, fmt.Errorf("open converse: %w", err)
	}
	return NewStream(bidi, bidi), nil
}

// ModeFromString maps a CLI mode string to the proto enum. Unknown/empty maps to
// UNSPECIFIED (the server defaults that to DEFAULT).
func ModeFromString(s string) mecatlv1.PermissionMode {
	switch s {
	case "plan":
		return mecatlv1.PermissionMode_PERMISSION_MODE_PLAN
	case "accept-edits", "acceptEdits", "accept_edits":
		return mecatlv1.PermissionMode_PERMISSION_MODE_ACCEPT_EDITS
	case "default", "":
		return mecatlv1.PermissionMode_PERMISSION_MODE_DEFAULT
	default:
		return mecatlv1.PermissionMode_PERMISSION_MODE_UNSPECIFIED
	}
}

// loadCAPool reads a PEM CA bundle into a cert pool for server verification.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // operator-supplied CA path
	if err != nil {
		return nil, fmt.Errorf("read TLS CA %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates parsed from CA %q", path)
	}
	return pool, nil
}

// bearerCreds implements grpc.PerRPCCredentials, attaching the mecated bearer
// token as the lowercase "authorization" metadata the server reads.
// RequireTransportSecurity() returns false ONLY for a loopback target (the
// documented plaintext single-user default); for any non-loopback target it
// returns true, so grpc-go refuses to send the token over a cleartext wire.
type bearerCreds struct {
	token         string
	allowInsecure bool // true only for loopback targets
}

func (b bearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearerCreds) RequireTransportSecurity() bool { return !b.allowInsecure }

// isLoopbackHost reports whether the host part of a "host:port" (or bare host)
// target is loopback: an IP in 127.0.0.0/8, ::1, or the name "localhost".
// A target with no resolvable/parseable host is treated as NON-loopback (fail
// safe — we'd rather demand TLS than leak a token).
func isLoopbackHost(server string) bool {
	host := server
	if h, _, err := net.SplitHostPort(server); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
