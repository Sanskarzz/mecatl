package client

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// fakeUserModelClient is a scripted HarnessServiceClient for the GetUserModel
// wrapper tests. It embeds the interface and overrides only GetUserModel.
type fakeUserModelClient struct {
	mecatlv1.HarnessServiceClient

	resp    *mecatlv1.GetUserModelResponse
	err     error
	lastReq *mecatlv1.GetUserModelRequest
}

func (f *fakeUserModelClient) GetUserModel(_ context.Context, in *mecatlv1.GetUserModelRequest, _ ...grpc.CallOption) (*mecatlv1.GetUserModelResponse, error) {
	f.lastReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestGetUserModelMapping(t *testing.T) {
	fake := &fakeUserModelClient{resp: &mecatlv1.GetUserModelResponse{
		Entries: []*mecatlv1.UserModelEntry{
			{Key: "name", Description: "the operator's name"},
			{Key: "stack", Description: "prefers Go"},
		},
		SizeBytes: 42,
		Sha256:    "cafef00d",
	}}
	cl := newFakeClient(fake)

	um, err := cl.GetUserModel(context.Background())
	if err != nil {
		t.Fatalf("GetUserModel: %v", err)
	}
	if fake.lastReq == nil {
		t.Fatal("request not sent")
	}
	if len(um.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(um.Entries))
	}
	if um.Entries[0].Key != "name" || um.Entries[0].Description != "the operator's name" {
		t.Fatalf("entry[0] = %+v", um.Entries[0])
	}
	if um.SizeBytes != 42 || um.SHA256 != "cafef00d" {
		t.Fatalf("aggregate = size:%d sha:%q", um.SizeBytes, um.SHA256)
	}
}

func TestGetUserModelMappingNilSafe(t *testing.T) {
	fake := &fakeUserModelClient{resp: &mecatlv1.GetUserModelResponse{}}
	cl := newFakeClient(fake)

	um, err := cl.GetUserModel(context.Background())
	if err != nil {
		t.Fatalf("GetUserModel: %v", err)
	}
	if len(um.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(um.Entries))
	}
}

func TestGetUserModelError(t *testing.T) {
	fake := &fakeUserModelClient{err: errors.New("boom")}
	cl := newFakeClient(fake)

	if _, err := cl.GetUserModel(context.Background()); err == nil {
		t.Fatal("GetUserModel should propagate the RPC error")
	}
}
