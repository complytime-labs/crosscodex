package catalog

import (
	"context"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// LLMCompleter adapts pkg/llmclient.Client to oscal.Completer.
type LLMCompleter struct {
	client   llmclient.Client
	model    string
	tenantID string
}

func NewLLMCompleter(client llmclient.Client, model, tenantID string) *LLMCompleter {
	return &LLMCompleter{client: client, model: model, tenantID: tenantID}
}

func (a *LLMCompleter) Complete(ctx context.Context, messages []oscal.Message) (string, error) {
	msgs := make([]llmclient.ChatMessage, len(messages))
	for i, m := range messages {
		msgs[i] = llmclient.ChatMessage{Role: m.Role, Content: m.Content}
	}
	resp, err := a.client.Complete(ctx, &llmclient.CompletionRequest{
		Model:    a.model,
		Messages: msgs,
		TenantID: a.tenantID,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// LLMEmbedder adapts pkg/llmclient.Client to oscal.Embedder.
type LLMEmbedder struct {
	client   llmclient.Client
	model    string
	tenantID string
}

func NewLLMEmbedder(client llmclient.Client, model, tenantID string) *LLMEmbedder {
	return &LLMEmbedder{client: client, model: model, tenantID: tenantID}
}

func (a *LLMEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	resp, err := a.client.Embed(ctx, &llmclient.EmbeddingRequest{
		Model:    a.model,
		Input:    texts,
		TenantID: a.tenantID,
	})
	if err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(resp.Data))
	for _, d := range resp.Data {
		if d.Index < len(vectors) {
			vectors[d.Index] = d.Embedding
		}
	}
	return vectors, nil
}
