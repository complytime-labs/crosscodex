package vectordbtest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// ErrUnscripted is wrapped by every error a fake returns for a call the test
// did not script. Match it with errors.Is; the message names the field to set.
var ErrUnscripted = errors.New("vectordbtest: call not scripted")

func unscripted(method string) error {
	return fmt.Errorf("%w: %s called but Fake.%sFunc is not set; set it to script this call",
		ErrUnscripted, method, method)
}

// validateTenant applies PgVectorStore's validateTenantContext check, with the
// same wording: ctx must carry a tenant and it must equal tenantID.
func validateTenant(ctx context.Context, tenantID string) error {
	contextTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("tenant context required: %w", err)
	}
	if contextTenant != tenantID {
		return fmt.Errorf("tenant mismatch: context=%s, param=%s: %w", contextTenant, tenantID, tenant.ErrTenantMismatch)
	}
	return nil
}

var _ vectordb.VectorDB = (*Fake)(nil)

// StoreCall is one accepted StoreEmbedding or StoreBatch call.
type StoreCall struct {
	Tenant     string
	Embeddings []vectordb.Embedding
}

// Fake fakes vectordb.VectorDB. See the package documentation for the checks
// applied before a call is recorded or scripted. The zero value is ready to
// use. Set the exported fields before the first call; the methods are then
// safe for concurrent use.
type Fake struct {
	// StoreEmbeddingFunc, if set, alone decides what StoreEmbedding returns
	// (unset: nil); accepted calls are recorded either way.
	StoreEmbeddingFunc func(ctx context.Context, tenantID string, embedding vectordb.Embedding) error
	// StoreBatchFunc, if set, alone decides what StoreBatch returns (unset:
	// nil); accepted calls are recorded either way.
	StoreBatchFunc func(ctx context.Context, tenantID string, embeddings []vectordb.Embedding) error
	// FindSimilarFunc, if set, alone decides what FindSimilar returns for a
	// query that passes validation; unset, FindSimilar returns ErrUnscripted.
	FindSimilarFunc func(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error)
	// DeleteByModelFunc, if set, alone decides what DeleteByModel returns;
	// unset, DeleteByModel returns ErrUnscripted.
	DeleteByModelFunc func(ctx context.Context, tenantID, catalogID, model string) error

	mu     sync.Mutex
	stored []StoreCall // deep copies, see cloneEmbeddings
}

// StoreEmbedding validates the tenant, records the call, then returns what
// StoreEmbeddingFunc decides, or nil when it is unset.
func (f *Fake) StoreEmbedding(ctx context.Context, tenantID string, embedding vectordb.Embedding) error {
	if err := validateTenant(ctx, tenantID); err != nil {
		return err
	}
	f.record(tenantID, []vectordb.Embedding{embedding})
	if f.StoreEmbeddingFunc == nil {
		return nil
	}
	return f.StoreEmbeddingFunc(ctx, tenantID, embedding)
}

// StoreBatch validates the tenant, records the call (an empty batch
// included), then returns what StoreBatchFunc decides, or nil when it is
// unset.
func (f *Fake) StoreBatch(ctx context.Context, tenantID string, embeddings []vectordb.Embedding) error {
	if err := validateTenant(ctx, tenantID); err != nil {
		return err
	}
	f.record(tenantID, embeddings)
	if f.StoreBatchFunc == nil {
		return nil
	}
	return f.StoreBatchFunc(ctx, tenantID, embeddings)
}

// FindSimilar validates the tenant and query like PgVectorStore, then returns
// what FindSimilarFunc decides, or an ErrUnscripted error when it is unset.
func (f *Fake) FindSimilar(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
	if err := validateTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if query.Limit <= 0 {
		return nil, fmt.Errorf("limit must be positive, got %d", query.Limit)
	}
	if len(query.Vector) == 0 {
		return nil, errors.New("query vector cannot be empty")
	}
	if f.FindSimilarFunc == nil {
		return nil, unscripted("FindSimilar")
	}
	return f.FindSimilarFunc(ctx, tenantID, query)
}

// DeleteByModel validates the tenant, then returns what DeleteByModelFunc
// decides, or an ErrUnscripted error when it is unset.
func (f *Fake) DeleteByModel(ctx context.Context, tenantID, catalogID, model string) error {
	if err := validateTenant(ctx, tenantID); err != nil {
		return err
	}
	if f.DeleteByModelFunc == nil {
		return unscripted("DeleteByModel")
	}
	return f.DeleteByModelFunc(ctx, tenantID, catalogID, model)
}

func (f *Fake) record(tenantID string, embeddings []vectordb.Embedding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, StoreCall{Tenant: tenantID, Embeddings: cloneEmbeddings(embeddings)})
}

// Stored returns a deep copy of every accepted StoreEmbedding and StoreBatch
// call, in call order, including calls whose Func failed.
func (f *Fake) Stored() []StoreCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StoreCall, len(f.stored))
	for i, s := range f.stored {
		out[i] = StoreCall{Tenant: s.Tenant, Embeddings: cloneEmbeddings(s.Embeddings)}
	}
	return out
}

// cloneEmbeddings copies every slice and map an Embedding holds, so neither a
// caller mutating its batch after the call nor a caller mutating a returned
// record alters the stored copy. A nil slice stays nil, an empty one stays empty.
func cloneEmbeddings(in []vectordb.Embedding) []vectordb.Embedding {
	if in == nil {
		return nil
	}
	out := make([]vectordb.Embedding, len(in))
	for i, e := range in {
		e.Vector = slices.Clone(e.Vector)
		e.Metadata = maps.Clone(e.Metadata)
		out[i] = e
	}
	return out
}
