package port

import (
	"reflect"
	"sort"
	"testing"
)

// TestLLMRequestStaysProviderNeutral is a DTO-neutrality TRIPWIRE (multi-provider
// Finding B): it asserts LLMRequest carries EXACTLY the four provider-neutral fields
// {System, Messages, Tools, Model}. A new field — especially a provider-specific one
// like a reasoning-effort or thinking-budget — would fail this test on purpose. Such
// provider-PRIVATE knobs belong on the ADAPTER (an openai.WithBaseURL-style Option),
// never on this domain-facing request, because the agent loop must never branch on
// provider. Widening the struct is a deliberate decision that must update this guard.
func TestLLMRequestStaysProviderNeutral(t *testing.T) {
	want := []string{"Messages", "Model", "System", "Tools"}

	typ := reflect.TypeOf(LLMRequest{})
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LLMRequest fields = %v, want %v.\n"+
			"If you added a field: is it provider-NEUTRAL? Provider-private knobs "+
			"(reasoning-effort, thinking-budget, store/include flags) belong on the "+
			"ADAPTER, not LLMRequest. Update this guard only with a deliberate decision.",
			got, want)
	}
}
