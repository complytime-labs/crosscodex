package llmclient

import "errors"

var (
	// ErrGatewayUnavailable indicates the LLM gateway is unreachable.
	ErrGatewayUnavailable = errors.New("LLM gateway unavailable")

	// ErrRateLimitExceeded indicates the rate limit was exceeded.
	ErrRateLimitExceeded = errors.New("rate limit exceeded")

	// ErrInvalidRequest indicates the request is malformed.
	ErrInvalidRequest = errors.New("invalid LLM request")

	// ErrMaxTokensExceeded indicates the request exceeds token limits.
	ErrMaxTokensExceeded = errors.New("max tokens exceeded")
)
