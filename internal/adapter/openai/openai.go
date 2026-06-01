// Package openai implements port.LLMProvider over the OpenAI Responses API
// (POST /v1/responses) using github.com/openai/openai-go/v3.
//
// The harness owns its own conversation state (the brief's "strategy B"): every
// request is stateless (Store:false, no previous_response_id) and resends the
// full input item slice, with reasoning items carried forward verbatim and
// reasoning.encrypted_content requested via Include so reasoning survives across
// turns. Tools are sent as function tools with their JSON schemas. The two-layer
// system prompt is rendered into Instructions.
//
// The streaming SSE events are translated into provider-neutral port.Chunk
// values by the pure translate function, which is exercised directly from
// recorded fixtures in tests; no network is required to test the translation.
package openai

import (
	"context"
	"iter"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/stacklok/mecatl/internal/port"
)

// Provider is a port.LLMProvider backed by the OpenAI Responses API. Construct
// it with New.
type Provider struct {
	client responses.ResponseService
}

// Option configures a Provider.
type Option func(*config)

type config struct {
	apiKey  string
	baseURL string
	extra   []option.RequestOption
}

// WithAPIKey sets the API key used to authenticate requests.
func WithAPIKey(key string) Option {
	return func(c *config) { c.apiKey = key }
}

// WithBaseURL overrides the API host so OpenAI-compatible endpoints (vLLM,
// LiteLLM, a local proxy, ...) can be targeted. The SDK appends "/responses".
func WithBaseURL(url string) Option {
	return func(c *config) { c.baseURL = url }
}

// WithRequestOption threads an arbitrary openai-go request option through to the
// client (e.g. option.WithHeader, option.WithMaxRetries). Multiple are applied
// in order, after the API key and base URL.
func WithRequestOption(opts ...option.RequestOption) Option {
	return func(c *config) { c.extra = append(c.extra, opts...) }
}

// New constructs a Provider. At minimum supply WithAPIKey; add WithBaseURL for
// compatible endpoints.
func New(opts ...Option) *Provider {
	var c config
	for _, o := range opts {
		o(&c)
	}
	reqOpts := make([]option.RequestOption, 0, len(c.extra)+2)
	if c.apiKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(c.apiKey))
	}
	if c.baseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(c.baseURL))
	}
	reqOpts = append(reqOpts, c.extra...)

	client := oai.NewClient(reqOpts...)
	return &Provider{client: client.Responses}
}

// Stream issues a streaming Responses request and yields provider-neutral
// chunks. The returned iterator translates each SSE event via translate; it
// stops (abandoning the underlying stream) when ctx is cancelled, and surfaces a
// terminal transport error as the iterator's error. The outer error is reserved
// for a failure to construct the request parameters.
func (p *Provider) Stream(ctx context.Context, req port.LLMRequest) (iter.Seq2[port.Chunk, error], error) {
	params, err := buildParams(req)
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
				// A terminal failure event (response.failed / error / incomplete)
				// carries the provider's real message; surface it as the stream's
				// error so the loop reports the reason rather than a bare stop.
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

// Capabilities reports the provider's multimodal input support. The OpenAI
// Responses input-message content union supports text + image + file but has NO
// audio member (openai-go v3.37.0), so Audio is false: audio is wired end-to-end
// through the harness but gated off at this provider until one declares it.
// Image is true; EmbeddedContext is true because inline text flattens into the
// input_text content part.
func (*Provider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{Image: true, Audio: false, EmbeddedContext: true}
}

// Compile-time assertion that Provider satisfies the port.
var _ port.LLMProvider = (*Provider)(nil)
