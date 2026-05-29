package server

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ozzv1 "github.com/stacklok/ozzharness/contracts/gen/go/ozz/v1"
	"github.com/stacklok/ozzharness/internal/agent"
	"github.com/stacklok/ozzharness/internal/session"
)

// HarnessServer implements the generated ozzv1.HarnessServiceServer over the
// shared Service. It is the primary (gRPC) surface; the HTTP/SSE adapter wraps
// the same Service.
type HarnessServer struct {
	ozzv1.UnimplementedHarnessServiceServer
	svc *Service
}

// NewHarnessServer constructs a HarnessServer over svc.
func NewHarnessServer(svc *Service) *HarnessServer {
	return &HarnessServer{svc: svc}
}

// compile-time assertion that HarnessServer satisfies the generated interface.
var _ ozzv1.HarnessServiceServer = (*HarnessServer)(nil)

// CreateSession allocates a new session and returns its id.
func (h *HarnessServer) CreateSession(ctx context.Context, req *ozzv1.CreateSessionRequest) (*ozzv1.CreateSessionResponse, error) {
	if req.GetWorkspace() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace is required")
	}
	sess, err := h.svc.CreateSession(ctx, req.GetWorkspace(), modeFromProto(req.GetMode()), limitsFromProto(req.GetLimits()))
	if err != nil {
		return nil, toStatus(err)
	}
	return &ozzv1.CreateSessionResponse{SessionId: string(sess.ID)}, nil
}

// GetSession returns a snapshot of the requested session.
func (h *HarnessServer) GetSession(ctx context.Context, req *ozzv1.GetSessionRequest) (*ozzv1.GetSessionResponse, error) {
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	sess, err := h.svc.GetSession(ctx, session.SessionID(req.GetSessionId()))
	if err != nil {
		return nil, toStatus(err)
	}
	return &ozzv1.GetSessionResponse{Session: toProtoSession(sess)}, nil
}

// Converse drives one run over a bidi stream. The first frame MUST be a Prompt;
// the server then relays the run's Events while concurrently reading
// ResumeApproval / Cancel control frames, until the events channel closes (the
// terminal result was delivered) or the stream context is cancelled.
func (h *HarnessServer) Converse(stream ozzv1.HarnessService_ConverseServer) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "converse: stream closed before a prompt frame")
		}
		return err
	}
	prompt := first.GetPrompt()
	if prompt == nil {
		return status.Error(codes.InvalidArgument, "converse: first frame must be a prompt")
	}
	if prompt.GetSessionId() == "" || prompt.GetText() == "" {
		return status.Error(codes.InvalidArgument, "converse: prompt session_id and text are required")
	}

	id := session.SessionID(prompt.GetSessionId())
	run, err := h.svc.StartRun(ctx, id, prompt.GetText())
	if err != nil {
		return toStatus(err)
	}
	defer h.svc.deregister(id, run)

	// Read subsequent control frames concurrently so an approval/cancel can be
	// delivered while events are still streaming. The reader exits on stream
	// EOF (client closed its send half) or context cancellation.
	go h.readControl(ctx, stream, run)

	// Relay events on this goroutine; the channel closes when the run ends.
	for ev := range run.Events() {
		if err := stream.Send(&ozzv1.ConverseResponse{Event: toProto(ev)}); err != nil {
			run.Cancel()
			return err
		}
	}
	return nil
}

// readControl reads ResumeApproval / Cancel frames until the client closes its
// send half or the context is cancelled, dispatching each onto run.
func (*HarnessServer) readControl(ctx context.Context, stream ozzv1.HarnessService_ConverseServer, run *agent.Run) {
	for {
		if ctx.Err() != nil {
			return
		}
		frame, err := stream.Recv()
		if err != nil {
			// EOF means "no more inputs"; any other error means the stream is
			// gone. Either way stop reading; the relay loop owns termination.
			return
		}
		switch k := frame.GetKind().(type) {
		case *ozzv1.ConverseRequest_ResumeApproval:
			if k.ResumeApproval != nil {
				run.Approve(k.ResumeApproval.GetAskId(), k.ResumeApproval.GetAllow())
			}
		case *ozzv1.ConverseRequest_Cancel:
			run.Cancel()
		default:
			// A second Prompt or an unknown frame is ignored: the run is
			// already driving and a new prompt cannot start a second run here.
		}
	}
}

// toStatus maps service sentinel errors to gRPC status codes.
func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
