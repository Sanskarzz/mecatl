package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
	"github.com/stacklok/mecatl/internal/adapter/mockllm"
	"github.com/stacklok/mecatl/internal/adapter/server"
	"github.com/stacklok/mecatl/internal/port"
	"github.com/stacklok/mecatl/internal/session"
)

// drainRun consumes a run's events to completion so the session reaches a
// terminal state and the store holds the final snapshot.
func drainRun(t *testing.T, run interface{ Events() <-chan session.Event }) {
	t.Helper()
	for range run.Events() {
	}
}

func TestStartRunContentRecordsMultimodal(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("I see it"))
	svc := newService(t, llm, allowRules())

	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{MaxTurns: 2})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	parts := []session.Content{
		{Kind: session.MediaImage, MIMEType: "image/png", Data: []byte{0x89, 0x50}},
	}
	run, err := svc.StartRunContent(context.Background(), sess.ID, "what is this", parts)
	if err != nil {
		t.Fatalf("StartRunContent: %v", err)
	}
	drainRun(t, run)
	// Persist while the run is still registered so the store holds the final
	// in-memory session the engine mutated; then deregister.
	svc.Persist(context.Background(), sess.ID)
	svc.FinishRun(sess.ID, run)

	got, err := svc.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// Find the recorded user message and assert it carries the media part.
	var found bool
	for _, m := range got.Conversation.Messages {
		if m.Role == session.RoleUser && len(m.Parts) == 1 && m.Parts[0].Kind == session.MediaImage {
			found = true
			if m.Text != "what is this" {
				t.Fatalf("user text = %q, want preserved", m.Text)
			}
		}
	}
	if !found {
		t.Fatalf("no multimodal user message recorded: %+v", got.Conversation.Messages)
	}
}

func TestStartRunContentEmptyRejected(t *testing.T) {
	svc := newService(t, mockllm.New(), allowRules())
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err = svc.StartRunContent(context.Background(), sess.ID, "", nil)
	if !errors.Is(err, server.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestStartRunDelegatesToContent(t *testing.T) {
	llm := mockllm.New(mockllm.TextTurn("ok"))
	svc := newService(t, llm, allowRules())
	sess, err := svc.CreateSession(context.Background(), "/ws", session.ModeDefault, session.Limits{MaxTurns: 2})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// StartRun with empty text must still reject (delegation preserves the guard).
	if _, err := svc.StartRun(context.Background(), sess.ID, ""); !errors.Is(err, server.ErrInvalidArgument) {
		t.Fatalf("StartRun(empty) err = %v, want ErrInvalidArgument", err)
	}
	// A normal text StartRun records a text-only user message (no parts).
	run, err := svc.StartRun(context.Background(), sess.ID, "hello")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	drainRun(t, run)
	svc.Persist(context.Background(), sess.ID)
	svc.FinishRun(sess.ID, run)

	got, err := svc.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	for _, m := range got.Conversation.Messages {
		if m.Role == session.RoleUser && m.Parts != nil {
			t.Fatalf("text StartRun recorded parts: %+v", m.Parts)
		}
	}
}

func TestProviderCapabilitiesSurface(t *testing.T) {
	want := port.ProviderCapabilities{Image: true, EmbeddedContext: true}
	llm := mockllm.NewWith([]mockllm.Option{mockllm.WithCapabilities(want)})
	svc := newService(t, llm, allowRules())
	if got := svc.ProviderCapabilities(); !got.Image || got.Audio {
		t.Fatalf("ProviderCapabilities() = %+v, want image-only", got)
	}
	_ = time.Now
}

// TestGRPCConverseRejectsBadPart asserts the gRPC Converse wire path rejects a
// malformed Content part (KIND_UNSPECIFIED) with InvalidArgument before starting
// a run.
func TestGRPCConverseRejectsBadPart(t *testing.T) {
	svc := newService(t, mockllm.New(mockllm.TextTurn("x")), allowRules())
	client, cleanup := dialGRPC(t, svc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := client.CreateSession(ctx, &mecatlv1.CreateSessionRequest{Workspace: "/ws"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	stream, err := client.Converse(ctx)
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if serr := stream.Send(&mecatlv1.ConverseRequest{Kind: &mecatlv1.ConverseRequest_Prompt{
		Prompt: &mecatlv1.Prompt{
			SessionId: cs.GetSessionId(),
			Text:      "look",
			Parts:     []*mecatlv1.Content{{Kind: mecatlv1.Content_KIND_UNSPECIFIED, MimeType: "image/png", Data: []byte{1}}},
		},
	}}); serr != nil {
		t.Fatalf("Send: %v", serr)
	}
	_, rerr := stream.Recv()
	if status.Code(rerr) != codes.InvalidArgument {
		t.Fatalf("recv code = %v, want InvalidArgument (err=%v)", status.Code(rerr), rerr)
	}
}
