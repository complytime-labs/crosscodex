package analyzer

import (
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

// ModelKind is the kind of LLM call an alias serves.
type ModelKind string

const (
	// ModelCompletion: chat completions; an empty alias resolves to the
	// tenant's llm.default_model, as the worker resolves it.
	ModelCompletion ModelKind = "completion"
	// ModelEmbedding: embeddings; an empty alias resolves to the tenant's
	// llm.embedding_model.
	ModelEmbedding ModelKind = "embedding"
)

// ModelUse is one model alias an analyzer's tasks name, as their payloads
// carry it, and the kind of call it serves.
type ModelUse struct {
	Alias string
	Kind  ModelKind
}

// CacheRequest is one task's request identity and the controls whose text
// the request carries: source and target are the same control for a
// per-control task. The engine maps the controls to their catalogs so a
// cached result is purged with either catalog.
type CacheRequest struct {
	Identity        resultcache.Identity
	SourceControlID string
	TargetControlID string
}

// Cacheable is the optional interface an analyzer implements to opt in to
// LLM result reuse (#243). An analyzer that does not implement it is never
// cached. Implementations must be safe for concurrent use.
type Cacheable interface {
	// CacheModels returns every model alias the analyzer's tasks name, so
	// the pipeline can probe each once per job before any task runs.
	CacheModels() []ModelUse
	// CacheIdentity returns task's request identity, or ok=false for a task
	// that must not be cached. A pre-built task (PreBuiltResult) is never
	// cached, whatever this returns. The identity must cover everything the
	// worker sends (it derives from the payload) and nothing job-specific.
	CacheIdentity(task Task) (req CacheRequest, ok bool)
	// CacheableResult returns nil when result, a worker's successful
	// result for one of the analyzer's tasks, is one Aggregate would use,
	// and otherwise an error saying why it is not. Only such results are
	// stored: one Aggregate discards (an unparseable completion, an empty
	// embedding) would be replayed as the same failure by every later job
	// instead of asking the model again.
	CacheableResult(result *structpb.Struct) error
}

// unwrapper is implemented by the registry's wrapper, which hides the
// registered analyzer behind RegisteredAnalyzer.
type unwrapper interface {
	unwrap() any
}

// AsCacheable returns ra's analyzer as a Cacheable, and false when it does
// not opt in. It sees through the registry's wrapper.
func AsCacheable(ra RegisteredAnalyzer) (Cacheable, bool) {
	if u, ok := ra.(unwrapper); ok {
		c, ok := u.unwrap().(Cacheable)
		return c, ok
	}
	c, ok := ra.(Cacheable)
	return c, ok
}
