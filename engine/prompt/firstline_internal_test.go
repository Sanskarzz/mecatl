package prompt

import "testing"

// TestFirstLineEdgeCases pins firstLine's behaviour across empty, whitespace-only,
// leading/trailing-newline, CRLF, and exotic-whitespace inputs. firstLine feeds the
// byte-stable tool inventory in the cache-stable StablePrefix (gauntlet #6), so a
// silent divergence here would break the provider prompt-cache invariant — this
// table is the tripwire that catches such a refactor.
func TestFirstLineEdgeCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""}, {"hello", "hello"}, {"  hello  ", "hello"},
		{"\nhello", "hello"}, {"\n\n\nhello", "hello"}, {"\n\nhello\nworld", "hello"},
		{"   ", ""}, {"\n", ""}, {"  \n  ", ""}, {"\n\n\n", ""},
		{"hello\n", "hello"}, {"line one\r\n", "line one"}, {"hello\r\nworld", "hello"},
		{"\r\n", ""}, {"\t\v\f \n x", "x"},
	}
	for _, c := range cases {
		if got := firstLine(c.in); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
