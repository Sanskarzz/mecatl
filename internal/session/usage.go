package session

// Usage is an immutable value object accounting for the token cost of a single
// model call (or an aggregate thereof). Construct it as a literal; it carries no
// mutating methods.
type Usage struct {
	// InputTokens is the number of prompt tokens billed for this call,
	// including any tokens served from cache.
	InputTokens int
	// OutputTokens is the number of completion tokens generated.
	OutputTokens int
	// CacheReadTokens is the number of input tokens served from the prompt
	// cache (a subset of InputTokens).
	CacheReadTokens int
	// CacheWriteTokens is the number of input tokens written into the prompt
	// cache on this call.
	CacheWriteTokens int
}

// CacheHitRate returns the fraction of input tokens that were served from the
// prompt cache: CacheReadTokens / InputTokens. It returns 0 when InputTokens is
// zero (guarding against division by zero).
func (u Usage) CacheHitRate() float64 {
	if u.InputTokens == 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(u.InputTokens)
}

// Add returns a new Usage that is the element-wise sum of u and other. Usage is
// immutable, so accumulation is expressed by replacement, not mutation.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
	}
}
