package llmclient

import (
	"fmt"
	"slices"
	"strings"
)

// CheckAnswered returns an error wrapping ErrOutputTruncated when resp holds
// no answer because the model stopped at req's max_tokens: every choice's
// content is empty or whitespace and at least one choice finished with
// "length". A reasoning model does this when its thinking uses the whole
// budget, which an analyzer's small max_tokens makes likely.
//
// Callers that consume the answer check it; Complete does not, because a
// request that only needs the response's metadata (the per-job model probe
// asks for one token) is valid without an answer. A partial answer cut off at
// max_tokens, an empty answer the model ended itself and a response with no
// choices are not this error: the caller's parser judges them.
func CheckAnswered(req *CompletionRequest, resp *CompletionResponse) error {
	if len(resp.Choices) == 0 {
		return nil
	}
	answered := slices.ContainsFunc(resp.Choices, func(c CompletionChoice) bool {
		return strings.TrimSpace(c.Message.Content) != ""
	})
	cutOff := slices.ContainsFunc(resp.Choices, func(c CompletionChoice) bool {
		return c.FinishReason == "length"
	})
	if answered || !cutOff {
		return nil
	}
	return fmt.Errorf("%w: model %q stopped at max_tokens %d after %d completion tokens without writing an answer; "+
		"turn the model's reasoning off in the LLM gateway or raise max_tokens",
		ErrOutputTruncated, req.Model, req.MaxTokens, resp.Usage.CompletionTokens)
}
