package client

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// fakeSkillsClient is a scripted HarnessServiceClient for the ListSkills wrapper
// tests. It embeds the interface and overrides only the one RPC under test, so
// the proto→plain mapping runs offline.
type fakeSkillsClient struct {
	mecatlv1.HarnessServiceClient

	resp *mecatlv1.ListSkillsResponse
	err  error

	lastReq *mecatlv1.ListSkillsRequest
}

func (f *fakeSkillsClient) ListSkills(_ context.Context, in *mecatlv1.ListSkillsRequest, _ ...grpc.CallOption) (*mecatlv1.ListSkillsResponse, error) {
	f.lastReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestListSkillsMapping(t *testing.T) {
	fake := &fakeSkillsClient{resp: &mecatlv1.ListSkillsResponse{Skills: []*mecatlv1.SkillInfo{
		{Name: "code-review", Description: "review a diff"},
		{Name: "deep-research", Description: "fan-out web research"},
	}}}
	cl := newFakeClient(fake)

	skills, err := cl.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if fake.lastReq == nil {
		t.Fatal("request not sent")
	}
	if len(skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(skills))
	}
	if skills[0].Name != "code-review" || skills[0].Description != "review a diff" {
		t.Fatalf("skills[0] = %+v", skills[0])
	}
	if skills[1].Name != "deep-research" {
		t.Fatalf("skills[1] = %+v", skills[1])
	}
}

func TestListSkillsMappingNilSafe(t *testing.T) {
	fake := &fakeSkillsClient{resp: &mecatlv1.ListSkillsResponse{}}
	cl := newFakeClient(fake)

	skills, err := cl.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("skills = %d, want 0 (empty response)", len(skills))
	}
}

func TestListSkillsCmdSuccess(t *testing.T) {
	fake := &fakeSkillsClient{resp: &mecatlv1.ListSkillsResponse{Skills: []*mecatlv1.SkillInfo{
		{Name: "code-review", Description: "review a diff"},
	}}}
	cl := newFakeClient(fake)

	msg := ListSkillsCmd(context.Background(), cl)()
	sm, ok := msg.(SkillsMsg)
	if !ok {
		t.Fatalf("msg type = %T, want SkillsMsg", msg)
	}
	if sm.Err != nil {
		t.Fatalf("unexpected err: %v", sm.Err)
	}
	if len(sm.Skills) != 1 || sm.Skills[0].Name != "code-review" {
		t.Fatalf("skills = %+v", sm.Skills)
	}
}

func TestListSkillsCmdError(t *testing.T) {
	fake := &fakeSkillsClient{err: errors.New("boom")}
	cl := newFakeClient(fake)

	msg := ListSkillsCmd(context.Background(), cl)()
	sm, ok := msg.(SkillsMsg)
	if !ok {
		t.Fatalf("msg type = %T, want SkillsMsg", msg)
	}
	if sm.Err == nil {
		t.Fatalf("expected an error in SkillsMsg")
	}
}
