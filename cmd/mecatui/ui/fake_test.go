package ui

import (
	"context"
	"io"
	"sync"

	"github.com/stacklok/mecatl/cmd/mecatui/client"
	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// fakeRecver replays a scripted slice of responses then returns io.EOF. It
// optionally gates at a named event type so a test can hold the stream at the
// permission.ask until it has driven the approval — then release the tail.
//
// It also exposes a DETERMINISTIC, output-independent progress signal the teatest
// cases use to sequence input without polling the rendered output: reachedGate
// closes the instant the gated event (the permission.ask) has been yielded to the
// production ReadLoop, i.e. the moment the ui will receive it.
//
// This fires on the reader goroutine (which keeps getting scheduled even under a
// CPU-starved -race run) and does NOT depend on Bubble Tea's 60fps flush ticker
// reaching teatest's output buffer — the ticker is what starves under `task test`'s
// parallel `go test -race ./...`, making a WaitFor(tm.Output()) deadline fire
// before any frame is flushed. Gating on this (plus the reducer phase observer, then
// asserting on FinalModel) makes the cases robust to that starvation without
// weakening them.
type fakeRecver struct {
	mu          sync.Mutex
	script      []*mecatlv1.ConverseResponse
	idx         int
	gateType    string
	gatedBefore bool
	gate        chan struct{}
	released    bool

	reachedGate chan struct{} // closed when the gated event has been yielded
	gateSignal  bool          // guards reachedGate's one-shot close
}

func (f *fakeRecver) Recv() (*mecatlv1.ConverseResponse, error) {
	f.mu.Lock()
	if f.idx >= len(f.script) {
		f.mu.Unlock()
		return nil, io.EOF
	}
	r := f.script[f.idx]
	f.idx++
	// Gate AFTER yielding the gated event: the ask is delivered, then the next
	// Recv blocks until the test approves, so the post-approval tail is held
	// back. (Gating before would swallow the ask itself.)
	gate := f.gateType != "" && f.gatedBefore
	if !gate && f.gateType != "" && r.GetEvent().GetType() == f.gateType {
		f.gatedBefore = true
		f.signalGateLocked()
	}
	f.mu.Unlock()
	if gate {
		<-f.gate
	}
	return r, nil
}

// signalGateLocked closes reachedGate once (the gated event has been yielded to
// the ReadLoop). Caller holds f.mu.
func (f *fakeRecver) signalGateLocked() {
	if !f.gateSignal && f.reachedGate != nil {
		f.gateSignal = true
		close(f.reachedGate)
	}
}

func (f *fakeRecver) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.released {
		f.released = true
		close(f.gate)
	}
}

// fakeSender records sent frames so a test can assert the ResumeApproval round
// trip, and releases the recv gate on approval so the post-approval tail flows.
type fakeSender struct {
	mu     sync.Mutex
	sent   []*mecatlv1.ConverseRequest
	onSend func(*mecatlv1.ConverseRequest)
}

func (f *fakeSender) Send(req *mecatlv1.ConverseRequest) error {
	f.mu.Lock()
	f.sent = append(f.sent, req)
	cb := f.onSend
	f.mu.Unlock()
	if cb != nil {
		cb(req)
	}
	return nil
}

func (f *fakeSender) frames() []*mecatlv1.ConverseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*mecatlv1.ConverseRequest, len(f.sent))
	copy(out, f.sent)
	return out
}

// fakeConv is the ui's Converser+SessionCreator backed by a fakeRecver/Sender. It
// builds a real client.Stream so the test exercises the production ReadLoop,
// EventToMsg, and send path — the only thing faked is the transport.
type fakeConv struct {
	recv *fakeRecver
	send *fakeSender
	caps client.Capabilities // capabilities returned from CreateSession (zero = all-false)

	// sessionReady closes when CreateSession has returned the id to the ui command
	// goroutine — a deterministic, output-independent signal that the SessionReadyMsg
	// is on its way to the reducer (so a follow-up prompt won't be dropped by the
	// sessionID == "" guard in submitPrompt). See fakeRecver's doc for why the
	// teatest cases sequence on signals like this rather than on rendered output.
	sessionReady chan struct{}
}

func (c *fakeConv) CreateSession(_ context.Context) (string, client.Capabilities, error) {
	if c.sessionReady != nil {
		select {
		case <-c.sessionReady:
		default:
			close(c.sessionReady)
		}
	}
	return "sess-test-0001", c.caps, nil
}

func (c *fakeConv) OpenConverse(_ context.Context) (*client.Stream, error) {
	return client.NewStream(c.recv, c.send), nil
}

// fakeMCP is a scripted client.MCP for the overlay tests: each method returns its
// canned data or a canned error. err, when set, is returned by every call so the
// overlay's classified-error rendering can be exercised. It implements client.MCP
// so the ui's MCP commands run with no proto and no network.
type fakeMCP struct {
	resources []client.MCPResource
	contents  []client.MCPResourceContents
	prompts   []client.MCPPrompt
	promptDsc string
	promptMsg []client.MCPPromptMessage
	sources   []client.MCPSource
	groups    []string

	err error // when non-nil, every RPC returns it (already a gRPC status)

	getPromptCalls int // how many times GetMCPPrompt was invoked (validation guard)

	// nextSources, when non-nil, is returned by the SECOND (and later)
	// ListMCPSources call — modelling a server whose live MCP status changed since
	// the first fetch, so a panel refresh can be asserted to pick it up. Likewise
	// nextGroups for ListToolHiveGroups. sourcesCalls counts ListMCPSources calls.
	nextSources  []client.MCPSource
	nextGroups   []string
	sourcesCalls int
}

func (f *fakeMCP) ListMCPResources(_ context.Context, _ string) ([]client.MCPResource, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resources, nil
}

func (f *fakeMCP) ReadMCPResource(_ context.Context, _, _ string) ([]client.MCPResourceContents, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.contents, nil
}

func (f *fakeMCP) ListMCPPrompts(_ context.Context, _ string) ([]client.MCPPrompt, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.prompts, nil
}

func (f *fakeMCP) GetMCPPrompt(_ context.Context, _, _ string, _ map[string]string) (string, []client.MCPPromptMessage, error) {
	f.getPromptCalls++
	if f.err != nil {
		return "", nil, f.err
	}
	return f.promptDsc, f.promptMsg, nil
}

func (f *fakeMCP) ListMCPSources(_ context.Context) ([]client.MCPSource, error) {
	f.sourcesCalls++
	if f.err != nil {
		return nil, f.err
	}
	if f.sourcesCalls > 1 && f.nextSources != nil {
		return f.nextSources, nil
	}
	return f.sources, nil
}

func (f *fakeMCP) ListToolHiveGroups(_ context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.sourcesCalls > 1 && f.nextGroups != nil {
		return f.nextGroups, nil
	}
	return f.groups, nil
}
