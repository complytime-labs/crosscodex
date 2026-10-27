package artifacts_test

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
)

// replying adapts a text-producing reply function into a Fake.CompleteFunc:
// a successful reply becomes a single assistant choice.
func replying(reply func(req *llmclient.CompletionRequest) (string, error)) func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
		text, err := reply(req)
		if err != nil {
			return nil, err
		}
		return &llmclient.CompletionResponse{Choices: []llmclient.CompletionChoice{
			{Message: llmclient.ChatMessage{Role: llmclient.RoleAssistant, Content: text}},
		}}, nil
	}
}

// answer is a well-formed panel reply.
func answer(decision string) string {
	return "SAME: " + decision + "\nJUSTIFICATION: Checked.\nCONFIDENCE: HIGH"
}

// newFixedPrompts returns a prompttest.Fake whose Render yields a fixed
// two-message prompt; the Fake records the vars it was given.
func newFixedPrompts() *prompttest.Fake {
	return &prompttest.Fake{
		RenderFunc: func(_ context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error) {
			return &prompt.ResolvedPrompt{Name: name, Version: "9.9.9", Messages: []prompt.Message{
				{Role: "system", Content: "judge"},
				{Role: "user", Content: vars["name_a"] + " vs " + vars["name_b"]},
			}}, nil
		},
	}
}
