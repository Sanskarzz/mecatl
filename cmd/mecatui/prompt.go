package main

import "strings"

// joinPromptBody concatenates the --prompt literal and the --prompt-file body.
// Both may be supplied; the literal comes first, separated by a blank line.
// Each side is included only when non-empty so a lone source never carries a
// stray separator.
func joinPromptBody(literal, fileBody string) string {
	parts := make([]string, 0, 2)
	if s := strings.TrimRight(literal, "\n"); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimRight(fileBody, "\n"); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}
