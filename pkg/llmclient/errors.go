package llmclient

import "errors"

var (
	// ErrGatewayUnavailable indicates the LLM gateway is unreachable.
	ErrGatewayUnavailable = errors.New("LLM gateway unavailable")

	// ErrRateLimitExceeded indicates the rate limit was exceeded after retries.
	ErrRateLimitExceeded = errors.New("rate limit exceeded")

	// ErrInvalidRequest indicates the request is malformed or missing required fields.
	ErrInvalidRequest = errors.New("invalid LLM request")

	// ErrMaxTokensExceeded indicates the request exceeds token limits.
	ErrMaxTokensExceeded = errors.New("max tokens exceeded")

	// ErrModelNotAllowed indicates the requested model is not in the allowed list.
	ErrModelNotAllowed = errors.New("model not in allowed list")

	// ErrCredentialResolution indicates a credential URI could not be resolved.
	ErrCredentialResolution = errors.New("credential resolution failed")

	// ErrAuthentication indicates the gateway rejected the API key (HTTP 401/403).
	ErrAuthentication = errors.New("LLM gateway authentication failed")

	// ErrNoGateway indicates no gateway URL was configured.
	ErrNoGateway = errors.New("no LLM gateway URL configured")

	// ErrResponseTooLarge indicates the LLM gateway response exceeded the maximum allowed size.
	ErrResponseTooLarge = errors.New("LLM gateway response too large")
)
