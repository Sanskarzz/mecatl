// Package cliconfig holds the small slices of CLI/composition wiring that the three
// command mains (cmd/mecated, cmd/mecatui, cmd/mecatequi) would otherwise copy-paste —
// extracted here so they cannot drift apart. It is a CMD-SIDE composition helper: it
// may read the process environment (os.Getenv) and register flags (flag.FlagSet), then
// apply the resolved values onto an app.Config.
//
// Why it lives in internal/ and not in internal/app: app.Build deliberately reads the
// environment ONLY through its injected envDetector seam (see internal/app/build.go),
// so the os.Getenv reads for provider credentials belong OUTSIDE app — in the cmd layer
// or a cmd-side helper like this one. The dependency direction stays inward
// (cmd -> cliconfig -> app); cliconfig never imports a cmd main.
package cliconfig

import (
	"flag"
	"os"

	"github.com/stacklok/mecatl/internal/app"
)

// Provider credential / base-URL environment variables. These are the SECRET-shaped
// inputs the cmd layer reads on the operator's behalf (the registry also auto-detects
// them via its envDetector, but reading them here makes credential custody explicit and
// keeps it identical across the three mains). They are NEVER logged or printed.
const (
	envOpenAIKey     = "OPENAI_API_KEY"
	envOpenRouterKey = "OPENROUTER_API_KEY"
	envAnthropicKey  = "ANTHROPIC_API_KEY"
)

// ProviderFlagHelp carries the per-main help text for the three provider base-URL
// flags. The three mains word these slightly differently (mecated is the daemon;
// mecatui prefixes "embedded server only:"), so the help is passed in rather than
// hard-coded — keeping each main's --help BYTE-IDENTICAL across the extraction. A
// zero ProviderFlagHelp falls back to DefaultProviderFlagHelp (the mecated wording),
// which is what a new consumer (mecatequi) uses.
type ProviderFlagHelp struct {
	OpenAIBaseURL     string
	OpenRouterBaseURL string
	AnthropicBaseURL  string
}

// DefaultProviderFlagHelp is the mecated-style wording, used when a field of the passed
// ProviderFlagHelp is empty. It is the right default for a fresh consumer (mecatequi).
var DefaultProviderFlagHelp = ProviderFlagHelp{
	OpenAIBaseURL:     "override the OpenAI API base URL (compatible endpoints)",
	OpenRouterBaseURL: "override the OpenRouter API base URL (default https://openrouter.ai/api/v1; key from OPENROUTER_API_KEY)",
	AnthropicBaseURL:  "override the native Anthropic API base URL (compatible/proxy endpoints; key from ANTHROPIC_API_KEY)",
}

// ProviderFlags holds the values bound by RegisterProviderFlags. The base-URL fields
// are populated by flag parsing; the key fields are populated by Apply (read from the
// environment) so a key never has to round-trip through the process argv. It is the ONE
// place the three provider credentials + base URLs are wired onto app.Config, so the
// six fields can never again be partially wired (the bug that left mecatequi unable to
// reach Anthropic / OpenRouter).
type ProviderFlags struct {
	openAIBaseURL     *string
	openRouterBaseURL *string
	anthropicBaseURL  *string
}

// RegisterProviderFlags registers --openai-base-url / --openrouter-base-url /
// --anthropic-base-url on fs and returns the binding to pass to Apply later. The help
// text comes from help, falling back per-field to DefaultProviderFlagHelp so a caller
// may pass a zero value (or override only the fields it words differently).
func RegisterProviderFlags(fs *flag.FlagSet, help ProviderFlagHelp) *ProviderFlags {
	help = help.withDefaults()
	pf := &ProviderFlags{
		openAIBaseURL:     new(string),
		openRouterBaseURL: new(string),
		anthropicBaseURL:  new(string),
	}
	fs.StringVar(pf.openAIBaseURL, "openai-base-url", "", help.OpenAIBaseURL)
	fs.StringVar(pf.openRouterBaseURL, "openrouter-base-url", "", help.OpenRouterBaseURL)
	fs.StringVar(pf.anthropicBaseURL, "anthropic-base-url", "", help.AnthropicBaseURL)
	return pf
}

// Apply reads the three provider-credential environment variables and writes all six
// values (3 keys + 3 base URLs) onto cfg. It is safe to call exactly once after fs has
// been parsed. It returns the resolved keys so a caller that needs to make a presence
// decision (mecated flips UseOpenAI on a present key; mecatui's "no provider at all"
// guard) can read them without re-querying the environment — but it NEVER logs them.
func (pf *ProviderFlags) Apply(cfg *app.Config) ResolvedKeys {
	keys := ReadProviderKeys()
	cfg.OpenAIKey = keys.OpenAI
	cfg.OpenRouterKey = keys.OpenRouter
	cfg.AnthropicKey = keys.Anthropic
	// A nil receiver (a config built WITHOUT RegisterProviderFlags — e.g. a test that
	// constructs the cmd config struct directly) applies the env keys but leaves the
	// base URLs at their zero value, exactly as the pre-extraction inline code did when
	// the base-url flags were never set. This keeps embeddedConfig/appConfig safe to
	// call on a hand-built config.
	if pf != nil {
		cfg.OpenAIBaseURL = *pf.openAIBaseURL
		cfg.OpenRouterBaseURL = *pf.openRouterBaseURL
		cfg.AnthropicBaseURL = *pf.anthropicBaseURL
	}
	return keys
}

// ReadProviderKeys reads the three provider credentials from the environment. It is the
// SINGLE definition of which env vars hold which credential — both Apply and any caller
// that must make a credential-presence decision BEFORE an app.Config exists (e.g.
// mecatui's "no provider configured" startup guard) read through it, so the var names
// can never diverge between the guard path and the wiring path. The values are
// SECRET-shaped; callers must not log or print them.
func ReadProviderKeys() ResolvedKeys {
	return ResolvedKeys{
		OpenAI:     os.Getenv(envOpenAIKey),
		OpenRouter: os.Getenv(envOpenRouterKey),
		Anthropic:  os.Getenv(envAnthropicKey),
	}
}

// ResolvedKeys is the set of provider credentials read from the environment by Apply.
// It lets a caller branch on credential presence (e.g. "an OpenAI key implies the user
// wants the real provider") without a second os.Getenv. The values are SECRET-shaped:
// callers must not log or print them.
type ResolvedKeys struct {
	OpenAI     string
	OpenRouter string
	Anthropic  string
}

// Any reports whether at least one provider credential is present. It is the shared
// "is any real provider configured?" predicate (mecatui uses it for its startup guard).
func (k ResolvedKeys) Any() bool {
	return k.OpenAI != "" || k.OpenRouter != "" || k.Anthropic != ""
}

func (h ProviderFlagHelp) withDefaults() ProviderFlagHelp {
	if h.OpenAIBaseURL == "" {
		h.OpenAIBaseURL = DefaultProviderFlagHelp.OpenAIBaseURL
	}
	if h.OpenRouterBaseURL == "" {
		h.OpenRouterBaseURL = DefaultProviderFlagHelp.OpenRouterBaseURL
	}
	if h.AnthropicBaseURL == "" {
		h.AnthropicBaseURL = DefaultProviderFlagHelp.AnthropicBaseURL
	}
	return h
}
