package grpcdriver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	driverv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/driver/v1"
	"github.com/stacklok/mecatl/engine/tool"
	"github.com/stacklok/mecatl/internal/adapter/skills"
	"github.com/stacklok/mecatl/internal/adapter/toolkit"
)

// SkillSource is a tool.SkillSource over a remote SkillSourceService driver.
// It is translation plus a DEFENSIVE normalization layer on ListSkills (the
// driver sits at the operator-infrastructure trust tier, but its metadata
// feeds the always-in-context tool description, so the client re-enforces the
// invariants the port promises rather than trusting the wire): blank-name
// skills are dropped, duplicate names de-dup first-wins, the result is
// name-sorted, descriptions are forced single-line (control characters →
// spaces) then re-truncated to the always-in-context cap
// (skills.MaxDescriptionBytes), and Origin is stamped SkillOriginDriver
// UNCONDITIONALLY — a driver-served skill IS driver tier; a driver must not
// claim the "project"/"user" admission labels (the wire origin field stays
// driver-side observability only). Bodies/assets pass through; the
// harness-side caps on materialized assets live in skills.AssetMaterializer.
type SkillSource struct {
	client driverv1.SkillSourceServiceClient
}

// compile-time assertion that SkillSource satisfies the port.
var _ tool.SkillSource = (*SkillSource)(nil)

// NewSkillSource wraps an established driver connection (see Dial) as a
// tool.SkillSource.
func NewSkillSource(conn grpc.ClientConnInterface) *SkillSource {
	return &SkillSource{client: driverv1.NewSkillSourceServiceClient(conn)}
}

// ListSkills returns the driver's skill metadata snapshot, defensively
// normalized (see the type doc).
func (s *SkillSource) ListSkills(ctx context.Context) ([]tool.SkillMeta, error) {
	resp, err := s.client.ListSkills(ctx, &driverv1.ListSkillsRequest{})
	if err != nil {
		return nil, rpcErr(ctx, "list skills", err)
	}
	wire := resp.GetSkills()
	out := make([]tool.SkillMeta, 0, len(wire))
	seen := make(map[string]bool, len(wire))
	for _, m := range wire {
		name := m.GetName()
		if strings.TrimSpace(name) == "" {
			continue // drop blank-name skills
		}
		if seen[name] {
			continue // de-dup first-wins (wire order)
		}
		seen[name] = true
		out = append(out, tool.SkillMeta{
			Name:        name,
			Description: toolkit.TruncateRunes(singleLine(m.GetDescription()), skills.MaxDescriptionBytes),
			// UNCONDITIONAL: every skill listed by a driver is driver tier —
			// the wire's origin label is never adopted (a driver claiming
			// "project"/"user" would launder itself into a trusted-looking tier).
			Origin:    tool.SkillOriginDriver,
			HasAssets: m.GetHasAssets(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SkillBody returns the named skill's full instruction body. A driver
// NOT_FOUND wraps tool.ErrSkillNotFound with the name in the message.
func (s *SkillSource) SkillBody(ctx context.Context, name string) (string, error) {
	resp, err := s.client.GetSkillBody(ctx, &driverv1.GetSkillBodyRequest{Name: name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", fmt.Errorf("%w: %q", tool.ErrSkillNotFound, name)
		}
		return "", rpcErr(ctx, "get skill body", err)
	}
	return resp.GetBody(), nil
}

// ListSkillAssets returns the named skill's payload descriptors. A driver
// NOT_FOUND wraps tool.ErrSkillNotFound.
func (s *SkillSource) ListSkillAssets(ctx context.Context, name string) ([]tool.SkillAsset, error) {
	resp, err := s.client.ListSkillAssets(ctx, &driverv1.ListSkillAssetsRequest{Name: name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: %q", tool.ErrSkillNotFound, name)
		}
		return nil, rpcErr(ctx, "list skill assets", err)
	}
	wire := resp.GetAssets()
	out := make([]tool.SkillAsset, 0, len(wire))
	for _, a := range wire {
		out = append(out, tool.SkillAsset{Name: a.GetName(), Size: a.GetSize(), Executable: a.GetExecutable()})
	}
	return out, nil
}

// ReadSkillAsset returns one payload's bytes. A driver NOT_FOUND (unknown
// skill OR asset) wraps tool.ErrSkillAssetNotFound; an INVALID_ARGUMENT (the
// server pre-validates logical names via tool.ValidSkillAssetName) surfaces
// as a non-nil infrastructure error — never content.
func (s *SkillSource) ReadSkillAsset(ctx context.Context, skill, asset string) ([]byte, error) {
	resp, err := s.client.ReadSkillAsset(ctx, &driverv1.ReadSkillAssetRequest{Skill: skill, Asset: asset})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: %q/%q", tool.ErrSkillAssetNotFound, skill, asset)
		}
		return nil, rpcErr(ctx, "read skill asset", err)
	}
	return resp.GetData(), nil
}

// singleLine enforces the port's single-line Description promise on wire
// metadata BEFORE the byte-cap truncation: every control character (newlines,
// carriage returns, tabs, DEL, ...) becomes a space, so a multi-line driver
// description cannot smuggle line structure into the always-in-context Skill
// tool description.
func singleLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
