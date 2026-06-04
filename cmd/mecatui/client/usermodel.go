package client

import (
	"context"

	tea "charm.land/bubbletea/v2"

	mecatlv1 "github.com/stacklok/mecatl/contracts/gen/go/mecatl/v1"
)

// The user-model inspection surface: plain client-owned structs mirroring the
// proto UserModelEntry + GetUserModelResponse, the unary RPC wrapper that maps
// proto → the structs, and the tea.Cmd constructor the ui's /usermodel panel
// calls. As with the soul/skills surfaces, NO proto type leaks past this file.

// UserModelEntry is one user-model fact's listing metadata (proto UserModelEntry,
// proto-free): its key + one-line description. The value is omitted — discovery is
// metadata only.
type UserModelEntry struct {
	Key         string
	Description string
}

// UserModel is the user-model index snapshot: the current entries plus aggregate
// size + hash, so the panel can show an at-a-glance footprint.
type UserModel struct {
	Entries   []UserModelEntry
	SizeBytes int64
	SHA256    string
}

// UserModelMsg carries a GetUserModel result for the /usermodel panel. Err is set
// on failure; the panel surfaces it rather than silently degrading.
type UserModelMsg struct {
	UserModel UserModel
	Err       error
}

// GetUserModel fetches the current user-model index (a LIVE read server-side).
func (c *Client) GetUserModel(ctx context.Context) (UserModel, error) {
	resp, err := c.svc.GetUserModel(ctx, &mecatlv1.GetUserModelRequest{})
	if err != nil {
		return UserModel{}, err
	}
	return mapUserModel(resp), nil
}

// mapUserModel maps a proto GetUserModelResponse (nil-safe) to the plain struct.
func mapUserModel(in *mecatlv1.GetUserModelResponse) UserModel {
	if in == nil {
		return UserModel{}
	}
	entries := in.GetEntries()
	out := make([]UserModelEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, UserModelEntry{Key: e.GetKey(), Description: e.GetDescription()})
	}
	return UserModel{
		Entries:   out,
		SizeBytes: in.GetSizeBytes(),
		SHA256:    in.GetSha256(),
	}
}

// UserModelLister is the subset of *Client the ui's /usermodel panel needs.
// Splitting it out keeps the ui injectable with a fake for offline tests; *Client
// satisfies it.
type UserModelLister interface {
	GetUserModel(ctx context.Context) (UserModel, error)
}

// GetUserModelCmd fetches the user-model index off the update goroutine; the
// result (success or error) arrives as a UserModelMsg.
func GetUserModelCmd(ctx context.Context, c UserModelLister) tea.Cmd {
	return func() tea.Msg {
		um, err := c.GetUserModel(ctx)
		if err != nil {
			return UserModelMsg{Err: err}
		}
		return UserModelMsg{UserModel: um}
	}
}
