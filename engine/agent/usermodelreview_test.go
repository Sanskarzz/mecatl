package agent_test

import (
	"context"
	"testing"

	"github.com/stacklok/mecatl/engine/adapter/mockllm"
	"github.com/stacklok/mecatl/engine/agent"
	"github.com/stacklok/mecatl/engine/session"
	"github.com/stacklok/mecatl/engine/tool"
)

// nilReviewStore is the minimal port.SessionStore stand-in for the nil-args
// guard below. The full reviewer behavior (R10: load-but-never-reopen, the fact
// landing in the real user-model store) is integration-tested next to the
// memory adapter in internal/adapter/memory.
type nilReviewStore struct{}

func (nilReviewStore) Load(context.Context, session.SessionID) (*session.Session, error) {
	return nil, nil
}

func (nilReviewStore) Save(context.Context, *session.Session) error { return nil }

// TestNewUserModelReviewerNilArgsPanic guards the composition-root contract.
func TestNewUserModelReviewerNilArgsPanic(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("nil store should panic")
			}
		}()
		_ = agent.NewUserModelReviewer(nil, newEngine(agent.Deps{LLM: mockllm.New(), Catalog: tool.NewCatalog()}))
	})
	t.Run("nil engine", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("nil engine should panic")
			}
		}()
		_ = agent.NewUserModelReviewer(nilReviewStore{}, nil)
	})
}
