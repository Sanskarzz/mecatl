package server

import (
	"errors"
	"testing"

	"github.com/stacklok/mecatl/internal/agent"
	"github.com/stacklok/mecatl/internal/team"
)

// TestClassifyAddMemberErr pins the AddMember-failure → wire-sentinel mapping
// (finding J): each failure class maps to the sentinel whose gRPC/HTTP status fits,
// instead of collapsing everything to InvalidArgument. It is an internal-package
// test because classifyAddMemberErr is unexported.
func TestClassifyAddMemberErr(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		// Bad client request → InvalidArgument.
		{"name required", agent.ErrMemberNameRequired, ErrInvalidArgument},
		{"already added", agent.ErrMemberAlreadyAdded, ErrInvalidArgument},
		{"member exists", team.ErrMemberExists, ErrInvalidArgument},
		{"reserved name", team.ErrReservedName, ErrInvalidArgument},
		{"too many members", team.ErrTooManyMembers, ErrInvalidArgument},
		// Server misconfiguration (well-formed request, harness wired wrong) →
		// FailedPrecondition. The read-only-shell-no-forker mis-wire is the new entry
		// the read-only-shell work added.
		{"no forker", agent.ErrNoForker, ErrFailedPrecondition},
		{"read-only shell no forker", agent.ErrReadOnlyShellNoForker, ErrFailedPrecondition},
		{"read-only member mutating", agent.ErrReadOnlyMemberMutating, ErrFailedPrecondition},
		// Server-internal fault → Internal.
		{"fork workspace", agent.ErrForkWorkspace, ErrInternal},
		{"nil engine", agent.ErrNilEngine, ErrInternal},
		// Unknown failure class falls back to InvalidArgument.
		{"unknown", errors.New("some novel failure"), ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAddMemberErr(tc.in)
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyAddMemberErr(%v) = %v, want wrapped %v", tc.in, got, tc.want)
			}
			// The original detail must be preserved (wrapped with %v).
			if !errors.Is(got, tc.want) || got.Error() == tc.want.Error() {
				t.Errorf("classified error %q should wrap the sentinel AND retain detail", got)
			}
		})
	}
}
