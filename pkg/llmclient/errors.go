package llmclient

import "errors"

var (
	// ErrGatewayUnavailable indicates the LLM gateway is unreachable.
	ErrGatewayUnavailable = errors.New("LLM gateway unavailable")

	// ErrRateLimitExceeded indicates the rate limit was exceeded after retries.
	ErrRateLimitExceeded = errors.New("rate limit exceeded")

	// ErrInvalidRequest indicates the request is malformed or missing required fields.
	ErrInvalidRequest = errors.New("invalid LLM request")

	// ErrInvalidMessages indicates a payload's messages value is not a list of
	// well-formed chat messages (see MessagesFromPayload).
	ErrInvalidMessages = errors.New("invalid chat messages")

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

	// ErrOutputTruncated indicates the model stopped at max_tokens before
	// writing any answer, typically a reasoning model that spent the budget
	// thinking (see CheckAnswered).
	ErrOutputTruncated = errors.New("model gave no answer within max_tokens")
)
