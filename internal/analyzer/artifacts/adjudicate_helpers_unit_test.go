//go:build !integration

package artifacts_test

import "github.com/complytime-labs/crosscodex/pkg/llmclient"

func (s *scriptedLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// sayAll replies decision to every request.
func sayAll(decision string) func(*llmclient.CompletionRequest) (string, error) {
	return func(*llmclient.CompletionRequest) (string, error) { return answer(decision), nil }
}
