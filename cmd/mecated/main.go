// Command mecated is the standalone mecatl server binary and a composition root:
// it parses the CLI/env configuration, builds the telemetry sink, delegates the
// engine + service assembly to internal/app (the SHARED composition layer also
// used by the embedded server in cmd/mecatui), and serves the resulting
// HarnessService over gRPC and HTTP/SSE concurrently, with graceful shutdown on
// SIGINT/SIGTERM.
//
// The agent loop, tool catalog, permission policy, provider, store, MCP and skills
// wiring all live in internal/app so the TUI can host the same server in-process;
// mecated owns only the things specific to a network daemon: flag parsing,
// TLS/auth/rate-limit, the HTTP + metrics listeners, and the `skills promote`
// operator subcommand.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/trace"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/port"
	"github.com/stacklok/mecatl/internal/adapter/agents"
	"github.com/stacklok/mecatl/internal/adapter/mcp"
	"github.com/stacklok/mecatl/internal/adapter/mcpperf"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/slogdiag"
	"github.com/stacklok/mecatl/internal/adapter/telemetry"
	"github.com/stacklok/mecatl/internal/app"
)

// TRUST MODEL (security): the mecated API exposes command and file execution
// against the configured workspace. The default listen addresses below bind the
// loopback interface (single-user localhost). Authentication is OPTIONAL and
// OFF by default for that loopback case: enable a bearer token (--auth-token /
// MECATL_AUTH_TOKEN) and/or TLS/mTLS (--tls-cert/--tls-key/--client-ca) before
// binding a non-loopback address. Binding non-loopback with NO authentication
// is permitted (an operator may front it with a mesh) but logs a prominent
// WARNING, since it exposes command/file execution to the network.
const (
	defaultGRPCAddr = "127.0.0.1:8080"
	defaultHTTPAddr = "127.0.0.1:8081"
)

// defaultMetricsAddr is the loopback listen address for the Prometheus /metrics
// endpoint. Unlike the harness API it is read-only and carries no secrets, but
// it is still bound to loopback by default. An empty --metrics-addr disables it.
const defaultMetricsAddr = "127.0.0.1:9090"

// config is the parsed command-line / environment configuration for mecated. The
// engine-build subset is mapped onto app.Config by appConfig; the rest (listen
// addresses, TLS, auth, rate limiting, metrics, tracing) is serve-time state
// owned by this binary.
type config struct {
	grpcAddr          string
	httpAddr          string
	workspace         string
	model             string
	defaultProvider   string
	defaultModel      string
	useOpenAI         bool
	openAIBaseURL     string
	openAIKey         string
	openRouterBaseURL string
	openRouterKey     string
	anthropicBaseURL  string
	anthropicKey      string
	useMock           bool
	storeDir          string
	shell             string
	noBash            bool

	// Context management: the compaction strategy and the token counter. Both
	// default to the current behaviour exactly (heuristic compactor + heuristic
	// counter); "cascade"/"tiktoken" opt into the tiered cascade / real tokenizer.
	compaction string
	tokenizer  string

	// Security: API authentication, transport security, and rate limiting.
	authToken string  // bearer token required on every RPC/request (empty disables auth)
	tlsCert   string  // PEM server certificate; enables TLS on gRPC + HTTP when set with tlsKey
	tlsKey    string  // PEM server private key (paired with tlsCert)
	clientCA  string  // PEM client CA bundle; enables mutual TLS (require+verify client certs)
	rateLimit float64 // sustained per-client request rate (req/s); 0 disables rate limiting
	rateBurst int     // token-bucket burst size; 0 -> derived from rateLimit

	// LLM resilience knobs (see package internal/adapter/llmresilience).
	llmMaxAttempts       int
	llmPerAttemptTimeout time.Duration
	llmStreamIdleTimeout time.Duration
	llmBreakerThreshold  int
	llmBreakerCooldown   time.Duration

	// maxRunTokens is the loop-level cumulative token ceiling for a single run (the
	// shared runaway brake). 0 (default) disables it.
	maxRunTokens int

	// maxTeamTokens is the team-wide cumulative token ceiling for a single team run
	// (the round-boundary brake). 0 (default) disables it.
	maxTeamTokens int

	// Observability: the Prometheus /metrics listen address (empty disables it),
	// plus the OTLP trace exporter knobs (empty endpoint disables tracing).
	metricsAddr  string
	otlpEndpoint string // OTLP collector endpoint (empty disables tracing)
	otlpProtocol string // OTLP transport: "grpc" (default) or "http"
	otlpInsecure bool   // skip TLS when dialing the OTLP collector (dev only)

	// Runtime-introspection admin surface (loopback only, on the --metrics-addr
	// listener): pprof + expvar + a runtime/metrics snapshot + a FlightRecorder.
	// mutexProfileFraction arms runtime.SetMutexProfileFraction (0 = off);
	// blockProfileRate arms runtime.SetBlockProfileRate in ns (0 = off); both add
	// runtime overhead when > 0. flightRecorder arms the bounded execution-trace
	// ring buffer (default ON — low, bounded overhead).
	mutexProfileFraction int
	blockProfileRate     int
	flightRecorder       bool

	// perfMCP mounts the read-only perf MCP server (internal/adapter/mcpperf) at
	// /mcp on the loopback admin listener, so an agent can introspect THIS
	// process's runtime/latency/profile state over MCP. OFF by default. It requires
	// --metrics-addr (the admin listener it rides) AND that address to be loopback:
	// the surface is UNAUTHENTICATED and can embed goroutine-derived names/timing,
	// so serve() FAILS CLOSED if --perf-mcp is set on a non-loopback --metrics-addr
	// (decision 6 + the security review's CWE-306 Low finding).
	perfMCP bool

	// goroutineWarnThreshold arms a background watchdog that logs slog.Warn when
	// runtime.NumGoroutine() exceeds it (decision 10: a live leak alarm, not just
	// the test-time goleak gate). 0 (default) disables it. The runtime collector
	// already exports the goroutine COUNT as a series; this is the ALARM on top.
	goroutineWarnThreshold int
	// goroutineWarnInterval is how often the watchdog samples NumGoroutine.
	goroutineWarnInterval time.Duration

	// Memory: per-project memory store directory (empty disables memory tools).
	memoryDir string

	// Remote store drivers (Phase B): gRPC driver endpoints replacing the local
	// session/memory stores (mutually exclusive with --store-dir/--memory-dir;
	// app.Build validates). The driver auth/TLS knobs apply to every driver
	// connection; the token also reads MECATL_DRIVER_AUTH_TOKEN when the flag
	// is unset (mirroring --auth-token / MECATL_AUTH_TOKEN).
	sessionStoreURL  string
	memoryStoreURL   string
	skillSourceURL   string
	soulSourceURL    string
	agentSourceURL   string
	commandSourceURL string
	driverAuthToken  string
	driverTLS        bool
	driverTLSCA      string
	driverTLSCert    string
	driverTLSKey     string

	// Soul (issue #14, Phase 1): a user-scoped, agent-READ-ONLY persona fragment.
	// ON by default reading the conventional ~/.config/mecatl/soul.md (fail-soft if
	// absent). soulFile overrides the path; noSoul disables it entirely.
	//
	// Drift baseline (issue #14, Phase 3, Item 1): the harness records the soul's
	// content hash in a sidecar (<soulPath>.sha256) trust-on-first-use; a later run
	// whose hash differs logs a drift WARN and still loads. approveSoul (re)writes the
	// baseline to the current hash (accept the edit); soulStrict makes a DRIFTED soul
	// contribute no fragment.
	soulFile    string
	noSoul      bool
	approveSoul bool
	soulStrict  bool

	// User model (issue #14, Phase 2): a user-scoped, cross-project memory of
	// durable FACTS about the operator (RememberUser/RecallUser/SearchUserModel +
	// a turn-0 <user-model> block). ON by default at the conventional
	// ~/.config/mecatl/usermodel; noUserModel disables it; userModelDir overrides
	// the dir. userModelReview enables the OPT-IN (off by default) Stop-triggered
	// background reviewer; userModelReviewInterval is its session-count debounce;
	// userModelConsolidateInterval drives a "user/"-scoped dream consolidator.
	userModelDir                 string
	noUserModel                  bool
	userModelReview              bool
	userModelReviewInterval      int
	userModelConsolidateInterval time.Duration

	// Skills: explicit directories of progressive-disclosure skill units laid out
	// as <dir>/<name>/SKILL.md (repeatable; highest precedence). Empty + no
	// conventional set disables the Skill tool. skillsConventional adds the
	// built-in conventional project/user locations (lower precedence), default OFF
	// to keep skills strictly opt-in (a trust boundary — see resolve.go / usage.md).
	skillsDirs         stringList
	skillsConventional bool

	// Agent definitions (Tier 1): named subagent specialists discovered from
	// <dir>/<name>.md files. agentsDirs are explicit dirs (repeatable; highest
	// precedence); agentsConventional adds the conventional project/user locations
	// (.mecatl/agents, .claude/agents, XDG/user) — ON by default and INERT when no
	// such dir exists, mirroring the teams/fork "on-but-inert" philosophy. subagentModel
	// globally overrides the model of every Subagent/member child that does not pin its
	// own; modelAliases maps short aliases (sonnet/opus/fast/...) to concrete ids.
	agentsDirs         stringList
	agentsConventional bool
	subagentModel      string
	modelAliases       keyValueList

	// Headless ask reviewer (issue #31): subagentAskReviewer names the model (or
	// --model-alias) of the OPT-IN one-turn reviewer that adjudicates a HEADLESS
	// child's otherwise-blanket-auto-denied permission ask; empty (default)
	// disables it. subagentAskReviewerMaxDenies is the per-run consecutive-deny
	// circuit-breaker threshold. subagentAskReviewerPolicyFile points at a TRUSTED
	// policy rubric file; parseFlags reads it (cmd mains may use os) and the
	// CONTENT travels on subagentAskReviewerPolicy into app.Config.
	subagentAskReviewer           string
	subagentAskReviewerMaxDenies  int
	subagentAskReviewerPolicyFile string
	subagentAskReviewerPolicy     string

	// Guardrails (issue #27): guardrailsModel names the tool-less checker model that
	// inspects PreToolUse (outbound-args exfil) and PostToolUse (inbound-result
	// injection) tool content; empty disables guardrails. guardrailsOff is the master
	// kill-switch (--guardrails=off) that forces guardrails off regardless of config.
	// The RULE LIST + cost knobs live in the OPERATOR-TIER `guardrails:` subtree of the
	// user-global settings.yaml (a flag cannot express a rule list); they are NEVER
	// read from the project-tier file (a security downgrade).
	guardrailsModel string
	guardrailsMode  string // the raw --guardrails value ("off" → guardrailsOff)
	guardrailsOff   bool

	// headless declares that NO human approver is attached to this deployment's
	// sessions: a child's unresolved permission ask must NOT be surfaced to the
	// client (there is nobody to answer it — it would park until run-end), and
	// instead engages the auto-deny path / the opt-in --subagent-ask-reviewer.
	// DEFAULT false: a normal mecated serving an interactive client (mecatui, an
	// IDE) surfaces asks for a human to answer. Set it for an autonomous / CI
	// deployment where clients drive runs but never answer permission prompts — it
	// is what makes --subagent-ask-reviewer actually engage.
	headless bool

	// Skills self-improvement loop (opt-in): when skillsDraftDir is non-empty the
	// writable SkillDraft tool is registered, writing model-authored candidate
	// SKILL.md files into this QUARANTINE directory (NEVER a catalog Source). An
	// operator promotes a candidate into an active --skills-dir with the
	// `mecated skills promote` subcommand. Empty disables the tool. The dir must be
	// disjoint from every active skills dir (fatal config error on overlap).
	skillsDraftDir       string
	skillsDraftThreshold float64

	// Memory consolidation (dream): background distillation interval. 0 disables.
	// Only meaningful when memoryDir is set; a positive value with an empty
	// memoryDir is a no-op (logged as a warning).
	memoryConsolidateInterval time.Duration

	// Child-session retention/GC (issue #38): age threshold, per-family count
	// cap, and sweep cadence for persisted subagent-/parallel-/team- child
	// snapshots. Only meaningful for a durable store (--store-dir or a prunable
	// remote driver); the in-memory default never accumulates across restarts.
	childRetention             time.Duration
	childRetentionMaxPerFamily int
	childGCInterval            time.Duration

	// Slash commands: directory of <name>.md command templates, and an explicit
	// enable switch. commandsDir set OR enableCommands true wires the
	// DirCommandExpander; otherwise the default NoopExpander is left in place.
	commandsDir    string
	enableCommands bool

	// WebSearch (issue #26): the vendor-neutral HTTP JSON search backend behind the
	// always-present WebSearch tool. websearchURL is the search endpoint (empty =>
	// the tool reports "not configured"); the API key is read from WEBSEARCH_API_KEY
	// (a secret, never a flag value); websearchAuthHeader/websearchQueryParam tune
	// the request shape for a generic JSON endpoint.
	websearchURL        string
	websearchAPIKey     string
	websearchAuthHeader string
	websearchQueryParam string

	// WebSearch backend ladder (issue #26): web search is ON by default (Exa
	// anonymous). searxngURL/braveAPIKey/exaAPIKey are read from SEARXNG_URL/
	// BRAVE_API_KEY/EXA_API_KEY (secrets/URLs, never flag values) and SWITCH the
	// backend; websearchMode is the raw --websearch value ("off" → websearchOff),
	// the kill switch mirroring --guardrails.
	searxngURL    string
	braveAPIKey   string
	exaAPIKey     string
	websearchMode string // raw --websearch value ("off" → websearchOff)
	websearchOff  bool

	// Parallel: enable the Parallel fan-out tool (parallel isolated child branches).
	enableParallel bool
	// forkPreservedCap bounds how many PRESERVED winner forks (join=first/judge)
	// survive at once; the oldest beyond the cap is LRU-reaped. 0 => the default.
	forkPreservedCap int

	// Teams: enable the experimental agent-teams capability (CreateTeam /
	// SpawnTeammate / RunTeam). Opt-in, default off.
	enableTeams bool

	// MCP: remote MCP servers to connect to and register tools from.
	mcpServers mcpServerList

	// MCP resources: register the ListMcpResources/ReadMcpResource meta-tools when
	// a connected server exposes resources. Default ON — the tools are registered
	// only when there is at least one resource to expose.
	mcpResourceTools bool
	// MCP prompts: compose the MCP prompt expander so "/mcp__<server>__<prompt>"
	// inputs expand to the server-rendered prompt. Default ON; expansion only fires
	// when the MCP prompt namespace is actually used.
	mcpPrompts bool

	// ACP: serve the Agent Client Protocol over stdio instead of the TCP/HTTP
	// listeners. When set, mecated speaks JSON-RPC 2.0 to an ACP editor (Zed, etc.)
	// that spawned it as a subprocess; the normal network daemon path is skipped.
	acp bool

	// ToolHive: discover MCP servers from the running ToolHive workloads (the
	// embedded ToolHive library lists already-running workloads and reads their
	// HTTP proxy URLs — mecatl never spawns a workload). Default ON; it fails soft
	// to zero servers when no container runtime is reachable.
	toolHiveEnabled bool
	// toolHiveGroup is the ToolHive group to discover from (empty -> "default").
	toolHiveGroup string

	// File-based permission config (issue #13). permissionConfigs are explicit
	// operator-pointed YAML files (repeatable, fully trusted). permissionsConventional
	// auto-discovers the per-project .mecatl/settings.yaml (and the user-global
	// file), re-resolved per session against each session's workspace root — ON by
	// default. importClaudePermissions also imports Claude-Code settings.json (with
	// the lossy fail-safe table). trustProject honours a project's ALLOW rules; a
	// project's deny/ask is always honoured regardless. Default OFF (the safe
	// stance): an untrusted repo's allows are ignored.
	permissionConfigs       stringList
	permissionsConventional bool
	importClaudePermissions bool
	trustProject            bool
	allowAllTools           bool
}

// mcpServerList is a repeatable flag.Value collecting --mcp-server name=URL
// entries into a slice of mcp.ServerConfig.
type mcpServerList []mcp.ServerConfig

func (l *mcpServerList) String() string {
	names := make([]string, 0, len(*l))
	for _, c := range *l {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// Set parses a single "name=URL" entry. A per-server bearer token is read from
// the environment variable MCP_<NAME>_TOKEN (name upper-cased) when present.
func (l *mcpServerList) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok || name == "" || url == "" {
		return fmt.Errorf("invalid --mcp-server %q: want name=URL", v)
	}
	cfg := mcp.ServerConfig{Name: name, URL: url}
	if tok := os.Getenv("MCP_" + strings.ToUpper(name) + "_TOKEN"); tok != "" {
		cfg.Headers = map[string]string{"Authorization": "Bearer " + tok}
	}
	*l = append(*l, cfg)
	return nil
}

// stringList is a repeatable string flag.Value, preserving order across multiple
// occurrences. A single occurrence behaves exactly like a plain StringVar, so a
// flag using it stays backward-compatible with single-value invocations.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// keyValueList is a repeatable "key=value" flag.Value collecting into an ordered
// map. A later occurrence of the same key overrides an earlier one. It backs
// --model-alias (e.g. --model-alias fast=gpt-4o-mini --model-alias smart=gpt-5).
type keyValueList map[string]string

func (m keyValueList) String() string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (m *keyValueList) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return fmt.Errorf("model alias must be key=value, got %q", v)
	}
	if *m == nil {
		*m = keyValueList{}
	}
	(*m)[k] = strings.TrimSpace(val)
	return nil
}

func main() {
	// Subcommand dispatch: `mecated skills promote ...` is the OPERATOR gate that
	// moves a model-authored candidate skill out of quarantine into an active
	// skills dir. It is a one-shot offline CLI action (no daemon), kept here so it
	// shares the binary and the skills adapter.
	if len(os.Args) >= 3 && os.Args[1] == "skills" && os.Args[2] == "promote" {
		if err := runSkillsPromote(os.Args[3:], os.Stdin, os.Stderr); err != nil {
			slog.Error("skills promote failed", "err", err)
			os.Exit(1)
		}
		return
	}
	// `mecated perf-mcp print-config` prints a paste-ready client .mcp.json snippet
	// for the loopback perf MCP server. Loopback + no auth (decision 6), so the
	// snippet carries NO Authorization header. One-shot offline CLI action.
	if len(os.Args) >= 3 && os.Args[1] == "perf-mcp" && os.Args[2] == "print-config" {
		if err := runPerfMCPPrintConfig(os.Args[3:], os.Stdout); err != nil {
			slog.Error("perf-mcp print-config failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("mecated exited with error", "err", err)
		os.Exit(1)
	}
}

// runSkillsPromote implements `mecated skills promote --skills-draft-dir
// <quarantine> --skills-dir <active> [--yes] <name>`: the operator-trust action that
// shows the full candidate, asks for confirmation (unless --yes), re-validates it
// (provenance + structure + injection scan), and moves it into the active skills
// tree. It requires filesystem access the model does not have, so it is the only
// path from model-authored quarantine to the trusted, live skill catalog. Flags
// precede the positional <name> (Go's flag parser stops at the first positional).
func runSkillsPromote(argv []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("mecated skills promote", flag.ContinueOnError)
	fs.SetOutput(out)
	var quarantine, active string
	var assumeYes bool
	fs.StringVar(&quarantine, "skills-draft-dir", "", "the QUARANTINE directory the candidate was drafted into")
	fs.StringVar(&active, "skills-dir", "", "the ACTIVE skills directory to promote the candidate into")
	fs.BoolVar(&assumeYes, "yes", false, "skip the interactive content review and promote without confirmation (scripted/CI use only)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	name := fs.Arg(0)
	if name == "" || quarantine == "" || active == "" {
		return fmt.Errorf("usage: mecated skills promote --skills-draft-dir <quarantine> --skills-dir <active> [--yes] <name>")
	}

	// Show the operator the FULL untrusted candidate (frontmatter + body) before
	// promoting: promotion is what makes model-authored text TRUSTED, so a human
	// must actually read it. The injection scan in skills.Promote is a backstop,
	// not a review.
	raw, err := skills.ReadCandidate(quarantine, name)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\n--- candidate skill %q (model-authored, UNTRUSTED until promoted) ---\n%s\n--- end candidate ---\n\n", name, raw)

	if !assumeYes {
		_, _ = fmt.Fprintf(out, "Promote %q into %s? This makes the above content TRUSTED and loadable by every future session. [y/N]: ", name, active)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			return fmt.Errorf("promotion of %q aborted by operator", name)
		}
	}

	if err := skills.Promote(quarantine, active, name); err != nil {
		return err
	}
	slog.Info("skill promoted to the active catalog (takes effect on next server start)",
		"name", name, "from", quarantine, "to", active)
	return nil
}

// runPerfMCPPrintConfig implements `mecated perf-mcp print-config [--metrics-addr
// host:port]`: it prints the paste-ready client .mcp.json snippet pointing at the
// loopback perf MCP server's /mcp endpoint. Per decision 6 (loopback, no auth) the
// snippet carries NO Authorization header — adding one is a future off-loopback
// concern. --metrics-addr sets the host:port in the printed URL (default
// 127.0.0.1:9090, matching defaultMetricsAddr). Output goes to stdout so it can be
// redirected into a client config.
func runPerfMCPPrintConfig(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("mecated perf-mcp print-config", flag.ContinueOnError)
	fs.SetOutput(out)
	var addr string
	fs.StringVar(&addr, "metrics-addr", defaultMetricsAddr, "the loopback admin listen address the perf MCP server is mounted on (host:port); sets the host:port in the printed URL")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	// Build the snippet via the JSON encoder so the structure (and absence of a
	// headers/Authorization field) is enforced by the type, not a fragile format
	// string. The "type":"http" transport matches the streamable-HTTP handler.
	type perfServer struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	cfg := struct {
		McpServers map[string]perfServer `json:"mcpServers"`
	}{
		McpServers: map[string]perfServer{
			"mecatl-perf": {Type: "http", URL: "http://" + addr + "/mcp"},
		},
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

// run parses flags, builds the engine/service via internal/app, and serves until a
// termination signal arrives. It is separated from main so it can return errors
// cleanly.
func run() error {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// slog.SetDefault stays for the daemon: this is the DELIBERATE, PERMANENT
	// third-party-slog bridge — a server's operational output belongs on
	// stderr/journald, so any ambient slog.Default() use (a transitive dependency, the
	// perf surface's nil-Logger fallback) is correctly routed there. cmd/ mains are the
	// only layer allowed to call slog.SetDefault; all of internal/ flows through the
	// injected port.Diagnostics (ban-guarded). The TUI, by contrast, redirects the
	// default to a FILE because it owns the alt-screen. See docs/design/DIAGNOSTICS.md.
	slog.SetDefault(logger)
	// Diagnostics sink for the composition's build-once facts and the relocated
	// operational logging. It wraps the SAME stderr/text/Info logger installed above,
	// so the facts print identically — but flow through the injected port.Diagnostics
	// rather than slog.Default().
	diag := slogdiag.NewFromLogger(logger)

	// Allow-all posture: refuse the dangerous flag when running privileged outside a
	// declared sandbox (root + no prompts can modify anything on the host). Checked
	// AFTER the slog handler is installed and BEFORE app.Build, so it covers both the
	// ACP and network serving modes.
	if err := validateAllowAll(cfg); err != nil {
		return err
	}
	if cfg.allowAllTools {
		slog.Warn("ALLOW-ALL POSTURE ACTIVE (--yolo): permission prompts for the built-in mutate-ask floor are SUPPRESSED for EVERY session on this daemon. A Deny in any scope and any deliberately configured Ask (managed/project/user) still apply — a configured Ask may block an unattended run. Intended for ephemeral, isolated, single-tenant deployments only.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Observability: install the OTel metrics pipeline (always on) + the OTLP
	// trace exporter/global TracerProvider (when --otlp-endpoint is set) BEFORE
	// building the sinks below. Setup returns the meter provider feeding the
	// domain instruments, the prometheus registry to serve at /metrics, the
	// tracer provider, and a combined shutdown that flushes both.
	providers, err := telemetry.Setup(ctx, telemetry.OTLPConfig{
		Endpoint:    cfg.otlpEndpoint,
		Protocol:    cfg.otlpProtocol,
		Insecure:    cfg.otlpInsecure,
		ServiceName: "mecatl",
	})
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := providers.Shutdown(shutdownCtx); serr != nil {
			slog.Warn("telemetry shutdown", "err", serr)
		}
	}()
	if cfg.otlpEndpoint == "" {
		slog.Info("tracing disabled (--otlp-endpoint empty); metrics + runtime collector active")
	} else {
		slog.Info("tracing enabled (OTLP exporter installed)", "endpoint", cfg.otlpEndpoint, "protocol", cfg.otlpProtocol, "insecure", cfg.otlpInsecure)
	}

	// Observability: the OTel meter provider feeds the EventSink/Logger adapter;
	// its prometheus exporter renders those series on providers.Registry, served
	// by the /metrics handler. Tracing uses the global OTel TracerProvider
	// installed by telemetry.Setup above (a no-op when tracing is disabled).
	metrics, err := telemetry.NewMetrics(providers.Meter)
	if err != nil {
		return fmt.Errorf("setup metrics: %w", err)
	}
	// Process-RSS gauge (mecatl.process.rss): Linux-only, no-op elsewhere. It
	// rides the same MeterProvider so it renders on /metrics (decision 9).
	if rerr := telemetry.RegisterProcessGauges(providers.Meter, diag); rerr != nil {
		return fmt.Errorf("setup process gauges: %w", rerr)
	}

	// Runtime profiling knobs: arm mutex/block sampling only when explicitly
	// requested (both add overhead; default 0 = off). pprof exposes the resulting
	// profiles at /debug/pprof/{mutex,block} on the loopback admin mux.
	if cfg.mutexProfileFraction > 0 {
		runtime.SetMutexProfileFraction(cfg.mutexProfileFraction)
		slog.Info("mutex profiling enabled", "fraction", cfg.mutexProfileFraction)
	}
	if cfg.blockProfileRate > 0 {
		runtime.SetBlockProfileRate(cfg.blockProfileRate)
		slog.Info("block profiling enabled", "rate_ns", cfg.blockProfileRate)
	}

	// FlightRecorder: arm the bounded execution-trace ring buffer (default ON) so
	// /debug/flightrecorder can snapshot recent activity. Stopped on shutdown so
	// the runtime trace subscription is torn down (goleak-clean).
	var recorder *telemetry.FlightRecorder
	if cfg.flightRecorder {
		// Use the process-singleton accessor: only one flight recorder may be
		// active process-wide (a stdlib constraint), and a second consumer (the
		// embedded server) must coalesce onto it rather than silently lose a Start.
		rec, rerr := telemetry.ProcessFlightRecorder(trace.FlightRecorderConfig{})
		switch {
		case errors.Is(rerr, telemetry.ErrFlightRecorderAlreadyActive):
			// Already armed elsewhere in this process — reuse the shared instance.
			recorder = rec
			slog.Info("flight recorder already active process-wide; reusing the shared instance (loopback /debug/flightrecorder)")
		case rerr != nil:
			slog.Warn("flight recorder failed to start; continuing without it", "err", rerr)
		default:
			recorder = rec
			slog.Info("flight recorder armed (loopback /debug/flightrecorder)")
			defer recorder.Stop()
		}
	}

	// Live goroutine-leak alarm (decision 10): the runtime collector already
	// exports the goroutine COUNT as a /metrics series; this is the operator-facing
	// ALARM on top — a background watchdog logging slog.Warn when the count exceeds
	// a configured ceiling. Disabled by default (threshold 0). It is bound to ctx
	// so it unwinds on shutdown — it would be ironic for the leak alarm to leak.
	if cfg.goroutineWarnThreshold > 0 {
		telemetry.StartGoroutineWatchdog(ctx, cfg.goroutineWarnThreshold, cfg.goroutineWarnInterval, runtime.NumGoroutine, slog.Default())
		slog.Info("goroutine-leak watchdog armed", "threshold", cfg.goroutineWarnThreshold, "interval", cfg.goroutineWarnInterval)
	}

	tracing := telemetry.NewTracing(otel.GetTracerProvider())

	// Role-scoped main pair (issue #47): the MAIN engine records through the
	// role="main" view so EVERY series carries the role label uniformly —
	// children get their own bounded-family views via the scoper below.
	mainScoped := metrics.WithRole(telemetry.RoleMain)

	// Slow-turn ring buffer: when the perf MCP server is mounted it observes
	// EvTurnEnd as one more EventSink fanned out alongside metrics/tracing, so its
	// list_slow_turns tool sees the SAME TurnEndPayload the latency histograms do.
	// It stores scalars only (redaction by shape) and spawns no goroutine. Built
	// only when --perf-mcp is set so a bare daemon carries no extra sink. Main
	// turns enter it with role="main"; child turns ride the scoper's fan-out.
	var slowTurns *telemetry.SlowTurnBuffer
	sinks := []port.EventSink{mainScoped, tracing}
	if cfg.perfMCP {
		slowTurns = telemetry.NewSlowTurnBuffer(telemetry.DefaultSlowTurnCapacity, time.Now)
		sinks = append(sinks, slowTurns.WithRole(telemetry.RoleMain))
	}
	sink := telemetry.NewSink(sinks...)

	// Child role scoper (issue #47): the composition hands each CHILD engine a
	// role-scoped (EventSink, ToolCallRecorder) pair keyed on the BOUNDED family
	// label internal/app's roleFamily already resolved ("subagent"/"member"/…).
	// The returned sink ALSO fans into the shared slow-turn ring buffer (when
	// mounted) so child turns appear in list_slow_turns carrying their role.
	roleScoper := func(familyRole string) (port.EventSink, port.ToolCallRecorder) {
		scoped := metrics.WithRole(familyRole)
		childSinks := []port.EventSink{scoped}
		if slowTurns != nil {
			childSinks = append(childSinks, slowTurns.WithRole(familyRole))
		}
		return telemetry.NewSink(childSinks...), scoped
	}

	built, err := app.Build(ctx, appConfig(cfg, sink, mainScoped, roleScoper, diag))
	if err != nil {
		return err
	}
	defer built.Close()

	// ACP mode: serve the Agent Client Protocol over stdio instead of the network
	// daemon. The same engine/service assembly (app.Build) backs it; only the wire
	// surface differs. No TLS/auth/rate-limit — stdio is a local parent-process
	// boundary. Logs still go to stderr (set above), keeping stdout pure JSON-RPC.
	if cfg.acp {
		// session/load (resume) is offered only when a durable session store is
		// configured: the in-memory store would lose snapshots across a restart, so
		// loadSession stays false there. A remote session-store driver is durable
		// (it replaces the JSONL dir), so it qualifies too.
		return serveACP(ctx, built.Service, cfg.storeDir != "" || cfg.sessionStoreURL != "", diag)
	}

	return serve(ctx, cfg, built.Service, providers.Registry, recorder, slowTurns)
}

// appConfig maps the CLI/env config onto the shared app.Config build contract,
// threading the telemetry sink (EventSink), the per-tool audit recorder
// (ToolCallRecorder), the child-engine role scoper (MetricsRoleScoper, issue
// #47), and the general-purpose operational logging sink (Diagnostics) into the
// engine/composition.
func appConfig(cfg config, sink port.EventSink, recorder port.ToolCallRecorder, roleScoper func(string) (port.EventSink, port.ToolCallRecorder), diag port.Diagnostics) app.Config {
	return app.Config{
		Workspace:                    cfg.workspace,
		Model:                        cfg.model,
		DefaultProvider:              cfg.defaultProvider,
		DefaultModel:                 cfg.defaultModel,
		UseOpenAI:                    cfg.useOpenAI,
		OpenAIBaseURL:                cfg.openAIBaseURL,
		OpenAIKey:                    cfg.openAIKey,
		OpenRouterBaseURL:            cfg.openRouterBaseURL,
		OpenRouterKey:                cfg.openRouterKey,
		AnthropicBaseURL:             cfg.anthropicBaseURL,
		AnthropicKey:                 cfg.anthropicKey,
		UseMock:                      cfg.useMock,
		StoreDir:                     cfg.storeDir,
		Shell:                        cfg.shell,
		NoBash:                       cfg.noBash,
		Compaction:                   cfg.compaction,
		Tokenizer:                    cfg.tokenizer,
		LLMMaxAttempts:               cfg.llmMaxAttempts,
		LLMPerAttemptTimeout:         cfg.llmPerAttemptTimeout,
		LLMStreamIdleTimeout:         cfg.llmStreamIdleTimeout,
		LLMBreakerThreshold:          cfg.llmBreakerThreshold,
		LLMBreakerCooldown:           cfg.llmBreakerCooldown,
		MaxRunTokens:                 cfg.maxRunTokens,
		MaxTeamTokens:                cfg.maxTeamTokens,
		MemoryDir:                    cfg.memoryDir,
		MemoryConsolidateInterval:    cfg.memoryConsolidateInterval,
		ChildRetention:               cfg.childRetention,
		ChildRetentionMaxPerFamily:   cfg.childRetentionMaxPerFamily,
		ChildGCInterval:              cfg.childGCInterval,
		SessionStoreURL:              cfg.sessionStoreURL,
		MemoryStoreURL:               cfg.memoryStoreURL,
		SkillSourceURL:               cfg.skillSourceURL,
		SoulSourceURL:                cfg.soulSourceURL,
		AgentSourceURL:               cfg.agentSourceURL,
		CommandSourceURL:             cfg.commandSourceURL,
		DriverAuthToken:              cfg.driverAuthToken,
		DriverTLS:                    cfg.driverTLS,
		DriverTLSCA:                  cfg.driverTLSCA,
		DriverTLSCert:                cfg.driverTLSCert,
		DriverTLSKey:                 cfg.driverTLSKey,
		SoulPath:                     cfg.soulFile,
		NoSoul:                       cfg.noSoul,
		ApproveSoul:                  cfg.approveSoul,
		SoulStrict:                   cfg.soulStrict,
		UserModelDir:                 cfg.userModelDir,
		NoUserModel:                  cfg.noUserModel,
		UserModelReview:              cfg.userModelReview,
		UserModelReviewInterval:      cfg.userModelReviewInterval,
		UserModelConsolidateInterval: cfg.userModelConsolidateInterval,
		SkillsDirs:                   cfg.skillsDirs,
		SkillsConventional:           cfg.skillsConventional,
		SkillsDraftDir:               cfg.skillsDraftDir,
		SkillsDraftThreshold:         cfg.skillsDraftThreshold,
		AgentsDirs:                   cfg.agentsDirs,
		AgentsConventional:           cfg.agentsConventional,
		SubagentModel:                cfg.subagentModel,
		SubagentAskReviewerModel:     cfg.subagentAskReviewer,
		SubagentAskReviewerMaxDenies: cfg.subagentAskReviewerMaxDenies,
		SubagentAskReviewerPolicy:    cfg.subagentAskReviewerPolicy,
		// Guardrails (issue #27): the checker model + master kill-switch. The rule list
		// and cost knobs are operator-tier YAML only (the `guardrails:` subtree of the
		// user-global settings.yaml), folded onto Config by foldOperatorGuardrails — a
		// flag cannot express a rule list.
		GuardrailsModel:         cfg.guardrailsModel,
		GuardrailsDisabled:      cfg.guardrailsOff,
		ModelAliases:            cfg.modelAliases,
		CommandsDir:             cfg.commandsDir,
		EnableCommands:          cfg.enableCommands,
		EnableParallel:          cfg.enableParallel,
		WebSearchURL:            cfg.websearchURL,
		WebSearchAPIKey:         cfg.websearchAPIKey,
		WebSearchAuthHeader:     cfg.websearchAuthHeader,
		WebSearchQueryParam:     cfg.websearchQueryParam,
		SearXNGURL:              cfg.searxngURL,
		BraveAPIKey:             cfg.braveAPIKey,
		ExaAPIKey:               cfg.exaAPIKey,
		WebSearchOff:            cfg.websearchOff,
		ForkPreservedCap:        cfg.forkPreservedCap,
		EnableTeams:             cfg.enableTeams,
		MCPServers:              cfg.mcpServers,
		MCPResourceTools:        cfg.mcpResourceTools,
		MCPPrompts:              cfg.mcpPrompts,
		ToolHiveEnabled:         cfg.toolHiveEnabled,
		ToolHiveGroup:           cfg.toolHiveGroup,
		PermissionsConventional: cfg.permissionsConventional,
		ImportClaudePermissions: cfg.importClaudePermissions,
		TrustProject:            cfg.trustProject,
		PermissionConfigs:       cfg.permissionConfigs,
		AllowAllTools:           cfg.allowAllTools,
		// mecated serves the bidi Converse + HTTP-SSE surfaces, whose clients CAN
		// answer a permission ask (ResumeApproval) — so by default a subagent's
		// unresolved Bash ask is SURFACED to the attached human rather than
		// auto-denied. --headless inverts this for an autonomous / CI deployment whose
		// clients drive runs but never answer permission prompts: surfacing there would
		// park the child until run-end, so we run NON-interactive (Interactive=false),
		// engaging the auto-deny path and the opt-in --subagent-ask-reviewer. (The
		// offline demo likewise leaves app.Config.Interactive false.)
		Interactive:       !cfg.headless,
		Sink:              sink,
		ToolCallRecorder:  recorder,
		MetricsRoleScoper: roleScoper,
		Diagnostics:       diag,
	}
}

// allowAllRefusalReason returns a non-nil error when an allow-all request must
// be refused: running privileged (euid 0) without a declared sandbox.
func allowAllRefusalReason(allowAll bool, euid int, sandbox bool) error {
	if allowAll && euid == 0 && !sandbox {
		return errors.New("--yolo refused: running as root (euid 0) without a declared sandbox; set MECATL_SANDBOX=1 (or IS_SANDBOX=1) to affirm an isolated, disposable environment")
	}
	return nil
}

func sandboxDeclared() bool {
	return os.Getenv("MECATL_SANDBOX") == "1" || os.Getenv("IS_SANDBOX") == "1"
}

func validateAllowAll(cfg config) error {
	return allowAllRefusalReason(cfg.allowAllTools, os.Geteuid(), sandboxDeclared())
}

// parseFlags turns argv into a config, resolving env-derived defaults.
func parseFlags(argv []string) (config, error) {
	fs := flag.NewFlagSet("mecated", flag.ContinueOnError)
	var cfg config

	cwd, _ := os.Getwd()

	fs.StringVar(&cfg.grpcAddr, "grpc-addr", defaultGRPCAddr,
		"gRPC listen address (defaults to loopback; set --auth-token and/or --tls-cert before binding non-loopback)")
	fs.StringVar(&cfg.httpAddr, "http-addr", defaultHTTPAddr,
		"HTTP/SSE listen address (defaults to loopback; set --auth-token and/or --tls-cert before binding non-loopback)")
	fs.StringVar(&cfg.workspace, "workspace", cwd, "default session workspace root")
	fs.StringVar(&cfg.model, "model", "", "model identifier sent to the provider (empty: use the provider-appropriate default)")
	fs.StringVar(&cfg.defaultProvider, "default-provider", "", "server-configured deployment-wide default provider id shared by every client (e.g. openai, openrouter, anthropic); overrides the built-in provider preference for zero-selector sessions while a client-side selector still wins. Validated FAIL-FAST at startup: an unknown or unavailable provider refuses to start")
	fs.StringVar(&cfg.defaultModel, "default-model", "", "server-configured deployment-wide default model id for the default provider, shared by every client; sits BELOW client-side defaults and ABOVE the per-provider built-in default. Validated FAIL-FAST at startup: a model not catalogued for the default provider refuses to start (stricter than per-session selectors, which allow passthrough)")
	fs.BoolVar(&cfg.useOpenAI, "openai", false, "use the OpenAI Responses provider (key from OPENAI_API_KEY)")
	fs.StringVar(&cfg.openAIBaseURL, "openai-base-url", "", "override the OpenAI API base URL (compatible endpoints)")
	fs.StringVar(&cfg.openRouterBaseURL, "openrouter-base-url", "", "override the OpenRouter API base URL (default https://openrouter.ai/api/v1; key from OPENROUTER_API_KEY)")
	fs.StringVar(&cfg.anthropicBaseURL, "anthropic-base-url", "", "override the native Anthropic API base URL (compatible/proxy endpoints; key from ANTHROPIC_API_KEY)")
	fs.BoolVar(&cfg.useMock, "mock", false, "use a canned offline mock provider (no network; for smoke tests only)")
	fs.StringVar(&cfg.storeDir, "store-dir", "", "directory for the JSONL session store (empty -> in-memory store)")
	fs.StringVar(&cfg.sessionStoreURL, "session-store-url", "", "host:port of a remote session-store gRPC driver (mecatl.driver.v1.SessionStoreService); replaces the local store, so it is mutually exclusive with --store-dir. Loopback may ride plaintext; pair a non-loopback target with --driver-tls (and --driver-auth-token as needed)")
	fs.StringVar(&cfg.shell, "shell", "/bin/sh", "shell used to execute Bash-tool commands; empty disables Bash (shell-less mode)")
	fs.BoolVar(&cfg.noBash, "no-bash", false, "disable the Bash tool entirely (shell-less mode); overrides --shell")

	fs.StringVar(&cfg.compaction, "compaction", "heuristic", "compaction strategy: \"heuristic\" (default, single-summary) or \"cascade\" (tiered snip→strip→collapse→summarize)")
	fs.StringVar(&cfg.tokenizer, "tokenizer", "heuristic", "token counter for the compaction trigger: \"heuristic\" (default, dependency-free) or \"tiktoken\" (offline tiktoken vocab)")

	fs.IntVar(&cfg.llmMaxAttempts, "llm-max-attempts", 3, "max LLM stream-establish attempts (initial call plus retries)")
	fs.DurationVar(&cfg.llmPerAttemptTimeout, "llm-per-attempt-timeout", 30*time.Second, "per-attempt timeout for establishing an LLM stream (0 disables)")
	fs.DurationVar(&cfg.llmStreamIdleTimeout, "llm-stream-idle-timeout", 120*time.Second, "max idle gap between LLM stream chunks after the first chunk; a longer stall terminates the turn (0 disables)")
	fs.IntVar(&cfg.llmBreakerThreshold, "llm-breaker-threshold", 5, "consecutive LLM failures that open the circuit breaker (0 disables)")
	fs.DurationVar(&cfg.llmBreakerCooldown, "llm-breaker-cooldown", 30*time.Second, "how long the LLM circuit breaker stays open before half-opening")
	fs.IntVar(&cfg.maxRunTokens, "max-run-tokens", 0, "loop-level cumulative token ceiling per run (input+output); a run that crosses it ends cleanly with stop=budget. Inherited by every subagent/team member. 0 (default) disables")
	fs.IntVar(&cfg.maxTeamTokens, "max-team-tokens", 0, "team-wide cumulative token ceiling per team run (input+output summed across ALL members and rounds). When crossed the team stops scheduling new rounds — the in-flight round and the lead's synthesis still complete, and the report states the budget stop. Applies to the Team tool and gRPC CreateTeam; a per-call Team max_team_tokens may only tighten it. Orthogonal to --max-run-tokens (per-run). 0 (default) disables")

	fs.StringVar(&cfg.metricsAddr, "metrics-addr", defaultMetricsAddr, "Prometheus /metrics listen address (empty disables the metrics endpoint)")

	fs.StringVar(&cfg.otlpEndpoint, "otlp-endpoint", "", "OTLP trace collector endpoint, e.g. localhost:4317 (empty disables tracing)")
	fs.StringVar(&cfg.otlpProtocol, "otlp-protocol", telemetry.ProtocolGRPC, "OTLP transport: \"grpc\" (default) or \"http\"")
	fs.BoolVar(&cfg.otlpInsecure, "otlp-insecure", false, "skip TLS when dialing the OTLP collector (development only)")

	fs.IntVar(&cfg.mutexProfileFraction, "mutex-profile-fraction", 0, "runtime.SetMutexProfileFraction: report 1/N mutex contention events for /debug/pprof/mutex. 0 (default) disables it. Adds per-contention sampling overhead; enable only when investigating lock contention")
	fs.IntVar(&cfg.blockProfileRate, "block-profile-rate", 0, "runtime.SetBlockProfileRate in nanoseconds: sample one blocking event per N ns blocked for /debug/pprof/block. 0 (default) disables it. Adds per-block-event overhead; enable only when investigating blocking")
	fs.BoolVar(&cfg.flightRecorder, "flight-recorder", true, "arm the execution-trace FlightRecorder (bounded in-memory ring buffer) so /debug/flightrecorder can snapshot recent activity. ON by default (low, bounded overhead). Pass --flight-recorder=false to disable")

	fs.BoolVar(&cfg.perfMCP, "perf-mcp", false, "mount the read-only perf MCP server at /mcp on the loopback admin listener, so an agent can introspect THIS process's runtime/latency/profile state over MCP (list_slow_turns, runtime/heap/CPU profiles, FlightRecorder). OFF by default. Requires --metrics-addr, and that address MUST be loopback: the surface is UNAUTHENTICATED (decision 6) and can embed goroutine-derived function names/timing, so a non-loopback --metrics-addr with --perf-mcp is REFUSED. Print a paste-ready client .mcp.json with `mecated perf-mcp print-config`")

	fs.IntVar(&cfg.goroutineWarnThreshold, "goroutine-warn-threshold", 0, "live goroutine-leak alarm: log a slog.Warn whenever runtime.NumGoroutine() exceeds this count (decision 10 of docs/design/perf-observability.md). 0 (default) disables the alarm; the runtime collector still exports the goroutine count as a /metrics series regardless. A healthy mecated holds a low-hundreds goroutine count; pick a high ceiling (e.g. 10000) so the alarm only fires on a genuine leak, not normal concurrency")
	fs.DurationVar(&cfg.goroutineWarnInterval, "goroutine-warn-interval", 30*time.Second, "how often the goroutine-leak watchdog samples runtime.NumGoroutine(). Only consulted when --goroutine-warn-threshold > 0")

	fs.StringVar(&cfg.memoryDir, "memory-dir", "", "per-project memory store directory (empty disables the Remember/Recall tools)")
	fs.DurationVar(&cfg.memoryConsolidateInterval, "memory-consolidate-interval", 0, "interval for background memory consolidation (dream); 0 disables. Only meaningful with --memory-dir")
	fs.DurationVar(&cfg.childRetention, "child-retention", 168*time.Hour, "how long persisted CHILD session snapshots (subagent-*/parallel-*/team-* ids — the InspectSubagent/resume handles) are retained before the GC sweep deletes them; main sessions are never touched. Only meaningful with a durable store (--store-dir or a prunable --session-store-url driver). 0 disables the age pass")
	fs.IntVar(&cfg.childRetentionMaxPerFamily, "child-retention-max-per-family", 500, "max persisted child session snapshots kept per delegation family (subagent/parallel/team); the oldest beyond the cap are deleted, skipping in-flight runs. Durable-store-only, like --child-retention. 0 disables the cap")
	fs.DurationVar(&cfg.childGCInterval, "child-gc-interval", time.Hour, "how often the child-session retention GC re-sweeps after the startup sweep; 0 = sweep at startup only. Only meaningful when --child-retention or --child-retention-max-per-family is active")
	fs.StringVar(&cfg.memoryStoreURL, "memory-store-url", "", "host:port of a remote memory-store gRPC driver (mecatl.driver.v1.MemoryStoreService); replaces the local flock store, so it is mutually exclusive with --memory-dir. Enables the Remember/Recall tools like --memory-dir does. Same auth/TLS posture as --session-store-url (equal URLs share one connection)")
	fs.StringVar(&cfg.driverAuthToken, "driver-auth-token", "", "bearer token sent on every store-driver RPC (or MECATL_DRIVER_AUTH_TOKEN; empty disables driver auth). Refused over cleartext to a non-loopback driver — pair with --driver-tls")
	fs.BoolVar(&cfg.driverTLS, "driver-tls", false, "enable transport TLS on the store-driver connections (--session-store-url/--memory-store-url)")
	fs.StringVar(&cfg.driverTLSCA, "driver-tls-ca", "", "PEM CA bundle to verify the store driver's server certificate (with --driver-tls; empty uses the system roots)")
	fs.StringVar(&cfg.driverTLSCert, "driver-tls-cert", "", "PEM client certificate for mutual TLS to the store driver (with --driver-tls and --driver-tls-key)")
	fs.StringVar(&cfg.driverTLSKey, "driver-tls-key", "", "PEM client private key (paired with --driver-tls-cert)")

	fs.StringVar(&cfg.soulFile, "soul-file", "", "path to a user-scoped, agent-READ-ONLY persona/\"soul\" file injected as turn-0 context (empty = the conventional $XDG_CONFIG_HOME/mecatl/soul.md, fallback ~/.config/mecatl/soul.md). Fail-soft: a missing/empty/oversized/injection-flagged file degrades to no fragment, never an error. No tool can write it")
	fs.StringVar(&cfg.soulSourceURL, "soul-source-url", "", "host:port of a remote soul-source gRPC driver (mecatl.driver.v1.SoulSourceService); occupies the USER slot of the soul selection, so it is mutually exclusive with --soul-file (--no-soul still wins). Probed at startup (fatal if unreachable); runtime faults degrade fail-soft to no fragment. The body is RE-VALIDATED locally (byte cap, injection scan, fence integrity); the drift baseline is SKIPPED for driver souls (--soul-strict/--approve-soul are no-ops for this provenance). Same auth/TLS posture as --session-store-url (equal URLs share one connection)")
	fs.BoolVar(&cfg.noSoul, "no-soul", false, "disable the user-scoped persona/soul fragment entirely (otherwise it is read from the conventional location, fail-soft if absent)")
	fs.BoolVar(&cfg.approveSoul, "approve-soul", false, "(re)write the soul DRIFT BASELINE to the current soul's content hash, accepting the file as-is. The baseline is a harness-owned sidecar next to the soul (<soul-path>.sha256); a later run whose hash differs logs a drift WARN. Use this once after intentionally editing your soul")
	fs.BoolVar(&cfg.soulStrict, "soul-strict", false, "refuse a DRIFTED soul: if the soul's content hash differs from the recorded baseline, contribute NO soul fragment this run (instead of the default warn-and-load). Pair with --approve-soul to accept an edit")

	fs.StringVar(&cfg.userModelDir, "user-model-dir", "", "directory for the user-scoped, CROSS-PROJECT user-model store of durable FACTS about the operator (empty = the conventional $XDG_CONFIG_HOME/mecatl/usermodel, fallback ~/.config/mecatl/usermodel). Exposes RememberUser/RecallUser/SearchUserModel and a turn-0 <user-model> block. Holds FACTS about the operator, never rules — how the agent behaves comes from its soul + system rules")
	fs.BoolVar(&cfg.noUserModel, "no-user-model", false, "disable the user-model entirely (the RememberUser/RecallUser/SearchUserModel tools and the <user-model> block)")
	fs.BoolVar(&cfg.userModelReview, "user-model-review", false, "enable the OPT-IN background user-model reviewer: after a session stops, a fresh single-shot child extracts durable operator FACTS from the transcript and writes them via RememberUser. OFF by default. It NEVER reopens the user session; the write path is injection-scanned")
	fs.IntVar(&cfg.userModelReviewInterval, "user-model-review-interval", 1, "session-count debounce for --user-model-review: review every Nth session that stops (1 = every session). Only consulted when --user-model-review is set")
	fs.DurationVar(&cfg.userModelConsolidateInterval, "user-model-consolidate-interval", 0, "interval for background consolidation (dream) of the user-model store, scoped to the user/ namespace; 0 disables. Only meaningful with the user-model enabled")

	fs.Var(&cfg.skillsDirs, "skills-dir", "directory to discover progressive-disclosure skills from, laid out as <name>/SKILL.md (repeatable; highest precedence); empty disables the Skill tool unless --skills-conventional is set. TRUST BOUNDARY: a SKILL.md steers the model like AGENTS.md/CLAUDE.md — point this only at directories you trust")
	fs.BoolVar(&cfg.skillsConventional, "skills-conventional", false, "also discover skills from the conventional locations: <workspace>/"+skills.ProjectDirMecatl+", <workspace>/"+skills.ProjectDirClaude+", $XDG_CONFIG_HOME/mecatl/skills (or ~/.config/mecatl/skills), and ~/.claude/skills (lower precedence than --skills-dir). Default OFF — opt in only for trusted locations (same trust class as AGENTS.md/CLAUDE.md)")
	fs.StringVar(&cfg.skillSourceURL, "skill-source-url", "", "host:port of a remote skill-source gRPC driver (mecatl.driver.v1.SkillSourceService); replaces local skills discovery, so it is mutually exclusive with --skills-dir/--skills-conventional. The driver's skill set is snapshotted at startup (fatal if unreachable); bundled files materialize lazily into a temporary asset cache on a skill's first activation (removed on shutdown). TRUST BOUNDARY: a driver-served SKILL.md steers the model like AGENTS.md/CLAUDE.md — point this only at a driver you trust. Same auth/TLS posture as --session-store-url (equal URLs share one connection)")

	fs.StringVar(&cfg.skillsDraftDir, "skills-draft-dir", "", "enable the writable SkillDraft tool and set the QUARANTINE directory for model-authored candidate skills. Empty disables the tool. TRUST BOUNDARY: must be OUTSIDE the workspace root (so the model's workspace-confined Write/Edit cannot reach it; fatal otherwise) and disjoint from every --skills-dir (fatal on overlap). Drafts are quarantined (never live); an operator reviews and promotes one with `mecated skills promote --skills-draft-dir <dir> --skills-dir <active> <name>`")
	fs.Float64Var(&cfg.skillsDraftThreshold, "skills-draft-similarity-threshold", skills.DefaultSimilarityThreshold, "2-gram Jaccard similarity above which a SkillDraft warns of a near-duplicate existing skill (warn-only, does not block)")

	fs.Var(&cfg.agentsDirs, "agents-dir", "directory to discover named agent definitions (subagent specialists) from, laid out as <name>.md with YAML frontmatter (repeatable; highest precedence). A def is reusable as a Subagent delegate (Subagent(agent=<name>)) and as a team-member role (AgentType). TRUST BOUNDARY: a def body steers the model like AGENTS.md/CLAUDE.md — point this only at directories you trust")
	fs.BoolVar(&cfg.agentsConventional, "agents-conventional", true, "also discover agent definitions from the conventional locations: <workspace>/"+agents.ProjectDirMecatl+", <workspace>/"+agents.ProjectDirClaude+", $XDG_CONFIG_HOME/mecatl/agents (or ~/.config/mecatl/agents), and ~/.claude/agents (lower precedence than --agents-dir). ON by default and INERT when no such dir exists (like teams/fork). Pass --agents-conventional=false to disable. TRUST BOUNDARY: same trust class as AGENTS.md/CLAUDE.md")
	fs.StringVar(&cfg.agentSourceURL, "agent-source-url", "", "host:port of a remote agent-definition gRPC driver (mecatl.driver.v1.AgentSourceService); the definition set is SNAPSHOTTED at startup (fatal if unreachable). Mutually exclusive with --agents-dir; the default-on conventional discovery is SUPERSEDED (not an error) — the driver becomes the only definition source. TRUST BOUNDARY: stronger than model steering — a def's hooks execute as UNGATED shell on the harness host (hookexec, every lifecycle phase, no permission ask); a compromised agent-source driver executes arbitrary shell on the harness host via def hooks, so treat it as harness-equivalent infrastructure. Same auth/TLS posture as --session-store-url (equal URLs share one connection)")
	fs.StringVar(&cfg.subagentModel, "subagent-model", "", "global default model for every Subagent / Parallel-branch / team-member child that does not pin its own model (via an agent definition or a per-call override) — the analogue of CLAUDE_CODE_SUBAGENT_MODEL; the Parallel judge stays on the session model. May be a concrete id or an alias from --model-alias; resolved on the session's provider (same-provider only). Empty inherits the parent --model; a non-empty value that does not resolve to a usable model id (unknown alias, or an alias meaning inherit) FAILS STARTUP")
	fs.Var(&cfg.modelAliases, "model-alias", "model alias mapping as name=model-id (repeatable), e.g. --model-alias fast=gpt-4o-mini. Aliases are resolved only in the composition layer; an agent def's `model: <alias>` resolves through this map (then the built-in sonnet/opus/haiku aliases)")
	fs.StringVar(&cfg.subagentAskReviewer, "subagent-ask-reviewer", "", "OPT-IN headless ask reviewer (issue #31): model id or --model-alias of a tool-less ONE-TURN reviewer that adjudicates a HEADLESS subagent/member/branch permission ask the 4-step model would otherwise blanket auto-deny. Allow = this call only (never learned); deny/error keeps the call denied (fail-safe). Configured Deny/Ask rules and an interactive approver always win; resolved on the session's provider (same-provider only). Empty (default) disables it; a value that does not resolve to a usable model id FAILS STARTUP. Deliberately a server flag, NOT a permission-config key: it grants an autonomous approval capability, an operator deployment decision")
	fs.IntVar(&cfg.subagentAskReviewerMaxDenies, "subagent-ask-reviewer-max-denies", 3, "circuit breaker for --subagent-ask-reviewer: after this many CONSECUTIVE non-allow reviewer outcomes (denies/errors/timeouts) in one run, further asks skip the reviewer and fall through to the plain auto-deny; an allow resets the count. <=0 uses the default (3)")
	fs.StringVar(&cfg.subagentAskReviewerPolicyFile, "subagent-ask-reviewer-policy", "", "path to a TRUSTED policy rubric file for --subagent-ask-reviewer; its CONTENT replaces the built-in read-only/verification rubric the reviewer applies. Empty keeps the built-in rubric. Read once at startup; an unreadable file FAILS STARTUP")
	fs.BoolVar(&cfg.headless, "headless", false, "run NON-interactive: declare that clients drive sessions but never answer permission prompts (autonomous / CI deployments). A child subagent/member/branch permission ask is then NOT surfaced to the client (nobody would answer it — it would park until run-end) but resolved by the auto-deny path / the opt-in --subagent-ask-reviewer. DEFAULT off: a normal mecated serving an interactive client (mecatui, an IDE) surfaces asks for a human. Setting --subagent-ask-reviewer WITHOUT --headless has no effect (asks surface to the client instead) — a startup WARNING says so")
	fs.StringVar(&cfg.guardrailsModel, "guardrails-model", "", "GUARDRAILS (issue #27): model id or --model-alias of a tool-less checker that inspects OUTBOUND tool-call args (PreToolUse, data exfil) and INBOUND tool results (PostToolUse, prompt injection) and enforces a verdict per the operator-tier `guardrails:` rule list. Empty (default) disables guardrails. A value that does not resolve to a usable model id FAILS STARTUP. The RULE LIST + cost knobs live in the user-global settings.yaml `guardrails:` subtree (operator-tier ONLY — a project repo cannot configure or weaken a checker); --guardrails-model overrides the YAML model")
	fs.StringVar(&cfg.guardrailsMode, "guardrails", "", "GUARDRAILS master switch: pass `--guardrails=off` to force the issue-#27 content checker OFF regardless of --guardrails-model / the guardrails: YAML config (the kill-switch). Any other value (or unset) leaves guardrails governed by the model + rule config")

	fs.StringVar(&cfg.commandsDir, "commands-dir", "", "directory of slash-command templates (<name>.md); setting it enables command expansion. Empty + --enable-commands uses the defaults (.mecatl/commands, .claude/commands)")
	fs.BoolVar(&cfg.enableCommands, "enable-commands", false, "enable slash-command expansion using the default directories (.mecatl/commands, .claude/commands) when --commands-dir is empty")
	fs.StringVar(&cfg.commandSourceURL, "command-source-url", "", "host:port of a remote slash-command gRPC driver (mecatl.driver.v1.CommandSourceService); COMPOSES with file-backed commands rather than replacing them — a local command file shadows a same-named driver command, and MCP prompts stay last. Consulted LIVE on every expansion/listing (no snapshot); probed once at startup (fatal if unreachable), runtime faults fail soft (raw text passes through). TRUST BOUNDARY: an expanded command body becomes the user prompt — point this only at a driver you trust. Same auth/TLS posture as --session-store-url (equal URLs share one connection)")

	fs.BoolVar(&cfg.enableParallel, "enable-parallel", true, "register the Parallel fan-out tool (parallel isolated child branches)")
	fs.StringVar(&cfg.websearchURL, "websearch-url", "", "WEBSEARCH (issue #26): base URL of a vendor-neutral HTTP JSON search endpoint (e.g. a SearXNG /search URL or a generic JSON search API) backing the always-present WebSearch tool. This is the EXPLICIT OVERRIDE — it wins over the SEARXNG_URL/BRAVE_API_KEY env tiers and the Exa anonymous default. The API key is read from WEBSEARCH_API_KEY, never a flag value. The adapter carries its own per-call timeout and concurrency limit. Setup walkthrough: docs/usage.md \"Enabling web search\"")
	fs.StringVar(&cfg.websearchMode, "websearch", "", "WEBSEARCH master switch: pass `--websearch=off` to DISABLE web search entirely (the kill switch — no outbound search calls, the tool reports it is disabled). Web search is ON by default (Exa anonymous tier; set EXA_API_KEY to upgrade the default tier, or SEARXNG_URL / BRAVE_API_KEY to switch backends). Any value other than \"off\" (or unset) leaves web search enabled. See docs/usage.md \"Enabling web search\"")
	fs.StringVar(&cfg.websearchAuthHeader, "websearch-auth-header", "", "WEBSEARCH: HTTP header the WEBSEARCH_API_KEY is sent in (default \"Authorization\" as a Bearer token; set e.g. \"X-API-Key\" to send the raw key). Ignored when no key is set. See docs/usage.md \"Enabling web search\"")
	fs.StringVar(&cfg.websearchQueryParam, "websearch-query-param", "", "WEBSEARCH: URL query parameter the search string is placed in (default \"q\"). Tune for a generic JSON search endpoint that expects a different parameter name. See docs/usage.md \"Enabling web search\"")
	fs.IntVar(&cfg.forkPreservedCap, "fork-preserved-cap", agent.DefaultPreservedForkCap, "max PRESERVED winner forks (join=first/judge) kept on disk at once; the oldest beyond this is LRU-reaped. Preserved forks stay inspectable until reaped")
	fs.BoolVar(&cfg.enableTeams, "enable-teams", true, "register the experimental agent-teams capability (CreateTeam/SpawnTeammate/RunTeam); on by default and inert until a client drives a team. Pass --enable-teams=false to disable")

	fs.Var(&cfg.mcpServers, "mcp-server", "remote MCP server as name=URL (repeatable); auth token read from MCP_<NAME>_TOKEN")
	fs.BoolVar(&cfg.mcpResourceTools, "mcp-resource-tools", true, "register the ListMcpResources/ReadMcpResource meta-tools when a connected MCP server exposes resources (no-op when none do). TRUST BOUNDARY: a remote resource's contents enter the model context like any other MCP output — enable only for servers you trust")
	fs.BoolVar(&cfg.mcpPrompts, "mcp-prompts", true, "expand \"/mcp__<server>__<prompt> key=value\" inputs into the server-rendered prompt (static snapshot taken at connect). TRUST BOUNDARY: an MCP prompt steers the model like a slash command — enable only for servers you trust")

	fs.BoolVar(&cfg.toolHiveEnabled, "toolhive", true, "discover MCP servers from the running ToolHive workloads (the embedded ToolHive library lists already-running workloads and reads their HTTP proxy URLs; mecatl NEVER starts or spawns a workload). Fails soft to zero servers when no container runtime is reachable. TRUST BOUNDARY: registering tools from running workloads is the same trust class as --mcp-server — every discovered workload's tools enter the model context")
	fs.StringVar(&cfg.toolHiveGroup, "toolhive-group", "", "ToolHive group to discover workloads from (empty -> the \"default\" group). Only consulted when --toolhive is set")

	fs.Var(&cfg.permissionConfigs, "permission-config", "path to a YAML permission-config file (.mecatl/settings.yaml schema: a permissions.{allow,ask,deny} list of \"Tool(pattern)\" specs) to load at the CLI scope — the HIGHEST config precedence, fully trusted (repeatable). Always loaded regardless of --permissions-conventional. A CLI rule out-ranks a project/user rule of the same effect; a config allow can LOOSEN ONLY the built-in Bash/Edit/Write ask, but a deny/ask in ANY scope still wins and a config allow never suppresses a configured ask")
	fs.BoolVar(&cfg.permissionsConventional, "permissions-conventional", true, "auto-discover the per-project permission config: <workspace>/.mecatl/settings.local.yaml (gitignored, personal — higher precedence) and <workspace>/.mecatl/settings.yaml (checked-in, shared), plus — with --import-claude-permissions — the matching .claude/settings.local.json and .claude/settings.json, plus the user-global file ($XDG_CONFIG_HOME/mecatl/settings.yaml). RE-RESOLVED PER SESSION against each session's workspace root (and revalidated on file mtime change), so two sessions in different repos get different decisions. ON by default and INERT when no such file exists. TRUST BOUNDARY: a project's ALLOW rules are honoured ONLY with --trust-project; its deny/ask rules are ALWAYS honoured")
	fs.BoolVar(&cfg.importClaudePermissions, "import-claude-permissions", false, "also import Claude-Code settings.json permissions (project <workspace>/.claude/settings{,.local}.json and user ~/.claude/settings.json) when --permissions-conventional is set. LOSSY (fail-safe): a WebFetch(domain:...) ALLOW is DEMOTED to ask, a Read(~/...) rule is left INERT (\"~\" unexpanded), an unparseable spec is DROPPED — every case is logged")
	fs.BoolVar(&cfg.trustProject, "trust-project", false, "honour a discovered PROJECT's ALLOW rules (its deny/ask rules are always honoured regardless). Default OFF (the safe stance): an untrusted repo's permission grants are ignored. TRUST BOUNDARY: enabling this lets a checked-in .mecatl/settings.yaml auto-approve tool calls — only pass it for a repo you trust")
	fs.BoolVar(&cfg.allowAllTools, "yolo", false,
		"OPERATOR POSTURE (dangerous): suppress permission prompts for the built-in mutate-ask floor server-wide, for ephemeral/sandboxed single-tenant use only. A Deny in ANY scope and any DELIBERATELY configured Ask still apply (see docs/design/ALLOW-ALL-POSTURE.md). Refused when running as root (euid 0) unless MECATL_SANDBOX=1 (or IS_SANDBOX=1) declares an isolated environment.")

	fs.BoolVar(&cfg.acp, "acp", false, "serve the Agent Client Protocol (ACP) over stdio for an editor that spawned mecated as a subprocess (JSON-RPC 2.0 on stdin/stdout). Skips the TCP/HTTP listeners; the single session workspace is the editor-provided cwd. No TLS/auth/rate-limit (stdio is a local, parent-process trust boundary)")

	fs.StringVar(&cfg.authToken, "auth-token", "", "bearer token required on every gRPC/HTTP request (or MECATL_AUTH_TOKEN; empty disables auth)")
	fs.StringVar(&cfg.tlsCert, "tls-cert", "", "PEM server certificate; with --tls-key enables TLS on the gRPC + HTTP servers")
	fs.StringVar(&cfg.tlsKey, "tls-key", "", "PEM server private key (paired with --tls-cert)")
	fs.StringVar(&cfg.clientCA, "client-ca", "", "PEM client-CA bundle; enables mutual TLS (require + verify client certs)")
	fs.Float64Var(&cfg.rateLimit, "rate-limit", 0, "sustained per-client request rate in req/s (0 disables rate limiting)")
	fs.IntVar(&cfg.rateBurst, "rate-burst", 0, "rate-limit token-bucket burst size (0 derives a sane default from --rate-limit)")

	if err := fs.Parse(argv); err != nil {
		return config{}, err
	}

	// --perf-mcp rides the admin listener, so it is meaningless without one.
	if cfg.perfMCP && cfg.metricsAddr == "" {
		return config{}, errors.New("--perf-mcp requires --metrics-addr (the loopback admin listener it mounts /mcp on)")
	}
	// FAIL CLOSED on a non-loopback --metrics-addr with --perf-mcp set, here in
	// config validation — BEFORE serve() binds any listener — so the refusal is a
	// pure config error with no side effects (matching the embed path, which
	// validates before arming any telemetry/listener). The /mcp surface is
	// UNAUTHENTICATED and can embed goroutine-derived function names and timing
	// (decision 6 + the security review's CWE-306 Low finding), so it must never
	// be reachable off loopback.
	if cfg.perfMCP && !isLoopbackHostPort(cfg.metricsAddr) {
		return config{}, fmt.Errorf("--perf-mcp refuses a non-loopback --metrics-addr=%s: it exposes unauthenticated runtime data; bind loopback or add auth (future work)", cfg.metricsAddr)
	}

	cfg.openAIKey = os.Getenv("OPENAI_API_KEY")
	// An API key in the environment implies the user wants the real provider.
	if cfg.openAIKey != "" {
		cfg.useOpenAI = true
	}
	// OpenRouter (multi-provider S1): the provider registry auto-detects it from
	// OPENROUTER_API_KEY too, but reading it here makes the credential custody
	// explicit and lets the registry prefer the dedicated key over a fallback.
	cfg.openRouterKey = os.Getenv("OPENROUTER_API_KEY")
	// Anthropic (multi-provider P1): the native Messages-API provider; the registry
	// also auto-detects ANTHROPIC_API_KEY, but reading it here makes the credential
	// custody explicit (extended thinking is ON, model-aware).
	cfg.anthropicKey = os.Getenv("ANTHROPIC_API_KEY")
	// WebSearch (issue #26): the search backend's API key is a SECRET, read from the
	// environment (never a flag value), mirroring the provider keys' custody rule.
	cfg.websearchAPIKey = os.Getenv("WEBSEARCH_API_KEY")
	// WebSearch backend ladder (issue #26): the SearXNG URL and the Brave/Exa keys
	// are secrets/URLs read from the environment, never flag values. Web search is ON
	// by default (Exa anonymous) — these only SWITCH the backend.
	cfg.searxngURL = os.Getenv("SEARXNG_URL")
	cfg.braveAPIKey = os.Getenv("BRAVE_API_KEY")
	cfg.exaAPIKey = os.Getenv("EXA_API_KEY")
	// An auth token from the environment is honored when the flag is unset, so a
	// secret need not appear in the process argv.
	if cfg.authToken == "" {
		cfg.authToken = os.Getenv("MECATL_AUTH_TOKEN")
	}
	// The store-driver bearer token mirrors the same custody rule.
	if cfg.driverAuthToken == "" {
		cfg.driverAuthToken = os.Getenv("MECATL_DRIVER_AUTH_TOKEN")
	}
	// The ask-reviewer policy rubric travels as a STRING into app.Config (the
	// composition layer never touches os); the cmd main reads the file here, once,
	// failing fast on an unreadable path (loud-misconfig posture).
	policy, err := readAskReviewerPolicy(cfg.subagentAskReviewerPolicyFile)
	if err != nil {
		return config{}, err
	}
	cfg.subagentAskReviewerPolicy = policy
	// Guardrails master switch: only `--guardrails=off` is meaningful (the kill-switch
	// — it forces guardrails off regardless of --guardrails-model / the YAML config).
	// An empty value leaves guardrails governed by the model + rule config. Any OTHER
	// value is a startup error rather than a silent no-op (so `--guardrails=on`, a
	// natural-but-wrong attempt to ENABLE, fails loudly instead of doing nothing).
	switch strings.ToLower(strings.TrimSpace(cfg.guardrailsMode)) {
	case "":
		cfg.guardrailsOff = false
	case "off":
		cfg.guardrailsOff = true
	default:
		return config{}, fmt.Errorf("--guardrails %q: only \"off\" is accepted (the kill-switch); to ENABLE guardrails set --guardrails-model (and a guardrails: rule list in your user-global settings.yaml). Leave --guardrails unset to keep guardrails governed by the model/rule config", cfg.guardrailsMode)
	}
	// WebSearch master switch (issue #26): only `--websearch=off` is meaningful (the
	// kill switch — it forces web search off regardless of the backend ladder). An
	// empty value leaves web search ON (Exa anonymous default). Any OTHER value is a
	// startup error rather than a silent no-op (mirroring --guardrails).
	switch strings.ToLower(strings.TrimSpace(cfg.websearchMode)) {
	case "":
		cfg.websearchOff = false
	case "off":
		cfg.websearchOff = true
	default:
		return config{}, fmt.Errorf("--websearch %q: only \"off\" is accepted (the kill switch); web search is ON by default (Exa anonymous tier). Set SEARXNG_URL or BRAVE_API_KEY to switch backends, or --websearch-url for an explicit endpoint. Leave --websearch unset to keep web search enabled", cfg.websearchMode)
	}
	return cfg, nil
}

// readAskReviewerPolicy reads the --subagent-ask-reviewer-policy rubric file and
// returns its content as a string. An empty path returns "" (the built-in rubric
// stands); an unreadable file is a config error (fail-fast — a silently dropped
// operator rubric would leave the reviewer on a policy the operator did not set).
func readAskReviewerPolicy(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--subagent-ask-reviewer-policy %q: %w", path, err)
	}
	return string(b), nil
}

// serve starts the gRPC and HTTP servers (and, when --metrics-addr is set, the
// loopback admin endpoint — /metrics plus the pprof/expvar/FlightRecorder
// runtime-introspection surface — on its own listener) concurrently and blocks
// until ctx is cancelled (a signal) or a server fails, then shuts them all down
// gracefully. recorder may be nil (FlightRecorder disabled), in which case
// /debug/flightrecorder is not mounted.
//
// The harness API is protected by the server.Authenticator (bearer auth + rate
// limiting, both off by default) and optionally by TLS / mutual TLS. The
// liveness/readiness probes and the standard gRPC health service are mounted
// OUTSIDE the auth/rate-limit layer so orchestrators can probe without
// credentials.
func serve(ctx context.Context, cfg config, svc *server.Service, reg *prometheus.Registry, recorder *telemetry.FlightRecorder, slowTurns *telemetry.SlowTurnBuffer) error {
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return err
	}

	auth := server.NewAuthenticator(server.SecurityConfig{
		AuthToken: cfg.authToken,
		RateLimit: cfg.rateLimit,
		RateBurst: cfg.rateBurst,
	})
	logSecurityPosture(cfg, tlsCfg)

	// --- gRPC: auth+rate interceptors, standard health service ---
	grpcOpts := []grpc.ServerOption{
		grpc.UnaryInterceptor(auth.UnaryInterceptor()),
		grpc.StreamInterceptor(auth.StreamInterceptor()),
	}
	if tlsCfg != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	mecatlv1.RegisterHarnessServiceServer(grpcSrv, server.NewHarnessServer(svc))
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthSrv.SetServingStatus("mecatl.v1.HarnessService", healthpb.HealthCheckResponse_SERVING)

	// --- HTTP: health endpoints mounted OUTSIDE auth/rate-limit; the API mux
	// wrapped in the auth middleware. The readiness probe reports ready as soon
	// as the engine/service are wired (they are, by the time serve runs). ---
	httpMux := http.NewServeMux()
	server.NewHealthHandler(func() bool { return true }).RegisterHealth(httpMux)
	httpMux.Handle("/", auth.Middleware(server.NewHTTPHandler(svc)))
	httpSrv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           httpMux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}

	// The admin endpoint runs on a separate loopback listener: it carries
	// /metrics (read-only, secret-free) PLUS the runtime-introspection surface —
	// pprof, expvar (/debug/vars), and the FlightRecorder snapshot
	// (/debug/flightrecorder). An empty --metrics-addr disables the whole mux.
	//
	// SECURITY: pprof/FlightRecorder/expvar output can embed prompt text, file
	// paths, and goroutine stacks. This listener is loopback-bound by default and
	// MUST stay loopback — these endpoints are never mounted on the public
	// gRPC/HTTP service surface (decision 6 in docs/design/perf-observability.md).
	var metricsSrv *http.Server
	adminPaths := "/metrics /debug/pprof /debug/vars /debug/flightrecorder"
	if cfg.metricsAddr != "" {
		adminMux := telemetry.NewAdminMux(reg, recorder)
		// Perf MCP server: mount /mcp on the SAME loopback admin mux. It is
		// UNAUTHENTICATED and its output can embed goroutine-derived function names
		// and timing, so a non-loopback --metrics-addr with --perf-mcp is REFUSED
		// (decision 6 + the security review's CWE-306 Low finding). That refusal is
		// enforced fail-closed in parseFlags (config validation), BEFORE serve()
		// binds anything — so by the time we reach here the address is loopback.
		if cfg.perfMCP {
			adminMux.Handle("/mcp", mcpperf.Handler(mcpperf.Deps{
				Snapshot:  telemetry.Snapshot,
				Gatherer:  reg,
				Recorder:  recorder, // nil-able: /debug/flightrecorder disabled ⇒ capture tool reports unavailable
				Profiler:  mcpperf.NewProfiler(),
				SlowTurns: slowTurnSource(slowTurns),
				Clock:     time.Now,
				Logger:    slog.Default(),
			}))
			adminPaths += " /mcp"
		}
		metricsSrv = &http.Server{
			Addr:              cfg.metricsAddr,
			Handler:           adminMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	authed := auth != nil && (cfg.authToken != "" || tlsCfg != nil)
	warnIfNonLoopback("grpc-addr", cfg.grpcAddr, authed)
	warnIfNonLoopback("http-addr", cfg.httpAddr, authed)

	grpcLis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen grpc %q: %w", cfg.grpcAddr, err)
	}

	errCh := make(chan error, 3)

	go func() {
		slog.Info("gRPC server listening", "addr", grpcLis.Addr().String())
		if serveErr := grpcSrv.Serve(grpcLis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			errCh <- fmt.Errorf("grpc serve: %w", serveErr)
		}
	}()

	go func() {
		slog.Info("HTTP/SSE server listening", "addr", cfg.httpAddr, "tls", tlsCfg != nil)
		// ListenAndServeTLS with empty cert/key paths uses the certificate already
		// loaded into TLSConfig.Certificates by buildTLSConfig.
		var serveErr error
		if tlsCfg != nil {
			serveErr = httpSrv.ListenAndServeTLS("", "")
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http serve: %w", serveErr)
		}
	}()

	if metricsSrv != nil {
		go func() {
			if cfg.perfMCP {
				slog.Info("admin server listening (loopback; /mcp is UNAUTHENTICATED perf MCP — keep loopback)", "addr", cfg.metricsAddr, "paths", adminPaths)
			} else {
				slog.Info("admin server listening (loopback)", "addr", cfg.metricsAddr, "paths", adminPaths)
			}
			if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics serve: %w", serveErr)
			}
		}()
	} else {
		slog.Info("metrics endpoint DISABLED (--metrics-addr empty)")
	}

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received; stopping servers")
	case err := <-errCh:
		slog.Error("server failed; shutting down", "err", err)
		shutdown(grpcSrv, httpSrv, metricsSrv)
		return err
	}

	shutdown(grpcSrv, httpSrv, metricsSrv)
	return nil
}

// warnIfNonLoopback logs the API trust assumption for the given bind address.
// Loopback binds are logged at info. A non-loopback bind WITH authentication
// (bearer token and/or TLS, indicated by authed) is logged at info; a
// non-loopback bind with NO authentication is logged as a prominent WARNING,
// since it exposes command/file execution to the network. It never hard-fails:
// an operator may legitimately front the server with a service mesh.
func warnIfNonLoopback(flagName, addr string, authed bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if loopback {
		slog.Info("API bound to loopback (single-user localhost trust model)",
			"flag", flagName, "addr", addr, "authenticated", authed)
		return
	}
	if authed {
		slog.Info("API bound to a non-loopback address WITH authentication (bearer token and/or TLS)",
			"flag", flagName, "addr", addr)
		return
	}
	slog.Warn("API bound to a NON-loopback address with NO authentication: it exposes UNAUTHENTICATED command/file execution to the network — set --auth-token / --tls-cert (or front it with a trusted mesh) before doing this",
		"flag", flagName, "addr", addr)
}

// isLoopbackHostPort reports whether a "host:port" listen address binds the
// loopback interface (127.0.0.0/8, ::1, or the literal "localhost"). It is the
// fail-closed gate for mounting the UNAUTHENTICATED perf MCP server: the admin
// surface can leak goroutine-derived names/timing, so it must never ride a
// non-loopback listener (decision 6 / CWE-306). A malformed address (no port) is
// treated as the bare host. An empty/unparseable host is NOT loopback (fail safe).
func isLoopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		// ACCEPTED assumption (security review Low): we trust the literal string
		// "localhost" as loopback without resolving it. A self-inflicted /etc/hosts
		// override is contrived and single-user; the SDK's DNS-rebinding/Host
		// validation remains the runtime backstop.
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// slowTurnSource bridges the telemetry slow-turn ring buffer to the
// mcpperf.SlowTurnSource read seam. The dependency points inward (cmd →
// telemetry, cmd → mcpperf); telemetry never imports the adapter, so the tiny
// field-copy adapter lives here at the composition boundary. A nil buffer yields
// a nil source so list_slow_turns reports "history not enabled".
func slowTurnSource(b *telemetry.SlowTurnBuffer) mcpperf.SlowTurnSource {
	if b == nil {
		return nil
	}
	return slowTurnBridge{b}
}

// slowTurnBridge maps telemetry.SlowTurn (scalars) to mcpperf.SlowTurn at the
// composition boundary. The shapes are identical by design, so this is a 1:1
// copy — but keeping the two types distinct is what lets telemetry stay ignorant
// of the adapter.
type slowTurnBridge struct{ b *telemetry.SlowTurnBuffer }

func (s slowTurnBridge) Recent(thresholdMs int64) []mcpperf.SlowTurn {
	src := s.b.Recent(thresholdMs)
	out := make([]mcpperf.SlowTurn, len(src))
	for i, t := range src {
		out[i] = mcpperf.SlowTurn{
			TurnIndex:       t.TurnIndex,
			DurationMs:      t.DurationMs,
			TTFTMs:          t.TTFTMs,
			InterTokenMaxMs: t.InterTokenMaxMs,
			EndedAt:         t.EndedAt,
			Role:            t.Role,
		}
	}
	return out
}

// buildTLSConfig assembles the *tls.Config for the gRPC + HTTP servers from the
// TLS flags. It returns nil (plaintext) when neither --tls-cert nor --tls-key is
// set. --tls-cert and --tls-key must be supplied together. When --client-ca is
// set it enables mutual TLS: the server requires and verifies a client
// certificate signed by the given CA bundle.
func buildTLSConfig(cfg config) (*tls.Config, error) {
	if cfg.tlsCert == "" && cfg.tlsKey == "" {
		if cfg.clientCA != "" {
			return nil, errors.New("--client-ca requires --tls-cert/--tls-key (mTLS needs server TLS)")
		}
		return nil, nil
	}
	if cfg.tlsCert == "" || cfg.tlsKey == "" {
		return nil, errors.New("--tls-cert and --tls-key must be supplied together")
	}
	cert, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cfg.clientCA != "" {
		caPEM, err := os.ReadFile(cfg.clientCA)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client CA %q: no certificates parsed", cfg.clientCA)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}

// logSecurityPosture logs the effective authentication / transport / rate-limit
// posture once at startup so an operator can confirm what is enabled.
func logSecurityPosture(cfg config, tlsCfg *tls.Config) {
	mtls := tlsCfg != nil && tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert
	slog.Info("API security posture",
		"bearer_auth", cfg.authToken != "",
		"tls", tlsCfg != nil,
		"mutual_tls", mtls,
		"rate_limit_rps", cfg.rateLimit,
		"rate_burst", cfg.rateBurst,
		"store", storeKind(cfg))
}

// storeKind returns a short label for the configured session store, noting the
// auto-resume implication of an in-memory store.
func storeKind(cfg config) string {
	if cfg.storeDir == "" {
		return "memory (no resume across restart)"
	}
	return "jsonl (resumable across restart)"
}

// shutdown gracefully stops the servers, bounding the HTTP drains with a
// timeout. metricsSrv may be nil when the /metrics endpoint is disabled.
func shutdown(grpcSrv *grpc.Server, httpSrv, metricsSrv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http graceful shutdown", "err", err)
	}
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("metrics graceful shutdown", "err", err)
		}
	}
	grpcSrv.GracefulStop()
}
