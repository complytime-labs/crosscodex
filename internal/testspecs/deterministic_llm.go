package testspecs

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ llmclient.Client = DeterministicLLM{}

// DeterministicLLM is a hermetic llmclient.Client for end-to-end tests. It
// lives in internal/testspecs, not pkg/, because its canned replies encode the
// internal analyzers' reply formats; deterministic_llm_test.go runs each reply
// through the real parsers so format drift fails there first. Embed
// returns EmbeddingDim-dimension vectors deterministically seeded from each
// input's fnv hash: identical text always yields the identical vector, so two
// controls with identical prepared text embed to cosine similarity 1.0 -> a
// stable candidate pair. Complete returns one canned, parseable response per
// analyzer, routed by CompletionRequest.PromptName, the prompt name the worker
// forwards from the task payload.
//
// It is injected at the Go level rather than mocked over HTTP because
// PromptName is `json:"-"` (pkg/llmclient/types.go): it is never serialized
// in the outgoing HTTP request, so an httptest server would have to
// content-sniff raw prompt text to know which analyzer is calling.
//
// EmbeddingDim must match the vector dimension of the store the pipeline
// writes to. Health and Close return nil.
type DeterministicLLM struct {
	EmbeddingDim int
}

// checkTenant rejects a missing or invalid tenant ID the way the real client
// does, so an e2e run cannot pass on a request production would refuse.
func checkTenant(tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("testspecs.DeterministicLLM: tenant ID is required: %w", llmclient.ErrInvalidRequest)
	}
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("testspecs.DeterministicLLM: invalid tenant ID: %w: %w", err, llmclient.ErrInvalidRequest)
	}
	return nil
}

// Complete returns the canned response for req.PromptName, or an error for a
// prompt name no analyzer uses.
func (DeterministicLLM) Complete(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("testspecs.DeterministicLLM: completion request must not be nil: %w", llmclient.ErrInvalidRequest)
	}
	if err := checkTenant(req.TenantID); err != nil {
		return nil, err
	}
	var content string
	switch req.PromptName {
	case "classify":
		// ParseClassification expects "Type|Level".
		content = "Technical|Operational"
	case "requires":
		// requires.ParseResponse keys on "REQUIRES: YES|NO"; a single YES vote
		// with HIGH confidence clears the consensus threshold.
		content = "REQUIRES: YES\nJUSTIFICATION: prerequisite control\nCONFIDENCE: HIGH"
	case "relationship":
		// relationship.Aggregate omits NO_RELATIONSHIP pairs, so the
		// relationship analyzer produces an (empty) result row and populates no
		// vote_summaries rows; only the requires contribution reaches synthesis.
		content = "RELATIONSHIP: NO_RELATIONSHIP\nCONFIDENCE: HIGH"
	case "artifacts":
		// artifacts.ParseResponse treats "ARTIFACTS: NONE" as the empty
		// sentinel — a valid, parseable "no artifacts" result.
		content = "ARTIFACTS: NONE"
	default:
		return nil, fmt.Errorf("testspecs.DeterministicLLM: unexpected prompt name %q", req.PromptName)
	}

	return &llmclient.CompletionResponse{
		Model: req.Model,
		Choices: []llmclient.CompletionChoice{
			{
				Index:        0,
				Message:      llmclient.ChatMessage{Role: "assistant", Content: content},
				FinishReason: "stop",
			},
		},
	}, nil
}

// Embed returns one EmbeddingDim-dimension vector per input, a pure function
// of the input text. It errors when EmbeddingDim is not positive, which would
// otherwise yield empty vectors that every store rejects with a confusing
// dimension error.
func (d DeterministicLLM) Embed(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("testspecs.DeterministicLLM: embedding request must not be nil: %w", llmclient.ErrInvalidRequest)
	}
	if err := checkTenant(req.TenantID); err != nil {
		return nil, err
	}
	if d.EmbeddingDim <= 0 {
		return nil, fmt.Errorf("testspecs.DeterministicLLM: EmbeddingDim is %d; set it to the store's vector dimension", d.EmbeddingDim)
	}
	data := make([]llmclient.EmbeddingData, len(req.Input))
	for i, text := range req.Input {
		h := fnv.New64a()
		if _, err := h.Write([]byte(text)); err != nil {
			return nil, fmt.Errorf("testspecs.DeterministicLLM: hashing input: %w", err)
		}
		rng := rand.New(rand.NewSource(int64(h.Sum64())))
		vec := make([]float32, d.EmbeddingDim)
		for j := range vec {
			vec[j] = float32(rng.NormFloat64())
		}
		data[i] = llmclient.EmbeddingData{Index: i, Embedding: vec}
	}
	return &llmclient.EmbeddingResponse{Data: data, Model: req.Model}, nil
}

// Health returns nil.
func (DeterministicLLM) Health(context.Context) error { return nil }

// Close returns nil.
func (DeterministicLLM) Close() error { return nil }
