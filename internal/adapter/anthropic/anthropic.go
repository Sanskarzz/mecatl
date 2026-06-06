package anthropic

import (
	"context"
	"iter"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/stacklok/mecatl/internal/port"
)

// defaultMaxTokens is the CONSERVATIVE flat fallback for the REQUIRED max_tokens
// param: the LOWEST common Claude output ceiling (claude-3-opus / claude-3-haiku
// = 4096), so a request for an UNCATALOGUED model never 400s on a too-high value.
// Anthropic requires max_tokens and rejects a value above the model's real output
// limit, so the per-REQUEST value is resolved per req.Model (maxTokensFor), not
// baked in — see WithMaxTokensResolver.
const defaultMaxTokens int64 = 4096

// defaultThinkingBudget is the default budget_tokens used for the manual
// (type:"enabled") thinking config on older thinking-capable model families. It
// must be ≥1024 and strictly less than max_tokens; the request builder clamps it
// below max_tokens. Adaptive-thinking models ignore it entirely (no budget
// field). Override with WithThinkingBudget.
const defaultThinkingBudget int64 = 4096

// maxTokensResolver maps a model id to its real max output ceiling. Composition
// builds it from the catalog's per-model output limit (the adapter stays
// catalog-free); a nil resolver, or a model the resolver does not know (returns
// ≤0), falls back to the construction default. This is the per-REQUEST
// max_tokens source — critical so a per-session/sub-agent route to a
// smaller-ceiling model (e.g. claude-3-5-haiku=8192) does not send the DEFAULT
// model's larger ceiling and 400 every turn.
type maxTokensResolver func(model string) int

// Provider is a port.LLMProvider backed by the native Anthropic Messages API.
// Construct it with New.
type Provider struct {
	client         sdk.MessageService
	maxTokens      int64             // construction fallback (WithMaxTokens / default)
	maxTokensFor   maxTokensResolver // per-request resolver (WithMaxTokensResolver)
	thinkingBudget int64
}

// Option configures a Provider.
type Option func(*config)

type config struct {
	apiKey         string
	baseURL        string
	maxTokens      int64
	maxTokensFor   maxTokensResolver
	thinkingBudget int64
	extra          []option.RequestOption
}

// WithAPIKey sets the API key used to authenticate requests (the x-api-key
// header). The SDK sets anthropic-version automatically.
func WithAPIKey(key string) Option {
	return func(c *config) { c.apiKey = key }
}

// WithBaseURL overrides the API host so compatible/proxy endpoints (a gateway,
// Bedrock/Vertex-style fronting) can be targeted. The SDK appends "/v1/messages".
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// WithMaxTokens sets the construction-FALLBACK max_tokens used only when the
// per-request resolver (WithMaxTokensResolver) is absent or does not know the
// request model. An unset/zero value falls back to defaultMaxTokens (the lowest
// common Claude ceiling). Prefer WithMaxTokensResolver so each request's
// max_tokens reflects ITS model's real output ceiling.
func WithMaxTokens(n int64) Option {
	return func(c *config) { c.maxTokens = n }
}

// WithMaxTokensResolver injects the per-model max-output-ceiling resolver.
// Composition builds it from the catalog so each request sends a max_tokens that
// matches req.Model's real ceiling (never the default model's larger value).
// A nil resolver — or a model it returns ≤0 for — falls back to WithMaxTokens /
// defaultMaxTokens. Keeps the adapter catalog-free (the catalog stays in
// composition).
func WithMaxTokensResolver(resolve maxTokensResolver) Option {
	return func(c *config) { c.maxTokensFor = resolve }
}

// WithThinkingBudget sets budget_tokens for the manual (type:"enabled") thinking
// config used on older model families (Sonnet 4.5, Opus 4.5, Haiku 4.5 and
// earlier). It is ignored by adaptive-thinking models (Opus 4.8/4.7/4.6, Sonnet
// 4.6). The builder clamps it to ≥1024 and strictly below max_tokens.
func WithThinkingBudget(n int64) Option {
	return func(c *config) { c.thinkingBudget = n }
}

// WithRequestOption threads an arbitrary anthropic-sdk-go request option through
// to the client (e.g. option.WithHeader, option.WithMaxRetries,
// option.WithHTTPClient for a mock transport in tests). Multiple are applied in
// order, after the API key and base URL.
func WithRequestOption(opts ...option.RequestOption) Option {
	return func(c *config) { c.extra = append(c.extra, opts...) }
}

// New constructs a Provider. At minimum supply WithAPIKey; add WithBaseURL for
// compatible endpoints and WithMaxTokens for the model's real output limit.
func New(opts ...Option) *Provider {
	var c config
	for _, o := range opts {
		o(&c)
	}
	// WithoutEnvironmentDefaults FIRST: suppress the SDK's ambient autoload of
	// ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / WIF profiles from the process env.
	// The adapter contributes ONLY what the harness resolved (the explicit key + an
	// optional flag base URL), so the documented single-knob credential/base-URL
	// custody and the availability gate are not bypassed by an ambient env var.
	reqOpts := make([]option.RequestOption, 0, len(c.extra)+3)
	reqOpts = append(reqOpts, option.WithoutEnvironmentDefaults())
	if c.apiKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(c.apiKey))
	}
	if c.baseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(c.baseURL))
	}
	reqOpts = append(reqOpts, c.extra...)

	maxTokens := c.maxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	budget := c.thinkingBudget
	if budget <= 0 {
		budget = defaultThinkingBudget
	}

	client := sdk.NewClient(reqOpts...)
	return &Provider{
		client:         client.Messages,
		maxTokens:      maxTokens,
		maxTokensFor:   c.maxTokensFor,
		thinkingBudget: budget,
	}
}

// maxTokensForModel resolves the REQUIRED max_tokens for a given request model:
// the per-model resolver's value when it knows the model (>0), else the
// construction fallback (WithMaxTokens / defaultMaxTokens). Never returns a value
// above the model's real ceiling for a catalogued model, so a per-session route
// to a smaller-ceiling model does not 400.
func (p *Provider) maxTokensForModel(model string) int64 {
	if p.maxTokensFor != nil {
		if n := p.maxTokensFor(model); n > 0 {
			return int64(n)
		}
	}
	return p.maxTokens
}

// Stream issues a streaming Messages request and yields provider-neutral chunks.
// The returned iterator translates each SSE event via translate; it stops
// (abandoning the underlying stream) when ctx is cancelled, and surfaces a
// terminal transport error as the iterator's error. The outer error is reserved
// for a failure to construct the request parameters.
func (p *Provider) Stream(ctx context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	params, err := p.buildParams(req)
	if err != nil {
		return nil, err
	}

	stream := p.client.NewStreaming(ctx, params)

	return func(yield func(port.Chunk, error) bool) {
		defer func() { _ = stream.Close() }()
		var st streamState
		for stream.Next() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			event := stream.Current()
			chunks, terr := translate(event, &st)
			for _, c := range chunks {
				if !yield(c, nil) {
					return
				}
			}
			if terr != nil {
				yield(port.Chunk{}, terr)
				return
			}
		}
		if err := stream.Err(); err != nil {
			// Don't report a plain context cancellation as a stream error; the
			// caller cancelled deliberately.
			if ctx.Err() != nil {
				return
			}
			yield(port.Chunk{}, err)
		}
	}, nil
}

// Capabilities reports the provider's multimodal input support. Anthropic vision
// is base64/url image blocks, so Image is true; the Messages content union has
// NO audio member, so Audio is false (audio is wired through the harness but
// gated off here). EmbeddedContext is true because inline text flattens into a
// text block. The composition-layer capability intersection (catalog ∩ adapter)
// then yields Image:false for any text-only Claude model with no adapter change.
func (*Provider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{Image: true, Audio: false, EmbeddedContext: true}
}

// Compile-time assertion that Provider satisfies the port.
var _ port.LLMProvider = (*Provider)(nil)
