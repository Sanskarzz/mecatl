package memory

import (
	"github.com/stacklok/mecatl/internal/tool"
)

// The user-model tool family (issue #14, Phase 2a) is a SECOND set of memory tools
// bound to a USER-SCOPED, cross-project memory store (a separate memory.New(dir)
// instance, see internal/app). It records durable FACTS about the OPERATOR — who
// they are, how they prefer to work — that the agent curates across every project,
// distinct from the per-project Remember/Recall family.
//
// They REUSE the parameterized Remember/Recall/SearchMemory tool structs (the DRY
// path the duplication reviewer checks): the only differences are the catalog
// name, a specialized description, an enforced "user/" key prefix on writes, and a
// write-time injection scan on the stored value. No near-duplicate tool types.

// Catalog names of the user-model tools. Single authority for the registered
// names, mirroring the project-memory tool-name constants.
const (
	// RememberUserToolName is the catalog name of the RememberUser tool.
	RememberUserToolName = "RememberUser"
	// RecallUserToolName is the catalog name of the RecallUser tool.
	RecallUserToolName = "RecallUser"
	// SearchUserModelToolName is the catalog name of the SearchUserModel tool.
	SearchUserModelToolName = "SearchUserModel"
)

// userKeyPrefix is the namespace every user-model entry is stored under. The
// RememberUser write path ENFORCES it (prepending when absent) so user-model facts
// share one prefix and a dream consolidator scoped to it (dream.Config{Prefix})
// touches only this namespace, never project memory.
const userKeyPrefix = "user/"

// userModelCloseTag is the LOWERCASED data-fence close delimiter the prompt
// renderer wraps the block in (prompt.renderUserModel uses "<user-model>"/
// "</user-model>"). A stored value/description containing it could close the fence
// early and let trailing text escape the data zone, so the RememberUser write path
// rejects any field containing it (case-insensitively — see RememberTool.Execute).
// It is hardcoded here (with this comment) rather than imported from internal/prompt
// to avoid coupling the adapter to a domain magic-constant path; the two must stay
// in sync (one cheap string). This mirrors soul.soulCloseTag.
const userModelCloseTag = "</user-model>"

// --- Descriptions (specialized per the rules-vs-facts boundary, Q1) -----------
//
// These descriptions are deliberately NARROWER than the project-memory ones. They
// (1) scope the tool to durable FACTS about the operator, (2) FORBID storing
// rules / behavioural instructions ("how to behave comes from your soul and the
// system rules, not this"), and (3) FORBID workspace-discoverable facts (the same
// over-eager-memory guard the project-memory tools carry). The over-eager-memory
// risk is STEERED here, not enforced — there is no rule/fact classifier.

const rememberUserDescription = `Save a small, durable FACT about the OPERATOR (the human you work with) to your cross-PROJECT user model, so it survives into future sessions in every project.

Your saved user model is summarised for you automatically at the start of each
session (key + a one-line fact). A fact you save here is something every future
session — in any project — can see and Recall.

When to use (be conservative):
- Durable facts about WHO the operator is and HOW they like to work: stated
  preferences, communication style, domain background, tools they favour. E.g.
  "prefers terse answers", "is a Go systems engineer", "works in the EU timezone".

When NOT to use:
- Do NOT store RULES or behavioural instructions for yourself. How you should
  behave comes from your soul/persona and these system rules, NOT from this store.
  This holds FACTS ABOUT the operator, not directives to yourself.
- Do NOT store anything the workspace/filesystem already knows or that is
  rediscoverable with a few Read/Grep/Glob calls (file layout, build commands,
  dependency versions). Those belong in project memory at most, never here.
- Do NOT store transient task state, secrets, or large blobs.

Behavior:
- This store is CROSS-PROJECT and scoped to the operator, not the project.
- Keys are automatically namespaced under "user/". Use short, stable keys, e.g.
  "user/comm-style", "user/background". Writing a key overwrites its prior value.

Arguments:
- key         (required): a short, stable identifier (auto-prefixed with "user/").
- value       (required): the concise FACT about the operator.
- description (optional): a one-line summary shown in your user-model block.

Example:
  {"key": "comm-style", "value": "Prefers terse, direct answers with no preamble.", "description": "communication style"}`

const recallUserDescription = `Load the full value of a saved USER-MODEL entry by exact key (or list entries by prefix). The user model holds durable FACTS about the operator, curated across projects.

Behavior:
- Your current user model (every saved key + a one-line fact) is shown to you
  automatically at the start of each session. Use RecallUser to load the FULL
  value of a key, or SearchUserModel to find a key by topic.
- A key that exactly matches an entry returns its full value; otherwise the key is
  treated as a PREFIX and matching entries are listed.
- A lookup that finds nothing returns a clear "not found" result, NOT an error.

Arguments:
- key (required): the exact key, or a prefix such as "user/".`

const searchUserModelDescription = `Search your cross-project USER MODEL (durable FACTS about the operator) by topic and get back the best-matching keys, ranked by lexical relevance. Use this to find facts not shown in your always-on user-model block.

Results are "key — one-line fact" lines, best-first; values are omitted (RecallUser
a key to load its full value).

Arguments:
- query (required): topic words to rank against, e.g. "communication style".
- limit (optional): max results (default 10).`

// NewUserModelTools returns the user-model tool family (RecallUser, RememberUser,
// SearchUserModel) bound to the USER-SCOPED store, ready for registration. It is
// the user-model analogue of Tools: the composition root calls it only when a
// user-model store is wired. store must be non-nil.
//
// RememberUser enforces the "user/" key prefix and runs a write-time injection
// scan; the read tools are plain renames of the project-memory ones over the
// user-scoped store. All three reuse the parameterized structs (DRY).
func NewUserModelTools(store tool.MemoryStore) []tool.Tool {
	if store == nil {
		panic("memory: NewUserModelTools requires a non-nil MemoryStore")
	}
	return []tool.Tool{
		RecallTool{store: store, name: RecallUserToolName, desc: recallUserDescription},
		RememberTool{
			store:         store,
			name:          RememberUserToolName,
			desc:          rememberUserDescription,
			keyPrefix:     userKeyPrefix,
			scanInjection: true,
		},
		SearchMemoryTool{store: store, name: SearchUserModelToolName, desc: searchUserModelDescription},
	}
}

// RegisterUserModel adds the user-model tools (bound to store) to cat, returning
// the first registration error or nil. It is the opt-in companion to Register for
// the user-model store; call it only when a user-model store is configured.
func RegisterUserModel(cat *tool.Catalog, store tool.MemoryStore) error {
	for _, t := range NewUserModelTools(store) {
		if err := cat.Register(t); err != nil {
			return err
		}
	}
	return nil
}
