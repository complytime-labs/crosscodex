package artifacts_test

import (
	"context"
	"errors"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

// scriptedLLM answers each Complete with reply(req) and records every request.
// A request for which noChoices returns true gets a response with no choices.
type scriptedLLM struct {
	mu        sync.Mutex
	calls     []*llmclient.CompletionRequest
	reply     func(req *llmclient.CompletionRequest) (string, error)
	noChoices func(req *llmclient.CompletionRequest) bool
}

func (s *scriptedLLM) Complete(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	if s.noChoices != nil && s.noChoices(req) {
		return &llmclient.CompletionResponse{}, nil
	}
	text, err := s.reply(req)
	if err != nil {
		return nil, err
	}
	return &llmclient.CompletionResponse{Choices: []llmclient.CompletionChoice{
		{Message: llmclient.ChatMessage{Role: llmclient.RoleAssistant, Content: text}},
	}}, nil
}

func (s *scriptedLLM) Embed(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	return nil, errors.New("scriptedLLM: Embed is not used")
}
func (s *scriptedLLM) Health(context.Context) error { return nil }
func (s *scriptedLLM) Close() error                 { return nil }

// answer is a well-formed panel reply.
func answer(decision string) string {
	return "SAME: " + decision + "\nJUSTIFICATION: Checked.\nCONFIDENCE: HIGH"
}

// fixedPrompts renders a fixed two-message prompt and records the vars.
type fixedPrompts struct {
	mu   sync.Mutex
	vars []map[string]string
	err  error
}

func (f *fixedPrompts) Render(_ context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars = append(f.vars, vars)
	if f.err != nil {
		return nil, f.err
	}
	return &prompt.ResolvedPrompt{Name: name, Version: "9.9.9", Messages: []prompt.Message{
		{Role: "system", Content: "judge"},
		{Role: "user", Content: vars["name_a"] + " vs " + vars["name_b"]},
	}}, nil
}

func (f *fixedPrompts) Resolve(context.Context, string) (*prompt.PromptSpec, error) {
	return nil, errors.New("fixedPrompts: Resolve is not used")
}
func (f *fixedPrompts) List(context.Context) ([]string, error) {
	return nil, errors.New("fixedPrompts: List is not used")
}
func (f *fixedPrompts) Layers(context.Context, string) ([]prompt.LayerInfo, error) {
	return nil, errors.New("fixedPrompts: Layers is not used")
}
