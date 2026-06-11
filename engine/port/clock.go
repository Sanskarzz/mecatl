package port

import "time"

// Clock abstracts the wall clock so the loop and stores are deterministically
// testable. Adapters provide a real clock and a fake.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}
