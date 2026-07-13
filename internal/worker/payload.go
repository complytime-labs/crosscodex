package worker

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

// validRoles is the allowlist of permitted ChatMessage role values.
// Validated at deserialization to prevent injection via untrusted NATS payloads.
var validRoles = map[string]bool{
	"system":    true,
	"user":      true,
	"assistant": true,
}

func extractCompletionRequest(payload *structpb.Struct, tenantID, jobID string) (*llmclient.CompletionRequest, error) {
	fields := payload.GetFields()

	msgValue, ok := fields["messages"]
	if !ok {
		return nil, fmt.Errorf("missing required field 'messages': %w", ErrInvalidPayload)
	}
	msgList := msgValue.GetListValue()
	if msgList == nil {
		return nil, fmt.Errorf("field 'messages' must be a list: %w", ErrInvalidPayload)
	}

	messages := make([]llmclient.ChatMessage, 0, len(msgList.Values))
	for i, v := range msgList.Values {
		msgStruct := v.GetStructValue()
		if msgStruct == nil {
			return nil, fmt.Errorf("messages[%d] must be a struct: %w", i, ErrInvalidPayload)
		}
		role := msgStruct.Fields["role"].GetStringValue()
		if role == "" {
			return nil, fmt.Errorf("messages[%d] missing required field 'role': %w", i, ErrInvalidPayload)
		}
		if !validRoles[role] {
			return nil, fmt.Errorf("messages[%d] has invalid role %q: must be one of system, user, assistant: %w", i, role, ErrInvalidPayload)
		}
		content := msgStruct.Fields["content"].GetStringValue()
		messages = append(messages, llmclient.ChatMessage{Role: role, Content: content})
	}

	// Verify content_hash if present. Analyzers embed the hash so workers can
	// detect payload tampering or serialization corruption mid-flight.
	if hashField, hasHash := fields["content_hash"]; hasHash {
		expectedHash := hashField.GetStringValue()
		if expectedHash != "" {
			actualHash := llmclient.ContentHash(messages)
			if actualHash != expectedHash {
				return nil, fmt.Errorf("content_hash mismatch: payload may be corrupted or tampered: %w", ErrInvalidPayload)
			}
		}
	}

	req := &llmclient.CompletionRequest{
		Model:         getStringField(fields, "model"),
		Messages:      messages,
		MaxTokens:     int(getNumberField(fields, "max_tokens")),
		TenantID:      tenantID,
		JobID:         jobID,
		PromptName:    getStringField(fields, "prompt_name"),
		PromptVersion: getStringField(fields, "prompt_version"),
	}

	if temp, ok := fields["temperature"]; ok {
		t := temp.GetNumberValue()
		req.Temperature = &t
	}

	return req, nil
}

func extractEmbeddingRequest(payload *structpb.Struct, tenantID, jobID string) (*llmclient.EmbeddingRequest, error) {
	fields := payload.GetFields()

	text := getStringField(fields, "text")
	if text == "" {
		return nil, fmt.Errorf("missing required field 'text': %w", ErrInvalidPayload)
	}

	return &llmclient.EmbeddingRequest{
		Model:    getStringField(fields, "model"),
		Input:    []string{text},
		TenantID: tenantID,
		JobID:    jobID,
	}, nil
}

func buildCompletionResult(resp *llmclient.CompletionResponse) (*structpb.Struct, error) {
	response := ""
	if len(resp.Choices) > 0 {
		response = resp.Choices[0].Message.Content
	}

	return structpb.NewStruct(map[string]interface{}{
		"response":          response,
		"model":             resp.Model,
		"tokens_used":       float64(resp.Usage.TotalTokens),
		"prompt_tokens":     float64(resp.Usage.PromptTokens),
		"completion_tokens": float64(resp.Usage.CompletionTokens),
	})
}

func buildEmbeddingResult(resp *llmclient.EmbeddingResponse) (*structpb.Struct, error) {
	embeddings := make([]interface{}, len(resp.Data))
	for i, d := range resp.Data {
		vec := make([]interface{}, len(d.Embedding))
		for j, v := range d.Embedding {
			vec[j] = float64(v)
		}
		embeddings[i] = vec
	}

	dimensions := 0
	if len(resp.Data) > 0 {
		dimensions = len(resp.Data[0].Embedding)
	}

	return structpb.NewStruct(map[string]interface{}{
		"embeddings":  embeddings,
		"model":       resp.Model,
		"tokens_used": float64(resp.Usage.TotalTokens),
		"dimensions":  float64(dimensions),
	})
}

func getStringField(fields map[string]*structpb.Value, key string) string {
	if v, ok := fields[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

func getNumberField(fields map[string]*structpb.Value, key string) float64 {
	if v, ok := fields[key]; ok {
		return v.GetNumberValue()
	}
	return 0
}
