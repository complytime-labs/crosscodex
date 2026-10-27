package llmclienttest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ErrUnscripted is wrapped by every error a fake returns for a call the test
// did not script. Match it with errors.Is; the message names the field to set.
var ErrUnscripted = errors.New("llmclienttest: call not scripted")

func unscripted(method string) error {
	return fmt.Errorf("%w: %s called but Fake.%sFunc is not set; set it to script this call",
		ErrUnscripted, method, method)
}

// validateRequest applies the checks the real client's prepareCompletionRequest
// and prepareEmbeddingRequest apply, with the same wording, so a spec cannot
// pass on a request production would reject. missing is the error text for an empty payload.
func validateRequest(tenantID string, empty bool, missing string) error {
	if tenantID == "" {
		return fmt.Errorf("tenant ID is required: %w", llmclient.ErrInvalidRequest)
	}
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("invalid tenant ID: %w: %w", err, llmclient.ErrInvalidRequest)
	}
	if empty {
		return fmt.Errorf("%s: %w", missing, llmclient.ErrInvalidRequest)
	}
	return nil
}

var _ llmclient.Client = (*Fake)(nil)

// Fake fakes llmclient.Client. Complete and Embed validate requests like the
// real client (nil request, missing or invalid tenant ID, empty messages or
// input) and return an error wrapping llmclient.ErrInvalidRequest without
// recording the request or calling the Func; they do not fill in the default
// model. Unset Complete and Embed return ErrUnscripted;
// Health returns HealthErr (nil by default) and Close returns nil. The zero
// value is ready to use. Set the exported fields before the first call; the
// methods are then safe for concurrent use.
type Fake struct {
	// CompleteFunc, if set, alone decides what Complete returns. Every
	// accepted request is recorded in CompleteRequests, whether or not
	// CompleteFunc is set.
	CompleteFunc func(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error)
	// EmbedFunc, if set, alone decides what Embed returns.
	EmbedFunc func(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error)
	// HealthErr is returned by every Health call.
	HealthErr error

	mu        sync.Mutex
	completes []llmclient.CompletionRequest // deep copies, see cloneCompletion
}

// Complete validates and records the request, then returns what CompleteFunc decides, or an
// ErrUnscripted error when it is unset.
func (f *Fake) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("completion request must not be nil: %w", llmclient.ErrInvalidRequest)
	}
	if err := validateRequest(req.TenantID, len(req.Messages) == 0, "at least one message is required"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.completes = append(f.completes, cloneCompletion(req))
	f.mu.Unlock()
	if f.CompleteFunc == nil {
		return nil, unscripted("Complete")
	}
	return f.CompleteFunc(ctx, req)
}

// Embed validates the request like the real client (see Complete), then
// returns what EmbedFunc decides, or an ErrUnscripted error when it is unset.
func (f *Fake) Embed(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("embedding request must not be nil: %w", llmclient.ErrInvalidRequest)
	}
	if err := validateRequest(req.TenantID, len(req.Input) == 0, "at least one input text is required"); err != nil {
		return nil, err
	}
	if f.EmbedFunc == nil {
		return nil, unscripted("Embed")
	}
	return f.EmbedFunc(ctx, req)
}

// Health returns HealthErr.
func (f *Fake) Health(context.Context) error { return f.HealthErr }

// Close returns nil.
func (f *Fake) Close() error { return nil }

// cloneCompletion copies req including every reference field, so neither the
// caller mutating its request after the call nor a caller mutating a returned
// request alters the record.
func cloneCompletion(req *llmclient.CompletionRequest) llmclient.CompletionRequest {
	c := *req
	c.Messages = slices.Clone(req.Messages)
	c.Stop = slices.Clone(req.Stop)
	if req.Temperature != nil {
		t := *req.Temperature
		c.Temperature = &t
	}
	if req.TopP != nil {
		p := *req.TopP
		c.TopP = &p
	}
	return c
}

// CompleteRequests returns every request Complete accepted, in call order.
// The requests are deep copies, so a caller mutating one does not alter the
// record. Requests rejected as invalid are not recorded.
func (f *Fake) CompleteRequests() []*llmclient.CompletionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*llmclient.CompletionRequest, len(f.completes))
	for i := range f.completes {
		r := cloneCompletion(&f.completes[i])
		out[i] = &r
	}
	return out
}
