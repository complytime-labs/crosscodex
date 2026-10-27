//go:build !integration

package artifacts_test

import "github.com/complytime-labs/crosscodex/pkg/llmclient"

// sayAll replies decision to every request.
func sayAll(decision string) func(*llmclient.CompletionRequest) (string, error) {
	return func(*llmclient.CompletionRequest) (string, error) { return answer(decision), nil }
}
