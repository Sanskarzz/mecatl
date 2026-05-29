package server

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// SecurityConfig configures the reusable authentication and rate-limiting
// interceptors/middleware shared by the gRPC and HTTP surfaces. The zero value
// is a valid, fully permissive (dev) configuration: no token is required and no
// rate limit is enforced.
//
// Both knobs compose with the loopback-default + off-loopback warning in the
// composition root (cmd/ozzd): an empty AuthToken on a non-loopback bind is a
// loud-but-not-fatal misconfiguration the operator is warned about.
type SecurityConfig struct {
	// AuthToken, when non-empty, requires every RPC/request to present
	// Authorization: Bearer <AuthToken> (gRPC: the "authorization" metadata
	// header). An empty token disables authentication (dev mode).
	AuthToken string
	// RateLimit is the sustained per-client request rate in requests/second. A
	// value <= 0 disables rate limiting entirely. Limiting is applied both
	// per-client (keyed by token or peer IP) and globally.
	RateLimit float64
	// RateBurst is the token-bucket burst size. It defaults to a small multiple
	// of RateLimit when left zero (see newLimiterSet).
	RateBurst int
}

// authEnabled reports whether a bearer token is configured.
func (c SecurityConfig) authEnabled() bool { return c.AuthToken != "" }

// rateEnabled reports whether rate limiting is configured.
func (c SecurityConfig) rateEnabled() bool { return c.RateLimit > 0 }

// authHeader is the canonical bearer-token header on both surfaces.
const authHeader = "authorization"

// bearerPrefix is the RFC 6750 scheme prefix on the Authorization value.
const bearerPrefix = "Bearer "

// constantTimeTokenMatch compares got against want in constant time so a
// malformed/short token cannot be distinguished from a wrong one by timing.
func constantTimeTokenMatch(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// bearerFromAuthValue extracts the token from an "Authorization: Bearer <tok>"
// value, returning the token and whether the Bearer scheme was present.
func bearerFromAuthValue(v string) (string, bool) {
	if len(v) < len(bearerPrefix) || !strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return v[len(bearerPrefix):], true
}

// --- rate limiting ----------------------------------------------------------

// limiterSet holds a global limiter plus a bounded, idle-evicting map of
// per-client limiters. It is safe for concurrent use.
type limiterSet struct {
	r     rate.Limit
	burst int

	global *rate.Limiter

	mu      sync.Mutex
	clients map[string]*clientLimiter
	// idleTTL evicts a client limiter that has not been seen for this long, so
	// memory stays bounded under churning client identities (e.g. peer IPs).
	idleTTL time.Duration
	now     func() time.Time
}

// clientLimiter is a per-client token bucket plus its last-seen stamp.
type clientLimiter struct {
	lim  *rate.Limiter
	seen time.Time
}

// maxTrackedClients caps the per-client map; once exceeded a sweep evicts idle
// entries. It bounds memory even if every request carries a fresh identity.
const maxTrackedClients = 4096

// newLimiterSet builds a limiterSet for the given sustained rate and burst. A
// non-positive burst defaults to max(rps, 1) rounded up so a single client can
// still issue a small burst.
func newLimiterSet(rps float64, burst int) *limiterSet {
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	return &limiterSet{
		r:       rate.Limit(rps),
		burst:   burst,
		global:  rate.NewLimiter(rate.Limit(rps), burst),
		clients: make(map[string]*clientLimiter),
		idleTTL: 10 * time.Minute,
		now:     time.Now,
	}
}

// allow reports whether a request from client key may proceed: it must satisfy
// both the per-client and the global limiter.
func (s *limiterSet) allow(key string) bool {
	cl := s.clientLimiter(key)
	// Check the per-client bucket first, then the global one. Both must permit.
	if !cl.Allow() {
		return false
	}
	return s.global.Allow()
}

// clientLimiter returns (creating if needed) the limiter for key, refreshing its
// last-seen stamp and opportunistically evicting idle entries.
func (s *limiterSet) clientLimiter(key string) *rate.Limiter {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[key]; ok {
		c.seen = now
		return c.lim
	}
	if len(s.clients) >= maxTrackedClients {
		s.evictIdleLocked(now)
	}
	c := &clientLimiter{lim: rate.NewLimiter(s.r, s.burst), seen: now}
	s.clients[key] = c
	return c.lim
}

// evictIdleLocked removes clients not seen within idleTTL. The caller holds mu.
func (s *limiterSet) evictIdleLocked(now time.Time) {
	for k, c := range s.clients {
		if now.Sub(c.seen) > s.idleTTL {
			delete(s.clients, k)
		}
	}
}

// --- client identity --------------------------------------------------------

// clientKeyFromToken keys rate limiting by the presented token when auth is on
// (so each credential gets its own bucket), falling back to a fixed key.
func clientKeyFromToken(token string) string {
	if token == "" {
		return "anon"
	}
	return "tok:" + token
}

// clientKeyFromAddr keys rate limiting by the peer IP (host portion), so a
// single host shares one bucket regardless of source port.
func clientKeyFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return "ip:unknown"
	}
	return "ip:" + host
}

// --- gRPC interceptors ------------------------------------------------------

// Authenticator bundles the configured auth + rate-limit policy and exposes the
// gRPC interceptors and HTTP middleware that enforce it. Construct it once and
// share it across both surfaces.
type Authenticator struct {
	cfg      SecurityConfig
	limiters *limiterSet
}

// NewAuthenticator builds an Authenticator from cfg. When rate limiting is
// disabled the limiter set is nil and the rate-limit checks are skipped.
func NewAuthenticator(cfg SecurityConfig) *Authenticator {
	a := &Authenticator{cfg: cfg}
	if cfg.rateEnabled() {
		a.limiters = newLimiterSet(cfg.RateLimit, cfg.RateBurst)
	}
	return a
}

// tokenFromMetadata extracts the bearer token from incoming gRPC metadata.
func tokenFromMetadata(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(authHeader)
	if len(vals) == 0 {
		return "", false
	}
	return bearerFromAuthValue(vals[0])
}

// authGRPC verifies the bearer token (when configured) and returns the token
// presented (empty when auth is off). It returns a codes.Unauthenticated status
// error on failure.
func (a *Authenticator) authGRPC(ctx context.Context) (string, error) {
	if !a.cfg.authEnabled() {
		return "", nil
	}
	tok, ok := tokenFromMetadata(ctx)
	if !ok || !constantTimeTokenMatch(tok, a.cfg.AuthToken) {
		return "", status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return tok, nil
}

// rateGRPC enforces the rate limit for a gRPC call keyed by token (when auth is
// on) or by peer IP. It returns codes.ResourceExhausted when over the limit.
func (a *Authenticator) rateGRPC(ctx context.Context, token string) error {
	if a.limiters == nil {
		return nil
	}
	key := clientKeyFromToken(token)
	if !a.cfg.authEnabled() {
		if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
			key = clientKeyFromAddr(p.Addr.String())
		}
	}
	if !a.limiters.allow(key) {
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	return nil
}

// UnaryInterceptor returns a grpc.UnaryServerInterceptor enforcing auth then
// rate limiting before the handler runs.
func (a *Authenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		tok, err := a.authGRPC(ctx)
		if err != nil {
			return nil, err
		}
		if err := a.rateGRPC(ctx, tok); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a grpc.StreamServerInterceptor enforcing auth then
// rate limiting before the stream handler runs. The rate check is applied once,
// at stream establishment.
func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		tok, err := a.authGRPC(ctx)
		if err != nil {
			return err
		}
		if err := a.rateGRPC(ctx, tok); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// --- HTTP middleware --------------------------------------------------------

// Middleware wraps next with bearer-auth and rate-limit enforcement. The health
// endpoints (/healthz, /readyz) MUST be mounted outside this middleware so they
// remain reachable without credentials and are never rate limited.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if a.cfg.authEnabled() {
			tok, ok := bearerFromAuthValue(r.Header.Get("Authorization"))
			if !ok || !constantTimeTokenMatch(tok, a.cfg.AuthToken) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
			token = tok
		}
		if a.limiters != nil {
			key := clientKeyFromToken(token)
			if !a.cfg.authEnabled() {
				key = clientKeyFromAddr(r.RemoteAddr)
			}
			if !a.limiters.allow(key) {
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
