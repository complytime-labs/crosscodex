package analysis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

// Dispatcher publishes analyzer tasks to NATS work subjects. collectionID is
// the CollectionHandle.CollectionID of the collection awaiting the tasks; it
// travels with every task so workers can drop the tasks of an abandoned
// collection (see natsbus.AbandonedCollection).
type Dispatcher interface {
	Dispatch(ctx context.Context, tasks []analyzer.Task, taskType natsbus.TaskType, jobID, collectionID string) error
	Redispatch(ctx context.Context, task analyzer.Task, taskType natsbus.TaskType, jobID, collectionID string, retryCount int) error
}

// ExecutionRequest specifies which analyzers to run and their input.
type ExecutionRequest struct {
	JobID         string
	AnalyzerNames []string
	Input         proto.Message

	// Controls, when non-empty, makes executeAnalyzer call each analyzer's
	// GenerateWork once per control instead of once with Input. Every real
	// analyzer is registered as Analyzer[*pb.Control]; Task.TaskID already
	// embeds the control ID, so per-control task sets never collide.
	Controls []*pb.Control

	// CatalogID is threaded into context (via analyzer.WithCatalogID) before
	// GenerateWork/Aggregate for every analyzer in this request. With
	// Controls, each control's own CatalogId is threaded too (via
	// analyzer.WithControlCatalogs); embedding.Aggregate stores each
	// embedding under its control's catalog and refuses a control without
	// one. CatalogID is its fallback only for a request without Controls.
	CatalogID string

	AnalyzerConfig map[string]analyzer.AnalyzerConfig

	// CompletedOutputs carries outputs of analyzers that already completed in a
	// prior Execute call for this same job (two-pass execution). Their entries
	// are pre-seeded as completed so they satisfy dependency gating for the
	// requested analyzers without being re-executed. A nil map is the common
	// single-pass case.
	CompletedOutputs map[string]*analyzer.Output

	// Cache is how this execution uses the LLM result cache (#243). The
	// zero value (Mode "") neither reads nor writes it and records nothing,
	// as before #243.
	Cache CacheSettings
}

// CacheModel is the model one alias resolved to for one job.
type CacheModel struct {
	// Alias is the alias as the gateway receives it: the task's, or for an
	// empty one the tenant default the worker substitutes.
	Alias string
	// Observed is the probe's resp.Model; empty means the alias is neither
	// looked up nor stored in this job, and Reason says why.
	Observed string
	// Deployment is the probe's deployment ID (llmclient.DeploymentHeader),
	// or "" when the gateway sent none. A result is stored only when its
	// call reported the same deployment.
	Deployment string
	// Revision is llm.model_revisions for Alias, or "".
	Revision string
	Reason   string
}

// CacheSettings is one job's use of the result cache, resolved by the
// pipeline from analysis.result_cache, the job's no_cache and the per-job
// model probe.
type CacheSettings struct {
	// Mode is resultcache.ModeEnabled (look up, reuse, store),
	// ModeBypassed (no_cache: store only) or ModeDisabled (the tenant's
	// kill switch). Disabled and "" make no cache call of any kind; only
	// Disabled reports the analyzers' cache use (ExecutionResult.CacheStats).
	// ModeSkipped is per analyzer and is refused here.
	Mode resultcache.Mode
	// Models maps each model use a cacheable analyzer declares
	// (analyzer.Cacheable.CacheModels) to its resolved model.
	Models map[analyzer.ModelUse]CacheModel
	// LookupBatchSize is how many keys one lookup reads
	// (analysis.result_cache.lookup_batch_size); at least 1 in ModeEnabled.
	LookupBatchSize int
	// ControlCatalogs maps the ControlId of every control of the job to its
	// catalog ID, so a stored pair result is purged with either catalog of
	// its request. A per-control task takes the catalog of the control it
	// was generated from instead.
	ControlCatalogs map[string]string
	// NoCacheCommand is the command that starts a new job like this one
	// with --no-cache, which the cache errors quote. Required in
	// ModeEnabled and ModeBypassed.
	NoCacheCommand string
	// Checkpoints signs and verifies the job's cache checkpoints. Required
	// in ModeEnabled and ModeBypassed: an attempt that cannot be sealed is
	// never trusted, so caching without it would only cost.
	Checkpoints CacheCheckpoints
}

// MetadataCacheAttempt is the analyzer.Output metadata key naming the
// result-cache attempt whose checkpoint sealed a completed stage. The engine
// sets it only when the stage was sealed (ModeEnabled or ModeBypassed); the
// pipeline persists it with the stage's completion so a later run can find
// that checkpoint. It is a pointer, not evidence: readers verify the
// checkpoint it names.
const MetadataCacheAttempt = "result_cache.attempt"

// ExecutionResult holds the outcome of an Engine.Execute call.
type ExecutionResult struct {
	JobID     string
	Outputs   map[string]*analyzer.Output
	Errors    map[string]error
	Completed []string
	Failed    []string
	Skipped   []string
	// CacheStats is the effective result-cache use of each analyzer this
	// call completed, keyed by analyzer: its mode (skipped with its reason
	// when the analyzer was not cached), aliases and counts. Empty when the
	// request had no cache mode. The pipeline records it in the analyzer's
	// attestation link.
	CacheStats map[string]resultcache.Stats
}

// CollectRequest configures a Collector.Collect call.
type CollectRequest struct {
	TaskType    natsbus.TaskType
	JobID       string
	ExpectedIDs []string
	Tasks       []analyzer.Task
	// Timeout is the idle timeout (analysis.engine.task_timeout): the
	// collection fails with ErrTaskTimeout when neither a result nor a
	// worker progress tick arrives for this long.
	Timeout time.Duration
	// StageTimeout is the hard cap (pipeline.stage_timeout) on the whole
	// collection, which ticks do not extend; past it the collection fails
	// with ErrStageTimeout. It must be positive.
	StageTimeout time.Duration
	// SuccessTimeout (analysis.engine.success_timeout) fails the
	// collection with a NoSuccessError once its results have been failures
	// only, with no success, for this long, counted from the first failure
	// after the last success, while its own failures are still arriving
	// (the latest within Timeout). Ticks never reset it. It must be
	// positive.
	SuccessTimeout time.Duration
	MaxRetries     int
	Backoff        time.Duration
	Dispatcher     Dispatcher
}

// CollectionHandle holds the subscription and channels for a two-phase collect operation.
// PrepareCollect establishes the subscription and returns a handle; AwaitResults uses the handle to wait for completion.
type CollectionHandle struct {
	sub natsbus.Subscription
	// progressSub delivers worker progress ticks into progress, a one-slot
	// buffer the AwaitResults loop drains to reset its idle timer.
	progressSub natsbus.Subscription
	progress    chan struct{}
	resultChan  chan analyzer.TaskResult
	doneChan    chan struct{}
	pending     map[string]bool
	mu          *sync.Mutex
	// watch is the collection's run of failures since its last success,
	// guarded by mu, and failed signals each failure to AwaitResults.
	// PrepareCollect always sets both; they are nil on a handle built by a
	// test Collector, which then gets no no-success check.
	watch   *successWatch
	failed  chan struct{}
	subject string
	Req     CollectRequest // Exported for test mocks

	// CollectionID identifies this collection, and only this one, to the
	// workers: PrepareCollect sets it to a fresh random UUID, the engine
	// dispatches every task with it, and an abandon names it. A job that is
	// resumed or retried keeps its job ID but gets new collections, so an
	// abandon from an earlier run cannot drop the new run's tasks. A handle
	// built by a test Collector may leave it empty.
	CollectionID string

	// released is closed by release, which stops retries still waiting on
	// their backoff. release closes it under redispatchMu, which a retry
	// holds while it checks released and publishes, so no retry publishes
	// after release returns. Both are nil on a handle built by a test
	// Collector.
	released     chan struct{}
	redispatchMu *sync.Mutex
	releaseOnce  sync.Once

	// publishAbandon announces this collection as abandoned; abandon calls
	// it at most once. It is nil on a handle built by a test Collector.
	publishAbandon func(ctx context.Context)
	abandonOnce    sync.Once
}

// abandon tells the workers to drop this collection's remaining tasks. It is
// called when the collection ends without every expected result: AwaitResults
// on an idle timeout, the stage cap or cancellation, and the engine when
// Dispatch fails. Both call release first, so no retry of this collection is
// published after the abandon. Only the first call publishes, so each abandoned collection
// sends exactly one control message. A publish failure is logged and counted
// by publishAbandon and never changes the stage's error.
func (h *CollectionHandle) abandon(ctx context.Context) {
	h.abandonOnce.Do(func() {
		if h.publishAbandon != nil {
			h.publishAbandon(ctx)
		}
	})
}

// release removes the handle's result and worker progress subscriptions and
// cancels retries still waiting on their backoff. AwaitResults calls it when
// it returns; a caller that never reaches AwaitResults (a failed Dispatch)
// must call it instead. Only the first call unsubscribes and reports errors;
// later calls return nil. A handle built by a test Collector has no
// subscriptions, and release ignores them.
func (h *CollectionHandle) release() error {
	var err error
	h.releaseOnce.Do(func() {
		if h.released != nil {
			h.redispatchMu.Lock()
			close(h.released)
			h.redispatchMu.Unlock()
		}
		var errs []error
		if h.sub != nil {
			if e := h.sub.Unsubscribe(); e != nil {
				errs = append(errs, fmt.Errorf("unsubscribe from %s: %w", h.subject, e))
			}
		}
		if h.progressSub != nil {
			if e := h.progressSub.Unsubscribe(); e != nil {
				errs = append(errs, fmt.Errorf("unsubscribe from %s: %w", natsbus.WorkerProgressSubject, e))
			}
		}
		err = errors.Join(errs...)
	})
	return err
}
