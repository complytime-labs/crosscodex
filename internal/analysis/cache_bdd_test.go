package analysis_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	cacheTenant    = "test-tenant"
	cacheAnalyzer  = "classify"
	cacheCatalog   = "cat-1"
	observedModel  = "qwen2.5:14b"
	noCacheCommand = "crosscodex run start --source cat-1 --target cat-1 --no-cache"
	// driverText stands in for a database driver's error text, which must
	// never reach a tenant-facing message (CWE-209).
	driverText = "dial tcp 10.0.0.7:5432: connection refused"
)

var defaultUse = analyzer.ModelUse{Alias: "default", Kind: analyzer.ModelCompletion}

// cacheEvents is the order in which the store, the checkpoints and the
// reporter were called, shared by the recording doubles of one spec.
type cacheEvents struct {
	mu     sync.Mutex
	events []string
	// reported is the metadata of each output reported completed.
	reported []map[string]string
}

func (c *cacheEvents) reportedMetadata() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reported)
}

func (c *cacheEvents) add(e string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *cacheEvents) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

func (c *cacheEvents) count(prefix string) int {
	n := 0
	for _, e := range c.list() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// recordingStore embeds the shared fake and records each call, so a spec
// can intercept one method (the hooks) and keep the fake's behaviour for
// the rest.
type recordingStore struct {
	*resultcachetest.Fake
	events *cacheEvents

	mu      sync.Mutex
	entries []resultcache.Entry
	// putErr, if set, is called before each put; a non-nil error fails it
	// without storing anything.
	putErr func(e resultcache.Entry) error
	// afterPut, if set, runs after each put the fake stored; a non-nil
	// error is returned as the put's error (the row stays stored).
	afterPut     func(e resultcache.Entry) error
	producersErr error
	lookupErr    error
	recordErr    error
}

func (s *recordingStore) Producers(ctx context.Context, analyzerName string, keys []string) (map[string]string, error) {
	s.events.add("producers")
	if s.producersErr != nil {
		return nil, s.producersErr
	}
	return s.Fake.Producers(ctx, analyzerName, keys)
}

func (s *recordingStore) LookupVerifiable(ctx context.Context, analyzerName string, keys []string, attempts []resultcache.AttemptRef) (resultcache.Snapshot, error) {
	jobs := make([]string, 0, len(attempts))
	for _, a := range attempts {
		jobs = append(jobs, a.JobID)
	}
	slices.Sort(jobs)
	s.events.add("lookup:" + strings.Join(slices.Compact(jobs), ","))
	if s.lookupErr != nil {
		return resultcache.Snapshot{}, s.lookupErr
	}
	return s.Fake.LookupVerifiable(ctx, analyzerName, keys, attempts)
}

func (s *recordingStore) Put(ctx context.Context, e resultcache.Entry) error {
	s.events.add("put")
	if s.putErr != nil {
		if err := s.putErr(e); err != nil {
			return err
		}
	}
	if err := s.Fake.Put(ctx, e); err != nil {
		return err
	}
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.mu.Unlock()
	if s.afterPut != nil {
		return s.afterPut(e)
	}
	return nil
}

func (s *recordingStore) RecordStage(ctx context.Context, rec resultcache.StageRecord) error {
	s.events.add("record")
	if s.recordErr != nil {
		return s.recordErr
	}
	return s.Fake.RecordStage(ctx, rec)
}

func (s *recordingStore) Checkpoint(ctx context.Context, expected resultcache.Manifest, seal func(ctx context.Context, digest string) error) error {
	s.events.add("checkpoint")
	err := s.Fake.Checkpoint(ctx, expected, seal)
	// The fake holds the job lock for the whole of its Checkpoint, as
	// pgstore holds FOR UPDATE until it commits.
	s.events.add("unlocked")
	return err
}

func (s *recordingStore) storedEntries() []resultcache.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.entries)
}

// recordThenFailStore records the stage as the fake does, then reports an
// error whose outcome is unknown (the commit's reply was lost).
type recordThenFailStore struct {
	*recordingStore
}

func (s *recordThenFailStore) RecordStage(ctx context.Context, rec resultcache.StageRecord) error {
	if err := s.recordingStore.RecordStage(ctx, rec); err != nil {
		return err
	}
	return errors.New(driverText)
}

// analyzerRegistry is a registry holding the rig's stub.
func analyzerRegistry(r *cacheRig) *analyzer.Registry {
	registry := analyzer.NewRegistry()
	Expect(analyzer.Register(registry, analyzer.Analyzer[*emptypb.Empty](r.stub))).To(Succeed())
	return registry
}

// fakeCheckpoints is an in-memory analysis.CacheCheckpoints: Sign returns
// an envelope naming the digest, Store makes it a trusted checkpoint, and
// Verified returns the trusted ones of the producers asked for. A local fake: the interface is internal to this
// package's caller contract (AGENTS.md allows it).
type fakeCheckpoints struct {
	events *cacheEvents

	mu sync.Mutex
	// trusted are checkpoints signed by a key the daemon trusts; untrusted
	// ones by a key it no longer trusts.
	trusted   map[resultcache.AttemptRef]string
	untrusted map[resultcache.AttemptRef]string
	sealed    []resultcache.AttemptRef
	verifyErr error
	signErr   error
	storeErr  error
	// afterVerified, if set, runs after Verified has read the checkpoints
	// and before it returns: the window between verification and lookup.
	afterVerified func()
}

func newFakeCheckpoints(events *cacheEvents) *fakeCheckpoints {
	return &fakeCheckpoints{events: events, trusted: map[resultcache.AttemptRef]string{}, untrusted: map[resultcache.AttemptRef]string{}}
}

func (c *fakeCheckpoints) Verified(ctx context.Context, analyzerName string, producers []string) (analysis.VerifiedCheckpoints, error) {
	if _, err := tenant.FromContext(ctx); err != nil {
		return analysis.VerifiedCheckpoints{}, err
	}
	c.events.add("verified:" + strings.Join(producers, ","))
	c.mu.Lock()
	if c.verifyErr != nil {
		err := c.verifyErr
		c.mu.Unlock()
		return analysis.VerifiedCheckpoints{}, err
	}
	out := analysis.VerifiedCheckpoints{Signed: map[resultcache.AttemptRef]string{}}
	for ref, d := range c.trusted {
		if ref.Analyzer == analyzerName && slices.Contains(producers, ref.JobID) {
			out.Signed[ref] = d
		}
	}
	for ref := range c.untrusted {
		if ref.Analyzer == analyzerName && slices.Contains(producers, ref.JobID) {
			out.Untrusted = append(out.Untrusted, analysis.UntrustedCheckpoint{
				Location: "checkpoints/" + ref.JobID + "/" + ref.Analyzer + "/" + ref.Attempt + ".json",
				KeyIDs:   []string{"retired-key"},
			})
		}
	}
	hook := c.afterVerified
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (c *fakeCheckpoints) Sign(ctx context.Context, ref resultcache.AttemptRef, digest string) ([]byte, error) {
	if _, err := tenant.FromContext(ctx); err != nil {
		return nil, err
	}
	c.events.add("sign")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.signErr != nil {
		return nil, c.signErr
	}
	return []byte(ref.JobID + "/" + ref.Analyzer + "/" + ref.Attempt + "=" + digest), nil
}

func (c *fakeCheckpoints) Store(ctx context.Context, ref resultcache.AttemptRef, signed []byte) error {
	if _, err := tenant.FromContext(ctx); err != nil {
		return err
	}
	c.events.add("store")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return c.storeErr
	}
	prefix := ref.JobID + "/" + ref.Analyzer + "/" + ref.Attempt + "="
	digest, ok := strings.CutPrefix(string(signed), prefix)
	if !ok {
		return fmt.Errorf("fakeCheckpoints: envelope %q was not signed for %v", signed, ref)
	}
	c.trusted[ref] = digest
	c.sealed = append(c.sealed, ref)
	return nil
}

func (c *fakeCheckpoints) sealedRefs() []resultcache.AttemptRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sealed)
}

// distrustAll moves every trusted checkpoint to a key no longer trusted,
// as a key rotation would.
func (c *fakeCheckpoints) distrustAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ref, d := range c.trusted {
		c.untrusted[ref] = d
		delete(c.trusted, ref)
	}
}

// cacheReporter records stage completions and failures.
type cacheReporter struct {
	events *cacheEvents
}

func (r *cacheReporter) ReportStageStarted(context.Context, string, string) error { return nil }

func (r *cacheReporter) ReportStageCompleted(_ context.Context, _, _ string, output *analyzer.Output) error {
	r.events.add("completed")
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	r.events.reported = append(r.events.reported, maps.Clone(output.Metadata))
	return nil
}

func (r *cacheReporter) ReportStageFailed(context.Context, string, string, error) error {
	r.events.add("failed")
	return nil
}

// cacheTask is a task of the cacheable stub: its identity derives from its
// ID, so the same ID in two jobs is the same request.
func cacheTask(id string) analyzer.Task {
	payload, err := structpb.NewStruct(map[string]any{"control_id": "ctl-" + id, "model": "default"})
	Expect(err).NotTo(HaveOccurred())
	return analyzer.Task{TaskID: cacheAnalyzer + "-" + id, TaskType: cacheAnalyzer, Payload: payload}
}

// identityOf is the cacheable stub's CacheIdentity: per-control, naming
// the payload's control.
func identityOf(t analyzer.Task) (analyzer.CacheRequest, bool) {
	control := t.Payload.(*structpb.Struct).GetFields()["control_id"].GetStringValue()
	return analyzer.CacheRequest{
		Identity: resultcache.Identity{TaskType: t.TaskType, ModelAlias: "default",
			ContentHash: strings.Repeat("a", 64-len(t.TaskID)) + t.TaskID},
		SourceControlID: control, TargetControlID: control,
	}, true
}

// answer is a worker result for taskID that names the model that answered.
func answer(taskID, model string) *structpb.Struct {
	fields := map[string]any{"answer": "for " + taskID}
	if model != "" {
		fields["model"] = model
	}
	s, err := structpb.NewStruct(fields)
	Expect(err).NotTo(HaveOccurred())
	return s
}

// cacheRig is one engine wired to the recording doubles.
type cacheRig struct {
	events      *cacheEvents
	fake        *resultcachetest.Fake
	store       *recordingStore
	checkpoints *fakeCheckpoints
	dispatcher  *engineTestDispatcher
	collector   *engineTestCollector
	stub        *analyzertest.CacheableStub[*emptypb.Empty]
	tasks       []analyzer.Task
	aggregated  [][]analyzer.TaskResult
	engine      *analysis.Engine
	ctx         context.Context
	// telemetry records the engine's metrics; its tracer is not wired, so
	// stage errors name the job rather than a trace ID.
	telemetry *telemetrytest.TestProvider
	logs      *gbytes.Buffer
}

func newCacheRig(taskIDs ...string) *cacheRig {
	r := &cacheRig{events: &cacheEvents{}, fake: &resultcachetest.Fake{}, dispatcher: &engineTestDispatcher{}}
	r.store = &recordingStore{Fake: r.fake, events: r.events}
	r.checkpoints = newFakeCheckpoints(r.events)
	r.fake.AddCatalog(cacheTenant, cacheCatalog)
	for _, id := range taskIDs {
		r.tasks = append(r.tasks, cacheTask(id))
	}
	r.collector = &engineTestCollector{resultsFn: func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
		out := make([]analyzer.TaskResult, len(req.ExpectedIDs))
		for i, id := range req.ExpectedIDs {
			out[i] = analyzer.TaskResult{TaskID: id, TaskType: string(req.TaskType), Result: answer(id, observedModel)}
		}
		return out, nil
	}}
	r.stub = &analyzertest.CacheableStub[*emptypb.Empty]{
		Stub:              *newStub(cacheAnalyzer),
		Models:            []analyzer.ModelUse{defaultUse},
		CacheIdentityFunc: identityOf,
	}
	r.stub.GenerateWorkFunc = func(context.Context, *emptypb.Empty, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
		return r.tasks, nil
	}
	r.stub.AggregateFunc = func(_ context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
		r.aggregated = append(r.aggregated, slices.Clone(results))
		return &analyzer.Output{AnalyzerName: cacheAnalyzer, Metadata: map[string]string{}}, nil
	}
	registry := analyzer.NewRegistry()
	Expect(analyzer.Register(registry, analyzer.Analyzer[*emptypb.Empty](r.stub))).To(Succeed())
	var err error
	r.telemetry, err = telemetrytest.NewTestProvider()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { Expect(r.telemetry.Shutdown(context.Background())).To(Succeed()) })
	r.logs = gbytes.NewBuffer()
	r.engine = analysis.New(registry, r.dispatcher, r.collector, validCfg(), allTaskTypes(), r.options()...)
	r.ctx, err = tenant.WithTenant(context.Background(), cacheTenant)
	Expect(err).NotTo(HaveOccurred())
	return r
}

// options are the engine options of the rig, for specs that build their
// own engine over its doubles.
func (r *cacheRig) options() []analysis.Option {
	return []analysis.Option{
		analysis.WithResultCache(r.store), analysis.WithStageReporter(&cacheReporter{events: r.events}),
		analysis.WithTelemetry(nil, r.telemetry.MeterProvider()),
		analysis.WithLogger(slog.New(slog.NewTextHandler(r.logs, nil))),
	}
}

// counted sums the data points of the int64 counter name whose reason
// attribute is reason ("" for every point).
func (r *cacheRig) counted(name, reason string) int64 {
	m := telemetrytest.FindMetric(r.telemetry.GetMetrics(), name)
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	Expect(ok).To(BeTrue(), "%s is an int64 counter", name)
	var n int64
	for _, dp := range sum.DataPoints {
		if v, _ := dp.Attributes.Value("reason"); reason == "" || v.AsString() == reason {
			n += dp.Value
		}
	}
	return n
}

func (r *cacheRig) settings(mode resultcache.Mode) analysis.CacheSettings {
	return analysis.CacheSettings{
		Mode:            mode,
		Models:          map[analyzer.ModelUse]analysis.CacheModel{defaultUse: {Alias: "default", Observed: observedModel}},
		LookupBatchSize: 1000,
		ControlCatalogs: map[string]string{},
		NoCacheCommand:  noCacheCommand,
		Checkpoints:     r.checkpoints,
	}
}

// run executes the stub for jobID, adding the job as running.
func (r *cacheRig) run(jobID string, cache analysis.CacheSettings) *analysis.ExecutionResult {
	r.fake.AddJob(cacheTenant, jobID)
	return r.runAdded(jobID, cache)
}

// runAdded executes the stub for jobID, a job already added.
func (r *cacheRig) runAdded(jobID string, cache analysis.CacheSettings) *analysis.ExecutionResult {
	for _, t := range r.tasks {
		// Every control of the stub's tasks is in cat-1.
		cache.ControlCatalogs[t.Payload.(*structpb.Struct).GetFields()["control_id"].GetStringValue()] = cacheCatalog
	}
	res, err := r.engine.Execute(r.ctx, analysis.ExecutionRequest{JobID: jobID, Input: &emptypb.Empty{}, Cache: cache})
	Expect(err).NotTo(HaveOccurred())
	return res
}

// produce runs jobID once with every result a miss, completes the job,
// and returns the attempt it sealed.
func (r *cacheRig) produce(jobID string) resultcache.AttemptRef {
	res := r.run(jobID, r.settings(resultcache.ModeEnabled))
	Expect(res.Failed).To(BeEmpty())
	r.fake.SetJobStatus(cacheTenant, jobID, "completed")
	refs := r.checkpoints.sealedRefs()
	Expect(refs).NotTo(BeEmpty())
	return refs[len(refs)-1]
}

// manifestOf returns the rows ref holds now.
func (r *cacheRig) manifestOf(ref resultcache.AttemptRef) resultcache.Manifest {
	m, err := r.fake.Manifest(r.ctx, ref.JobID, ref.Analyzer, ref.Attempt)
	Expect(err).NotTo(HaveOccurred())
	return m
}

// producersOf returns who stored each task's cached row now.
func (r *cacheRig) producersOf() map[string]string {
	keys := make([]string, 0, len(r.tasks))
	for _, t := range r.tasks {
		req, _ := identityOf(t)
		keys = append(keys, resultcache.Key(cacheAnalyzer, req.Identity, resultcache.ModelIdentity{Observed: observedModel}))
	}
	p, err := r.fake.Producers(r.ctx, cacheAnalyzer, keys)
	Expect(err).NotTo(HaveOccurred())
	return p
}

// keyOf is the key a task of the stub is stored under.
func keyOf(t analyzer.Task) string {
	req, _ := identityOf(t)
	return resultcache.Key(cacheAnalyzer, req.Identity, resultcache.ModelIdentity{Observed: observedModel})
}

// stageErr returns the stage's error as a StageError.
func stageErr(res *analysis.ExecutionResult) *analysis.StageError {
	err := res.Errors[cacheAnalyzer]
	Expect(err).To(HaveOccurred())
	var se *analysis.StageError
	Expect(errors.As(err, &se)).To(BeTrue(), "the stage error is a StageError: %v", err)
	return se
}

var _ = Describe("Result reuse", func() {
	Describe("Storing and reusing results", func() {
		It("stores every dispatched result under the model that answered, records the stage and seals it", func() {
			rig := newCacheRig("a", "b", "c")
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(1))

			refs := rig.checkpoints.sealedRefs()
			Expect(refs).To(HaveLen(1))
			m := rig.manifestOf(refs[0])
			Expect(m.Entries).To(HaveLen(3))
			for _, e := range m.Entries {
				Expect(e.Role).To(Equal(resultcache.RoleWrite))
			}
			Expect(m.Stats).To(ConsistOf(resultcache.Stats{Mode: resultcache.ModeEnabled,
				Models: map[string]resultcache.ModelIdentity{"default": {Observed: observedModel}},
				Tasks:  3, Dispatched: 3}))
			Expect(rig.checkpoints.trusted[refs[0]]).To(Equal(m.Digest()))
			Expect(res.CacheStats).To(HaveKeyWithValue(cacheAnalyzer, m.Stats[0]))
			Expect(rig.counted("analysis.result_cache.puts.total", "")).To(BeEquivalentTo(3))
			Expect(rig.counted("analysis.result_cache.lookups.total", "")).To(BeEquivalentTo(3))
			Expect(rig.counted("analysis.result_cache.hits.total", "")).To(BeZero())
		})

		It("reuses another job's sealed results as pre-built results without dispatching them", func() {
			rig := newCacheRig("a", "b", "c")
			rig.produce("job-1")
			dispatched := rig.dispatcher.dispatchCount()

			res := rig.run("job-2", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(dispatched))
			Expect(rig.aggregated[1]).To(HaveLen(3))
			for _, r := range rig.aggregated[1] {
				Expect(r.Result.(*structpb.Struct).GetFields()["answer"].GetStringValue()).To(Equal("for " + r.TaskID))
			}

			refs := rig.checkpoints.sealedRefs()
			m := rig.manifestOf(refs[len(refs)-1])
			Expect(m.JobID).To(Equal("job-2"))
			Expect(m.Entries).To(HaveLen(3))
			for _, e := range m.Entries {
				Expect(e.Role).To(Equal(resultcache.RoleHit))
				Expect(e.ProducerJobID).To(Equal("job-1"))
			}
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(3, 3, 0))
			Expect(rig.counted("analysis.result_cache.hits.total", "")).To(BeEquivalentTo(3))
		})

		It("never collects a stage whose every task is cached, so success_timeout cannot fail it", func() {
			rig := newCacheRig("a", "b", "c")
			rig.produce("job-1")
			collects := rig.collector.collectCount()
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				return nil, &analysis.NoSuccessError{JobID: req.JobID, Window: time.Minute}
			}

			res := rig.run("job-2", rig.settings(resultcache.ModeEnabled))

			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.collector.collectCount()).To(Equal(collects))
		})

		It("treats a result whose producer's checkpoint is signed by a key no longer trusted as a miss", func() {
			rig := newCacheRig("a", "b")
			rig.produce("job-1")
			rig.checkpoints.distrustAll()

			res := rig.run("job-2", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(2))
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(2, 0, 2))
			Expect(rig.producersOf()).To(HaveEach("job-2"))
			Expect(rig.counted("analysis.result_cache.integrity_failures.total", "untrusted_key")).To(BeEquivalentTo(1))
			logged := string(rig.logs.Contents())
			Expect(logged).To(And(
				ContainSubstring("signed by a key the daemon does not trust were ignored"),
				ContainSubstring("expected after a signing key rotation; if no rotation happened, the checkpoints may have been altered"),
				ContainSubstring("checkpoints/job-1/"+cacheAnalyzer+"/"),
				ContainSubstring("retired-key")))
		})

		It("stores neither error results nor results that name no model", func() {
			rig := newCacheRig("a", "b", "c")
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				return []analyzer.TaskResult{
					{TaskID: req.ExpectedIDs[0], Result: answer(req.ExpectedIDs[0], observedModel)},
					{TaskID: req.ExpectedIDs[1], Error: errors.New("worker failed")},
					{TaskID: req.ExpectedIDs[2], Result: answer(req.ExpectedIDs[2], "")},
				}, nil
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(HaveLen(1))
			Expect(rig.store.storedEntries()[0].TaskID).To(Equal(rig.tasks[0].TaskID))
		})

		It("reuses a failed stage's sealed results when the job is retried", func() {
			rig := newCacheRig("a", "b")
			rig.stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
				return nil, errors.New("aggregate failed")
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Failed).To(ConsistOf(cacheAnalyzer))
			sealed := rig.checkpoints.sealedRefs()
			Expect(sealed).To(HaveLen(1))
			Expect(rig.manifestOf(sealed[0]).Entries).To(HaveLen(2))
			Expect(rig.manifestOf(sealed[0]).Stats).To(BeEmpty())

			rig.stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
				return &analyzer.Output{AnalyzerName: cacheAnalyzer}, nil
			}
			res = rig.runAdded("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(1))
			refs := rig.checkpoints.sealedRefs()
			m := rig.manifestOf(refs[len(refs)-1])
			Expect(m.Attempt).NotTo(Equal(sealed[0].Attempt))
			for _, e := range m.Entries {
				Expect(e.Role).To(Equal(resultcache.RoleHit))
				Expect(e.ProducerJobID).To(Equal("job-1"))
			}
		})

		It("lets the stage complete when a put is refused, and leaves that write out of the sealed record", func() {
			rig := newCacheRig("a", "b")
			rig.store.putErr = func(e resultcache.Entry) error {
				if e.TaskID == rig.tasks[0].TaskID {
					return fmt.Errorf("put: %w", resultcache.ErrJobNotRunning)
				}
				return nil
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			refs := rig.checkpoints.sealedRefs()
			Expect(refs).To(HaveLen(1))
			m := rig.manifestOf(refs[0])
			Expect(m.Entries).To(HaveLen(1))
			Expect(m.Entries[0].TaskID).To(Equal(rig.tasks[1].TaskID))
			Expect(rig.counted("analysis.result_cache.put_errors.total", "")).To(BeEquivalentTo(1))
			Expect(rig.counted("analysis.result_cache.puts.total", "")).To(BeEquivalentTo(1))
		})

		// Q-I1: results a failed collection already paid for are stored and
		// sealed, so a retry dispatches only the rest.
		It("stores and seals the results a failed collection returned, so a retry dispatches only the rest", func() {
			rig := newCacheRig("a", "b")
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				return []analyzer.TaskResult{{TaskID: req.ExpectedIDs[0], Result: answer(req.ExpectedIDs[0], observedModel)}},
					fmt.Errorf("stage ran longer than pipeline.stage_timeout: %w", analysis.ErrStageTimeout)
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Failed).To(ConsistOf(cacheAnalyzer))
			Expect(res.Errors[cacheAnalyzer]).To(MatchError(analysis.ErrStageTimeout))
			sealed := rig.checkpoints.sealedRefs()
			Expect(sealed).To(HaveLen(1))
			Expect(rig.manifestOf(sealed[0]).Entries).To(HaveLen(1))

			rig.collector.resultsFn = nil
			var retried []string
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				retried = req.ExpectedIDs
				out := make([]analyzer.TaskResult, len(req.ExpectedIDs))
				for i, id := range req.ExpectedIDs {
					out[i] = analyzer.TaskResult{TaskID: id, Result: answer(id, observedModel)}
				}
				return out, nil
			}
			res = rig.runAdded("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(retried).To(HaveLen(1))
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(2, 1, 1))
		})

		It("stores nothing from a collection its cancelled context stopped", func() {
			rig := newCacheRig("a", "b")
			ctx, cancel := context.WithCancel(rig.ctx)
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				cancel()
				return []analyzer.TaskResult{{TaskID: req.ExpectedIDs[0], Result: answer(req.ExpectedIDs[0], observedModel)}}, context.Canceled
			}
			rig.fake.AddJob(cacheTenant, "job-1")
			cache := rig.settings(resultcache.ModeEnabled)
			cache.ControlCatalogs["ctl-a"], cache.ControlCatalogs["ctl-b"] = cacheCatalog, cacheCatalog
			_, err := rig.engine.Execute(ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{}, Cache: cache})
			Expect(err).To(MatchError(context.Canceled))
			Expect(rig.store.storedEntries()).To(BeEmpty())
		})

		It("does not store a successful result the analyzer would discard, and counts and logs the refusal", func() {
			rig := newCacheRig("a", "b")
			rig.stub.CacheableResultFunc = func(r *structpb.Struct) error {
				if r.GetFields()["answer"].GetStringValue() == "for "+rig.tasks[0].TaskID {
					return errors.New("the classification does not parse")
				}
				return nil
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(ConsistOf(HaveField("TaskID", rig.tasks[1].TaskID)))
			Expect(rig.counted("analysis.result_cache.puts_refused.total", "unusable_result")).To(BeEquivalentTo(1))
			Expect(rig.logs).To(gbytes.Say("the classification does not parse"))
		})

		It("does not store a result answered by another model than the probe observed, and counts and logs both models", func() {
			rig := newCacheRig("a", "b")
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				return []analyzer.TaskResult{
					{TaskID: req.ExpectedIDs[0], Result: answer(req.ExpectedIDs[0], "fallback-model")},
					{TaskID: req.ExpectedIDs[1], Result: answer(req.ExpectedIDs[1], observedModel)},
				}, nil
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(ConsistOf(HaveField("TaskID", rig.tasks[1].TaskID)))
			Expect(rig.counted("analysis.result_cache.puts_refused.total", "model_mismatch")).To(BeEquivalentTo(1))
			Expect(rig.logs).To(gbytes.Say(`response_model=fallback-model observed_model=qwen2.5:14b`))
		})

		DescribeTable("stores a result only when its deployment is the one the probe observed",
			func(probed, answered string, stored bool) {
				rig := newCacheRig("a")
				rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
					r := answer(req.ExpectedIDs[0], observedModel)
					if answered != "" {
						r.Fields["deployment"] = structpb.NewStringValue(answered)
					}
					return []analyzer.TaskResult{{TaskID: req.ExpectedIDs[0], Result: r}}, nil
				}
				cache := rig.settings(resultcache.ModeEnabled)
				cache.Models[defaultUse] = analysis.CacheModel{Alias: "default", Observed: observedModel, Deployment: probed}
				res := rig.run("job-1", cache)
				Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
				Expect(res.CacheStats[cacheAnalyzer].Models).To(Equal(map[string]resultcache.ModelIdentity{
					"default": {Observed: observedModel, Deployment: probed}}))
				if !stored {
					Expect(rig.store.storedEntries()).To(BeEmpty())
					Expect(rig.counted("analysis.result_cache.puts_refused.total", "deployment_mismatch")).To(BeEquivalentTo(1))
					Expect(rig.counted("analysis.result_cache.puts_refused.total", "model_mismatch")).To(BeZero())
					// slog's text handler quotes an empty value.
					logged := func(v string) string { return cmp.Or(v, `""`) }
					Expect(rig.logs).To(gbytes.Say(`only results of the deployment the probe reached are stored; ` +
						`to cache every deployment, give each its own alias`))
					Expect(rig.logs).To(gbytes.Say(`response_deployment=` + logged(answered) + ` deployment=` + logged(probed)))
					return
				}
				req, _ := identityOf(rig.tasks[0])
				Expect(rig.store.storedEntries()).To(ConsistOf(HaveField("CacheKey", resultcache.Key(cacheAnalyzer, req.Identity,
					resultcache.ModelIdentity{Observed: observedModel, Deployment: probed}))))
				Expect(rig.counted("analysis.result_cache.puts_refused.total", "")).To(BeZero())
			},
			Entry("the same deployment is stored under the key naming it", "e96b0292", "e96b0292", true),
			Entry("another deployment is refused", "e96b0292", "0a1b2c3d", false),
			Entry("no deployment where the probe saw one is refused", "e96b0292", "", false),
			Entry("a deployment where the probe saw none is refused", "", "0a1b2c3d", false),
		)

		DescribeTable("logs a refused result's gateway-reported model and deployment cut to 256 bytes, saying it was cut",
			func(field string) {
				rig := newCacheRig("a")
				long := strings.Repeat("x", 1000)
				rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
					r := answer(req.ExpectedIDs[0], observedModel)
					r.Fields[field] = structpb.NewStringValue(long)
					return []analyzer.TaskResult{{TaskID: req.ExpectedIDs[0], Result: r}}, nil
				}
				rig.run("job-1", rig.settings(resultcache.ModeEnabled))
				Expect(rig.store.storedEntries()).To(BeEmpty())
				Expect(rig.logs.Contents()).To(ContainSubstring(strings.Repeat("x", 256) + "…(cut from 1000 bytes)"))
				Expect(rig.logs.Contents()).NotTo(ContainSubstring(strings.Repeat("x", 257)))
			},
			Entry("model", "model"),
			Entry("deployment", "deployment"),
		)

		It("cuts a logged value between characters, never inside one", func() {
			rig := newCacheRig("a")
			rig.collector.resultsFn = func(req analysis.CollectRequest) ([]analyzer.TaskResult, error) {
				// 255 bytes, then a 3-byte character across the 256-byte bound.
				return []analyzer.TaskResult{{TaskID: req.ExpectedIDs[0],
					Result: answer(req.ExpectedIDs[0], strings.Repeat("x", 255)+"€tail")}}, nil
			}
			rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(rig.logs.Contents()).To(ContainSubstring("response_model=\"" + strings.Repeat("x", 255) + "…(cut from 262 bytes)\""))
		})

		It("loads each producer's checkpoints before the snapshot that uses them, batch by batch", func() {
			rig := newCacheRig("a", "b")
			rig.produce("job-1")
			rig.tasks = append(rig.tasks, cacheTask("c"))
			rig.tasks = rig.tasks[2:]
			rig.produce("job-2")
			rig.tasks = []analyzer.Task{cacheTask("a"), cacheTask("b"), cacheTask("c")}
			rig.events.mu.Lock()
			rig.events.events = nil
			rig.events.mu.Unlock()

			cache := rig.settings(resultcache.ModeEnabled)
			cache.LookupBatchSize = 1
			res := rig.run("job-3", cache)
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(3, 3, 0))

			verified := map[string]bool{}
			lookups := 0
			for _, e := range rig.events.list() {
				if jobs, ok := strings.CutPrefix(e, "verified:"); ok {
					for _, j := range strings.Split(jobs, ",") {
						verified[j] = true
					}
				}
				if jobs, ok := strings.CutPrefix(e, "lookup:"); ok {
					lookups++
					for _, j := range strings.Split(jobs, ",") {
						Expect(verified).To(HaveKey(j), "job %s's checkpoints were loaded before the snapshot", j)
					}
				}
			}
			Expect(lookups).To(Equal(3))
			Expect(rig.events.count("verified:")).To(Equal(2))
		})

		It("finishes every put and the stage record before sealing, and seals before reporting the stage completed", func() {
			rig := newCacheRig("a", "b")
			rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(rig.events.list()).To(Equal([]string{"producers", "put", "put", "record", "checkpoint", "sign", "unlocked", "store", "completed"}))
		})
	})

	Describe("The completed stage's attempt", func() {
		It("names the sealed attempt in the output reported completed, so the pipeline can persist it with the result", func() {
			rig := newCacheRig("a", "b")
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			refs := rig.checkpoints.sealedRefs()
			Expect(refs).To(HaveLen(1))
			Expect(rig.events.reportedMetadata()).To(ConsistOf(HaveKeyWithValue(analysis.MetadataCacheAttempt, refs[0].Attempt)))
			Expect(res.Outputs[cacheAnalyzer].Metadata).To(HaveKeyWithValue(analysis.MetadataCacheAttempt, refs[0].Attempt))
		})

		It("names the sealed attempt of a stage with no tasks", func() {
			rig := newCacheRig()
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			refs := rig.checkpoints.sealedRefs()
			Expect(refs).To(HaveLen(1))
			Expect(rig.events.reportedMetadata()).To(ConsistOf(HaveKeyWithValue(analysis.MetadataCacheAttempt, refs[0].Attempt)))
		})

		It("names no attempt when the tenant disables the cache, since nothing was sealed", func() {
			rig := newCacheRig("a")
			cache := rig.settings(resultcache.ModeDisabled)
			cache.Checkpoints = nil
			rig.run("job-1", cache)
			Expect(rig.events.reportedMetadata()).To(ConsistOf(Not(HaveKey(analysis.MetadataCacheAttempt))))
		})
	})

	Describe("Modes", func() {
		It("looks nothing up with --no-cache, and still stores, records and seals", func() {
			rig := newCacheRig("a", "b")
			rig.produce("job-1")
			rig.events.mu.Lock()
			rig.events.events = nil
			rig.events.mu.Unlock()

			res := rig.run("job-2", rig.settings(resultcache.ModeBypassed))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.events.list()).To(Equal([]string{"put", "put", "record", "checkpoint", "sign", "unlocked", "store", "completed"}))
			Expect(res.CacheStats[cacheAnalyzer].Mode).To(Equal(resultcache.ModeBypassed))
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(2, 0, 2))
			Expect(rig.producersOf()).To(HaveEach("job-2"))
		})

		It("makes no store or checkpoint call at all when the tenant disables the cache, and reports the mode", func() {
			rig := newCacheRig("a", "b")
			cache := rig.settings(resultcache.ModeDisabled)
			cache.Checkpoints = nil
			res := rig.run("job-1", cache)
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(1))
			Expect(rig.events.list()).To(Equal([]string{"completed"}))
			Expect(res.CacheStats[cacheAnalyzer].Mode).To(Equal(resultcache.ModeDisabled))
			Expect(res.CacheStats[cacheAnalyzer].Reason).To(ContainSubstring("analysis.result_cache.enabled is false"))
		})

		It("makes no store call and reports no cache use for a request without a cache mode", func() {
			rig := newCacheRig("a")
			res := rig.run("job-1", analysis.CacheSettings{ControlCatalogs: map[string]string{}})
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.events.list()).To(Equal([]string{"completed"}))
			Expect(res.CacheStats).To(BeEmpty())
		})

		It("records an analyzer that does not opt in as skipped, with the reason, and seals it", func() {
			rig := newCacheRig()
			plain := newStubWithTasks("embedding", makeTasks("embedding", 2))
			registry := analyzer.NewRegistry()
			Expect(analyzer.Register(registry, analyzer.Analyzer[*emptypb.Empty](plain))).To(Succeed())
			engine := analysis.New(registry, rig.dispatcher, rig.collector, validCfg(), allTaskTypes(), rig.options()...)
			rig.fake.AddJob(cacheTenant, "job-1")
			res, err := engine.Execute(rig.ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{},
				Cache: rig.settings(resultcache.ModeEnabled)})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Completed).To(ConsistOf("embedding"))
			Expect(res.CacheStats["embedding"].Mode).To(Equal(resultcache.ModeSkipped))
			Expect(res.CacheStats["embedding"].Reason).To(ContainSubstring("does not opt in"))
			Expect(rig.events.list()).To(Equal([]string{"record", "checkpoint", "sign", "unlocked", "store", "completed"}))
		})

		It("skips an analyzer whose aliases have no observed model, naming the alias", func() {
			rig := newCacheRig("a")
			cache := rig.settings(resultcache.ModeEnabled)
			cache.Models[defaultUse] = analysis.CacheModel{Alias: "default", Reason: `the gateway named no model for alias "default"`}
			res := rig.run("job-1", cache)
			Expect(res.CacheStats[cacheAnalyzer].Mode).To(Equal(resultcache.ModeSkipped))
			Expect(res.CacheStats[cacheAnalyzer].Reason).To(ContainSubstring(`alias "default"`))
			Expect(rig.store.storedEntries()).To(BeEmpty())
		})

		DescribeTable("refuses settings it cannot honour, before any work",
			func(mutate func(*analysis.CacheSettings, *analysis.Engine) *analysis.Engine, want error) {
				rig := newCacheRig("a")
				cache := rig.settings(resultcache.ModeEnabled)
				engine := mutate(&cache, rig.engine)
				rig.fake.AddJob(cacheTenant, "job-1")
				_, err := engine.Execute(rig.ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{}, Cache: cache})
				Expect(err).To(MatchError(want))
				Expect(rig.events.list()).To(BeEmpty())
				Expect(rig.dispatcher.dispatchCount()).To(BeZero())
			},
			Entry("an unknown mode", func(c *analysis.CacheSettings, e *analysis.Engine) *analysis.Engine {
				c.Mode = "sometimes"
				return e
			}, analysis.ErrInvalidCacheRequest),
			Entry("the per-analyzer skipped mode", func(c *analysis.CacheSettings, e *analysis.Engine) *analysis.Engine {
				c.Mode = resultcache.ModeSkipped
				return e
			}, analysis.ErrInvalidCacheRequest),
			Entry("enabled without checkpoints", func(c *analysis.CacheSettings, e *analysis.Engine) *analysis.Engine {
				c.Checkpoints = nil
				return e
			}, analysis.ErrInvalidCacheRequest),
			Entry("bypassed without checkpoints", func(c *analysis.CacheSettings, e *analysis.Engine) *analysis.Engine {
				c.Mode, c.Checkpoints = resultcache.ModeBypassed, nil
				return e
			}, analysis.ErrInvalidCacheRequest),
			Entry("enabled with a lookup batch size below 1", func(c *analysis.CacheSettings, e *analysis.Engine) *analysis.Engine {
				c.LookupBatchSize = 0
				return e
			}, analysis.ErrInvalidCacheRequest),
			Entry("enabled without the --no-cache command its errors quote", func(c *analysis.CacheSettings, e *analysis.Engine) *analysis.Engine {
				c.NoCacheCommand = ""
				return e
			}, analysis.ErrInvalidCacheRequest),
			Entry("enabled on an engine without a store", func(_ *analysis.CacheSettings, _ *analysis.Engine) *analysis.Engine {
				registry := analyzer.NewRegistry()
				Expect(analyzer.Register(registry, analyzer.Analyzer[*emptypb.Empty](newStub(cacheAnalyzer)))).To(Succeed())
				return analysis.New(registry, &engineTestDispatcher{}, &engineTestCollector{}, validCfg(), allTaskTypes())
			}, analysis.ErrResultCacheNotWired),
		)

		It("accepts disabled mode on an engine without a store or checkpoints", func() {
			registry := analyzer.NewRegistry()
			Expect(analyzer.Register(registry, analyzer.Analyzer[*emptypb.Empty](newStubWithTasks(cacheAnalyzer, makeTasks(cacheAnalyzer, 1))))).To(Succeed())
			engine := analysis.New(registry, &engineTestDispatcher{}, &engineTestCollector{}, validCfg(), allTaskTypes())
			ctx, err := tenant.WithTenant(context.Background(), cacheTenant)
			Expect(err).NotTo(HaveOccurred())
			res, err := engine.Execute(ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{},
				Cache: analysis.CacheSettings{Mode: resultcache.ModeDisabled}})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(res.CacheStats[cacheAnalyzer].Mode).To(Equal(resultcache.ModeDisabled))
		})
	})

	Describe("Catalogs of stored results", func() {
		It("stores a per-control task named by its control's Identifier under that control's catalog", func() {
			rig := newCacheRig()
			stub := &analyzertest.CacheableStub[*pb.Control]{Stub: *newControlStub("artifacts"), Models: []analyzer.ModelUse{defaultUse}}
			stub.GenerateWorkFunc = func(_ context.Context, c *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				payload, err := structpb.NewStruct(map[string]any{"control_id": c.GetIdentifier()})
				Expect(err).NotTo(HaveOccurred())
				return []analyzer.Task{{TaskID: "artifacts-" + c.GetControlId(), TaskType: "artifacts", Payload: payload}}, nil
			}
			stub.CacheIdentityFunc = func(t analyzer.Task) (analyzer.CacheRequest, bool) {
				id := t.Payload.(*structpb.Struct).GetFields()["control_id"].GetStringValue()
				return analyzer.CacheRequest{Identity: resultcache.Identity{TaskType: "artifacts", ModelAlias: "default",
					ContentHash: strings.Repeat("b", 64-len(t.TaskID)) + t.TaskID}, SourceControlID: id, TargetControlID: id}, true
			}
			registry := analyzer.NewRegistry()
			Expect(analyzer.Register(registry, analyzer.Analyzer[*pb.Control](stub))).To(Succeed())
			engine := analysis.New(registry, rig.dispatcher, rig.collector, validCfg(), allTaskTypes(), rig.options()...)
			rig.fake.AddCatalog(cacheTenant, "cat-src")
			rig.fake.AddCatalog(cacheTenant, "cat-tgt")
			rig.fake.AddJob(cacheTenant, "job-1")
			controls := []*pb.Control{
				{ControlId: "src-ac-1", Identifier: "AC-1", CatalogId: "cat-src"},
				{ControlId: "tgt-pr-1", Identifier: "PR-1", CatalogId: "cat-tgt"},
			}
			// ControlCatalogs is keyed by ControlId: the Identifier-named
			// tasks must not need it.
			res, err := engine.Execute(rig.ctx, analysis.ExecutionRequest{JobID: "job-1", Controls: controls,
				Cache: rig.settings(resultcache.ModeEnabled)})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Completed).To(ConsistOf("artifacts"))
			catalogs := map[string][2]string{}
			for _, e := range rig.store.storedEntries() {
				catalogs[e.TaskID] = [2]string{e.SourceCatalogID, e.TargetCatalogID}
			}
			Expect(catalogs).To(Equal(map[string][2]string{
				"artifacts-src-ac-1": {"cat-src", "cat-src"},
				"artifacts-tgt-pr-1": {"cat-tgt", "cat-tgt"},
			}))
		})

		It("stores a pair task under both controls' catalogs", func() {
			rig := newCacheRig()
			payload, err := structpb.NewStruct(map[string]any{"source": "src-ac-1", "target": "tgt-pr-1"})
			Expect(err).NotTo(HaveOccurred())
			rig.tasks = []analyzer.Task{{TaskID: "classify-pair", TaskType: cacheAnalyzer, Payload: payload}}
			rig.stub.CacheIdentityFunc = func(t analyzer.Task) (analyzer.CacheRequest, bool) {
				f := t.Payload.(*structpb.Struct).GetFields()
				return analyzer.CacheRequest{Identity: resultcache.Identity{TaskType: cacheAnalyzer, ModelAlias: "default",
					ContentHash: strings.Repeat("c", 64)}, SourceControlID: f["source"].GetStringValue(),
					TargetControlID: f["target"].GetStringValue()}, true
			}
			rig.fake.AddCatalog(cacheTenant, "cat-src")
			rig.fake.AddCatalog(cacheTenant, "cat-tgt")
			rig.fake.AddJob(cacheTenant, "job-1")
			cache := rig.settings(resultcache.ModeEnabled)
			cache.ControlCatalogs = map[string]string{"src-ac-1": "cat-src", "tgt-pr-1": "cat-tgt"}
			res, err := rig.engine.Execute(rig.ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{}, Cache: cache})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(HaveLen(1))
			Expect(rig.store.storedEntries()[0].SourceCatalogID).To(Equal("cat-src"))
			Expect(rig.store.storedEntries()[0].TargetCatalogID).To(Equal("cat-tgt"))
		})

		It("neither looks up nor stores a task whose control has no known catalog, and says so", func() {
			rig := newCacheRig("a", "b")
			rig.fake.AddJob(cacheTenant, "job-1")
			cache := rig.settings(resultcache.ModeEnabled)
			cache.ControlCatalogs = map[string]string{"ctl-a": cacheCatalog}
			res, err := rig.engine.Execute(rig.ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{}, Cache: cache})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(HaveLen(1))
			Expect(rig.store.storedEntries()[0].TaskID).To(Equal(rig.tasks[0].TaskID))
			Expect(res.CacheStats[cacheAnalyzer].Reason).To(ContainSubstring("1 task was not cached: no catalog is known for its controls"))
		})

		It("neither looks up nor stores tasks that share a task ID, and says so", func() {
			rig := newCacheRig("a", "a", "b")
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(HaveLen(1))
			Expect(rig.store.storedEntries()[0].TaskID).To(Equal(rig.tasks[2].TaskID))
			Expect(res.CacheStats[cacheAnalyzer].Reason).To(ContainSubstring("2 tasks were not cached: their task ID is not unique in the stage"))
		})
	})

	Describe("Negative paths", func() {
		// assertNothingWritten asserts that job-2 stored, recorded and
		// sealed nothing, dispatched nothing, and saved no result, and
		// that job-1's rows are as they were.
		assertNothingWritten := func(rig *cacheRig, before resultcache.Manifest, producers map[string]string, dispatched int) {
			Expect(rig.dispatcher.dispatchCount()).To(Equal(dispatched))
			Expect(rig.events.count("put")).To(BeZero())
			Expect(rig.events.count("record")).To(BeZero())
			Expect(rig.events.count("checkpoint")).To(BeZero())
			Expect(rig.events.count("completed")).To(BeZero())
			Expect(rig.manifestOf(resultcache.AttemptRef{JobID: before.JobID, Analyzer: cacheAnalyzer, Attempt: before.Attempt})).To(Equal(before))
			Expect(rig.producersOf()).To(Equal(producers))
		}

		// primed returns a rig in which job-1 produced and sealed every
		// result, with the event log cleared, and job-2 added as running.
		primed := func() (*cacheRig, resultcache.Manifest, map[string]string, int) {
			rig := newCacheRig("a", "b")
			ref := rig.produce("job-1")
			rig.fake.AddJob(cacheTenant, "job-2")
			rig.events.mu.Lock()
			rig.events.events = nil
			rig.events.mu.Unlock()
			return rig, rig.manifestOf(ref), rig.producersOf(), rig.dispatcher.dispatchCount()
		}

		DescribeTable("stops before any LLM call when the cache cannot be read",
			func(breakIt func(*cacheRig)) {
				rig, before, producers, dispatched := primed()
				breakIt(rig)
				res := rig.runAdded("job-2", rig.settings(resultcache.ModeEnabled))

				se := stageErr(res)
				Expect(se).To(MatchError(resultcache.ErrUnavailable))
				Expect(se).NotTo(MatchError(resultcache.ErrCorrupt))
				Expect(se.Error()).To(And(
					ContainSubstring(`Stage "classify" stopped before making any LLM calls`),
					ContainSubstring("crosscodex run retry job-2"),
					ContainSubstring(noCacheCommand),
					ContainSubstring("job ID job-2 (tracing is off, so there is no trace ID)")))
				Expect(se.Error()).NotTo(ContainSubstring(driverText))
				assertNothingWritten(rig, before, producers, dispatched)
				Expect(rig.counted("analysis.result_cache.lookups.total", "")).To(BeEquivalentTo(4), "job-1's 2 keys and job-2's 2")
				Expect(telemetrytest.FindMetric(rig.telemetry.GetMetrics(), "analysis.result_cache.lookup.duration_ms")).NotTo(BeNil())
			},
			Entry("the producers cannot be read", func(rig *cacheRig) {
				rig.store.producersErr = errors.New(driverText)
			}),
			Entry("the producers' checkpoints cannot be loaded", func(rig *cacheRig) {
				rig.checkpoints.verifyErr = errors.New(driverText)
			}),
			Entry("the snapshot cannot be read", func(rig *cacheRig) {
				rig.store.lookupErr = fmt.Errorf("%w: %s", resultcache.ErrUnavailable, driverText)
			}),
			Entry("the snapshot is missing an attempt it was asked for, a programming error", func(rig *cacheRig) {
				rig.fake.LookupVerifiableFunc = func(ctx context.Context, a string, keys []string, refs []resultcache.AttemptRef) (resultcache.Snapshot, error) {
					hits := map[string]resultcache.Hit{}
					for _, k := range keys {
						hits[k] = resultcache.Hit{CacheKey: k, Result: []byte(`{"model":"m"}`), ProducerJobID: "job-1", ProducerAttempt: refs[0].Attempt}
					}
					return resultcache.NewSnapshot(a, hits, nil), nil
				}
			}),
		)

		DescribeTable("stops before any LLM call when a cached result fails its integrity check",
			func(breakIt func(*cacheRig), reason string, failures int) {
				rig, _, _, dispatched := primed()
				breakIt(rig)
				// The tampering changed job-1's rows; what must not change
				// is anything of job-2's, and job-1's rows from here on.
				refs := rig.checkpoints.sealedRefs()
				producers := rig.producersOf()
				res := rig.runAdded("job-2", rig.settings(resultcache.ModeEnabled))

				se := stageErr(res)
				Expect(se).To(MatchError(resultcache.ErrCorrupt))
				Expect(se.Error()).To(And(
					ContainSubstring(`Stage "classify" stopped: a cached result failed its integrity check`),
					ContainSubstring("Start a new job with --no-cache, which recomputes and replaces the cached results: "+noCacheCommand),
					ContainSubstring("job ID job-2 (tracing is off, so there is no trace ID)")))
				Expect(se.Error()).NotTo(ContainSubstring(driverText))
				Expect(rig.dispatcher.dispatchCount()).To(Equal(dispatched))
				Expect(rig.events.count("put")).To(BeZero())
				Expect(rig.events.count("record")).To(BeZero())
				Expect(rig.events.count("completed")).To(BeZero())
				Expect(rig.checkpoints.sealedRefs()).To(Equal(refs))
				Expect(rig.producersOf()).To(Equal(producers))
				Expect(rig.counted("analysis.result_cache.integrity_failures.total", reason)).To(BeEquivalentTo(failures))
			},
			Entry("a producer's checkpoint has an invalid signature under a trusted key", func(rig *cacheRig) {
				rig.checkpoints.verifyErr = fmt.Errorf("%w: %s", resultcache.ErrCorrupt, driverText)
			}, "checkpoint", 1),
			Entry("a row is planted in the producer's signed attempt between verification and lookup", func(rig *cacheRig) {
				ref := rig.checkpoints.sealedRefs()[0]
				rig.checkpoints.afterVerified = func() {
					rig.fake.SetJobStatus(cacheTenant, "job-1", "running")
					Expect(rig.fake.Put(rig.ctx, resultcache.Entry{JobID: "job-1", Analyzer: cacheAnalyzer, Attempt: ref.Attempt,
						TaskID: rig.tasks[0].TaskID, CacheKey: keyOf(rig.tasks[0]), Result: []byte(`{"model":"evil","answer":"forged"}`),
						ResponseModel: "evil", SourceCatalogID: cacheCatalog, TargetCatalogID: cacheCatalog})).To(Succeed())
					rig.fake.SetJobStatus(cacheTenant, "job-1", "completed")
				}
			}, "result", 2), // both keys of the tampered attempt
			Entry("a vouched-for result does not decode", func(rig *cacheRig) {
				rig.fake.SetJobStatus(cacheTenant, "job-1", "running")
				ref := resultcache.AttemptRef{JobID: "job-1", Analyzer: cacheAnalyzer, Attempt: "undecodable"}
				Expect(rig.fake.Put(rig.ctx, resultcache.Entry{JobID: "job-1", Analyzer: cacheAnalyzer, Attempt: ref.Attempt,
					TaskID: rig.tasks[0].TaskID, CacheKey: keyOf(rig.tasks[0]), Result: []byte(`{"answer":"no model"}`),
					ResponseModel: "m", SourceCatalogID: cacheCatalog, TargetCatalogID: cacheCatalog})).To(Succeed())
				rig.fake.SetJobStatus(cacheTenant, "job-1", "completed")
				rig.checkpoints.trusted[ref] = rig.manifestOf(ref).Digest()
			}, "result", 1),
		)

		// A1: the producer re-stored the key in an attempt it never sealed
		// (a crash, or a planted row). Nothing signed was changed, so it is
		// a miss to recompute, not corruption.
		It("treats a result its producer re-stored under an unsealed attempt as a miss", func() {
			rig, _, _, dispatched := primed()
			rig.fake.SetJobStatus(cacheTenant, "job-1", "running")
			Expect(rig.fake.Put(rig.ctx, resultcache.Entry{JobID: "job-1", Analyzer: cacheAnalyzer, Attempt: "unsealed",
				TaskID: rig.tasks[0].TaskID, CacheKey: keyOf(rig.tasks[0]), Result: []byte(`{"model":"evil","answer":"forged"}`),
				ResponseModel: "evil", SourceCatalogID: cacheCatalog, TargetCatalogID: cacheCatalog})).To(Succeed())
			rig.fake.SetJobStatus(cacheTenant, "job-1", "completed")

			res := rig.runAdded("job-2", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(2, 1, 1))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(dispatched + 1))
			Expect(rig.producersOf()[keyOf(rig.tasks[0])]).To(Equal("job-2"))
		})

		It("treats a result another job stored between verification and lookup as a miss", func() {
			rig, _, _, dispatched := primed()
			rig.fake.AddJob(cacheTenant, "job-x")
			rig.checkpoints.afterVerified = func() {
				Expect(rig.fake.Put(rig.ctx, resultcache.Entry{JobID: "job-x", Analyzer: cacheAnalyzer, Attempt: "unsigned",
					TaskID: rig.tasks[0].TaskID, CacheKey: keyOf(rig.tasks[0]), Result: []byte(`{"model":"evil","answer":"forged"}`),
					ResponseModel: "evil", SourceCatalogID: cacheCatalog, TargetCatalogID: cacheCatalog})).To(Succeed())
			}
			res := rig.runAdded("job-2", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(2, 1, 1))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(dispatched + 1))
			Expect(rig.producersOf()[keyOf(rig.tasks[0])]).To(Equal("job-2"))
		})

		It("recomputes a result planted under a made-up attempt of the running job itself, and never reuses it", func() {
			rig := newCacheRig("a", "b")
			rig.fake.AddJob(cacheTenant, "job-1")
			forged := []byte(`{"model":"evil","answer":"forged"}`)
			Expect(rig.fake.Put(rig.ctx, resultcache.Entry{JobID: "job-1", Analyzer: cacheAnalyzer, Attempt: "made-up",
				TaskID: rig.tasks[0].TaskID, CacheKey: keyOf(rig.tasks[0]), Result: forged,
				ResponseModel: "evil", SourceCatalogID: cacheCatalog, TargetCatalogID: cacheCatalog})).To(Succeed())

			res := rig.runAdded("job-1", rig.settings(resultcache.ModeEnabled))
			Expect(res.Completed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.dispatcher.dispatchCount()).To(Equal(1))
			Expect(res.CacheStats[cacheAnalyzer]).To(statsCounts(2, 0, 2))
			m := rig.manifestOf(rig.checkpoints.sealedRefs()[0])
			for _, e := range m.Entries {
				Expect(e.Role).To(Equal(resultcache.RoleWrite))
				Expect(e.ResultDigest).NotTo(Equal(resultcache.ResultDigest(forged)))
			}
		})

		It("refuses to seal, and saves nothing, when the database holds a row the stage did not write", func() {
			rig := newCacheRig("a", "b")
			var planted resultcache.Entry
			rig.store.afterPut = func(e resultcache.Entry) error {
				if planted.JobID != "" {
					return nil
				}
				planted = resultcache.Entry{JobID: e.JobID, Analyzer: e.Analyzer, Attempt: e.Attempt, TaskID: "planted",
					CacheKey: strings.Repeat("f", 64), Result: []byte(`{"model":"evil"}`), ResponseModel: "evil",
					SourceCatalogID: cacheCatalog, TargetCatalogID: cacheCatalog}
				return rig.fake.Put(rig.ctx, planted)
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))

			se := stageErr(res)
			Expect(se).To(MatchError(resultcache.ErrCorrupt))
			Expect(se.Error()).To(And(
				ContainSubstring(`Stage "classify" stopped: the database holds result-cache records for this stage run that the stage did not write`),
				ContainSubstring(noCacheCommand),
				ContainSubstring("job ID job-1 (tracing is off, so there is no trace ID)")))
			Expect(rig.checkpoints.sealedRefs()).To(BeEmpty())
			Expect(rig.events.count("completed")).To(BeZero())
			Expect(rig.counted("analysis.result_cache.integrity_failures.total", "stage_record")).To(BeEquivalentTo(1))
			// The rows are as the stage and the planter left them.
			m := rig.manifestOf(resultcache.AttemptRef{JobID: "job-1", Analyzer: cacheAnalyzer, Attempt: planted.Attempt})
			Expect(m.Entries).To(HaveLen(3))
			Expect(m.Stats).To(HaveLen(1))
		})

		It("fails as unavailable, not corrupt, when a put whose outcome is unknown left a row the record lacks", func() {
			rig := newCacheRig("a", "b")
			rig.store.afterPut = func(e resultcache.Entry) error {
				if e.TaskID == rig.tasks[0].TaskID {
					return errors.New(driverText)
				}
				return nil
			}
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))

			se := stageErr(res)
			Expect(se).To(MatchError(resultcache.ErrUnavailable))
			Expect(se).NotTo(MatchError(resultcache.ErrCorrupt))
			Expect(se.Error()).To(And(
				ContainSubstring("could not be sealed, so its results were not saved"),
				ContainSubstring("crosscodex run retry job-1")))
			Expect(se.Error()).NotTo(ContainSubstring(driverText))
			Expect(rig.checkpoints.sealedRefs()).To(BeEmpty())
			Expect(rig.events.count("completed")).To(BeZero())
			Expect(rig.counted("analysis.result_cache.integrity_failures.total", "stage_record_uncertain")).To(BeEquivalentTo(1))
			Expect(rig.counted("analysis.result_cache.integrity_failures.total", "stage_record")).To(BeZero(), "not a tamper alarm")
		})

		// Q-M8: RecordStage committed but reported an error, so the failure
		// seal finds rows the record lacks; that is not tampering.
		It("fails as unavailable when a stage record whose outcome is unknown leaves rows the failure seal does not expect", func() {
			rig := newCacheRig("a")
			rig.store.recordErr = nil
			recordAndFail := &recordThenFailStore{recordingStore: rig.store}
			rig.engine = analysis.New(analyzerRegistry(rig), rig.dispatcher, rig.collector, validCfg(), allTaskTypes(),
				append(rig.options(), analysis.WithResultCache(recordAndFail))...)
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))

			se := stageErr(res)
			Expect(se).To(MatchError(resultcache.ErrUnavailable))
			Expect(se).NotTo(MatchError(resultcache.ErrCorrupt))
			Expect(se.Error()).To(ContainSubstring("Retry this job once the database is reachable: crosscodex run retry job-1"))
			Expect(rig.checkpoints.sealedRefs()).To(BeEmpty())
			Expect(rig.counted("analysis.result_cache.integrity_failures.total", "stage_record_uncertain")).To(BeEquivalentTo(1))
		})

		// Q-M8: sealing a failed stage is best effort; the stage's own error
		// is what the job reports.
		DescribeTable("keeps a failed stage's own error when its writes cannot be sealed",
			func(fail func(*fakeCheckpoints)) {
				rig := newCacheRig("a")
				rig.stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
					return nil, errors.New("aggregate failed")
				}
				fail(rig.checkpoints)
				res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))
				Expect(res.Errors[cacheAnalyzer]).To(MatchError(And(ContainSubstring("aggregate failed"), Not(ContainSubstring(driverText)))))
				Expect(res.Errors[cacheAnalyzer]).To(MatchError(analysis.ErrAnalyzerFailed))
				Expect(res.Errors[cacheAnalyzer]).NotTo(MatchError(resultcache.ErrUnavailable))
				Expect(rig.checkpoints.sealedRefs()).To(BeEmpty())
				Expect(rig.logs).To(gbytes.Say("could not be sealed, so a later run recomputes them"))
			},
			Entry("the checkpoint cannot be signed", func(c *fakeCheckpoints) { c.signErr = errors.New(driverText) }),
			Entry("the signed checkpoint cannot be stored", func(c *fakeCheckpoints) { c.storeErr = errors.New(driverText) }),
		)

		It("stores the signed checkpoint only after the job lock is released", func() {
			rig := newCacheRig("a")
			rig.checkpoints.storeErr = errors.New(driverText)
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))

			Expect(stageErr(res)).To(MatchError(resultcache.ErrUnavailable))
			events := rig.events.list()
			Expect(events).To(ContainElements("sign", "unlocked", "store"))
			Expect(slices.Index(events, "sign")).To(BeNumerically("<", slices.Index(events, "unlocked")),
				"signing runs under the lock")
			Expect(slices.Index(events, "unlocked")).To(BeNumerically("<", slices.Index(events, "store")),
				"a slow or failing store must not hold the job lock")
		})

		DescribeTable("fails as unavailable, saving nothing, when the stage cannot be sealed",
			func(fail func(*fakeCheckpoints)) {
				rig := newCacheRig("a")
				fail(rig.checkpoints)
				res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))

				se := stageErr(res)
				Expect(se).To(MatchError(resultcache.ErrUnavailable))
				Expect(se).NotTo(MatchError(resultcache.ErrCorrupt))
				Expect(se.Error()).To(And(
					ContainSubstring(`Stage "classify" stopped after its LLM calls: its result-cache record could not be sealed`),
					ContainSubstring("crosscodex run retry job-1"),
					ContainSubstring("job ID job-1 (tracing is off, so there is no trace ID)")))
				Expect(se.Error()).NotTo(ContainSubstring(driverText))
				Expect(rig.checkpoints.sealedRefs()).To(BeEmpty())
				Expect(rig.events.count("completed")).To(BeZero())
			},
			Entry("the signer fails", func(c *fakeCheckpoints) { c.signErr = errors.New(driverText) }),
			Entry("the signer's error wraps ErrCorrupt", func(c *fakeCheckpoints) {
				c.signErr = fmt.Errorf("%w: %s", resultcache.ErrCorrupt, driverText)
			}),
			Entry("the signed checkpoint cannot be stored", func(c *fakeCheckpoints) { c.storeErr = errors.New(driverText) }),
			Entry("the store's error wraps ErrCorrupt", func(c *fakeCheckpoints) {
				c.storeErr = fmt.Errorf("%w: %s", resultcache.ErrCorrupt, driverText)
			}),
		)

		It("fails as unavailable when the stage record cannot be written, and seals the writes so a retry reuses them", func() {
			rig := newCacheRig("a", "b")
			rig.store.recordErr = fmt.Errorf("record stage: %w", resultcache.ErrJobNotRunning)
			res := rig.run("job-1", rig.settings(resultcache.ModeEnabled))

			se := stageErr(res)
			Expect(se).To(MatchError(resultcache.ErrUnavailable))
			Expect(se.Error()).To(And(
				ContainSubstring(`Stage "classify" stopped after its LLM calls: its result-cache audit entries could not be written`),
				ContainSubstring("retrying this job reuses them: crosscodex run retry job-1")))
			Expect(rig.events.count("completed")).To(BeZero())
			refs := rig.checkpoints.sealedRefs()
			Expect(refs).To(HaveLen(1))
			m := rig.manifestOf(refs[0])
			Expect(m.Entries).To(HaveLen(2))
			Expect(m.Stats).To(BeEmpty())
		})

		It("leaves the attempt unsealed when the stage stops because its context was cancelled", func() {
			rig := newCacheRig("a")
			ctx, cancel := context.WithCancel(rig.ctx)
			rig.ctx = ctx
			rig.stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
				cancel()
				return nil, context.Canceled
			}
			rig.fake.AddJob(cacheTenant, "job-1")
			cache := rig.settings(resultcache.ModeEnabled)
			cache.ControlCatalogs["ctl-a"] = cacheCatalog
			res, err := rig.engine.Execute(ctx, analysis.ExecutionRequest{JobID: "job-1", Input: &emptypb.Empty{}, Cache: cache})
			Expect(err).To(MatchError(context.Canceled))
			Expect(res.Failed).To(ConsistOf(cacheAnalyzer))
			Expect(rig.store.storedEntries()).To(HaveLen(1))
			Expect(rig.events.count("checkpoint")).To(BeZero())
			Expect(rig.checkpoints.sealedRefs()).To(BeEmpty())
		})
	})

	Describe("TraceReference", func() {
		It("names the trace ID when the context carries a span", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			ctx, span := tp.TracerProvider().Tracer("t").Start(context.Background(), "s")
			defer span.End()
			Expect(analysis.TraceReference(ctx, "job-1")).To(Equal("trace ID " + span.SpanContext().TraceID().String()))
		})

		It("names the job when tracing is off", func() {
			Expect(analysis.TraceReference(context.Background(), "job-1")).To(Equal("job ID job-1 (tracing is off, so there is no trace ID)"))
		})
	})
})

// statsCounts matches Stats' tasks, hits and dispatched counts.
func statsCounts(tasks, hits, dispatched int) OmegaMatcher {
	return WithTransform(func(s resultcache.Stats) [3]int { return [3]int{s.Tasks, s.Hits, s.Dispatched} },
		Equal([3]int{tasks, hits, dispatched}))
}
