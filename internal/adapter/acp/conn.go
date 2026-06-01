package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// jsonrpcVersion is the only JSON-RPC version this codec emits or accepts.
const jsonrpcVersion = "2.0"

// maxFrameBytes caps a single inbound ndjson LINE (one JSON message). The reader
// (readLine) enforces it INCREMENTALLY — it accumulates fixed-size fragments and
// rejects the line as an error (never truncates) the moment the accumulator would
// exceed the cap, BEFORE the over-limit bytes are buffered. So worst-case
// resident memory is O(maxFrameBytes) regardless of what the peer streams: a
// malicious or buggy peer sending a huge line with no newline cannot drive an
// unbounded buffer (CWE-789, memory exhaustion). 16 MiB is far above any
// legitimate ACP message (a prompt + tool results) while bounding the worst case.
const maxFrameBytes = 16 << 20 // 16 MiB

// FRAMING — newline-delimited JSON (ndjson), NOT Content-Length headers. Each
// message on the wire is one JSON object on its own line, terminated by a single
// '\n':
//
//	{"jsonrpc":"2.0",...}\n
//	{"jsonrpc":"2.0",...}\n
//
// This is what the ACP spec mandates (transports.mdx: "Messages are delimited by
// newlines (\n), and MUST NOT contain embedded newlines"), what the reference TS
// SDK (ndJsonStream) emits, and what coder/acp-go-sdk reads. An earlier comment
// here claimed "ACP mirrors LSP" and used Content-Length framing — that was
// WRONG and could not interoperate with a real Zed/ACP client. json.Marshal
// never emits a raw newline in its output, so a marshalled message is always a
// single safe line; the writer appends exactly one '\n' terminator. The reader
// tolerates a stray trailing '\r' (CRLF peers) and skips bare blank lines.

// message is the union JSON-RPC envelope covering requests, responses, and
// notifications. The discriminators the codec uses:
//   - a request    has Method set AND a non-null ID,
//   - a notification has Method set AND no ID,
//   - a response   has no Method and a non-null ID (with Result xor Error).
//
// ID is json.RawMessage so it round-trips a number or string id verbatim (a
// notification omits it via omitempty).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// JSON-RPC standard error codes (the subset this adapter emits).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Handler dispatches one inbound request or notification. For a request, the
// returned value is JSON-marshalled into the response Result; a returned error
// becomes a JSON-RPC error response (a *MethodError sets the code, anything else
// is codeInternalError). For a notification (isRequest false) the return value
// and error are ignored — a notification gets no reply per JSON-RPC.
type Handler func(ctx context.Context, method string, params json.RawMessage, isRequest bool) (any, error)

// MethodError is an error a Handler can return to control the JSON-RPC error
// code sent back to the peer (e.g. codeInvalidParams for a bad request).
type MethodError struct {
	Code    int
	Message string
}

func (e *MethodError) Error() string { return e.Message }

// newMethodErr is a small constructor for the common Handler error cases.
func newMethodErr(code int, msg string) *MethodError { return &MethodError{Code: code, Message: msg} }

// Conn is a bidirectional JSON-RPC 2.0 connection over a reader/writer pair
// (stdin/stdout for ACP). It is BOTH a server (it reads inbound requests and
// notifications and routes them to a Handler) AND a client (Call issues an
// outbound request and blocks for the correlated response; Notify sends a
// fire-and-forget notification). Writes are serialized by a mutex so concurrent
// session/update notifications and a request_permission round-trip never
// interleave a frame. It is safe for concurrent use.
type Conn struct {
	r *bufio.Reader

	writeMu sync.Mutex
	w       io.Writer

	// pending correlates an outbound request id with the goroutine awaiting its
	// response. nextID hands out monotonically increasing client request ids.
	mu      sync.Mutex
	pending map[int64]chan *message
	nextID  int64
	closed  bool

	// wg tracks dispatched inbound request/notification handler goroutines so
	// Serve can wait for them to finish writing before it returns on EOF (a
	// handler may still be marshalling its response when input ends).
	wg sync.WaitGroup

	handler Handler
}

// NewConn builds a Conn over r/w with the given inbound Handler. The handler may
// be nil if the connection is used purely as a client (no inbound dispatch).
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	return &Conn{
		r:       bufio.NewReader(r),
		w:       w,
		pending: make(map[int64]chan *message),
		handler: h,
	}
}

// Serve runs the read loop until the input stream ends or ctx is cancelled. Each
// inbound frame is classified and routed: a response wakes the matching pending
// Call; a request is dispatched to the Handler on its own goroutine (so a Handler
// that itself issues an outbound Call — e.g. session/prompt issuing
// request_permission — does not deadlock the read loop); a notification is
// dispatched to the Handler with isRequest=false and produces no reply. Serve
// returns nil on a clean EOF, or the read/decode error otherwise.
func (c *Conn) Serve(ctx context.Context) error {
	defer c.shutdown()
	// Wait for dispatched handler goroutines to drain before returning, so the
	// last response/notification is fully written before the connection is torn
	// down (and so a test can read the complete output after Serve returns).
	defer c.wg.Wait()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, err := c.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		c.route(ctx, msg)
	}
}

// route classifies one decoded frame and dispatches it.
func (c *Conn) route(ctx context.Context, msg *message) {
	switch {
	case msg.Method == "" && len(msg.ID) > 0:
		// A response to one of our outbound Calls.
		c.deliver(msg)
	case msg.Method != "" && len(msg.ID) > 0:
		// An inbound request: reply on its own goroutine so a Handler that issues
		// an outbound Call (request_permission during session/prompt) cannot
		// deadlock the single read loop that must deliver that Call's response.
		c.wg.Add(1)
		go func() { defer c.wg.Done(); c.handleRequest(ctx, msg) }()
	case msg.Method != "":
		// An inbound notification: dispatch, no reply.
		if c.handler != nil {
			c.wg.Add(1)
			go func() { defer c.wg.Done(); _, _ = c.handler(ctx, msg.Method, msg.Params, false) }()
		}
	default:
		// A frame with neither method nor id is malformed; ignore it (we cannot
		// even address an error response without an id).
	}
}

// handleRequest dispatches an inbound request to the Handler and writes back the
// result or a JSON-RPC error response.
func (c *Conn) handleRequest(ctx context.Context, msg *message) {
	if c.handler == nil {
		c.writeError(msg.ID, codeMethodNotFound, "no handler registered")
		return
	}
	result, err := c.handler(ctx, msg.Method, msg.Params, true)
	if err != nil {
		var me *MethodError
		if errors.As(err, &me) {
			c.writeError(msg.ID, me.Code, me.Message)
			return
		}
		c.writeError(msg.ID, codeInternalError, err.Error())
		return
	}
	c.writeResult(msg.ID, result)
}

// Call issues an outbound request and blocks until the correlated response
// arrives, ctx is cancelled, or the connection closes. It is how the adapter
// issues session/request_permission to the editor. The result is unmarshalled
// into out (when non-nil); a JSON-RPC error response is returned as *rpcError.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	id, ch, err := c.registerCall()
	if err != nil {
		return err
	}
	defer c.forget(id)

	if err := c.writeRequest(id, method, params); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return errors.New("acp: connection closed before response")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// Notify sends a fire-and-forget notification (no id, no reply). It is how the
// adapter pushes session/update notifications to the editor.
func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.writeFrame(&message{JSONRPC: jsonrpcVersion, Method: method, Params: raw})
}

// registerCall allocates a fresh request id and a response channel, recording
// the correlation so deliver can wake the waiting Call.
func (c *Conn) registerCall() (int64, chan *message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, errors.New("acp: connection closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *message, 1)
	c.pending[id] = ch
	return id, ch, nil
}

// forget removes a pending correlation (on Call return).
func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// deliver routes an inbound response frame to the Call waiting on its id.
func (c *Conn) deliver(msg *message) {
	id, err := strconv.ParseInt(strings.TrimSpace(string(msg.ID)), 10, 64)
	if err != nil {
		return // a response id we never issued (or non-numeric); drop it.
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if ok {
		ch <- msg
	}
}

// shutdown marks the connection closed and wakes every pending Call so they do
// not block forever once the read loop has exited.
func (c *Conn) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// --- framing -----------------------------------------------------------------

// readMessage reads one newline-delimited JSON-RPC message (ndjson), skipping
// bare blank lines between messages, tolerating a stray trailing '\r', and
// decoding the line. A clean EOF (empty final line) propagates io.EOF so Serve
// returns nil; a non-empty final line without a trailing '\n' (peer closed
// mid-stream) is still decoded as a complete message.
func (c *Conn) readMessage() (*message, error) {
	for {
		line, err := c.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// readLine returns the bytes read so far plus io.EOF when the stream
				// ends without a trailing newline. A non-empty final line is a complete
				// message the peer sent before closing; decode it. An empty line means a
				// clean EOF — propagate it so Serve returns nil.
				if line = trimFrame(line); len(line) == 0 {
					return nil, io.EOF
				}
				return decodeFrame(line)
			}
			return nil, err
		}
		if line = trimFrame(line); len(line) == 0 {
			continue // bare blank line between messages; skip it.
		}
		return decodeFrame(line)
	}
}

// readLine reads one '\n'-terminated line off c.r, bounding memory DURING the
// read: it accumulates the fixed-size fragments bufio.Reader.ReadSlice yields
// (each ≤ the bufio buffer, ~4 KiB) and rejects the line BEFORE the accumulator
// can exceed maxFrameBytes — so a peer streaming a huge line with no newline can
// never drive resident memory past ~maxFrameBytes (+ one bufio-buffer slack),
// restoring the bounded-by-construction guarantee the old length-prefixed reader
// had (CWE-789). The returned bytes include the trailing '\n' (trimFrame strips
// it). On a final line with no '\n', the accumulated bytes are returned with
// io.EOF.
func (c *Conn) readLine() ([]byte, error) {
	var buf []byte
	for {
		frag, err := c.r.ReadSlice('\n')
		if len(buf)+len(frag) > maxFrameBytes {
			return nil, fmt.Errorf("acp: frame too large: exceeds cap %d bytes", maxFrameBytes)
		}
		buf = append(buf, frag...) // append copies; the bufio-buffer aliasing is safe.
		switch {
		case err == nil:
			return buf, nil // found '\n' — line complete.
		case errors.Is(err, bufio.ErrBufferFull):
			continue // line longer than the bufio buffer; keep accumulating fragments.
		default:
			return buf, err // io.EOF (final line, maybe no '\n') or a read error.
		}
	}
}

// trimFrame strips the trailing '\n' and a defensive trailing '\r' (tolerating a
// CRLF peer), returning the bare JSON bytes.
func trimFrame(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// decodeFrame unmarshals one ndjson line into a message envelope.
func decodeFrame(line []byte) (*message, error) {
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("acp: decode frame: %w", err)
	}
	return &msg, nil
}

// writeFrame marshals msg as a single ndjson line: the JSON object followed by
// one '\n' terminator. The terminator is appended to the marshalled bytes BEFORE
// the write so the whole frame goes out in one c.w.Write under writeMu — atomic,
// with no torn body/newline and no interleave with a concurrent frame.
// json.Marshal never emits a raw newline, so the line never contains an embedded
// '\n' (per the ACP spec).
func (c *Conn) writeFrame(msg *message) error {
	msg.JSONRPC = jsonrpcVersion
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(body)
	return err
}

func (c *Conn) writeRequest(id int64, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.writeFrame(&message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(strconv.FormatInt(id, 10)),
		Method:  method,
		Params:  raw,
	})
}

func (c *Conn) writeResult(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		c.writeError(id, codeInternalError, err.Error())
		return
	}
	_ = c.writeFrame(&message{JSONRPC: jsonrpcVersion, ID: id, Result: raw})
}

func (c *Conn) writeError(id json.RawMessage, code int, msg string) {
	// A null id is still valid in an error response (e.g. parse error), so we send
	// the frame even when id is empty.
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	_ = c.writeFrame(&message{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// marshalParams marshals params to a json.RawMessage, treating nil as omitted.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
