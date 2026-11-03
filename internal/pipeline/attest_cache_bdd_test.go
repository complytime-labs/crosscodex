package pipeline_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// attestCacheJob is the job the result-cache attestation specs run.
const attestCacheJob = "job-attest-cache"

// checkpointReadFailer is object storage whose checkpoint reads fail while
// failing is set; everything else is the shared fake's.
type checkpointReadFailer struct {
	*storagetest.Fake
	failing atomic.Bool
}

func (f *checkpointReadFailer) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.failing.Load() && strings.Contains(key, "/cache-checkpoints/") {
		return nil, errors.New("object storage unreachable")
	}
	return f.Fake.Get(ctx, key)
}

// noCacheRecord is the cache_manifest material of a stage no checkpoint
// sealed.
var noCacheRecord = resultcache.Manifest{}.Digest()

var _ = Describe("Attesting a job's result-cache use", Ordered, func() {
	var gen attestation.Generator

	BeforeAll(func() {
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	var (
		ctx      context.Context
		store    *fakeStore
		cache    *resultcachetest.Fake
		objects  *checkpointReadFailer
		cacheCfg config.ResultCacheConfig
		signer   attestation.Generator
		// wired is whether the service wires the result cache.
		wired bool
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		store = newFakeStore()
		cache = &resultcachetest.Fake{}
		cache.AddCatalog("test-tenant", "cat-1")
		objects = &checkpointReadFailer{Fake: &storagetest.Fake{}}
		cacheCfg = config.ResultCacheConfig{Enabled: true, LookupBatchSize: 10}
		signer = gen
		wired = true
		now := time.Now()
		Expect(store.CreateJob(ctx, &pipeline.Job{JobID: attestCacheJob, TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
			Config: []byte(`{"catalog_id":"cat-1","target_catalog_id":"cat-1"}`), CreatedBy: "user-a",
			CreatedAt: now, UpdatedAt: now})).To(Succeed())
		Expect(store.CreateStages(ctx, attestCacheJob, []string{"classify", "requires", "synthesis", "graph"})).To(Succeed())
		cache.AddJob("test-tenant", attestCacheJob)
	})

	// run resumes the job (it is running) with a cacheable classify over
	// one control, signing with signer, and returns it once it ends.
	run := func() *pipeline.Job {
		reg := analyzer.NewRegistry()
		Expect(analyzer.Register[*pb.Control](reg, cacheableControlStub("classify",
			analyzer.ModelUse{Alias: "", Kind: analyzer.ModelCompletion}))).To(Succeed())
		// requires runs in the candidate-dependent pass, after classify's.
		Expect(analyzer.Register[*pb.Control](reg, newControlStub("requires", "classify"))).To(Succeed())
		bus := &natsbustest.Fake{}
		llm := &llmclienttest.Fake{CompleteFunc: func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return &llmclient.CompletionResponse{Model: "qwen-observed"}, nil
		}}
		eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{},
			config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
			map[string]natsbus.TaskType{"classify": natsbus.TaskClassify, "requires": natsbus.TaskRequires},
			analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)),
			analysis.WithResultCache(cache))
		opts := []pipeline.Option{
			pipeline.WithCatalogNameReader(serviceTestCatalogs),
			pipeline.WithCatalogControlsReader(&fakeCatalogControlsReader{controls: []*pb.Control{{ControlId: "c1", CatalogId: "cat-1", Statement: "s"}}}),
		}
		if wired {
			opts = append(opts, pipeline.WithResultCache(cache, cacheCfg, config.LLMConfig{DefaultModel: "default-chat"}, llm))
		}
		svc := pipeline.New(store, eng, reg, &fakeSynthesisExecutor{}, attestedBy(signer), pipelineattestation.NewConverter(), bus,
			objects, config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil, opts...)
		Expect(svc.Start(ctx)).To(Succeed())
		var j *pipeline.Job
		Eventually(func() pipeline.JobStatus {
			j, _ = store.GetJob(ctx, attestCacheJob)
			if j == nil {
				return ""
			}
			return j.Status
		}).Should(BeElementOf(pipeline.JobStatusCompleted, pipeline.JobStatusFailed))
		Expect(svc.Stop(context.Background())).To(Succeed())
		return j
	}
	// rerun makes the completed job run again from synthesis, as a retry
	// from failure would: classify completed in the earlier run.
	rerun := func() *pipeline.Job {
		Expect(store.UpdateJobStatus(ctx, attestCacheJob, pipeline.JobStatusRunning, nil)).To(Succeed())
		Expect(store.ResetStagesFrom(ctx, attestCacheJob, "synthesis", []string{"classify", "requires", "synthesis", "graph"})).To(Succeed())
		cache.SetJobStatus("test-tenant", attestCacheJob, "running")
		return run()
	}
	classifyStage := func() *pipeline.Stage {
		stages, err := store.GetStages(ctx, attestCacheJob)
		Expect(err).NotTo(HaveOccurred())
		for _, st := range stages {
			if st.StageName == "classify" {
				return st
			}
		}
		Fail("the job has no classify stage")
		return nil
	}
	setClassifyAttempt := func(attempt string) {
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, st := range store.stages[attestCacheJob] {
			if st.StageName == "classify" {
				st.CacheAttempt = attempt
			}
		}
	}
	classifyLink := func() *attestation.VerifiedLink {
		raw, ok := objects.Objects()["attestations/test-tenant/"+attestCacheJob+"/links/classify.json"]
		Expect(ok).To(BeTrue(), "the classify link is stored")
		v, err := signer.Verify(ctx, raw)
		Expect(err).NotTo(HaveOccurred())
		return v
	}
	cacheMaterial := func(v *attestation.VerifiedLink) string {
		for _, m := range v.Materials {
			if m.URI == "classify.cache_manifest" {
				return m.Digest
			}
		}
		Fail("the classify link pins no cache manifest")
		return ""
	}
	sealedDigest := func(attempt string) string {
		m, err := cache.Manifest(ctx, attestCacheJob, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		return m.Digest()
	}

	It("pins the checkpoint of the attempt that completed the analyzer, and signs its mode and attempt", func() {
		j := run()
		Expect(j.Status).To(Equal(pipeline.JobStatusCompleted), j.ErrorMessage)
		attempt := classifyStage().CacheAttempt
		Expect(uuid.Parse(attempt)).Error().NotTo(HaveOccurred())
		Expect(objects.Objects()).To(HaveKey(pipeline.CheckpointKey("test-tenant", attestCacheJob, "classify", attempt)))

		link := classifyLink()
		Expect(cacheMaterial(link)).To(Equal(sealedDigest(attempt)))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.mode", "enabled"))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.attempt", attempt))
		Expect(link.ByProducts).To(HaveKey("result_cache.reason"))
	})

	It("signs disabled mode and pins the no-record material when the tenant disables the cache", func() {
		cacheCfg.Enabled = false
		j := run()
		Expect(j.Status).To(Equal(pipeline.JobStatusCompleted), j.ErrorMessage)
		Expect(classifyStage().CacheAttempt).To(BeEmpty())
		link := classifyLink()
		Expect(cacheMaterial(link)).To(Equal(noCacheRecord))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.mode", "disabled"))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.reason", ContainSubstring("analysis.result_cache.enabled is false")))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.attempt", ""))
	})

	It("re-pins, in a later run, the verified checkpoint of the attempt that completed the analyzer earlier", func() {
		Expect(run().Status).To(Equal(pipeline.JobStatusCompleted))
		attempt := classifyStage().CacheAttempt
		j := rerun()
		Expect(j.Status).To(Equal(pipeline.JobStatusCompleted), j.ErrorMessage)
		link := classifyLink()
		Expect(cacheMaterial(link)).To(Equal(sealedDigest(attempt)))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.mode", "enabled"))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.attempt", attempt))
	})

	It("signs an earlier-run analyzer with no recorded attempt and no signed record as unrecorded, never inferring a mode", func() {
		Expect(run().Status).To(Equal(pipeline.JobStatusCompleted))
		Expect(objects.Delete(ctx, pipeline.CheckpointKey("test-tenant", attestCacheJob, "classify", classifyStage().CacheAttempt))).To(Succeed())
		setClassifyAttempt("")
		j := rerun()
		Expect(j.Status).To(Equal(pipeline.JobStatusCompleted), j.ErrorMessage)
		link := classifyLink()
		Expect(cacheMaterial(link)).To(Equal(noCacheRecord))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.mode", "unrecorded"))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.attempt", ""))
	})

	DescribeTable("fails a later run whose earlier analyzer's checkpoint cannot be trusted, signing no analyzer link and changing no data",
		func(tamper func(attempt string), want string) {
			Expect(run().Status).To(Equal(pipeline.JobStatusCompleted))
			attempt := classifyStage().CacheAttempt
			before, err := cache.Manifest(ctx, attestCacheJob, "classify", attempt)
			Expect(err).NotTo(HaveOccurred())
			Expect(objects.Delete(ctx, "attestations/test-tenant/"+attestCacheJob+"/links/classify.json")).To(Succeed())
			tamper(attempt)

			j := rerun()
			Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
			Expect(j.ErrorClass).To(Equal(pipeline.JobErrorAttestationInvalid))
			Expect(store.failureCause(attestCacheJob)).To(ContainSubstring(want))
			Expect(store.failureCause(attestCacheJob)).To(ContainSubstring(`analyzer "classify"`))
			Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("crosscodex run start --source cat-1 --target cat-1"))
			Expect(objects.Objects()).NotTo(HaveKey("attestations/test-tenant/" + attestCacheJob + "/links/classify.json"))
			after, err := cache.Manifest(ctx, attestCacheJob, "classify", attempt)
			Expect(err).NotTo(HaveOccurred())
			Expect(after.Digest()).To(Equal(before.Digest()))
		},
		Entry("the stage names an attempt that has no checkpoint", func(string) {
			setClassifyAttempt(uuid.NewString())
		}, "has no stored checkpoint"),
		Entry("the stage names something that is not an attempt", func(string) {
			setClassifyAttempt("../../links/graph")
		}, "is not a canonical UUID"),
		Entry("the checkpoint was replaced", func(attempt string) {
			Expect(objects.Put(ctx, pipeline.CheckpointKey("test-tenant", attestCacheJob, "classify", attempt),
				strings.NewReader("not a checkpoint"))).To(Succeed())
		}, "does not verify"),
		Entry("the checkpoint is signed by a key the daemon no longer trusts", func(string) {
			kp, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			signer, err = attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())
		}, "not trusted"),
		Entry("the attempt's records changed after signing", func(string) {
			cache.VerifiedSummariesFunc = func(_ context.Context, _ string, signed map[string]resultcache.SignedAttempt) ([]resultcache.Summary, error) {
				var out []resultcache.Summary
				for a, s := range signed {
					out = append(out, resultcache.Summary{Analyzer: a, Attempt: s.Attempt})
				}
				return out, nil
			}
		}, "changed after it was signed"),
		Entry("the stage's pointer was removed although a signed record of its completion exists", func(string) {
			setClassifyAttempt("")
		}, "result-cache record pointer is missing although a signed record of its completion exists"),
	)

	It("fails a later run as retryable when the earlier analyzer's cache records cannot be read", func() {
		Expect(run().Status).To(Equal(pipeline.JobStatusCompleted))
		cache.VerifiedSummariesFunc = func(context.Context, string, map[string]resultcache.SignedAttempt) ([]resultcache.Summary, error) {
			return nil, errors.New("database unreachable")
		}
		j := rerun()
		Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
		Expect(j.ErrorClass).To(Equal(pipeline.JobErrorRetryable))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("Check that the database is reachable"))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("crosscodex run retry --from-failure " + attestCacheJob))
	})

	It("fails a later run when the daemon no longer wires the result cache, saying how to fix it", func() {
		Expect(run().Status).To(Equal(pipeline.JobStatusCompleted))
		wired = false
		j := rerun()
		Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("configure analysis.result_cache and restart crosscodexd"))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("crosscodex run retry --from-failure " + attestCacheJob))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("crosscodex run start --source cat-1 --target cat-1"))
	})

	It("fails a later run as retryable when the earlier analyzer's checkpoint cannot be read", func() {
		Expect(run().Status).To(Equal(pipeline.JobStatusCompleted))
		objects.failing.Store(true)
		j := rerun()
		Expect(j.Status).To(Equal(pipeline.JobStatusFailed))
		Expect(j.ErrorClass).To(Equal(pipeline.JobErrorRetryable))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("Check that object storage"))
		Expect(store.failureCause(attestCacheJob)).To(ContainSubstring("crosscodex run retry --from-failure " + attestCacheJob))
	})
})

var _ = Describe("The attestation layout of a job", func() {
	It("requires every analyzer's link to pin its result-cache manifest", func() {
		ctx, err := tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		store := newFakeStore()
		attestor := newFakeAttestation()
		reg := analyzer.NewRegistry()
		Expect(analyzer.Register[*pb.Control](reg, newControlStub("classify"))).To(Succeed())
		bus := &natsbustest.Fake{}
		eng := analysis.New(reg, &executorTestDispatcher{}, &executorTestCollector{},
			config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
			map[string]natsbus.TaskType{"classify": natsbus.TaskClassify},
			analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), store)))
		svc := pipeline.New(store, eng, reg, &fakeSynthesisExecutor{}, attestedBy(attestor), pipelineattestation.NewConverter(), bus,
			&storagetest.Fake{}, config.PipelineConfig{MaxConcurrentJobs: 10}, nil, pipeline.WithCatalogNameReader(serviceTestCatalogs))
		DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })
		now := time.Now()
		Expect(store.CreateJob(ctx, &pipeline.Job{JobID: attestCacheJob, TenantID: "test-tenant", Status: pipeline.JobStatusRunning,
			Config: []byte(`{"catalog_id":"cat-1","target_catalog_id":"cat-1"}`), CreatedAt: now, UpdatedAt: now})).To(Succeed())
		Expect(store.CreateStages(ctx, attestCacheJob, []string{"classify", "synthesis", "graph"})).To(Succeed())
		Expect(svc.Start(ctx)).To(Succeed())
		Eventually(func() pipeline.JobStatus {
			j, _ := store.GetJob(ctx, attestCacheJob)
			return j.Status
		}).Should(Equal(pipeline.JobStatusCompleted))

		attestor.mu.Lock()
		defer attestor.mu.Unlock()
		Expect(attestor.layoutCalls[0].Steps).To(ContainElement(And(HaveField("Name", "classify"),
			HaveField("ExpectedMaterials", ContainElement("classify.cache_manifest")))))
		var classify fakeLinkCall
		for _, c := range attestor.linkCalls {
			if c.step == "classify" {
				classify = c
			}
		}
		Expect(classify.materials).To(ContainElement(attestation.Artifact{URI: "classify.cache_manifest", Digest: noCacheRecord}))
	})
})
