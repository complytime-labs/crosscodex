package llmclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// Operation type constants for audit events and telemetry.
const (
	// OpComplete identifies a chat completion operation.
	OpComplete = "complete"
	// OpEmbed identifies an embedding operation.
	OpEmbed = "embed"
)

// Chat message role constants.
const (
	// RoleSystem identifies a system prompt message.
	RoleSystem = "system"
	// RoleUser identifies a user message.
	RoleUser = "user"
	// RoleAssistant identifies an assistant response message.
	RoleAssistant = "assistant"
)

// --- Request types (outgoing to OpenAI-compatible API) ---

// ChatMessage represents a single message in a chat conversation.
type ChatMessage struct {
	Role    string `json:"role"`    // RoleSystem, RoleUser, or RoleAssistant
	Content string `json:"content"` // Message content
}

// CompletionRequest represents a chat completion request.
type CompletionRequest struct {
	Model         string        `json:"model"`                 // Model identifier
	Messages      []ChatMessage `json:"messages"`              // Conversation messages
	MaxTokens     int           `json:"max_tokens,omitempty"`  // Maximum tokens to generate
	Temperature   *float64      `json:"temperature,omitempty"` // Sampling temperature (0.0-2.0); nil = provider default
	TopP          *float64      `json:"top_p,omitempty"`       // Nucleus sampling; nil = provider default
	Stop          []string      `json:"stop,omitempty"`        // Stop sequences
	TenantID      string        `json:"-"`                     // Tenant context (not serialized to API)
	JobID         string        `json:"-"`                     // Job identifier for audit correlation (not serialized)
	PromptName    string        `json:"-"`                     // Prompt template name for audit trail (not serialized)
	PromptVersion string        `json:"-"`                     // Prompt template version for audit trail (not serialized)
}

// EmbeddingRequest represents a request for text embeddings.
type EmbeddingRequest struct {
	Model    string   `json:"model"` // Embedding model name
	Input    []string `json:"input"` // Texts to embed
	TenantID string   `json:"-"`     // Tenant context (not serialized)
	JobID    string   `json:"-"`     // Job identifier for audit correlation (not serialized)
}

// --- Response types (incoming from OpenAI-compatible API) ---

// CompletionResponse represents the LLM chat completion response.
type CompletionResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   TokenUsage         `json:"usage"`
}

// CompletionChoice is a single completion result.
type CompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"` // "stop", "length", "content_filter"
}

// TokenUsage reports token consumption.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// EmbeddingResponse represents the embedding API response.
type EmbeddingResponse struct {
	Data  []EmbeddingData `json:"data"`
	Model string          `json:"model"`
	Usage EmbeddingUsage  `json:"usage"`
}

// EmbeddingData holds a single embedding vector.
type EmbeddingData struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// EmbeddingUsage reports token usage for embedding requests.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// --- API error response ---

// APIError represents an error response from the OpenAI-compatible API.
type APIError struct {
	StatusCode int    // HTTP status code
	Type       string `json:"type"`
	Message    string `json:"message"`
	Code       string `json:"code"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("LLM API error (HTTP %d, code=%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("LLM API error (HTTP %d): %s", e.StatusCode, e.Message)
}

// --- Audit event ---

// AuditEvent captures a completed LLM operation for audit trail emission.
type AuditEvent struct {
	Timestamp     time.Time `json:"timestamp"`
	TenantID      string    `json:"tenant_id"`
	JobID         string    `json:"job_id"`
	Model         string    `json:"model"`
	Operation     string    `json:"operation"`   // "complete" or "embed"
	PromptHash    string    `json:"prompt_hash"` // SHA-256 of serialized prompt
	TokensUsed    int       `json:"tokens_used"`
	DurationMS    int64     `json:"duration_ms"`
	Success       bool      `json:"success"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	TraceID       string    `json:"trace_id"`
	PromptName    string    `json:"prompt_name,omitempty"`
	PromptVersion string    `json:"prompt_version,omitempty"`
}

// ContentHash computes the SHA-256 hash of the serialized request for provenance.
// All callers pass JSON-marshalable types (ChatMessage slices, string slices);
// a marshal failure indicates a programming error and panics.
func ContentHash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("llmclient.ContentHash: json.Marshal failed: %v", err))
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
