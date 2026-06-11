package session

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// MediaKind discriminates a non-text content Part of a user Message.
type MediaKind string

const (
	// MediaImage is an image part (e.g. image/png, image/jpeg).
	MediaImage MediaKind = "image"
	// MediaAudio is an audio part (e.g. audio/wav, audio/mp3).
	MediaAudio MediaKind = "audio"
)

// Media size caps. They bound the memory/persistence/per-turn-resend cost of a
// multimodal prompt (CWE-770). They apply ONLY to inline Data bytes — a part
// sourced from a URL is fetched by the remote provider, not held here, so its
// payload is not counted (its URL is validated by ValidateMediaURL instead).
const (
	// MaxMediaBytes is the cap on a single Content part's inline Data. 10 MiB
	// comfortably holds a high-resolution screenshot or photo while bounding the
	// cost of statelessly re-sending it every turn and persisting it on disk.
	MaxMediaBytes = 10 << 20 // 10 MiB
	// MaxPromptMediaBytes is the cap on the SUM of all inline part bytes in one
	// prompt, so many parts cannot collectively blow memory even when each is
	// under MaxMediaBytes.
	MaxPromptMediaBytes = 20 << 20 // 20 MiB
	// MaxPromptMediaParts is the cap on the number of parts in one prompt, so a
	// flood of tiny parts cannot exhaust memory either.
	MaxPromptMediaParts = 16
)

// Content is an immutable value object: one non-text part of a user Message.
// The source is EITHER inline bytes (Data, already base64-decoded) OR a remote
// reference (URL) — never both. MIMEType is the IANA media type (e.g.
// "image/png", "audio/wav"). Construct it via the validating constructors
// (NewImageContent / NewImageURLContent / NewAudioContent / NewAudioURLContent,
// or NewContent for the dynamic case) so the exactly-one-of(Data,URL),
// kind-set, and mime-consistency invariants hold structurally rather than by
// prose. It carries no mutating methods; treat it as immutable.
//
// Data []byte is technically mutable; by convention callers MUST NOT mutate
// Data after construction — the same treatment ToolCall.Args (json.RawMessage)
// already receives.
type Content struct {
	// Kind discriminates the media kind (image / audio).
	Kind MediaKind
	// MIMEType is the IANA media type of the part.
	MIMEType string
	// Data is the inline content bytes; nil when the part is URL-sourced.
	Data []byte
	// URL is the remote reference; "" when the part is inline.
	URL string
}

// Errors returned by the Content constructors / validators.
var (
	// ErrInvalidContent is returned when a Content part violates a structural
	// invariant (no kind, no mime, both/neither of Data/URL, mime/kind mismatch).
	ErrInvalidContent = errors.New("session: invalid media content")
	// ErrInvalidMediaURL is returned by ValidateMediaURL for a URL that is not an
	// absolute https URL to a non-internal host.
	ErrInvalidMediaURL = errors.New("session: invalid media URL")
)

// NewContent builds and validates a media Content part. EXACTLY ONE of data/url
// must be set; kind must be MediaImage or MediaAudio; mime must be non-empty and
// consistent with kind (image/* for MediaImage, audio/* for MediaAudio). A URL
// source is additionally validated by ValidateMediaURL (absolute https, no
// internal host) — the SSRF backstop, since a remote provider DEREFERENCES it.
// It is the blessed wire→domain construction path the mappers (and ACP) use; the
// raw struct stays usable for tests that intentionally bypass validation.
func NewContent(kind MediaKind, mime string, data []byte, rawURL string) (Content, error) {
	if kind != MediaImage && kind != MediaAudio {
		return Content{}, fmt.Errorf("%w: kind is required (got %q)", ErrInvalidContent, kind)
	}
	if mime == "" {
		return Content{}, fmt.Errorf("%w: mime type is required", ErrInvalidContent)
	}
	if err := validateMIME(kind, mime); err != nil {
		return Content{}, err
	}
	hasData := len(data) > 0
	hasURL := rawURL != ""
	switch {
	case hasData && hasURL:
		return Content{}, fmt.Errorf("%w: exactly one of data/url must be set, not both", ErrInvalidContent)
	case !hasData && !hasURL:
		return Content{}, fmt.Errorf("%w: exactly one of data/url must be set, got neither", ErrInvalidContent)
	}
	if hasURL {
		if err := ValidateMediaURL(rawURL); err != nil {
			return Content{}, err
		}
		return Content{Kind: kind, MIMEType: mime, URL: rawURL}, nil
	}
	return Content{Kind: kind, MIMEType: mime, Data: data}, nil
}

// NewImageContent builds a validated inline-image part.
func NewImageContent(mime string, data []byte) (Content, error) {
	return NewContent(MediaImage, mime, data, "")
}

// NewImageURLContent builds a validated URL-sourced image part.
func NewImageURLContent(mime, rawURL string) (Content, error) {
	return NewContent(MediaImage, mime, nil, rawURL)
}

// NewAudioContent builds a validated inline-audio part.
func NewAudioContent(mime string, data []byte) (Content, error) {
	return NewContent(MediaAudio, mime, data, "")
}

// NewAudioURLContent builds a validated URL-sourced audio part.
func NewAudioURLContent(mime, rawURL string) (Content, error) {
	return NewContent(MediaAudio, mime, nil, rawURL)
}

// ValidateMediaParts enforces the per-prompt media caps (CWE-770) on an already
// constructed slice of parts: at most MaxPromptMediaParts parts, each inline
// part at most MaxMediaBytes, and the SUM of inline bytes at most
// MaxPromptMediaBytes. URL-sourced parts contribute no bytes (the provider
// fetches those). It is called at the wire→domain choke point after the parts
// are built via NewContent. A nil/empty slice passes.
func ValidateMediaParts(parts []Content) error {
	if len(parts) > MaxPromptMediaParts {
		return fmt.Errorf("%w: too many media parts (%d > %d)", ErrInvalidContent, len(parts), MaxPromptMediaParts)
	}
	total := 0
	for i, p := range parts {
		n := len(p.Data)
		if n > MaxMediaBytes {
			return fmt.Errorf("%w: part[%d] inline data %d bytes exceeds the %d-byte cap", ErrInvalidContent, i, n, MaxMediaBytes)
		}
		total += n
		if total > MaxPromptMediaBytes {
			return fmt.Errorf("%w: total inline media exceeds the %d-byte cap", ErrInvalidContent, MaxPromptMediaBytes)
		}
	}
	return nil
}

// validateMIME enforces that mime names a type consistent with kind: image/*
// for MediaImage, audio/* for MediaAudio. It is honest input validation (a
// mismatched mime is a client error), not a security boundary — JSON encodes the
// value safely regardless.
func validateMIME(kind MediaKind, mime string) error {
	want := string(kind) + "/" // "image/" or "audio/"
	if !strings.HasPrefix(strings.ToLower(mime), want) {
		return fmt.Errorf("%w: mime %q is not %s* for kind %q", ErrInvalidContent, mime, kind, kind)
	}
	return nil
}

// ValidateMediaURL is the SSRF backstop (CWE-918) for a CLIENT-SUPPLIED media
// URL. Because the URL is handed to a REMOTE vision/audio provider that
// DEREFERENCES it — possibly a self-hosted/proxying provider behind the
// provider-agnostic port — an unvalidated URL could reach the cloud metadata
// endpoint or an internal host. It is STRICTER than mcp.ValidateClientURL: there
// is NO plaintext-http-to-loopback allowance, because the consumer is a remote
// service, not a local trusted process.
//
// The contract: the URL must be an absolute "https" URL with a host. A single
// trailing root dot is normalized away first. A literal IP is rejected unless it
// is global-unicast (NOT loopback, private/RFC1918, CGNAT 100.64.0.0/10,
// link-local incl. the 169.254.169.254 metadata IP, unspecified, or multicast).
// A non-canonical inet_aton-style numeric host ("0x7f.0.0.1", "0177.0.0.1",
// "127.1", "2130706433") is rejected — a resolver would decode it to a real
// (often internal) IP that net.ParseIP never canonicalised. A DNS hostname that
// obviously names an internal target ("localhost", a bare single-label name, or
// a ".local"/".internal"/".localhost" suffix) is rejected too.
//
// Residual risk (documented honestly): a PUBLIC https URL can still point at any
// public address, and the provider will fetch it. This validator blocks the
// internal/metadata SSRF shapes; a self-hosted provider that dereferences media
// URLs should ALSO apply network egress controls (deny RFC1918/link-local at the
// provider's own egress) for defense in depth. Inline base64 data is the
// primary, unaffected path and is preferred when the bytes are available.
func ValidateMediaURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%w: url is required", ErrInvalidMediaURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidMediaURL, raw, err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("%w: %q must be absolute", ErrInvalidMediaURL, raw)
	}
	if strings.ToLower(u.Scheme) != "https" {
		return fmt.Errorf("%w: %q scheme %q not allowed (https only)", ErrInvalidMediaURL, raw, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: %q has no host", ErrInvalidMediaURL, raw)
	}
	// Normalize a single trailing dot (the DNS root label): "169.254.169.254." and
	// "localhost." would otherwise slip past the checks but a resolver strips the
	// dot and reaches the same internal target.
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return fmt.Errorf("%w: %q has no host", ErrInvalidMediaURL, raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isGlobalUnicast(ip) {
			return fmt.Errorf("%w: %q resolves to a non-global/internal address %s", ErrInvalidMediaURL, raw, ip)
		}
		return nil
	}
	// A host that is NOT a canonical net.ParseIP IP but is "numeric-ish"
	// (all-numeric/dotted, or carries a 0x.../0... octal/hex label) is an
	// inet_aton-style address — e.g. "0x7f.0.0.1", "0177.0.0.1", "127.1",
	// "2130706433". The libc resolver/glibc decodes these to a real (often
	// internal) IP that net.ParseIP never canonicalised, so they would otherwise
	// fall through to the DNS branch unscreened. Reject them outright: a legitimate
	// public host always has an alphabetic TLD, and a legitimate public literal IP
	// already passed the canonical net.ParseIP screen above.
	if isNumericish(host) {
		return fmt.Errorf("%w: %q is a non-canonical numeric (inet_aton-style) host", ErrInvalidMediaURL, raw)
	}
	// DNS hostname: reject obvious-internal forms. We deliberately do NOT resolve
	// the name here (a TOCTOU resolve-then-fetch would be racy and is the
	// provider's egress responsibility per the residual note); we block the
	// clearly-internal shapes that should never be sent to a remote provider.
	if isInternalHostname(host) {
		return fmt.Errorf("%w: %q names an internal host", ErrInvalidMediaURL, raw)
	}
	return nil
}

// cgnatNet is the 100.64.0.0/10 carrier-grade NAT range (RFC 6598). Go's
// net.IP.IsPrivate does NOT cover it, so we screen it explicitly. Parsed once.
var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// isGlobalUnicast reports whether ip is a routable, public address — i.e. it is
// NOT loopback, link-local (unicast or multicast, which covers 169.254.0.0/16
// and the 169.254.169.254 metadata IP), private/RFC1918, CGNAT (100.64.0.0/10,
// which IsPrivate misses), unspecified, or multicast. Only such addresses are
// permitted as a literal-IP media host.
func isGlobalUnicast(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if cgnatNet != nil && cgnatNet.Contains(ip) {
		return false
	}
	return ip.IsGlobalUnicast()
}

// isNumericish reports whether host is composed ONLY of dot-separated labels
// that are each purely decimal, or a 0x-prefixed hex / 0-prefixed octal token —
// i.e. an inet_aton-style numeric address (whole-integer "2130706433", short
// "127.1", octal "0177.0.0.1", hex "0x7f.0.0.1"). Such a host has no alphabetic
// TLD and is never a legitimate public DNS name; a canonical public literal IP
// is already handled by the net.ParseIP branch before this is consulted, so any
// numeric-ish host reaching here is a non-canonical form to reject. An empty
// host or a label with non-numeric/non-hex characters is NOT numeric-ish.
func isNumericish(host string) bool {
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !isNumericLabel(label) {
			return false
		}
	}
	return true
}

// isNumericLabel reports whether a single host label is a decimal, hex (0x...),
// or octal (0...) integer token with no alphabetic characters beyond the hex
// digits / the "0x" marker.
func isNumericLabel(label string) bool {
	if label == "" {
		return false
	}
	s := strings.ToLower(label)
	if rest, ok := strings.CutPrefix(s, "0x"); ok {
		if rest == "" {
			return false
		}
		for _, r := range rest {
			if !isHexDigit(r) {
				return false
			}
		}
		return true
	}
	// Decimal or octal: all digits. (Octal "0177" and decimal "127" both qualify;
	// distinguishing them is unnecessary — either way it is a numeric token, not a
	// canonical IP, so it is rejected.)
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isHexDigit reports whether r is a hexadecimal digit (0-9, a-f; s is lowercased
// by the caller).
func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

// isInternalHostname reports whether a DNS hostname obviously names an internal
// target that must never be sent to a remote provider: the literal "localhost",
// a bare single-label name (no dot — an intranet short name), or a name ending
// in an internal-only suffix (".local", ".internal", ".localhost"). The caller
// has already stripped a trailing root dot.
func isInternalHostname(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" {
		return true
	}
	if !strings.Contains(h, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".internal", ".localhost"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}
