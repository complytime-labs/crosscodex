package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// summaryJob is the job whose cache summaries the GetJob specs read.
const summaryJob = "job-now"

// storedResultDigest is the content digest of the result every completed
// stage of these specs stores ("[]").
var storedResultDigest = func() string {
	sum := sha256.Sum256([]byte(`[]`))
	return "sha256:" + hex.EncodeToString(sum[:])
}()

// summaryJobConfig is summaryJob's stored config.
const summaryJobConfig = `{"catalog_id":"cat-1","target_catalog_id":"cat-1"}`

// countingObjects is object storage that counts reads and lists, and fails
// the reads of keys containing failOn.
type countingObjects struct {
	*storagetest.Fake
	mu     sync.Mutex
	gets   int
	lists  int
	failOn string
}

func (o *countingObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	o.gets++
	failOn := o.failOn
	o.mu.Unlock()
	if failOn != "" && strings.Contains(key, failOn) {
		return nil, errors.New(driverDetail)
	}
	return o.Fake.Get(ctx, key)
}

func (o *countingObjects) List(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	o.mu.Lock()
	o.lists++
	o.mu.Unlock()
	return o.Fake.List(ctx, prefix)
}

func (o *countingObjects) reads() (gets, lists int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gets, o.lists
}

// driverDetail is error text from a dependency, which GetJob's summaries
// never quote to the tenant.
const driverDetail = "SECRET-DRIVER-DETAIL pq: connection reset"

// failingJobsCreatedBy is the fake job store with JobsCreatedBy failing.
type failingJobsCreatedBy struct{ *fakeStore }

func (failingJobsCreatedBy) JobsCreatedBy(context.Context, []string, string) ([]string, error) {
	return nil, errors.New(driverDetail)
}

var _ = Describe("Cache summaries in GetJob", Ordered, func() {
	var gen attestation.Generator

	BeforeAll(func() {
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	var (
		ctx     context.Context
		store   *fakeStore
		jobs    pipeline.Store
		cache   *resultcachetest.Fake
		objects *countingObjects
		logs    *bytes.Buffer
		attests pipeline.Attestors
	)

	addJob := func(id, owner string, status pipeline.JobStatus) {
		now := time.Now()
		Expect(store.CreateJob(ctx, &pipeline.Job{JobID: id, TenantID: "test-tenant", Status: status,
			Config: []byte(summaryJobConfig), CreatedBy: owner, CreatedAt: now, UpdatedAt: now})).To(Succeed())
	}
	service := func() *pipeline.Service {
		return pipeline.New(jobs, nil, nil, nil, attests, nil, nil, objects, config.PipelineConfig{}, nil,
			pipeline.WithResultCache(cache, config.ResultCacheConfig{Enabled: true, LookupBatchSize: 10}, config.LLMConfig{}, nil),
			pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	}
	// seal records stats and hits for an attempt of analyzerName, signs and
	// stores its checkpoint with signer, and completes the stage naming it.
	seal := func(analyzerName string, signer attestation.Generator, stats resultcache.Stats, hits ...resultcache.HitEntry) string {
		attempt := uuid.NewString()
		Expect(cache.RecordStage(ctx, resultcache.StageRecord{JobID: summaryJob, Analyzer: analyzerName, Attempt: attempt,
			Hits: hits, Stats: stats})).To(Succeed())
		m, err := cache.Manifest(ctx, summaryJob, analyzerName, attempt)
		Expect(err).NotTo(HaveOccurred())
		job, err := store.GetJob(ctx, summaryJob)
		Expect(err).NotTo(HaveOccurred())
		cp := service().CacheCheckpointsFor(job, &pipeline.Attestor{Generator: signer}, analyzerName)
		ref := resultcache.AttemptRef{JobID: summaryJob, Analyzer: analyzerName, Attempt: attempt}
		raw, err := cp.Sign(ctx, ref, m.Digest())
		Expect(err).NotTo(HaveOccurred())
		Expect(cp.Store(ctx, ref, raw)).To(Succeed())
		Expect(store.CompleteAnalysisStage(ctx, summaryJob, analyzerName, []byte(`[]`), attempt)).To(Succeed())
		return attempt
	}
	// link signs and stores analyzerName's attestation link with the
	// result_cache byproducts mode and reason, and completes the stage
	// naming no attempt.
	link := func(analyzerName, mode, reason string) {
		sum := sha256.Sum256([]byte(summaryJobConfig))
		subject := attestation.Artifact{URI: "job:test-tenant/" + summaryJob, Digest: "sha256:" + hex.EncodeToString(sum[:])}
		l, err := gen.CreateLink(ctx, analyzerName, []attestation.Artifact{subject,
			{URI: analyzerName + ".cache_manifest", Digest: resultcache.Manifest{}.Digest()}},
			[]attestation.Artifact{{URI: analyzerName + ".output", Digest: storedResultDigest}},
			attestation.WithByProducts(map[string]any{"result_cache.mode": mode, "result_cache.reason": reason, "result_cache.attempt": ""}))
		Expect(err).NotTo(HaveOccurred())
		Expect(objects.Put(ctx, "attestations/test-tenant/"+summaryJob+"/links/"+analyzerName+".json", bytes.NewReader(l.Raw))).To(Succeed())
		Expect(store.CompleteAnalysisStage(ctx, summaryJob, analyzerName, []byte(`[]`), "")).To(Succeed())
	}
	getJob := func(c context.Context) []*pb.AnalyzerCacheSummary {
		resp, err := service().GetJob(c, connect.NewRequest(&pb.GetJobRequest{JobId: summaryJob, IncludeCacheSummaries: true}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg.GetJob().GetCacheSummaries()
	}
	summaryOf := func(sums []*pb.AnalyzerCacheSummary, analyzerName string) *pb.AnalyzerCacheSummary {
		for _, s := range sums {
			if s.GetAnalyzer() == analyzerName {
				return s
			}
		}
		Fail("no cache summary for " + analyzerName)
		return nil
	}
	enabled := resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 3, Hits: 2, Dispatched: 1,
		Models: map[string]resultcache.ModelIdentity{"default": {Observed: "qwen2.5:14b", Deployment: "e96b0292", Revision: "r1"}}}
	reuse := func() []resultcache.HitEntry {
		digest := resultcache.ResultDigest([]byte("x"))
		return []resultcache.HitEntry{
			{TaskID: "t1", CacheKey: strings.Repeat("1", 64), ResultDigest: digest, ProducerJobID: "job-mine"},
			{TaskID: "t2", CacheKey: strings.Repeat("2", 64), ResultDigest: digest, ProducerJobID: "job-theirs"},
		}
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		store = newFakeStore()
		jobs = store
		cache = &resultcachetest.Fake{}
		objects = &countingObjects{Fake: &storagetest.Fake{}}
		logs = &bytes.Buffer{}
		attests = attestedBy(gen)
		addJob("job-mine", "user-a", pipeline.JobStatusCompleted)
		addJob("job-theirs", "user-b", pipeline.JobStatusCompleted)
		addJob(summaryJob, "user-a", pipeline.JobStatusRunning)
		Expect(store.CreateStages(ctx, summaryJob, []string{"classify", "embedding", "requires", "synthesis", "graph"})).To(Succeed())
		cache.AddJob("test-tenant", summaryJob)
	})

	It("lists each analyzer's attested cache use in stage order, naming only the producers the caller created", func() {
		seal("classify", gen, enabled, reuse()...)
		sums := getJob(tenant.WithUser(ctx, "user-a"))
		Expect(sums).To(HaveLen(3))
		Expect(sums[0].GetAnalyzer()).To(Equal("classify"))
		Expect(sums[0].GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED))
		Expect(sums[0].GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED))
		Expect(sums[0].GetTasks()).To(BeEquivalentTo(3))
		Expect(sums[0].GetHits()).To(BeEquivalentTo(2))
		Expect(sums[0].GetDispatched()).To(BeEquivalentTo(1))
		Expect(sums[0].GetProducerJobIds()).To(Equal([]string{"job-mine"}))
		Expect(sums[0].GetOtherProducerJobs()).To(BeEquivalentTo(1))
		Expect(sums[0].GetModels()).To(HaveLen(1))
		Expect(sums[0].GetModels()[0].GetAlias()).To(Equal("default"))
		Expect(sums[0].GetModels()[0].GetObservedModel()).To(Equal("qwen2.5:14b"))
		Expect(sums[0].GetModels()[0].GetRevision()).To(Equal("r1"))
		Expect(sums[0].GetModels()[0].GetDeployment()).To(Equal("e96b0292"))
		Expect(sums[1].GetAnalyzer()).To(Equal("embedding"))
		Expect(sums[2].GetAnalyzer()).To(Equal("requires"))
	})

	It("shows a skipped analyzer's mode and reason from its checkpointed stage run", func() {
		seal("embedding", gen, resultcache.Stats{Mode: resultcache.ModeSkipped, Reason: "the analyzer does not opt in"})
		s := summaryOf(getJob(ctx), "embedding")
		Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED))
		Expect(s.GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_SKIPPED))
		Expect(s.GetReason()).To(Equal("the analyzer does not opt in"))
	})

	DescribeTable("names no producer the caller did not create, only counting them",
		func(c func() context.Context) {
			seal("classify", gen, enabled, reuse()...)
			s := summaryOf(getJob(c()), "classify")
			Expect(s.GetProducerJobIds()).To(BeEmpty())
			Expect(s.GetOtherProducerJobs()).To(BeEquivalentTo(2))
		},
		Entry("a caller without a user", func() context.Context { return ctx }),
		Entry("another user", func() context.Context { return tenant.WithUser(ctx, "user-c") }),
	)

	DescribeTable("shows a completed analyzer that kept no cache records with the mode its signed link records",
		func(mode, reason string, want pb.ResultCacheMode) {
			link("embedding", mode, reason)
			s := summaryOf(getJob(ctx), "embedding")
			Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED))
			Expect(s.GetMode()).To(Equal(want))
			Expect(s.GetReason()).To(Equal(reason))
			Expect(s.GetTasks()).To(BeZero())
		},
		Entry("disabled for the tenant", "disabled", "analysis.result_cache.enabled is false for this tenant",
			pb.ResultCacheMode_RESULT_CACHE_MODE_DISABLED),
		Entry("run without the result cache", "off", "the result cache is not wired on this daemon",
			pb.ResultCacheMode_RESULT_CACHE_MODE_DISABLED),
		Entry("completed in an earlier run that recorded no attempt, signed as unrecorded, never as disabled",
			"unrecorded", "the stage completed in an earlier run that recorded no result-cache attempt",
			pb.ResultCacheMode_RESULT_CACHE_MODE_UNRECORDED),
	)

	It("shows an analyzer that has not completed as in progress while the job runs, and unattested once it has stopped", func() {
		Expect(summaryOf(getJob(ctx), "requires").GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS))
		Expect(store.UpdateJobStatus(ctx, summaryJob, pipeline.JobStatusFailed, errors.New("stopped"))).To(Succeed())
		s := summaryOf(getJob(ctx), "requires")
		Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED))
		Expect(s.GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_UNSPECIFIED))
	})

	DescribeTable("shows a completed analyzer with no verifiable record as unattested, with every other field empty",
		func(setup func()) {
			setup()
			s := summaryOf(getJob(tenant.WithUser(ctx, "user-a")), "classify")
			Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED))
			Expect(s.GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_UNSPECIFIED))
			Expect(s.GetTasks()).To(BeZero())
			Expect(s.GetHits()).To(BeZero())
			Expect(s.GetModels()).To(BeEmpty())
			Expect(s.GetProducerJobIds()).To(BeEmpty())
			Expect(s.GetOtherProducerJobs()).To(BeZero())
		},
		Entry("the attempt it names has no checkpoint", func() {
			Expect(store.CompleteAnalysisStage(ctx, summaryJob, "classify", []byte(`[]`), uuid.NewString())).To(Succeed())
		}),
		Entry("it names something that is not an attempt", func() {
			Expect(store.CompleteAnalysisStage(ctx, summaryJob, "classify", []byte(`[]`), "../links/graph")).To(Succeed())
		}),
		Entry("its checkpoint is signed by a key the daemon no longer trusts", func() {
			kp, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			old, err := attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())
			seal("classify", old, enabled, reuse()...)
		}),
		Entry("its checkpoint was replaced", func() {
			attempt := seal("classify", gen, enabled, reuse()...)
			Expect(objects.Put(ctx, pipeline.CheckpointKey("test-tenant", summaryJob, "classify", attempt),
				strings.NewReader("not a checkpoint"))).To(Succeed())
		}),
		Entry("a stats row was planted in its attempt after signing", func() {
			attempt := seal("classify", gen, enabled, reuse()...)
			Expect(cache.RecordStage(ctx, resultcache.StageRecord{JobID: summaryJob, Analyzer: "classify", Attempt: attempt,
				Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 3, Dispatched: 0}})).To(Succeed())
		}),
		Entry("the attempt it names sealed a failed stage run, not the completing one", func() {
			cache.AddCatalog("test-tenant", "cat-1")
			attempt := uuid.NewString()
			Expect(cache.Put(ctx, resultcache.Entry{JobID: summaryJob, Analyzer: "classify", Attempt: attempt, TaskID: "t1",
				CacheKey: strings.Repeat("3", 64), Result: []byte(`{"r":1}`), ResponseModel: "m",
				SourceCatalogID: "cat-1", TargetCatalogID: "cat-1"})).To(Succeed())
			m, err := cache.Manifest(ctx, summaryJob, "classify", attempt)
			Expect(err).NotTo(HaveOccurred())
			job, err := store.GetJob(ctx, summaryJob)
			Expect(err).NotTo(HaveOccurred())
			cp := service().CacheCheckpointsFor(job, &pipeline.Attestor{Generator: gen}, "classify")
			ref := resultcache.AttemptRef{JobID: summaryJob, Analyzer: "classify", Attempt: attempt}
			raw, err := cp.Sign(ctx, ref, m.Digest())
			Expect(err).NotTo(HaveOccurred())
			Expect(cp.Store(ctx, ref, raw)).To(Succeed())
			Expect(store.CompleteAnalysisStage(ctx, summaryJob, "classify", []byte(`[]`), attempt)).To(Succeed())
		}),
		Entry("it kept no records and the stopped job has no link for it", func() {
			Expect(store.CompleteAnalysisStage(ctx, summaryJob, "classify", []byte(`[]`), "")).To(Succeed())
			Expect(store.UpdateJobStatus(ctx, summaryJob, pipeline.JobStatusFailed, errors.New("stopped"))).To(Succeed())
		}),
		Entry("its link pins a result other than the one stored now", func() {
			link("classify", "disabled", "off for the tenant")
			Expect(store.CompleteAnalysisStage(ctx, summaryJob, "classify", []byte(`[{"replaced":true}]`), "")).To(Succeed())
		}),
		Entry("its link records no mode", func() { link("classify", "", "") }),
		Entry("its link records a cached mode, though the stage names no sealing attempt", func() {
			link("classify", "enabled", "")
		}),
	)

	It("shows a completed stage that kept no records as in progress while the job runs and its link is not signed yet", func() {
		Expect(store.CompleteAnalysisStage(ctx, summaryJob, "classify", []byte(`[]`), "")).To(Succeed())
		s := summaryOf(getJob(ctx), "classify")
		Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS))
		Expect(s.GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_UNSPECIFIED))
	})

	It("reads no signed object and no cache record for a request that does not ask for summaries", func() {
		seal("classify", gen, enabled, reuse()...)
		link("embedding", "disabled", "off for the tenant")
		cache.VerifiedSummariesFunc = func(context.Context, string, map[string]resultcache.SignedAttempt) ([]resultcache.Summary, error) {
			Fail("VerifiedSummaries was called for a request that did not ask for summaries")
			return nil, nil
		}
		resp, err := service().GetJob(tenant.WithUser(ctx, "user-a"), connect.NewRequest(&pb.GetJobRequest{JobId: summaryJob}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetJob().GetCacheSummaries()).To(BeEmpty())
		gets, lists := objects.reads()
		Expect(gets).To(BeZero())
		Expect(lists).To(BeZero())
	})

	It("still returns the job when object storage fails, showing the analyzers it cannot verify as unattested and logging the cause once", func() {
		seal("classify", gen, enabled, reuse()...)
		link("embedding", "disabled", "off for the tenant")
		objects.failOn = "attestations/"
		sums := getJob(tenant.WithUser(ctx, "user-a"))
		for _, name := range []string{"classify", "embedding"} {
			s := summaryOf(sums, name)
			Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED), name)
			Expect(s.GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_UNSPECIFIED), name)
		}
		Expect(summaryOf(sums, "requires").GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS))
		Expect(strings.Count(logs.String(), "object storage could not be read")).To(Equal(1))
		Expect(logs.String()).To(ContainSubstring("trace_id="))
		Expect(logs.String()).To(ContainSubstring(driverDetail))
	})

	It("logs a checkpoint that fails its integrity check, telling the tenant nothing of it", func() {
		attempt := seal("classify", gen, enabled, reuse()...)
		Expect(objects.Put(ctx, pipeline.CheckpointKey("test-tenant", summaryJob, "classify", attempt),
			strings.NewReader(strings.Repeat("x", pipeline.MaxAttestationObjectBytes+1)))).To(Succeed())
		s := summaryOf(getJob(ctx), "classify")
		Expect(s.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED))
		Expect(s.GetReason()).To(BeEmpty())
		Expect(logs.String()).To(ContainSubstring("level=ERROR"))
		Expect(logs.String()).To(ContainSubstring(attempt))
	})

	It("reads at most one signed object per analyzer and lists nothing", func() {
		seal("classify", gen, enabled, reuse()...)
		link("embedding", "disabled", "off for the tenant")
		getJob(tenant.WithUser(ctx, "user-a"))
		gets, lists := objects.reads()
		Expect(gets).To(Equal(2))
		Expect(lists).To(BeZero())
	})

	DescribeTable("returns no summaries when no reuse is possible",
		func(setup func() *pipeline.Service) {
			seal("classify", gen, enabled, reuse()...)
			resp, err := setup().GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: summaryJob, IncludeCacheSummaries: true}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetJob().GetCacheSummaries()).To(BeEmpty())
		},
		Entry("the tenant's jobs are not attested", func() *pipeline.Service {
			attests = pipeline.Attestors{}
			return service()
		}),
		Entry("the daemon has no result cache", func() *pipeline.Service {
			return pipeline.New(jobs, nil, nil, nil, attests, nil, nil, objects, config.PipelineConfig{}, nil)
		}),
	)

	DescribeTable("fails GetJob as internal when a record cannot be read",
		func(setup func()) {
			setup()
			_, err := service().GetJob(tenant.WithUser(ctx, "user-a"), connect.NewRequest(&pb.GetJobRequest{JobId: summaryJob, IncludeCacheSummaries: true}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err.Error()).To(ContainSubstring("cache summaries"))
		},
		Entry("the attempt's records cannot be read", func() {
			seal("classify", gen, enabled, reuse()...)
			cache.VerifiedSummariesFunc = func(context.Context, string, map[string]resultcache.SignedAttempt) ([]resultcache.Summary, error) {
				return nil, fmt.Errorf("%w: %s", resultcache.ErrUnavailable, driverDetail)
			}
		}),
		Entry("the jobs the caller created cannot be read", func() {
			seal("classify", gen, enabled, reuse()...)
			jobs = failingJobsCreatedBy{store}
		}),
	)
})
