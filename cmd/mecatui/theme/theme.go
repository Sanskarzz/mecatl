// Package theme is the pure styling layer for mecatui. It owns the semantic
// colour palette, the derived lipgloss styles, and the glamour markdown style
// config — and nothing else. It imports only the charm styling libraries and
// stdlib: NO contracts/gen, NO grpc, NO mecatui/ui, NO internal/... packages.
// This keeps the visual language reusable and the architectural layering clean
// (ui depends on theme; theme depends on nobody in this repo).
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Palette is the raw, semantic colour set a theme is defined by. Every field is
// a hex string ("#rrggbb"). Themes are authored (in Go or JSON) purely as a
// Palette; the lipgloss styles and the glamour StyleConfig are derived from it
// in compile(). The slots are semantic (what a colour means) rather than literal
// (what colour it is), so a new theme only has to answer "what is my accent?",
// not restyle every widget.
type Palette struct {
	// Brand / structural accents.
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
	Accent    string `json:"accent"`

	// Surfaces (backgrounds), darkest to lightest.
	Bg        string `json:"bg"`
	BgPanel   string `json:"bgPanel"`
	BgElement string `json:"bgElement"`

	// Borders.
	Border       string `json:"border"`
	BorderActive string `json:"borderActive"`
	BorderSubtle string `json:"borderSubtle"`

	// Foreground text.
	Text      string `json:"text"`
	TextMuted string `json:"textMuted"`

	// Status colours.
	Success string `json:"success"`
	Warning string `json:"warning"`
	Error   string `json:"error"`
	Info    string `json:"info"`

	// Speaker / block roles.
	User      string `json:"user"`
	Assistant string `json:"assistant"`
	Tool      string `json:"tool"`

	// Markdown slots (glamour body styling).
	MdText    string `json:"mdText"`
	MdHeading string `json:"mdHeading"`
	MdLink    string `json:"mdLink"`
	MdCode    string `json:"mdCode"`
	MdQuote   string `json:"mdQuote"`

	// Syntax-highlight slots (glamour code-block chroma).
	SynComment     string `json:"synComment"`
	SynKeyword     string `json:"synKeyword"`
	SynFunction    string `json:"synFunction"`
	SynVariable    string `json:"synVariable"`
	SynString      string `json:"synString"`
	SynNumber      string `json:"synNumber"`
	SynType        string `json:"synType"`
	SynOperator    string `json:"synOperator"`
	SynPunctuation string `json:"synPunctuation"`
}

// Theme is a named Palette plus its derived, ready-to-use styles. Construct one
// with New (which calls compile); never build the styles map by hand.
type Theme struct {
	Name    string
	Palette Palette

	styles map[string]lipgloss.Style // derived in compile()
	slots  map[string]string         // slot-name → hex, derived in compile()
}

// New builds a Theme from a name and palette, compiling the derived styles. It
// is the only constructor: it guarantees the styles map is populated so Style()
// never returns a zero value for a known slot.
func New(name string, p Palette) Theme {
	t := Theme{Name: name, Palette: p}
	t.compile()
	return t
}

// col turns a hex string into a color.Color. Empty strings yield nil, which
// lipgloss treats as "no colour" (terminal default) — safe for partial themes.
func col(hex string) color.Color {
	if hex == "" {
		return nil
	}
	return lipgloss.Color(hex)
}

// Color returns the color.Color for a semantic slot name (e.g. "accent",
// "error", "user"). Unknown slots return nil (terminal default). The lookup is
// by the palette's JSON field name so callers and JSON authors share one
// vocabulary.
func (t Theme) Color(slot string) color.Color {
	return col(t.hex(slot))
}

// Style returns the derived lipgloss.Style for a named UI element. Known names:
// header, footer, viewport, userBlock, userLabel, assistantLabel, toolCard,
// toolName, toolArgs, toolOk, toolErr, askCard, askTitle, askButton,
// askButtonActive, spinner, muted, errorText. Unknown names return an empty
// style so callers degrade gracefully rather than panic.
func (t Theme) Style(name string) lipgloss.Style {
	if s, ok := t.styles[name]; ok {
		return s
	}
	return lipgloss.NewStyle()
}

// hex resolves a palette slot name to its raw hex string via the slot map built
// in compile(). Unknown slots return "". Centralised so Color and JSON authors
// share one slot vocabulary.
func (t Theme) hex(slot string) string {
	return t.slots[slot]
}

// buildSlots returns the slot-name → hex map for a palette. The slot names match
// the Palette JSON tags so config authors and Color() callers use one vocabulary.
// Kept as data (not a switch) so adding a slot is a one-line change and the
// linter's cyclomatic budget is not a factor.
func buildSlots(p Palette) map[string]string {
	return map[string]string{
		"primary":        p.Primary,
		"secondary":      p.Secondary,
		"accent":         p.Accent,
		"bg":             p.Bg,
		"bgPanel":        p.BgPanel,
		"bgElement":      p.BgElement,
		"border":         p.Border,
		"borderActive":   p.BorderActive,
		"borderSubtle":   p.BorderSubtle,
		"text":           p.Text,
		"textMuted":      p.TextMuted,
		"success":        p.Success,
		"warning":        p.Warning,
		"error":          p.Error,
		"info":           p.Info,
		"user":           p.User,
		"assistant":      p.Assistant,
		"tool":           p.Tool,
		"mdText":         p.MdText,
		"mdHeading":      p.MdHeading,
		"mdLink":         p.MdLink,
		"mdCode":         p.MdCode,
		"mdQuote":        p.MdQuote,
		"synComment":     p.SynComment,
		"synKeyword":     p.SynKeyword,
		"synFunction":    p.SynFunction,
		"synVariable":    p.SynVariable,
		"synString":      p.SynString,
		"synNumber":      p.SynNumber,
		"synType":        p.SynType,
		"synOperator":    p.SynOperator,
		"synPunctuation": p.SynPunctuation,
	}
}

// compile derives the lipgloss style set from the palette. Done once at New so
// View() never allocates styles in the hot path.
func (t *Theme) compile() {
	p := t.Palette
	t.slots = buildSlots(p)
	rounded := lipgloss.RoundedBorder()
	thick := lipgloss.ThickBorder()

	t.styles = map[string]lipgloss.Style{
		// Top header bar: session/model/mode, primary border bottom.
		"header": lipgloss.NewStyle().
			Foreground(col(p.Primary)).
			Bold(true).
			Padding(0, 1).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(col(p.Border)),

		// Footer/status bar: muted, top border.
		"footer": lipgloss.NewStyle().
			Foreground(col(p.TextMuted)).
			Padding(0, 1).
			BorderStyle(lipgloss.NormalBorder()).
			BorderTop(true).
			BorderForeground(col(p.Border)),

		// Conversation viewport surface.
		"viewport": lipgloss.NewStyle().
			Foreground(col(p.Text)),

		// User prompt block: gold left bar.
		"userBlock": lipgloss.NewStyle().
			Foreground(col(p.Text)).
			BorderStyle(lipgloss.NormalBorder()).
			BorderLeft(true).
			BorderForeground(col(p.User)).
			PaddingLeft(1),
		"userLabel": lipgloss.NewStyle().
			Foreground(col(p.User)).
			Bold(true),
		"assistantLabel": lipgloss.NewStyle().
			Foreground(col(p.Assistant)).
			Bold(true),

		// Tool-call card.
		"toolCard": lipgloss.NewStyle().
			BorderStyle(rounded).
			BorderForeground(col(p.Tool)).
			Padding(0, 1),
		"toolName": lipgloss.NewStyle().
			Foreground(col(p.Tool)).
			Bold(true),
		"toolArgs": lipgloss.NewStyle().
			Foreground(col(p.TextMuted)),
		"toolOk": lipgloss.NewStyle().
			Foreground(col(p.Success)).
			Bold(true),
		"toolErr": lipgloss.NewStyle().
			Foreground(col(p.Error)).
			Bold(true),

		// Permission-ask modal card: unmissable warning border.
		"askCard": lipgloss.NewStyle().
			BorderStyle(thick).
			BorderForeground(col(p.Warning)).
			Padding(1, 2),
		"askTitle": lipgloss.NewStyle().
			Foreground(col(p.Warning)).
			Bold(true),
		"askButton": lipgloss.NewStyle().
			Foreground(col(p.Text)).
			Padding(0, 2).
			BorderStyle(rounded).
			BorderForeground(col(p.Border)),
		"askButtonActive": lipgloss.NewStyle().
			Foreground(col(p.Bg)).
			Background(col(p.BorderActive)).
			Bold(true).
			Padding(0, 2).
			BorderStyle(rounded).
			BorderForeground(col(p.BorderActive)),

		// Misc.
		"spinner": lipgloss.NewStyle().
			Foreground(col(p.Accent)),
		"muted": lipgloss.NewStyle().
			Foreground(col(p.TextMuted)).
			Italic(true),
		"errorText": lipgloss.NewStyle().
			Foreground(col(p.Error)).
			Bold(true),
	}
}
