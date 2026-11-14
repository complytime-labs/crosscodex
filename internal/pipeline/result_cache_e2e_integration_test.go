//go:build integration

package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// cachedAnalyzers are the analyzers that opt in to result reuse.
var cachedAnalyzers = []string{"classify", "embedding", "artifacts", "requires", "relationship"}

// Result reuse (#243) over the durable pipeline: real analyzers, real
// Postgres as app_user, the pgstore, attested checkpoints in local object
// storage. Each spec seeds its own catalogs with a statement of its own, so
// no spec reuses another's results.
var _ = Describe("Result reuse end-to-end", Ordered, func() {
	var env *e2eEnv

	BeforeAll(func() {
		env = newE2EEnv(e2eOptions{resultCache: true})
	})

	// seed seeds a source and a target catalog whose controls' identifiers
	// differ from their control IDs (O9: the artifacts analyzer names a
	// control by identifier), and returns the job config mapping one onto
	// the other.
	seed := func() (cfg *pb.JobConfig, src, tgt string) {
		tag := uuid.New().String()
		statement := e2eIdenticalStatement + " " + tag
		src, tgt = "cat-src-"+tag, "cat-tgt-"+tag
		ctx := context.Background()
		for _, cat := range []struct{ id, prefix string }{{src, "S"}, {tgt, "T"}} {
			ExpectWithOffset(1, env.idb.suPool.Exec(ctx, `
				INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
				VALUES ($1, $2, $3, 'v1', 'test', 'unused')
			`, cat.id, env.tenantA, "Framework "+cat.prefix)).To(Succeed())
			for _, c := range []struct{ id, identifier, title, statement, class, parent string }{
				{cat.id + "/section", cat.prefix, "Access Control", "Access control family.", classifySectionClass, ""},
				{cat.id + "/c1", cat.prefix + "-1", "Control One of " + cat.prefix, statement, "SC", "section"},
				{cat.id + "/c2", cat.prefix + "-2", "Control Two of " + cat.prefix, statement, "SC", "section"},
			} {
				ExpectWithOffset(1, env.idb.suPool.Exec(ctx, `
					INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class, parent_id)
					VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
				`, env.tenantA, c.id, cat.id, c.identifier, c.title, c.statement, c.class, c.parent)).To(Succeed())
			}
		}
		return &pb.JobConfig{CatalogId: src, TargetCatalogId: tgt}, src, tgt
	}

	// statsOf sums column (tasks, hits or dispatched) of the job's stage records
	// for analyzerName, read as app_user under RLS.
	statsOf := func(jobID, analyzerName, column string) int {
		Expect(column).To(BeElementOf("tasks", "hits", "dispatched"))
		return e2eScalar(env.idb.tenantConn, env.ctxA,
			"SELECT COALESCE(SUM("+column+"), 0)::int FROM job_cache_stats WHERE job_id = $1 AND analyzer = $2", jobID, analyzerName)
	}

	// failedJob creates a job with cfg and waits for it to fail, returning
	// it.
	failedJob := func(cfg *pb.JobConfig) *Job {
		resp, err := env.svc.CreateJob(env.ctxA, connect.NewRequest(&pb.CreateJobRequest{Config: cfg}))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		var job *Job
		EventuallyWithOffset(1, func(g Gomega) {
			job, err = env.store.GetJob(env.ctxA, resp.Msg.JobId)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(job.Status).NotTo(Equal(JobStatusCompleted), "the job completed")
			g.Expect(job.Status).To(Equal(JobStatusFailed))
		}, 90*time.Second, 250*time.Millisecond).Should(Succeed())
		return job
	}

	checkpointsOf := func(jobID, analyzerName string) []string {
		objs, err := env.storage.List(context.Background(), checkpointDir(env.tenantA, jobID, analyzerName))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		keys := make([]string, len(objs))
		for i, o := range objs {
			keys[i] = o.Key
		}
		return keys
	}

	It("reuses every cached result of an identical earlier job, sending nothing to the model, through verified checkpoints", func() {
		cfg, src, tgt := seed()
		first := env.runJob(cfg)
		for _, name := range cachedAnalyzers {
			Expect(statsOf(first, name, "dispatched")).To(BeNumerically(">", 0), "%s dispatched in the first job", name)
			Expect(statsOf(first, name, "hits")).To(BeZero(), "%s hits in the first job", name)
			Expect(checkpointsOf(first, name)).To(HaveLen(1), "%s checkpoint of the first job", name)
		}

		calls := env.workerLLM.calls.Load()
		second := env.runJob(cfg)
		Expect(env.workerLLM.calls.Load()).To(Equal(calls), "the second job sent nothing to the worker")
		for _, name := range cachedAnalyzers {
			Expect(statsOf(second, name, "dispatched")).To(BeZero(), "%s dispatched in the second job", name)
			Expect(statsOf(second, name, "hits")).To(BeNumerically(">", 0), "%s hits in the second job", name)
		}

		// O9: an artifacts request names its control by identifier (S-1,
		// T-1), and each is stored under the catalog of the control it was
		// generated from. (Classify's request is the statement alone, which
		// every leaf here shares, so its one key's row names whichever
		// catalog stored it last.)
		for _, cat := range []string{src, tgt} {
			Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
				"SELECT count(*) FROM llm_result_cache WHERE job_id = $1 AND analyzer = 'artifacts' AND source_catalog_id = $2 AND target_catalog_id = $2",
				first, cat)).To(Equal(2), "artifacts results of catalog %s", cat)
		}
		for _, name := range []string{"classify", "artifacts"} {
			Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
				"SELECT count(*) FROM llm_result_cache WHERE job_id = $1 AND analyzer = $2 AND source_catalog_id <> target_catalog_id",
				first, name)).To(BeZero(), "%s results stored under two catalogs", name)
		}
	})

	It("reuses nothing after the attestation key pair changes, recomputing every result", func() {
		cfg, _, _ := seed()
		env.runJob(cfg)

		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		env.newService(gen)

		calls := env.workerLLM.calls.Load()
		second := env.runJob(cfg)
		Expect(env.workerLLM.calls.Load()).To(BeNumerically(">", calls))
		for _, name := range cachedAnalyzers {
			Expect(statsOf(second, name, "hits")).To(BeZero(), "%s hits after the key changed", name)
			Expect(statsOf(second, name, "dispatched")).To(BeNumerically(">", 0), "%s dispatched after the key changed", name)
		}
	})

	It("fails as result_cache_corrupt, reusing nothing, when a producer's checkpoint was tampered with", func() {
		cfg, _, _ := seed()
		first := env.runJob(cfg)
		keys := checkpointsOf(first, "classify")
		Expect(keys).To(HaveLen(1))

		rc, err := env.storage.Get(context.Background(), keys[0])
		Expect(err).NotTo(HaveOccurred())
		raw, err := io.ReadAll(rc)
		Expect(err).NotTo(HaveOccurred())
		Expect(rc.Close()).To(Succeed())
		var envelope map[string]any
		Expect(json.Unmarshal(raw, &envelope)).To(Succeed())
		payload, err := base64.StdEncoding.DecodeString(envelope["payload"].(string))
		Expect(err).NotTo(HaveOccurred())
		i := strings.Index(string(payload), "sha256:")
		Expect(i).To(BeNumerically(">=", 0))
		tamperedPayload := []byte(string(payload[:i]) + "sha256:" + strings.Repeat("0", 64) + string(payload[i+len("sha256:")+64:]))
		envelope["payload"] = base64.StdEncoding.EncodeToString(tamperedPayload)
		tampered, err := json.Marshal(envelope)
		Expect(err).NotTo(HaveOccurred())
		Expect(env.storage.Put(context.Background(), keys[0], bytes.NewReader(tampered))).To(Succeed())

		calls := env.workerLLM.calls.Load()
		job := failedJob(cfg)
		Expect(job.ErrorClass).To(Equal(JobErrorResultCacheCorrupt))
		Expect(job.ErrorMessage).To(ContainSubstring("failed its integrity check"))
		Expect(job.ErrorMessage).To(ContainSubstring("--no-cache"))
		Expect(statsOf(job.JobID, "classify", "hits")).To(BeZero())
		Expect(env.workerLLM.calls.Load()).To(Equal(calls), "classify sent nothing to the model")
	})

	// O2: someone holding app_user's credentials writes into a running
	// job's live attempt.
	It("refuses to seal a stage whose live record holds a write it did not make, leaving the rows as they are", func() {
		cfg, _, _ := seed()
		env.cache.mu.Lock()
		env.cache.plant = &plantSpec{analyzer: "classify"}
		env.cache.mu.Unlock()
		DeferCleanup(func() {
			env.cache.mu.Lock()
			env.cache.plant = nil
			env.cache.mu.Unlock()
		})

		job := failedJob(cfg)
		planted := env.cache.planted()
		Expect(planted).NotTo(BeNil(), "the plant ran")
		Expect(job.ErrorClass).To(Equal(JobErrorResultCacheCorrupt))
		Expect(job.ErrorMessage).To(ContainSubstring("holds result-cache records for this stage run that the stage did not write"))
		Expect(checkpointsOf(job.JobID, "classify")).To(BeEmpty(), "no checkpoint was signed for classify")
		Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
			"SELECT count(*) FROM job_cache_entries WHERE job_id = $1 AND analyzer = 'classify' AND attempt = $2 AND task_id = 'planted-task'",
			job.JobID, planted.Attempt)).To(Equal(1), "the planted row is left in place")
		Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
			"SELECT count(*) FROM job_cache_entries WHERE job_id = $1 AND analyzer = 'classify' AND attempt = $2 AND role = 'write'",
			job.JobID, planted.Attempt)).To(BeNumerically(">", 1), "the stage's own writes are left in place")
	})
	// asSuperuser runs stmts in one transaction as the database superuser
	// in replica mode, which skips every trigger, the freeze of a stopped
	// job's record included: the strongest database attacker, who still
	// lacks the attestation signing key.
	asSuperuser := func(stmts ...func(ctx context.Context, exec func(string, ...any))) {
		ctx := context.Background()
		tx, err := env.idb.suPool.Begin(ctx)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		exec := func(q string, args ...any) { ExpectWithOffset(2, tx.Exec(ctx, q, args...)).To(Succeed(), q) }
		exec(`SET LOCAL session_replication_role = replica`)
		for _, stmt := range stmts {
			stmt(ctx, exec)
		}
		ExpectWithOffset(1, tx.Commit()).To(Succeed())
	}
	// cachedClassify is the producer, its attempt and the key of the cached
	// classify result of a seeded pair of catalogs (one key: every leaf
	// shares its statement, so its row names whichever catalog stored it
	// last).
	cachedClassify := func(src, tgt string) (producer, attempt, key string) {
		ExpectWithOffset(1, env.idb.suPool.QueryRow(context.Background(), `
			SELECT job_id, producer_attempt, cache_key FROM llm_result_cache
			WHERE tenant_id = $1 AND analyzer = 'classify' AND source_catalog_id IN ($2, $3)`, env.tenantA, src, tgt).
			Scan(&producer, &attempt, &key)).To(Succeed())
		return producer, attempt, key
	}
	// rowsOf is a fingerprint of every cache row of the tenant's classify
	// results and of producer's classify record.
	rowsOf := func(producer string) string {
		var fp string
		ExpectWithOffset(1, env.idb.suPool.QueryRow(context.Background(), `
			SELECT md5(coalesce((SELECT string_agg(cache_key || result || job_id || producer_attempt, ',' ORDER BY cache_key)
				FROM llm_result_cache WHERE tenant_id = $1 AND analyzer = 'classify'), '') ||
			coalesce((SELECT string_agg(attempt || task_id || role || cache_key || result_digest, ',' ORDER BY attempt, task_id, role, cache_key, result_digest)
				FROM job_cache_entries WHERE tenant_id = $1 AND job_id = $2 AND analyzer = 'classify'), ''))`,
			env.tenantA, producer).Scan(&fp)).To(Succeed())
		return fp
	}
	summaryOf := func(jobID, analyzerName string) *pb.AnalyzerCacheSummary {
		job, err := env.store.GetJob(env.ctxA, jobID)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		resp, err := env.svc.GetJob(tenant.WithUser(env.ctxA, job.CreatedBy), connect.NewRequest(&pb.GetJobRequest{JobId: jobID, IncludeCacheSummaries: true}))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		for _, sum := range resp.Msg.GetJob().GetCacheSummaries() {
			if sum.GetAnalyzer() == analyzerName {
				return sum
			}
		}
		Fail("no cache summary for " + analyzerName)
		return nil
	}
	sealingAttempt := func(jobID, analyzerName string) string {
		stages, err := env.store.GetStages(env.ctxA, jobID)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		for _, st := range stages {
			if st.StageName == analyzerName {
				return st.CacheAttempt
			}
		}
		Fail("no stage " + analyzerName)
		return ""
	}
	classifyLink := func(jobID string) *attestation.VerifiedLink {
		rc, err := env.storage.Get(context.Background(), "attestations/"+env.tenantA+"/"+jobID+"/links/classify.json")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		raw, err := io.ReadAll(rc)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, rc.Close()).To(Succeed())
		v, err := env.svc.attestors.forTenant(env.tenantA).Generator.Verify(env.ctxA, raw)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return v
	}
	cacheMaterial := func(v *attestation.VerifiedLink) string {
		for _, m := range v.Materials {
			if m.URI == cacheManifestURI(v.Step) {
				return m.Digest
			}
		}
		Fail("the link pins no cache manifest")
		return ""
	}
	// failInSynthesis marks the completed jobID failed in synthesis, as a
	// crash there would have left it (a completed job's record is frozen,
	// so this takes the superuser): its analyzers completed.
	failInSynthesis := func(jobID string) {
		asSuperuser(func(_ context.Context, exec func(string, ...any)) {
			exec(`UPDATE job_stages SET status = 'failed', error_message = 'synthesis crashed'
				WHERE tenant_id = $1 AND job_id = $2 AND stage_name IN ('synthesis', 'graph')`, env.tenantA, jobID)
			exec(`UPDATE jobs SET status = 'failed', error_message = 'synthesis crashed' WHERE tenant_id = $1 AND job_id = $2`,
				env.tenantA, jobID)
		})
	}
	// retryFromFailure retries the failed jobID from its failure and waits
	// for it to stop.
	retryFromFailure := func(jobID string) *Job {
		_, err := env.svc.RetryJob(env.ctxA, connect.NewRequest(&pb.RetryJobRequest{JobId: jobID, RetryFromFailure: true}))
		Expect(err).NotTo(HaveOccurred())
		var job *Job
		Eventually(func(g Gomega) {
			job, err = env.store.GetJob(env.ctxA, jobID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(job.Status).To(BeElementOf(JobStatusCompleted, JobStatusFailed))
		}, 90*time.Second, 250*time.Millisecond).Should(Succeed())
		return job
	}

	// See docs/dev/design-principles.md, "LLM Result Reuse": a superuser who edits a cached result and
	// adds a matching write entry inside the producer's signed attempt
	// passes every row check, but not the signed checkpoint.
	It("refuses a superuser's forged hit: a result edited with a matching write entry fails the next job as result_cache_corrupt, changing nothing", func() {
		cfg, src, tgt := seed()
		env.runJob(cfg)
		env.runJob(cfg)
		producer, attempt, key := cachedClassify(src, tgt)
		forged := []byte(`{"model":"` + e2eModel + `","response":"Organizational|Strategic"}`)
		asSuperuser(func(_ context.Context, exec func(string, ...any)) {
			exec(`UPDATE llm_result_cache SET result = $1 WHERE tenant_id = $2 AND analyzer = 'classify' AND cache_key = $3`,
				string(forged), env.tenantA, key)
			exec(`INSERT INTO job_cache_entries (tenant_id, job_id, analyzer, attempt, task_id, cache_key, result_digest, role)
				VALUES ($1, $2, 'classify', $3, 'forged', $4, $5, 'write')`,
				env.tenantA, producer, attempt, key, resultcache.ResultDigest(forged))
		})
		before := rowsOf(producer)

		job := failedJob(cfg)
		Expect(job.ErrorClass).To(Equal(JobErrorResultCacheCorrupt))
		Expect(job.ErrorMessage).To(ContainSubstring(`Stage "classify" stopped: a cached result failed its integrity check, so it was not used`))
		Expect(job.ErrorMessage).To(ContainSubstring("--no-cache"))
		Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
			`SELECT count(*) FROM analysis_results WHERE job_id = $1 AND analyzer_name = 'classify'`, job.JobID)).To(BeZero())
		Expect(e2eScalar(env.idb.tenantConn, env.ctxA,
			`SELECT count(*) FROM job_cache_entries WHERE job_id = $1 AND analyzer = 'classify'`, job.JobID)).To(BeZero())
		Expect(rowsOf(producer)).To(Equal(before), "the failed job changed no cache row")
	})

	It("recomputes a cached result a superuser repointed to an attempt that was never signed", func() {
		cfg, src, tgt := seed()
		env.runJob(cfg)
		_, _, key := cachedClassify(src, tgt)
		asSuperuser(func(_ context.Context, exec func(string, ...any)) {
			exec(`UPDATE llm_result_cache SET producer_attempt = $1 WHERE tenant_id = $2 AND analyzer = 'classify' AND cache_key = $3`,
				uuid.NewString(), env.tenantA, key)
		})
		next := env.runJob(cfg)
		Expect(statsOf(next, "classify", "hits")).To(BeZero())
		Expect(statsOf(next, "classify", "dispatched")).To(BeNumerically(">", 0))
	})

	It("shows run status figures only from the signed record: unchanged by a planted stats row, unattested once the record is edited", func() {
		cfg, _, _ := seed()
		first := env.runJob(cfg)
		second := env.runJob(cfg)
		sum := summaryOf(second, "classify")
		Expect(sum.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED))
		Expect(sum.GetMode()).To(Equal(pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED))
		Expect(sum.GetHits()).To(BeNumerically(">", 0))
		Expect(sum.GetDispatched()).To(BeZero())
		Expect(sum.GetProducerJobIds()).To(ConsistOf(first))
		Expect(sum.GetModels()).NotTo(BeEmpty())

		asSuperuser(func(_ context.Context, exec func(string, ...any)) {
			exec(`INSERT INTO job_cache_stats (tenant_id, job_id, analyzer, attempt, mode, tasks, hits, dispatched)
				VALUES ($1, $2, 'classify', $3, 'enabled', 999, 999, 0)`, env.tenantA, second, uuid.NewString())
		})
		Expect(summaryOf(second, "classify").GetHits()).To(Equal(sum.GetHits()), "a stats row outside the signed attempt changes nothing")

		attempt := sealingAttempt(second, "classify")
		asSuperuser(func(_ context.Context, exec func(string, ...any)) {
			exec(`INSERT INTO job_cache_stats (tenant_id, job_id, analyzer, attempt, mode, tasks, hits, dispatched)
				VALUES ($1, $2, 'classify', $3, 'enabled', 999, 999, 0)`, env.tenantA, second, attempt)
		})
		edited := summaryOf(second, "classify")
		Expect(edited.GetAttestation()).To(Equal(pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED))
		Expect(edited.GetHits()).To(BeZero())
	})

	It("pins each analyzer's sealed checkpoint and mode in its link, and re-pins the same after a retry from failure", func() {
		cfg, _, _ := seed()
		jobID := env.runJob(cfg)
		attempt := sealingAttempt(jobID, "classify")
		m, err := env.cache.Manifest(env.ctxA, jobID, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		link := classifyLink(jobID)
		Expect(cacheMaterial(link)).To(Equal(m.Digest()))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.mode", "enabled"))
		Expect(link.ByProducts).To(HaveKeyWithValue("result_cache.attempt", attempt))

		failInSynthesis(jobID)
		job := retryFromFailure(jobID)
		Expect(job.Status).To(Equal(JobStatusCompleted), job.ErrorMessage)
		Expect(cacheMaterial(classifyLink(jobID))).To(Equal(m.Digest()))
	})

	It("fails a retry from failure as attestation_invalid when an analyzer's sealing attempt was repointed, changing no cache data", func() {
		cfg, _, _ := seed()
		jobID := env.runJob(cfg)
		failInSynthesis(jobID)
		before := rowsOf(jobID)
		// app_user may update a failed job's stages; the pointer is not
		// trusted.
		tx, err := env.idb.tenantConn.Begin(env.ctxA)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Exec(env.ctxA,
			`UPDATE job_stages SET cache_attempt = $1 WHERE job_id = $2 AND stage_name = 'classify'`, uuid.NewString(), jobID)).To(Succeed())
		Expect(tx.Commit()).To(Succeed())

		job := retryFromFailure(jobID)
		Expect(job.Status).To(Equal(JobStatusFailed))
		Expect(job.ErrorClass).To(Equal(JobErrorAttestationInvalid))
		// The detail is the operator's: the stored message withholds it.
		Expect(job.ErrorMessage).To(HavePrefix("job " + job.JobID + " failed because of an internal error (error class attestation_invalid), " +
			"which a retry from its failure cannot fix. Start a new job: crosscodex run start "))
		Expect(job.ErrorMessage).NotTo(ContainSubstring("checkpoint"))
		Expect(rowsOf(jobID)).To(Equal(before))
	})
})
