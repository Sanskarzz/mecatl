package session

import "testing"

func TestUsageCacheHitRate(t *testing.T) {
	tests := []struct {
		name string
		u    Usage
		want float64
	}{
		{"zero input avoids div by zero", Usage{InputTokens: 0, CacheReadTokens: 0}, 0},
		{"zero input with cache reads still zero", Usage{InputTokens: 0, CacheReadTokens: 100}, 0},
		{"half cached", Usage{InputTokens: 100, CacheReadTokens: 50}, 0.5},
		{"fully cached", Usage{InputTokens: 200, CacheReadTokens: 200}, 1.0},
		{"none cached", Usage{InputTokens: 100, CacheReadTokens: 0}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.CacheHitRate(); got != tc.want {
				t.Fatalf("CacheHitRate() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUsageAdd(t *testing.T) {
	a := Usage{InputTokens: 10, OutputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 3}
	b := Usage{InputTokens: 5, OutputTokens: 4, CacheReadTokens: 1, CacheWriteTokens: 1}
	got := a.Add(b)
	want := Usage{InputTokens: 15, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 4}
	if got != want {
		t.Fatalf("Add = %+v, want %+v", got, want)
	}
}
