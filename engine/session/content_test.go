package session

import (
	"testing"
	"time"
)

func TestNewUserMessageWithParts(t *testing.T) {
	parts := []Content{
		{Kind: MediaImage, MIMEType: "image/png", Data: []byte{0x89, 0x50}},
		{Kind: MediaAudio, MIMEType: "audio/wav", URL: "https://example.com/a.wav"},
	}
	m := NewUserMessageWithParts("look at this", parts)
	if m.Role != RoleUser {
		t.Fatalf("role = %q, want user", m.Role)
	}
	if m.Text != "look at this" {
		t.Fatalf("text = %q", m.Text)
	}
	if len(m.Parts) != 2 {
		t.Fatalf("parts len = %d, want 2", len(m.Parts))
	}
	if m.Parts[0].Kind != MediaImage || m.Parts[0].MIMEType != "image/png" {
		t.Fatalf("part[0] = %+v", m.Parts[0])
	}
	if m.Parts[1].Kind != MediaAudio || m.Parts[1].URL != "https://example.com/a.wav" {
		t.Fatalf("part[1] = %+v", m.Parts[1])
	}
}

func TestNewUserMessageWithPartsNilIsTextOnly(t *testing.T) {
	m := NewUserMessageWithParts("hi", nil)
	if m.Parts != nil {
		t.Fatalf("parts = %v, want nil", m.Parts)
	}
	plain := NewUserMessage("hi")
	if m.Role != plain.Role || m.Text != plain.Text {
		t.Fatalf("with-parts(nil) %+v != NewUserMessage %+v", m, plain)
	}
}

func TestRecordUserPromptWithParts(t *testing.T) {
	s := New("s1", ModeDefault, "/ws", Limits{}, time.Unix(0, 0))
	parts := []Content{{Kind: MediaImage, MIMEType: "image/jpeg", Data: []byte{1, 2, 3}}}
	if err := s.RecordUserPromptWithParts("describe", parts, nil); err != nil {
		t.Fatalf("RecordUserPromptWithParts: %v", err)
	}
	if got := s.Conversation.Len(); got != 1 {
		t.Fatalf("len = %d, want 1", got)
	}
	m := s.Conversation.Messages[0]
	if m.Role != RoleUser || m.Text != "describe" || len(m.Parts) != 1 {
		t.Fatalf("recorded = %+v", m)
	}
}

func TestRecordUserPromptWithPartsInstructionsPrepended(t *testing.T) {
	s := New("s1", ModeDefault, "/ws", Limits{}, time.Unix(0, 0))
	instr := []Message{NewSystemMessage("CLAUDE.md")}
	parts := []Content{{Kind: MediaImage, MIMEType: "image/png", Data: []byte{9}}}
	if err := s.RecordUserPromptWithParts("go", parts, instr); err != nil {
		t.Fatalf("record: %v", err)
	}
	if s.Conversation.Len() != 2 {
		t.Fatalf("len = %d, want 2 (instruction + user)", s.Conversation.Len())
	}
	if s.Conversation.Messages[0].Role != RoleSystem {
		t.Fatalf("first message role = %q, want system", s.Conversation.Messages[0].Role)
	}
	if got := s.Conversation.Messages[1].Parts; len(got) != 1 {
		t.Fatalf("user parts = %v", got)
	}
}

func TestRecordUserPromptWithPartsTerminalRejected(t *testing.T) {
	s := New("s1", ModeDefault, "/ws", Limits{}, time.Unix(0, 0))
	if err := s.Complete(); err != nil {
		t.Fatalf("complete: %v", err)
	}
	err := s.RecordUserPromptWithParts("x", []Content{{Kind: MediaImage}}, nil)
	if err == nil {
		t.Fatal("expected error recording from terminal state")
	}
}

func TestRecordUserPromptDelegatesNilParts(t *testing.T) {
	s := New("s1", ModeDefault, "/ws", Limits{}, time.Unix(0, 0))
	if err := s.RecordUserPrompt("plain", nil); err != nil {
		t.Fatalf("RecordUserPrompt: %v", err)
	}
	if got := s.Conversation.Messages[0].Parts; got != nil {
		t.Fatalf("parts = %v, want nil after RecordUserPrompt", got)
	}
}

func TestValidateMediaURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"public https", "https://media.example.com/cat.png", true},
		{"public https with port", "https://images.example.org:8443/a.jpg", true},
		{"http rejected", "http://media.example.com/a.png", false},
		{"file scheme rejected", "file:///etc/passwd", false},
		{"data scheme rejected", "data:image/png;base64,AAAA", false},
		{"ftp rejected", "ftp://media.example.com/a.png", false},
		{"gopher rejected", "gopher://media.example.com/a", false},
		{"empty rejected", "", false},
		{"relative rejected", "/local/path.png", false},
		{"hostless rejected", "https:///a.png", false},
		{"loopback ip rejected", "https://127.0.0.1/a.png", false},
		{"ipv6 loopback rejected", "https://[::1]/a.png", false},
		{"metadata ip rejected", "https://169.254.169.254/latest/meta-data/", false},
		{"link-local rejected", "https://169.254.0.5/a.png", false},
		{"rfc1918 10 rejected", "https://10.0.0.5/a.png", false},
		{"rfc1918 192.168 rejected", "https://192.168.1.10/a.png", false},
		{"rfc1918 172.16 rejected", "https://172.16.0.9/a.png", false},
		{"unspecified rejected", "https://0.0.0.0/a.png", false},
		{"localhost name rejected", "https://localhost/a.png", false},
		{"single-label host rejected", "https://intranet/a.png", false},
		{".local suffix rejected", "https://printer.local/a.png", false},
		{".internal suffix rejected", "https://db.internal/a.png", false},
		{"public ip allowed", "https://93.184.216.34/a.png", true},
		// inet_aton-style numeric forms a resolver decodes to internal IPs.
		{"inet_aton hex rejected", "https://0x7f.0.0.1/a.png", false},
		{"inet_aton octal rejected", "https://0177.0.0.1/a.png", false},
		{"inet_aton short rejected", "https://127.1/a.png", false},
		{"inet_aton whole-int rejected", "https://2130706433/a.png", false},
		// CGNAT 100.64.0.0/10 — IsPrivate misses it.
		{"cgnat rejected", "https://100.64.0.1/a.png", false},
		// Trailing root dot a resolver strips to reach the same internal target.
		{"metadata ip trailing dot rejected", "https://169.254.169.254./latest/meta-data/", false},
		{"localhost trailing dot rejected", "https://localhost./a.png", false},
		// A canonical PUBLIC IPv6 must still be allowed (numeric-form rejection
		// must not over-block legitimate public literal IPs).
		{"public ipv6 allowed", "https://[2606:2800:220:1::]/a.png", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMediaURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("ValidateMediaURL(%q) = %v, want nil", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateMediaURL(%q) = nil, want error", tc.url)
			}
		})
	}
}

func TestNewContentEnforcesInvariants(t *testing.T) {
	// Valid inline image.
	if _, err := NewImageContent("image/png", []byte{1, 2}); err != nil {
		t.Fatalf("valid inline image: %v", err)
	}
	// Valid URL image (public https).
	if _, err := NewImageURLContent("image/jpeg", "https://media.example.com/x.jpg"); err != nil {
		t.Fatalf("valid url image: %v", err)
	}
	// Valid inline audio.
	if _, err := NewAudioContent("audio/wav", []byte{1}); err != nil {
		t.Fatalf("valid inline audio: %v", err)
	}

	bad := []struct {
		name string
		fn   func() (Content, error)
	}{
		{"empty kind", func() (Content, error) { return NewContent("", "image/png", []byte{1}, "") }},
		{"empty mime", func() (Content, error) { return NewImageContent("", []byte{1}) }},
		{"both data and url", func() (Content, error) {
			return NewContent(MediaImage, "image/png", []byte{1}, "https://media.example.com/x")
		}},
		{"neither data nor url", func() (Content, error) { return NewContent(MediaImage, "image/png", nil, "") }},
		{"image kind with audio mime", func() (Content, error) { return NewImageContent("audio/wav", []byte{1}) }},
		{"audio kind with image mime", func() (Content, error) { return NewAudioContent("image/png", []byte{1}) }},
		{"url image with internal host", func() (Content, error) { return NewImageURLContent("image/png", "https://localhost/x.png") }},
		{"url image with http", func() (Content, error) { return NewImageURLContent("image/png", "http://media.example.com/x.png") }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.fn(); err == nil {
				t.Fatalf("%s: expected error, got nil", tc.name)
			}
		})
	}
}

func TestValidateMediaParts(t *testing.T) {
	// Empty passes.
	if err := ValidateMediaParts(nil); err != nil {
		t.Fatalf("nil parts: %v", err)
	}
	// A part over the per-part cap is rejected.
	over := []Content{{Kind: MediaImage, MIMEType: "image/png", Data: make([]byte, MaxMediaBytes+1)}}
	if err := ValidateMediaParts(over); err == nil {
		t.Fatal("expected reject for oversized part")
	}
	// Too many parts rejected.
	many := make([]Content, MaxPromptMediaParts+1)
	for i := range many {
		many[i] = Content{Kind: MediaImage, MIMEType: "image/png", Data: []byte{1}}
	}
	if err := ValidateMediaParts(many); err == nil {
		t.Fatal("expected reject for too many parts")
	}
	// Sum over the prompt cap rejected (each under per-part, sum over total).
	per := MaxMediaBytes
	n := (MaxPromptMediaBytes / per) + 1
	if n > MaxPromptMediaParts {
		n = MaxPromptMediaParts // keep under the count cap so the SIZE cap is what trips
	}
	sum := make([]Content, n)
	for i := range sum {
		sum[i] = Content{Kind: MediaImage, MIMEType: "image/png", Data: make([]byte, per)}
	}
	if err := ValidateMediaParts(sum); err == nil {
		t.Fatal("expected reject for total inline media over the prompt cap")
	}
	// URL parts contribute no bytes — many URL parts under the count cap pass.
	urls := make([]Content, MaxPromptMediaParts)
	for i := range urls {
		urls[i] = Content{Kind: MediaImage, MIMEType: "image/png", URL: "https://media.example.com/x.png"}
	}
	if err := ValidateMediaParts(urls); err != nil {
		t.Fatalf("url parts under count cap: %v", err)
	}
}
