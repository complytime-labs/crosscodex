package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// probeLeak is text a failing probe's error carries, which must never
// reach the tenant (CWE-209).
const probeLeak = "SECRET-DRIVER-DETAIL dial tcp 10.0.0.9:4000"

// cacheableControlStub is a per-control analyzer that opts in to result
// reuse: one task per control, cached under its declared model uses.
func cacheableControlStub(name string, uses ...analyzer.ModelUse) *analyzertest.CacheableStub[*pb.Control] {
	stub := &analyzertest.CacheableStub[*pb.Control]{Stub: *newControlStub(name), Models: uses}
	stub.GenerateWorkFunc = func(_ context.Context, c *pb.Control, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
		return []analyzer.Task{{TaskID: name + "-" + c.GetControlId(), TaskType: name}}, nil
	}
	stub.CacheIdentityFunc = func(t analyzer.Task) (analyzer.CacheRequest, bool) {
		sum := sha256.Sum256([]byte(t.TaskID))
		id := strings.TrimPrefix(t.TaskID, name+"-")
		return analyzer.CacheRequest{
			Identity:        resultcache.Identity{TaskType: name, ModelAlias: uses[0].Alias, ContentHash: hex.EncodeToString(sum[:])},
			SourceControlID: id, TargetControlID: id,
		}, true
	}
	return stub
}

// countingCache is the shared result cache fake, counting the lookup calls
// and recording each stage's mode. Its jobs need no registration: the stage
// record is accepted and the checkpoint sealed as given, since these specs
// are about which calls the job makes, not the record's contents.
type countingCache struct {
	*resultcachetest.Fake
	producers atomic.Int32
	lookups   atomic.Int32
	mu        sync.Mutex
	modes     map[string]resultcache.Mode
}

func newCountingCache() *countingCache {
	c := &countingCache{Fake: &resultcachetest.Fake{}, modes: map[string]resultcache.Mode{}}
	c.RecordStageFunc = func(_ context.Context, rec resultcache.StageRecord) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.modes[rec.JobID+"/"+rec.Analyzer] = rec.Stats.Mode
		return nil
	}
	c.CheckpointFunc = func(ctx context.Context, expected resultcache.Manifest, seal func(context.Context, string) error) error {
		return seal(ctx, expected.Digest())
	}
	return c
}

func (c *countingCache) Producers(ctx context.Context, analyzerName string, keys []string) (map[string]string, error) {
	c.producers.Add(1)
	return c.Fake.Producers(ctx, analyzerName, keys)
}

func (c *countingCache) LookupVerifiable(ctx context.Context, analyzerName string, keys []string, attempts []resultcache.AttemptRef) (resultcache.Snapshot, error) {
	c.lookups.Add(1)
	return c.Fake.LookupVerifiable(ctx, analyzerName, keys, attempts)
}

func (c *countingCache) modeOf(jobID, analyzerName string) resultcache.Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modes[jobID+"/"+analyzerName]
}

var _ = Describe("Result cache settings and the model probe", Ordered, func() {
	var gen attestation.Generator

	BeforeAll(func() {
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	var (
		ctx      context.Context
		llm      *llmclienttest.Fake
		embeds   atomic.Int32
		logs     *bytes.Buffer
		reg      *analyzer.Registry
		store    *fakeStore
		cacheCfg config.ResultCacheConfig
		llmCfg   config.LLMConfig
		att      *pipeline.Attestor
		job      *pipeline.Job
		controls []*pb.Control
	)

	completionUse := analyzer.ModelUse{Alias: "", Kind: analyzer.ModelCompletion}
	embeddingUse := analyzer.ModelUse{Alias: "", Kind: analyzer.ModelEmbedding}
	panelUse := analyzer.ModelUse{Alias: "panel-a", Kind: analyzer.ModelCompletion}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		embeds.Store(0)
		llm = &llmclienttest.Fake{
			CompleteFunc: func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return &llmclient.CompletionResponse{Model: "observed-" + req.Model}, nil
			},
			EmbedFunc: func(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				embeds.Add(1)
				return &llmclient.EmbeddingResponse{Model: "observed-" + req.Model}, nil
			},
		}
		logs = &bytes.Buffer{}
		reg = analyzer.NewRegistry()
		Expect(analyzer.Register[*pb.Control](reg, cacheableControlStub("classify", completionUse))).To(Succeed())
		Expect(analyzer.Register[*pb.Control](reg, cacheableControlStub("embedding", embeddingUse))).To(Succeed())
		Expect(analyzer.Register[*pb.Control](reg, cacheableControlStub("artifacts", panelUse, completionUse))).To(Succeed())
		Expect(analyzer.Register[*pb.Control](reg, newControlStub("requires"))).To(Succeed())
		store = newFakeStore()
		cacheCfg = config.ResultCacheConfig{Enabled: true, LookupBatchSize: 500}
		llmCfg = config.LLMConfig{DefaultModel: "default-chat", EmbeddingModel: "default-embed",
			ModelRevisions: map[string]string{"default-chat": "rev-7"}}
		att = &pipeline.Attestor{Generator: gen}
		job = &pipeline.Job{JobID: "job-1", TenantID: "test-tenant", Config: []byte(`{"catalog_id":"cat-1","target_catalog_id":"cat-2"}`)}
		controls = []*pb.Control{{ControlId: "cat-1/ac-1", CatalogId: "cat-1"}, {ControlId: "cat-2/ac-1", CatalogId: "cat-2"}}
	})

	serviceWith := func(opts ...pipeline.Option) *pipeline.Service {
		opts = append([]pipeline.Option{pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil)))}, opts...)
		return pipeline.New(store, nil, reg, nil, pipeline.Attestors{}, nil, nil, &storagetest.Fake{},
			config.PipelineConfig{}, nil, opts...)
	}
	wired := func() *pipeline.Service {
		return serviceWith(pipeline.WithResultCache(&resultcachetest.Fake{}, cacheCfg, llmCfg, llm))
	}
	allAnalyzers := []string{"classify", "embedding", "artifacts", "requires"}
	probeCount := func() int { return len(llm.CompleteRequests()) + int(embeds.Load()) }

	It("records probe durations in milliseconds", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		svc := serviceWith(pipeline.WithResultCache(&resultcachetest.Fake{}, cacheCfg, llmCfg, llm),
			pipeline.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		_, err = svc.PrepareResultCache(ctx, job, att, allAnalyzers, controls)
		Expect(err).NotTo(HaveOccurred())
		m := telemetrytest.FindMetric(tp.GetMetrics(), "pipeline.result_cache.probe.duration_ms")
		Expect(m).NotTo(BeNil())
		Expect(m.Unit).To(Equal("ms"))
	})

	Describe("resolving the job's cache mode", func() {
		DescribeTable("takes the first that applies: not wired, disabled, unattested, --no-cache, enabled",
			func(setup func() (*pipeline.Service, *pipeline.Attestor), want resultcache.Mode, probes int) {
				svc, attestor := setup()
				settings, err := svc.PrepareResultCache(ctx, job, attestor, allAnalyzers, controls)
				Expect(err).NotTo(HaveOccurred())
				Expect(settings.Mode).To(Equal(want))
				Expect(probeCount()).To(Equal(probes))
				if want == "" || want == resultcache.ModeDisabled {
					Expect(settings.Checkpoints).To(BeNil())
					Expect(settings.Models).To(BeEmpty())
				} else {
					Expect(settings.Checkpoints).NotTo(BeNil())
				}
			},
			Entry("without WithResultCache, the cache is off", func() (*pipeline.Service, *pipeline.Attestor) {
				return serviceWith(), att
			}, resultcache.Mode(""), 0),
			Entry("analysis.result_cache.enabled false is disabled, and nothing is probed", func() (*pipeline.Service, *pipeline.Attestor) {
				cacheCfg.Enabled = false
				return wired(), att
			}, resultcache.ModeDisabled, 0),
			Entry("a tenant override disabling it is disabled", func() (*pipeline.Service, *pipeline.Attestor) {
				off := false
				cacheCfg.TenantOverrides = map[string]config.ResultCacheOverride{"test-tenant": {Enabled: &off}}
				return wired(), att
			}, resultcache.ModeDisabled, 0),
			Entry("a tenant whose jobs are not attested gets no cache, and nothing is probed", func() (*pipeline.Service, *pipeline.Attestor) {
				return wired(), nil
			}, resultcache.Mode(""), 0),
			Entry("a job stored with no_cache is bypassed, and still probed", func() (*pipeline.Service, *pipeline.Attestor) {
				job.Config = []byte(`{"catalog_id":"cat-1","no_cache":true}`)
				return wired(), att
			}, resultcache.ModeBypassed, 3),
			Entry("otherwise it is enabled, probing each distinct alias and kind once", func() (*pipeline.Service, *pipeline.Attestor) {
				return wired(), att
			}, resultcache.ModeEnabled, 3),
		)

		It("takes the batch size from the tenant's override, and the catalogs and --no-cache command from the job", func() {
			n := 7
			cacheCfg.TenantOverrides = map[string]config.ResultCacheOverride{"test-tenant": {LookupBatchSize: &n}}
			settings, err := wired().PrepareResultCache(ctx, job, att, allAnalyzers, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.LookupBatchSize).To(Equal(7))
			Expect(settings.ControlCatalogs).To(Equal(map[string]string{"cat-1/ac-1": "cat-1", "cat-2/ac-1": "cat-2"}))
			Expect(settings.NoCacheCommand).To(Equal("crosscodex run start --source cat-1 --target cat-2 --no-cache"))
		})
	})

	Describe("probing", func() {
		It("resolves an empty alias as the worker does, and records each alias's observed model and revision", func() {
			llmCfg.TenantOverrides = map[string]config.LLMOverride{"test-tenant": {DefaultModel: ptr("tenant-chat")}}
			settings, err := wired().PrepareResultCache(ctx, job, att, allAnalyzers, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.Models).To(Equal(map[analyzer.ModelUse]analysis.CacheModel{
				completionUse: {Alias: "tenant-chat", Observed: "observed-tenant-chat"},
				embeddingUse:  {Alias: "default-embed", Observed: "observed-default-embed"},
				panelUse:      {Alias: "panel-a", Observed: "observed-panel-a"},
			}))
		})

		It("names the alias's revision from llm.model_revisions", func() {
			settings, err := wired().PrepareResultCache(ctx, job, att, []string{"classify"}, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.Models[completionUse]).To(Equal(analysis.CacheModel{
				Alias: "default-chat", Observed: "observed-default-chat", Revision: "rev-7"}))
		})

		It("sends a one-token, zero-temperature completion and a fixed embedding, for the job's tenant", func() {
			var embedReq *llmclient.EmbeddingRequest
			llm.EmbedFunc = func(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				embedReq = req
				return &llmclient.EmbeddingResponse{Model: "m"}, nil
			}
			_, err := wired().PrepareResultCache(ctx, job, att, []string{"classify", "embedding"}, controls)
			Expect(err).NotTo(HaveOccurred())
			reqs := llm.CompleteRequests()
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Model).To(Equal("default-chat"))
			Expect(reqs[0].MaxTokens).To(Equal(1))
			Expect(*reqs[0].Temperature).To(BeZero())
			Expect(reqs[0].TenantID).To(Equal("test-tenant"))
			Expect(reqs[0].JobID).To(Equal("job-1"))
			Expect(reqs[0].PromptName).To(Equal(resultcache.ProbePromptName))
			Expect(embedReq).NotTo(BeNil())
			Expect(embedReq.Model).To(Equal("default-embed"))
			Expect(embedReq.Input).To(HaveLen(1))
			Expect(embedReq.TenantID).To(Equal("test-tenant"))
		})

		It("probes only the analyzers it is given, as a resume gives the incomplete ones", func() {
			_, err := wired().PrepareResultCache(ctx, job, att, []string{"embedding"}, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(llm.CompleteRequests()).To(BeEmpty())
			Expect(embeds.Load()).To(BeEquivalentTo(1))
		})

		DescribeTable("leaves an alias uncached, with a fixed reason, when the gateway names no usable model",
			func(model, want string) {
				llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
					return &llmclient.CompletionResponse{Model: model}, nil
				}
				settings, err := wired().PrepareResultCache(ctx, job, att, []string{"classify"}, controls)
				Expect(err).NotTo(HaveOccurred())
				Expect(settings.Models[completionUse]).To(Equal(analysis.CacheModel{Alias: "default-chat", Revision: "rev-7", Reason: want}))
			},
			Entry("no model name", "",
				`the model gateway returned no model name for alias "default-chat", so its calls were neither looked up nor stored`),
			Entry("a name over 256 bytes", strings.Repeat("m", 257),
				`the model gateway returned a model name for alias "default-chat" that is not printable text of at most 256 bytes, so its calls were neither looked up nor stored`),
			Entry("an unprintable name", "model\x1b[31m",
				`the model gateway returned a model name for alias "default-chat" that is not printable text of at most 256 bytes, so its calls were neither looked up nor stored`),
		)

		DescribeTable("takes the deployment into the model's identity, and caches an alias the gateway echoes only when a deployment or revision names its model",
			func(model, deployment string, revisions map[string]string, want analysis.CacheModel) {
				llmCfg.ModelRevisions = revisions
				llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
					return &llmclient.CompletionResponse{Model: model, Deployment: deployment}, nil
				}
				settings, err := wired().PrepareResultCache(ctx, job, att, []string{"classify"}, controls)
				Expect(err).NotTo(HaveOccurred())
				Expect(settings.Models[completionUse]).To(Equal(want))
			},
			Entry("an echoed alias with no deployment and no revision is not cached, naming the setting that would cache it",
				"default-chat", "", nil, analysis.CacheModel{Alias: "default-chat", Reason: `the model gateway named only the alias ` +
					`"default-chat" as the model and sent no deployment ID, so a change of the model behind the alias could not be ` +
					`detected and its calls were neither looked up nor stored. To cache them, set llm.model_revisions.default-chat ` +
					`and change it whenever the model behind the alias changes`}),
			Entry("an echoed alias with a deployment is cached under both",
				"default-chat", "e96b0292", nil, analysis.CacheModel{Alias: "default-chat", Observed: "default-chat", Deployment: "e96b0292"}),
			Entry("an echoed alias with a revision is cached under both",
				"default-chat", "", map[string]string{"default-chat": "rev-7"},
				analysis.CacheModel{Alias: "default-chat", Observed: "default-chat", Revision: "rev-7"}),
			Entry("a concrete model with no deployment is cached under the model",
				"qwen2.5:14b", "", nil, analysis.CacheModel{Alias: "default-chat", Observed: "qwen2.5:14b"}),
			Entry("a concrete model with a deployment is cached under both",
				"qwen2.5:14b", "e96b0292", nil, analysis.CacheModel{Alias: "default-chat", Observed: "qwen2.5:14b", Deployment: "e96b0292"}),
			Entry("an unprintable deployment is not cached",
				"qwen2.5:14b", "e96b\x1b[31m", nil, analysis.CacheModel{Alias: "default-chat", Reason: `the model gateway returned a ` +
					`deployment ID for alias "default-chat" that is not printable text of at most 256 bytes, so its calls were ` +
					`neither looked up nor stored`}),
			Entry("a deployment over 256 bytes is not cached",
				"qwen2.5:14b", strings.Repeat("d", 257), nil, analysis.CacheModel{Alias: "default-chat", Reason: `the model gateway ` +
					`returned a deployment ID for alias "default-chat" that is not printable text of at most 256 bytes, so its ` +
					`calls were neither looked up nor stored`}),
		)

		DescribeTable("counts each probe by outcome: observed when the alias is cached, uncached or empty when it is not",
			func(model, deployment string, revisions map[string]string, want string) {
				llmCfg.ModelRevisions = revisions
				llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
					return &llmclient.CompletionResponse{Model: model, Deployment: deployment}, nil
				}
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
				svc := serviceWith(pipeline.WithResultCache(&resultcachetest.Fake{}, cacheCfg, llmCfg, llm),
					pipeline.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
				_, err = svc.PrepareResultCache(ctx, job, att, []string{"classify"}, controls)
				Expect(err).NotTo(HaveOccurred())
				m := telemetrytest.FindMetric(tp.GetMetrics(), "pipeline.result_cache.probes.total")
				Expect(m).NotTo(BeNil())
				sum, ok := m.Data.(metricdata.Sum[int64])
				Expect(ok).To(BeTrue())
				outcomes := map[string]int64{}
				for _, dp := range sum.DataPoints {
					o, _ := dp.Attributes.Value("outcome")
					outcomes[o.AsString()] += dp.Value
				}
				Expect(outcomes).To(Equal(map[string]int64{want: 1}))
			},
			Entry("a concrete model", "qwen2.5:14b", "", nil, "observed"),
			Entry("an echoed alias with a deployment", "default-chat", "e96b0292", nil, "observed"),
			Entry("an echoed alias with a revision", "default-chat", "", map[string]string{"default-chat": "rev-7"}, "observed"),
			Entry("no model name", "", "", nil, "empty"),
			Entry("an unprintable model name", "m\x1b", "", nil, "empty"),
			Entry("an echoed alias with no deployment and no revision", "default-chat", "", nil, "uncached"),
			Entry("an unprintable deployment", "qwen2.5:14b", "d\x1b", nil, "uncached"),
		)

		It("takes an embedding probe's deployment into the model's identity", func() {
			llm.EmbedFunc = func(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				return &llmclient.EmbeddingResponse{Model: req.Model, Deployment: "0a1b2c3d"}, nil
			}
			settings, err := wired().PrepareResultCache(ctx, job, att, []string{"embedding"}, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.Models[embeddingUse]).To(Equal(analysis.CacheModel{
				Alias: "default-embed", Observed: "default-embed", Deployment: "0a1b2c3d"}))
		})

		It("accepts a printable name of exactly 256 bytes", func() {
			name := strings.Repeat("m", 256)
			llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return &llmclient.CompletionResponse{Model: name}, nil
			}
			settings, err := wired().PrepareResultCache(ctx, job, att, []string{"classify"}, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.Models[completionUse].Observed).To(Equal(name))
		})

		It("leaves an alias uncached, with a fixed reason and no probe, when no default model is configured", func() {
			llmCfg.DefaultModel = ""
			settings, err := wired().PrepareResultCache(ctx, job, att, []string{"classify"}, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.Models[completionUse]).To(Equal(analysis.CacheModel{
				Reason: "no completion model is configured (llm.default_model is empty)"}))
			Expect(llm.CompleteRequests()).To(BeEmpty())
		})

		It("stops with llm_unavailable on the first analyzer using the alias, naming no cause to the tenant", func() {
			llm.EmbedFunc = func(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				return nil, errors.New(probeLeak)
			}
			job.Status = pipeline.JobStatusRunning
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", allAnalyzers)).To(Succeed())
			settings, err := wired().PrepareResultCache(ctx, job, att, allAnalyzers, controls)

			// (1) the class
			Expect(err).To(MatchError(pipeline.ErrLLMUnavailable))
			Expect(pipeline.ClassifyJobError(err)).To(Equal(pipeline.JobErrorLLMUnavailable))
			// (2) an actionable message without the cause
			Expect(err.Error()).To(Equal(`Stage "embedding" stopped: the model gateway did not complete a probe request before analysis, ` +
				`and no analysis LLM calls were made (error class llm_unavailable). Retry this job once the gateway is working ` +
				`(crosscodex run retry job-1). If this persists, give your operator job ID job-1 (tracing is off, so there is no trace ID).`))
			Expect(err.Error()).NotTo(ContainSubstring("SECRET"))
			Expect(settings).To(BeZero())
			// (3) only that stage is marked failed; the cause is in the
			// operator log, once.
			stages, serr := store.GetStages(ctx, "job-1")
			Expect(serr).NotTo(HaveOccurred())
			for _, st := range stages {
				if st.StageName == "embedding" {
					Expect(st.Status).To(Equal(pipeline.StageStatusFailed))
					Expect(st.ErrorMessage).NotTo(ContainSubstring("SECRET"))
				} else {
					Expect(st.Status).To(Equal(pipeline.StageStatusPending), "stage %s", st.StageName)
				}
			}
			Expect(strings.Count(logs.String(), "SECRET-DRIVER-DETAIL")).To(Equal(1))
		})

		It("returns the context's error, not llm_unavailable, when the job is cancelled during the probe", func() {
			job.Status = pipeline.JobStatusRunning
			Expect(store.CreateJob(ctx, job)).To(Succeed())
			Expect(store.CreateStages(ctx, "job-1", allAnalyzers)).To(Succeed())
			cancelled, cancel := context.WithCancel(ctx)
			llm.CompleteFunc = func(c context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				cancel()
				return nil, c.Err()
			}

			_, err := wired().PrepareResultCache(cancelled, job, att, allAnalyzers, controls)
			Expect(err).To(MatchError(context.Canceled))
			Expect(err).NotTo(MatchError(pipeline.ErrLLMUnavailable))
			stages, serr := store.GetStages(ctx, "job-1")
			Expect(serr).NotTo(HaveOccurred())
			Expect(stages).To(HaveEach(HaveField("Status", pipeline.StageStatusPending)), "no stage was marked failed")
			Expect(logs.String()).NotTo(ContainSubstring("llm.gateway_url"))
			Expect(logs.String()).NotTo(ContainSubstring("level=ERROR"))
		})

		It("never puts the gateway's text into a reason", func() {
			llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return &llmclient.CompletionResponse{Model: "\x1b[2J" + probeLeak}, nil
			}
			settings, err := wired().PrepareResultCache(ctx, job, att, allAnalyzers, controls)
			Expect(err).NotTo(HaveOccurred())
			Expect(settings.Models).NotTo(BeEmpty())
			for use, cm := range settings.Models {
				Expect(cm.Reason).NotTo(ContainSubstring("SECRET"), "reason of %v", use)
				Expect(cm.Observed).NotTo(ContainSubstring("SECRET"), "observed model of %v", use)
			}
			Expect(settings.Models[completionUse].Reason).To(ContainSubstring("not printable text"))
		})

		It("fails closed when wired without a probe client", func() {
			svc := serviceWith(pipeline.WithResultCache(&resultcachetest.Fake{}, cacheCfg, llmCfg, nil))
			_, err := svc.PrepareResultCache(ctx, job, att, allAnalyzers, controls)
			Expect(err).To(MatchError(ContainSubstring("without a model probe client")))
		})
	})

	// A job runs end to end: real engine and executor, the result cache
	// fake counting lookups.
	Describe("in a running job", func() {
		var (
			cache      *countingCache
			jobs       *fakeStore
			svc        *pipeline.Service
			dispatcher *executorTestDispatcher
		)

		BeforeEach(func() {
			cache = newCountingCache()
			jobs = newFakeStore()
			jobReg := analyzer.NewRegistry()
			Expect(analyzer.Register[*pb.Control](jobReg, cacheableControlStub("classify", completionUse))).To(Succeed())
			bus := &natsbustest.Fake{}
			dispatcher = &executorTestDispatcher{}
			engine := analysis.New(jobReg, dispatcher, &executorTestCollector{},
				config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
				map[string]natsbus.TaskType{"classify": natsbus.TaskClassify},
				analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), jobs)),
				analysis.WithResultCache(cache))
			svc = pipeline.New(jobs, engine, jobReg, &fakeSynthesisExecutor{}, attestedBy(gen),
				pipelineattestation.NewConverter(), bus, &storagetest.Fake{},
				config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil,
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
				pipeline.WithCatalogControlsReader(&fakeCatalogControlsReader{controls: []*pb.Control{{ControlId: "cat-1/ac-1", CatalogId: "cat-1"}}}),
				pipeline.WithResultCache(cache, cacheCfg, llmCfg, llm),
				pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
			DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })
		})

		finished := func(id string) *pipeline.Job {
			var j *pipeline.Job
			Eventually(func() pipeline.JobStatus {
				j, _ = jobs.GetJob(ctx, id)
				if j == nil {
					return ""
				}
				return j.Status
			}).Should(Or(Equal(pipeline.JobStatusCompleted), Equal(pipeline.JobStatusFailed)))
			return j
		}

		It("looks results up in a job started without --no-cache", func() {
			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"}}))
			Expect(err).NotTo(HaveOccurred())
			Expect(finished(resp.Msg.JobId).Status).To(Equal(pipeline.JobStatusCompleted))
			Expect(cache.producers.Load()).To(BeNumerically(">", 0))
			Expect(cache.modeOf(resp.Msg.JobId, "classify")).To(Equal(resultcache.ModeEnabled))
		})

		It("carries --no-cache from the job's config through the stored job into bypassed mode", func() {
			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1", NoCache: true}}))
			Expect(err).NotTo(HaveOccurred())
			j := finished(resp.Msg.JobId)
			Expect(j.Status).To(Equal(pipeline.JobStatusCompleted))
			var stored pb.JobConfig
			Expect(json.Unmarshal(j.Config, &stored)).To(Succeed())
			Expect(stored.GetNoCache()).To(BeTrue())
			Expect(cache.modeOf(resp.Msg.JobId, "classify")).To(Equal(resultcache.ModeBypassed))
			Expect(cache.producers.Load()).To(BeZero())
			Expect(cache.lookups.Load()).To(BeZero())
		})

		// O10: the retry exception for a corrupt --no-cache job is safe only
		// because such a job never reads the cache.
		It("runs a new-job retry of a corrupt --no-cache job bypassed, reading nothing from the cache", func() {
			now := time.Now()
			configBytes, err := json.Marshal(&pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1", NoCache: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs.CreateJob(ctx, &pipeline.Job{JobID: "job-corrupt", TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
				Config: configBytes, CreatedBy: "test-tenant", CreatedAt: now, UpdatedAt: now})).To(Succeed())
			Expect(jobs.CreateStages(ctx, "job-corrupt", []string{"classify", "synthesis", "graph"})).To(Succeed())
			failure := &analysis.StageError{Analyzer: "classify", Sentinel: resultcache.ErrCorrupt, Message: "corrupt"}
			Expect(jobs.UpdateStageError(ctx, "job-corrupt", "classify", failure)).To(Succeed())
			Expect(jobs.UpdateJobStatus(ctx, "job-corrupt", pipeline.JobStatusFailed, failure)).To(Succeed())

			resp, err := svc.RetryJob(ctx, connect.NewRequest(&pb.RetryJobRequest{JobId: "job-corrupt"}))
			Expect(err).NotTo(HaveOccurred())
			Expect(finished(resp.Msg.NewJobId).Status).To(Equal(pipeline.JobStatusCompleted))
			Expect(cache.modeOf(resp.Msg.NewJobId, "classify")).To(Equal(resultcache.ModeBypassed))
			Expect(cache.producers.Load()).To(BeZero())
			Expect(cache.lookups.Load()).To(BeZero())
		})

		It("fails the job as llm_unavailable before any analysis call when the probe fails", func() {
			llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return nil, errors.New(probeLeak)
			}
			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"}}))
			Expect(err).NotTo(HaveOccurred())
			j := finished(resp.Msg.JobId)
			Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
			Expect(j.ErrorClass).To(Equal(pipeline.JobErrorLLMUnavailable))
			Expect(j.ErrorMessage).To(HavePrefix(`Stage "classify" stopped: the model gateway did not complete a probe request`))
			Expect(j.ErrorMessage).NotTo(ContainSubstring("SECRET"))
			Expect(cache.producers.Load()).To(BeZero())
			Expect(cache.modeOf(resp.Msg.JobId, "classify")).To(BeEmpty(), "the stage never ran")
			Expect(dispatcher.dispatchCount()).To(BeZero(), "no analysis task was dispatched")
		})
	})
})

func ptr[T any](v T) *T { return &v }
