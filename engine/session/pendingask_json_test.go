package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// PendingAsk.HookOriginated is the SERIALIZED, cross-process load-bearing marker
// (ADR 0062): the awaiting-resume path keys the skip-preHook branch on it, and that
// path runs in a FRESH process that loaded the durable jsonlstore record. These tests
// pin the JSON contract DIRECTLY (independent of sessnap's in-process struct copy).

// HookOriginated must survive a marshal+unmarshal round-trip as true.
func TestPendingAskHookOriginatedRoundTrips(t *testing.T) {
	in := PendingAsk{AskID: "a1", Tool: "Bash", Call: "c1", HookOriginated: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out PendingAsk
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.HookOriginated {
		t.Fatalf("HookOriginated must round-trip true; got %+v (json=%s)", out, b)
	}
}

// omitempty must keep the field ABSENT from the JSON when false, so an old record (or a
// policy ask) deserializes HookOriginated=false without the key being present.
func TestPendingAskHookOriginatedOmitemptyWhenFalse(t *testing.T) {
	b, err := json.Marshal(PendingAsk{AskID: "a1", Tool: "Bash"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "hook_originated") {
		t.Fatalf("a false HookOriginated must be omitted (omitempty); json=%s", b)
	}
	// And a record with no key deserializes to false.
	var out PendingAsk
	if err := json.Unmarshal([]byte(`{"AskID":"a1","Tool":"Bash"}`), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.HookOriginated {
		t.Fatal("a record missing hook_originated must deserialize to false")
	}
}
