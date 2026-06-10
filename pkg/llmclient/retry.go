package llmclient

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// baseBackoff is the initial retry delay.
	baseBackoff = 500 * time.Millisecond

	// maxBackoff caps the retry delay.
	maxBackoff = 30 * time.Second

	// jitterFraction is the maximum proportion of jitter added to the backoff.
	jitterFraction = 0.25

	// maxRetryAfter caps the Retry-After header delay to prevent abuse.
	maxRetryAfter = 60 * time.Second
)

// shouldRetry returns true if the HTTP status code warrants a retry, along
// with a suggested delay parsed from the Retry-After header (RFC 9110).
// When statusCode is 429 and retryAfterHeader parses successfully, the
// parsed duration is returned (capped at 60s). For 5xx or when the header
// is empty/unparseable, the returned delay is 0 and the caller should use
// normal exponential backoff.
func shouldRetry(statusCode int, retryAfterHeader string) (bool, time.Duration) {
	if statusCode == 429 {
		if d := parseRetryAfter(retryAfterHeader); d > 0 {
			if d > maxRetryAfter {
				d = maxRetryAfter
			}
			return true, d
		}
		return true, 0
	}
	if statusCode >= 500 && statusCode < 600 {
		return true, 0
	}
	return false, 0
}

// parseRetryAfter parses a Retry-After header value per RFC 9110.
// It handles both integer-seconds ("120") and HTTP-date
// ("Fri, 31 Dec 1999 23:59:59 GMT") formats. Returns 0 if unparseable.
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}

	// Try integer-seconds first.
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}

	// Try HTTP-date format (RFC 7231 / RFC 9110).
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}

	return 0
}

// backoffDuration calculates the delay before retry attempt n (0-indexed).
// Uses exponential backoff with jitter: base * 2^n * (1 + random jitter).
// The result is capped at maxBackoff.
func backoffDuration(attempt int) time.Duration {
	exp := math.Pow(2, float64(attempt))
	base := float64(baseBackoff) * exp

	if base > float64(maxBackoff) {
		base = float64(maxBackoff)
	}

	// Add jitter: ±jitterFraction of the base delay.
	jitter := base * jitterFraction * (2*rand.Float64() - 1)
	d := time.Duration(base + jitter)

	if d > maxBackoff {
		d = maxBackoff
	}
	if d < 0 {
		d = 0
	}

	return d
}
