package client

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// fakeSoulClient is a scripted HarnessServiceClient for the GetSoul wrapper tests.
// It embeds the interface and overrides only GetSoul, so the proto→plain mapping
// runs offline.
type fakeSoulClient struct {
	mecatlv1.HarnessServiceClient

	resp    *mecatlv1.GetSoulResponse
	err     error
	lastReq *mecatlv1.GetSoulRequest
}

func (f *fakeSoulClient) GetSoul(_ context.Context, in *mecatlv1.GetSoulRequest, _ ...grpc.CallOption) (*mecatlv1.GetSoulResponse, error) {
	f.lastReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestGetSoulMapping(t *testing.T) {
	fake := &fakeSoulClient{resp: &mecatlv1.GetSoulResponse{Soul: &mecatlv1.SoulInfo{
		Content:    "You are terse.",
		SizeBytes:  14,
		Sha256:     "deadbeef",
		Present:    true,
		Provenance: mecatlv1.SoulProvenance_SOUL_PROVENANCE_PROJECT,
		Trusted:    true,
		Drifted:    true,
	}}}
	cl := newFakeClient(fake)

	soul, err := cl.GetSoul(context.Background())
	if err != nil {
		t.Fatalf("GetSoul: %v", err)
	}
	if fake.lastReq == nil {
		t.Fatal("request not sent")
	}
	if soul.Content != "You are terse." || soul.SizeBytes != 14 || soul.SHA256 != "deadbeef" {
		t.Fatalf("soul = %+v", soul)
	}
	if !soul.Present || !soul.Trusted || !soul.Drifted {
		t.Fatalf("flags = %+v", soul)
	}
	if soul.Provenance != SoulProvenanceProject {
		t.Fatalf("provenance = %v, want project", soul.Provenance)
	}
}

func TestGetSoulMappingNilSafe(t *testing.T) {
	fake := &fakeSoulClient{resp: &mecatlv1.GetSoulResponse{}}
	cl := newFakeClient(fake)

	soul, err := cl.GetSoul(context.Background())
	if err != nil {
		t.Fatalf("GetSoul: %v", err)
	}
	if soul.Present || soul.Content != "" || soul.Provenance != SoulProvenanceNone {
		t.Fatalf("nil soul should map to a zero-value struct, got %+v", soul)
	}
}

func TestGetSoulError(t *testing.T) {
	fake := &fakeSoulClient{err: errors.New("boom")}
	cl := newFakeClient(fake)

	if _, err := cl.GetSoul(context.Background()); err == nil {
		t.Fatal("GetSoul should propagate the RPC error")
	}
}
