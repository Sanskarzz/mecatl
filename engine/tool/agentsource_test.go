package tool

import "testing"

// TestAgentMCPServerIsReference pins the reference-vs-inline classification:
// an empty (or whitespace-only) URL is a REFERENCE to a configured main
// server; any non-blank URL is an inline server.
func TestAgentMCPServerIsReference(t *testing.T) {
	cases := []struct {
		name string
		srv  AgentMCPServer
		want bool
	}{
		{"empty url", AgentMCPServer{Name: "github"}, true},
		{"whitespace url", AgentMCPServer{Name: "github", URL: "   \t"}, true},
		{"inline http url", AgentMCPServer{Name: "jira", URL: "https://example.test/mcp"}, false},
		{"inline url with headers", AgentMCPServer{
			Name:    "auth",
			URL:     "https://example.test/mcp",
			Headers: map[string]string{"Authorization": "Bearer tok"},
		}, false},
		{"nameless inline still inline", AgentMCPServer{URL: "https://example.test/mcp"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.srv.IsReference(); got != c.want {
				t.Errorf("IsReference(%+v) = %v, want %v", c.srv, got, c.want)
			}
		})
	}
}
