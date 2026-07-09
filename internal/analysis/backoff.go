package analysis

import (
	"math"
	"math/rand/v2"
	"time"
)

// computeBackoff calculates exponential backoff with ±25% jitter.
// Formula: base * 2^attempt * jitter where jitter ∈ [0.75, 1.25].
func computeBackoff(attempt int, base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	// Exponential: base * 2^attempt
	multiplier := math.Pow(2, float64(attempt))
	backoff := float64(base) * multiplier

	// Jitter: ±25% → [0.75, 1.25]
	jitter := 0.75 + rand.Float64()*0.5
	backoff *= jitter

	result := time.Duration(backoff)
	if result < 0 {
		return 0
	}
	return result
}
