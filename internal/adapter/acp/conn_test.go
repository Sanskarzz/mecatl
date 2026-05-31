package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// frame builds a Content-Length-framed JSON-RPC wire message from a raw body.
func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// readFrames parses every Content-Length frame out of a buffer, returning the
// decoded message envelopes in order.
func readFrames(t *testing.T, raw []byte) []message {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader(raw))
	var out []message
	for {
		length, err := readTestHeaders(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("headers: %v", err)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal frame %q: %v", body, err)
		}
		out = append(out, m)
	}
	return out
}

// readTestHeaders mirrors Conn.readHeaders for the test parser.
func readTestHeaders(br *bufio.Reader) (int, error) {
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				return 0, io.EOF
			}
			return 0, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return 0, fmt.Errorf("bad header %q", trimmed)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, perr := strconv.Atoi(strings.TrimSpace(value))
			if perr != nil {
				return 0, perr
			}
			length = n
		}
	}
	if length < 0 {
		return 0, fmt.Errorf("missing content-length")
	}
	return length, nil
}

func TestReadMessageClassification(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantMethod string
		wantID     string // raw id, "" for notification/none
		isResponse bool
	}{
		{
			name:       "request",
			body:       `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
			wantMethod: "initialize",
			wantID:     "1",
		},
		{
			name:       "notification",
			body:       `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`,
			wantMethod: "session/cancel",
			wantID:     "",
		},
		{
			name:       "response",
			body:       `{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"selected","optionId":"allow_once"}}}`,
			wantMethod: "",
			wantID:     "7",
			isResponse: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConn(strings.NewReader(frame(tc.body)), &bytes.Buffer{}, nil)
			msg, err := c.readMessage()
			if err != nil {
				t.Fatalf("readMessage: %v", err)
			}
			if msg.Method != tc.wantMethod {
				t.Errorf("method = %q, want %q", msg.Method, tc.wantMethod)
			}
			gotID := strings.TrimSpace(string(msg.ID))
			if gotID != tc.wantID {
				t.Errorf("id = %q, want %q", gotID, tc.wantID)
			}
			isResp := msg.Method == "" && len(msg.ID) > 0
			if isResp != tc.isResponse {
				t.Errorf("isResponse = %v, want %v", isResp, tc.isResponse)
			}
		})
	}
}

func TestReadHeadersErrors(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{"missing content-length", "X-Foo: bar\r\n\r\n{}"},
		{"bad content-length", "Content-Length: notanumber\r\n\r\n{}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConn(strings.NewReader(tc.wire), &bytes.Buffer{}, nil)
			if _, err := c.readMessage(); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// TestReadMessageRejectsOversizedFrame asserts a Content-Length above the cap is
// rejected in readHeaders BEFORE the body buffer is allocated (CWE-789): we feed
// only the header (no giant body) and expect an error, proving no huge alloc /
// full-body read was attempted.
func TestReadMessageRejectsOversizedFrame(t *testing.T) {
	tests := []struct {
		name      string
		length    int
		cappedErr bool // true => rejected by the cap; false => passes cap, fails later (EOF)
	}{
		{"at cap passes the cap check", maxFrameBytes, false},
		{"one over cap rejected", maxFrameBytes + 1, true},
		{"absurd length rejected", 1 << 40, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Header only — no body bytes follow. An oversized length must error on
			// the header alone (the cap check), before any body allocation; an at-cap
			// length passes the cap check and then fails on the missing body (EOF).
			wire := fmt.Sprintf("Content-Length: %d\r\n\r\n", tc.length)
			c := NewConn(strings.NewReader(wire), &bytes.Buffer{}, nil)
			_, err := c.readMessage()
			if err == nil {
				t.Fatalf("expected an error (no body provided)")
			}
			gotCapped := strings.Contains(err.Error(), "frame too large")
			if gotCapped != tc.cappedErr {
				t.Fatalf("cap-rejection = %v (err %v), want %v", gotCapped, err, tc.cappedErr)
			}
		})
	}
}

func TestReadHeadersIgnoresUnknown(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"x"}`
	wire := fmt.Sprintf("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	c := NewConn(strings.NewReader(wire), &bytes.Buffer{}, nil)
	msg, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if msg.Method != "x" {
		t.Errorf("method = %q, want x", msg.Method)
	}
}

func TestNotifyWritesFrame(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(strings.NewReader(""), &buf, nil)
	if err := c.Notify("session/update", map[string]any{"sessionId": "s1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	frames := readFrames(t, buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Method != "session/update" {
		t.Errorf("method = %q", frames[0].Method)
	}
	if len(frames[0].ID) != 0 {
		t.Errorf("notification must have no id, got %q", frames[0].ID)
	}
}

// TestServeHandlesRequestAndError exercises the inbound request path: a handler
// result is written as a response; a *MethodError becomes a JSON-RPC error.
func TestServeHandlesRequestAndError(t *testing.T) {
	in := frame(`{"jsonrpc":"2.0","id":1,"method":"ok","params":{}}`) +
		frame(`{"jsonrpc":"2.0","id":2,"method":"bad","params":{}}`)
	var out bytes.Buffer
	h := func(_ context.Context, method string, _ json.RawMessage, _ bool) (any, error) {
		if method == "bad" {
			return nil, newMethodErr(codeInvalidParams, "nope")
		}
		return map[string]string{"hello": "world"}, nil
	}
	c := NewConn(strings.NewReader(in), &out, h)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Serve(ctx); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	frames := readFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("got %d response frames, want 2", len(frames))
	}
	byID := map[string]message{}
	for _, f := range frames {
		byID[strings.TrimSpace(string(f.ID))] = f
	}
	if got := byID["1"]; got.Error != nil || !strings.Contains(string(got.Result), "world") {
		t.Errorf("id 1 result = %q err=%v", got.Result, got.Error)
	}
	if got := byID["2"]; got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Errorf("id 2 expected invalid-params error, got %+v", got.Error)
	}
}

// TestCallCorrelation drives an OUTBOUND request (Call) against a scripted peer
// that echoes the request id back in a response — verifying response
// correlation, the substance of request_permission.
func TestCallCorrelation(t *testing.T) {
	clientReader, clientWriter := io.Pipe() // peer -> conn (responses arrive here)
	sw := &syncWriter{w: &bytes.Buffer{}}   // conn -> peer (requests captured)

	c := NewConn(clientReader, sw, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Serve(ctx) }()

	// Issue the outbound Call in a goroutine; it blocks for the response.
	type result struct {
		out map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		var out map[string]any
		err := c.Call(ctx, "session/request_permission", map[string]any{"sessionId": "s1"}, &out)
		done <- result{out, err}
	}()

	// Read the request the conn wrote, extract its id, and respond with it.
	id := waitForRequestID(t, sw)
	respBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, id)
	if _, err := clientWriter.Write([]byte(frame(respBody))); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Call err: %v", r.err)
		}
		if r.out["ok"] != true {
			t.Errorf("Call out = %v, want ok:true", r.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return")
	}
}

// waitForRequestID polls the captured outbound buffer until a framed request is
// present, returning its raw id.
func waitForRequestID(t *testing.T, sent *syncWriter) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frames := readFrames(t, sent.snapshot())
		for _, f := range frames {
			if f.Method == "session/request_permission" && len(f.ID) > 0 {
				return strings.TrimSpace(string(f.ID))
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no outbound request observed")
	return ""
}

// syncWriter serializes concurrent writes into a buffer for the test (the Conn
// already locks writes, but the test reads the buffer concurrently).
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// snapshot returns a copy of the bytes written so far, under lock.
func (s *syncWriter) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.w.Bytes()...)
}
