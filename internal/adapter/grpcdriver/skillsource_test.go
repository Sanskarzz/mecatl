package grpcdriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/adapter/sourceconformance"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/skills"
)

// hostileSkillServer is a RAW driverv1 server (NOT the harness wrapper) so the
// client's defensive normalization can be fed wire data a well-behaved Go
// source could never produce: blank names, duplicates, unsorted order,
// oversized descriptions, unknown origin labels.
type hostileSkillServer struct {
	driverv1.UnimplementedSkillSourceServiceServer
	skills []*driverv1.SkillMeta
}

func (s *hostileSkillServer) ListSkills(context.Context, *driverv1.ListSkillsRequest) (*driverv1.ListSkillsResponse, error) {
	return &driverv1.ListSkillsResponse{Skills: s.skills}, nil
}

func newHostileSkillClient(t *testing.T, metas []*driverv1.SkillMeta) *SkillSource {
	t.Helper()
	conn := dialBufconn(t, func(gs *grpc.Server) {
		driverv1.RegisterSkillSourceServiceServer(gs, &hostileSkillServer{skills: metas})
	})
	return NewSkillSource(conn)
}

// TestSkillSourceListDefensiveDiscipline pins the §H client discipline: drop
// blank names, de-dup first-wins, sort by name, force descriptions
// single-line then truncate to the always-in-context cap, and stamp Origin
// SkillOriginDriver UNCONDITIONALLY (a driver must not claim project/user
// tier labels).
func TestSkillSourceListDefensiveDiscipline(t *testing.T) {
	long := strings.Repeat("d", skills.MaxDescriptionBytes*2)
	src := newHostileSkillClient(t, []*driverv1.SkillMeta{
		{Name: "zeta", Description: "claims a trusted tier", Origin: "user"},
		{Name: "", Description: "blank name must be dropped"},
		{Name: "   ", Description: "whitespace name must be dropped"},
		{Name: "dup", Description: "first wins", Origin: "registry-of-doom"},
		{Name: "dup", Description: "second loses"},
		{Name: "alpha", Description: long, Origin: ""},
		{Name: "multiline", Description: "line one\nline two\r\n\tline three\x7f!", Origin: "project"},
	})
	got, err := src.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("ListSkills kept %d skills, want 4 (blank dropped, dup de-duped): %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "dup" || got[2].Name != "multiline" || got[3].Name != "zeta" {
		t.Errorf("not name-sorted: %q, %q, %q, %q", got[0].Name, got[1].Name, got[2].Name, got[3].Name)
	}
	if len(got[0].Description) > skills.MaxDescriptionBytes {
		t.Errorf("description not truncated to the cap: %d > %d", len(got[0].Description), skills.MaxDescriptionBytes)
	}
	if got[1].Description != "first wins" {
		t.Errorf("de-dup kept %q, want the FIRST wire entry", got[1].Description)
	}
	// Single-line promise: every control char (newline/CR/tab/DEL) becomes a
	// space BEFORE the byte-cap truncation, so no line structure survives into
	// the always-in-context tool description.
	if strings.ContainsAny(got[2].Description, "\n\r\t\x7f") {
		t.Errorf("multi-line description leaked control characters: %q", got[2].Description)
	}
	if want := "line one line two   line three !"; got[2].Description != want {
		t.Errorf("single-line normalization = %q, want %q", got[2].Description, want)
	}
	// Origin is stamped driver UNCONDITIONALLY — even "user"/"project" claims.
	for _, m := range got {
		if m.Origin != tool.SkillOriginDriver {
			t.Errorf("skill %q Origin = %q, want driver (a driver-listed skill is ALWAYS driver tier)", m.Name, m.Origin)
		}
	}
}

// newFixtureSkillClient wires the client to the REAL server wrapper over the
// canonical fixture, for the sentinel/ctx tests.
func newFixtureSkillClient(t *testing.T) *SkillSource {
	t.Helper()
	conn := dialBufconn(t, func(gs *grpc.Server) {
		driverv1.RegisterSkillSourceServiceServer(gs, NewSkillSourceServer(sourceconformance.NewFixtureSource()))
	})
	return NewSkillSource(conn)
}

// TestSkillSourceSentinelMapping pins the §H NOT_FOUND rows: unknown skill →
// ErrSkillNotFound (body/assets, name in message), unknown skill or asset →
// ErrSkillAssetNotFound (read).
func TestSkillSourceSentinelMapping(t *testing.T) {
	src := newFixtureSkillClient(t)
	ctx := context.Background()

	_, err := src.SkillBody(ctx, "ghost")
	if !errors.Is(err, tool.ErrSkillNotFound) {
		t.Errorf("SkillBody(unknown) = %v, want ErrSkillNotFound", err)
	}
	if err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("SkillBody(unknown) error must carry the name, got %v", err)
	}
	if _, err := src.ListSkillAssets(ctx, "ghost"); !errors.Is(err, tool.ErrSkillNotFound) {
		t.Errorf("ListSkillAssets(unknown) = %v, want ErrSkillNotFound", err)
	}
	if _, err := src.ReadSkillAsset(ctx, "ghost", "x.md"); !errors.Is(err, tool.ErrSkillAssetNotFound) {
		t.Errorf("ReadSkillAsset(unknown skill) = %v, want ErrSkillAssetNotFound", err)
	}
	if _, err := src.ReadSkillAsset(ctx, "review", "no-such.md"); !errors.Is(err, tool.ErrSkillAssetNotFound) {
		t.Errorf("ReadSkillAsset(unknown asset) = %v, want ErrSkillAssetNotFound", err)
	}
	// Invalid logical name: a non-nil non-sentinel error carrying the server's
	// INVALID_ARGUMENT (the wrapper pre-validates via ValidSkillAssetName),
	// never content. The explicit code assertion guards against a regression
	// to Internal/Unknown silently passing as "some error".
	data, err := src.ReadSkillAsset(ctx, "review", "../escape")
	if err == nil || len(data) != 0 {
		t.Errorf("ReadSkillAsset(invalid name) = %q, %v; want an error and no content", data, err)
	}
	if errors.Is(err, tool.ErrSkillAssetNotFound) {
		t.Errorf("invalid name mapped to the not-found sentinel %v; want a distinct infra error", err)
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("ReadSkillAsset(invalid name) status code = %v, want InvalidArgument (server pre-validation)", got)
	}
}

// TestSkillSourceCtxRewrap pins the §H context row: a caller-cancelled ctx
// surfaces so errors.Is(err, context.Canceled) holds harness-side.
func TestSkillSourceCtxRewrap(t *testing.T) {
	src := newFixtureSkillClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.ListSkills(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ListSkills(cancelled ctx) = %v, want errors.Is(_, context.Canceled)", err)
	}
	if _, err := src.SkillBody(ctx, "review"); !errors.Is(err, context.Canceled) {
		t.Errorf("SkillBody(cancelled ctx) = %v, want errors.Is(_, context.Canceled)", err)
	}
}
