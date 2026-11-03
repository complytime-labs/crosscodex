package resultcachetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// StoreEnv is one spec's store plus its privileged seeding path.
type StoreEnv struct {
	// Store is the store under test, as the restricted application role
	// uses it.
	Store resultcache.Store
	// AddJob makes jobID a running job of tenantID (a jobs row), provisioning
	// the tenant if needed. AddCatalog does the same for a catalog.
	AddJob     func(tenantID, jobID string)
	AddCatalog func(tenantID, catalogID string)
	// SetJobStatus moves an added job to status ("running", "completed",
	// "failed" or "cancelled"), as the pipeline does.
	SetJobStatus func(tenantID, jobID, status string)
}

// StoreSetup returns a store for one spec. It runs inside a BeforeEach and
// should register teardown with DeferCleanup. Every tenant, job and catalog
// the contract uses ends with token, so teardown can remove exactly them.
type StoreSetup func(token string) StoreEnv

// StoreContract registers the specs every resultcache.Store must pass. Run
// it against Fake in this package's suite and against pgstore as app_user
// in pkg/resultcache/pgstore's integration suite, so they cannot drift. It
// covers: tenant required (db.ErrTenantRequired, nothing changed); tenant
// isolation of every read and write; Lookup's hits and their producer's
// trusted digests; Put's upsert and atomic write entry; refusal of invalid
// entries and of unknown parents; RecordStage's idempotent hit entries and
// appended stats; Manifest's order and a digest any backend reproduces;
// VerifiedSummaries of signed attempts only; refusal of any record for a
// job that is not running (ErrJobNotRunning) or unknown (ErrUnknownJob);
// attempts: manifests scoped to one, facts kept per attempt;
// LookupVerifiable's snapshot and Snapshot.Verify over checkpoint-signed
// attempts (only the storing attempt vouches, so a re-put in an unsealed
// attempt is a miss; tampering corrupt, no checkpoint a miss, refs for
// another analyzer refused, repeated refs counted once); Checkpoint comparing the
// record under the job lock and sealing only a match.
func StoreContract(setup StoreSetup) {
	ginkgo.Describe("resultcache.Store contract", func() {
		var (
			env                                    StoreEnv
			ctxA, ctxB                             context.Context
			tenA, tenB, producer, consumer, jobOfB string
			catA, catB, catOfB                     string
		)
		// attemptA is the stage run every spec records under unless it is
		// about attempts; attemptB is a second run of the same stage.
		const (
			attemptA = "3b6c0e7a-1f2d-4e5c-8a9b-0c1d2e3f4a5b"
			attemptB = "9e8d7c6b-5a4f-4e3d-9c2b-1a0f9e8d7c6b"
		)
		key := func(n int) string { return fmt.Sprintf("%064x", n) }
		entry := func(job, task string, n int, result string) resultcache.Entry {
			return resultcache.Entry{JobID: job, Analyzer: "classify", Attempt: attemptA, TaskID: task, CacheKey: key(n),
				Result: []byte(result), ResponseModel: "qwen2.5:14b", SourceCatalogID: catA, TargetCatalogID: catB}
		}
		stats := func(hits, dispatched int) resultcache.Stats {
			return resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: hits + dispatched, Hits: hits, Dispatched: dispatched,
				Models: map[string]resultcache.ModelIdentity{"default": {Observed: "qwen2.5:14b"}}}
		}
		producersA := func(keys ...string) map[string]string {
			producers, err := env.Store.Producers(ctxA, "classify", keys)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return producers
		}
		manifestAt := func(job, attempt string) resultcache.Manifest {
			m, err := env.Store.Manifest(ctxA, job, "classify", attempt)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return m
		}
		manifestA := func(job string) resultcache.Manifest { return manifestAt(job, attemptA) }
		ref := func(job, attempt string) resultcache.AttemptRef {
			return resultcache.AttemptRef{JobID: job, Analyzer: "classify", Attempt: attempt}
		}
		// checkpoint plays the executor: its in-process record equals the
		// rows the store holds now, and seal returns the digest it signed.
		checkpoint := func(ctx context.Context, r resultcache.AttemptRef) string {
			record, err := env.Store.Manifest(ctx, r.JobID, r.Analyzer, r.Attempt)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			var signed string
			gomega.Expect(env.Store.Checkpoint(ctx, record, func(_ context.Context, digest string) error {
				signed = digest
				return nil
			})).To(gomega.Succeed())
			return signed
		}
		// sign checkpoints each attempt and returns what was signed.
		sign := func(ctx context.Context, refs ...resultcache.AttemptRef) map[resultcache.AttemptRef]string {
			out := map[resultcache.AttemptRef]string{}
			for _, r := range refs {
				out[r] = checkpoint(ctx, r)
			}
			return out
		}
		// signedSummary checkpoints attempt of job's classify stage for
		// VerifiedSummaries.
		signedSummary := func(ctx context.Context, job, attempt string) map[string]resultcache.SignedAttempt {
			return map[string]resultcache.SignedAttempt{"classify": {Attempt: attempt, Digest: checkpoint(ctx, ref(job, attempt))}}
		}
		// verify reads key and the signed attempts in one snapshot and
		// verifies the hit from it, as the engine will.
		verify := func(ctx context.Context, k string, signed map[resultcache.AttemptRef]string) (resultcache.Hit, error) {
			refs := make([]resultcache.AttemptRef, 0, len(signed))
			for r := range signed {
				refs = append(refs, r)
			}
			snap, err := env.Store.LookupVerifiable(ctx, "classify", []string{k}, refs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return snap.Verify(k, signed)
		}

		ginkgo.BeforeEach(func() {
			raw := make([]byte, 6)
			_, err := rand.Read(raw)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			token := hex.EncodeToString(raw)
			tenA, tenB = "rc-a-"+token, "rc-b-"+token
			producer, consumer, jobOfB = "job-p-"+token, "job-c-"+token, "job-x-"+token
			catA, catB, catOfB = "cat-a-"+token, "cat-b-"+token, "cat-x-"+token
			ctxA, err = tenant.WithTenant(context.Background(), tenA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ctxB, err = tenant.WithTenant(context.Background(), tenB)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			env = setup(token)
			env.AddJob(tenA, producer)
			env.AddJob(tenA, consumer)
			env.AddCatalog(tenA, catA)
			env.AddCatalog(tenA, catB)
			env.AddJob(tenB, jobOfB)
			env.AddCatalog(tenB, catOfB)
		})

		ginkgo.DescribeTable("refuses every call without a tenant in ctx, changing nothing",
			func(call func(ctx context.Context) error) {
				gomega.Expect(call(context.Background())).To(gomega.MatchError(db.ErrTenantRequired))
				gomega.Expect(producersA(key(1))).To(gomega.BeEmpty())
				m := manifestA(producer)
				gomega.Expect(m.Entries).To(gomega.BeEmpty())
				gomega.Expect(m.Stats).To(gomega.BeEmpty())
			},
			ginkgo.Entry("Producers", func(ctx context.Context) error {
				_, err := env.Store.Producers(ctx, "classify", []string{key(1)})
				return err
			}),
			ginkgo.Entry("Put", func(ctx context.Context) error {
				return env.Store.Put(ctx, entry(producer, "t1", 1, `{"model":"m"}`))
			}),
			ginkgo.Entry("RecordStage", func(ctx context.Context) error {
				return env.Store.RecordStage(ctx, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 1)})
			}),
			ginkgo.Entry("Manifest", func(ctx context.Context) error {
				_, err := env.Store.Manifest(ctx, producer, "classify", attemptA)
				return err
			}),
			ginkgo.Entry("VerifiedSummaries", func(ctx context.Context) error {
				_, err := env.Store.VerifiedSummaries(ctx, producer, map[string]resultcache.SignedAttempt{"classify": {Attempt: attemptA}})
				return err
			}),
			ginkgo.Entry("LookupVerifiable", func(ctx context.Context) error {
				_, err := env.Store.LookupVerifiable(ctx, "classify", []string{key(1)}, []resultcache.AttemptRef{ref(producer, attemptA)})
				return err
			}),
			ginkgo.Entry("Checkpoint", func(ctx context.Context) error {
				return env.Store.Checkpoint(ctx, resultcache.Manifest{TenantID: tenA, JobID: producer, Analyzer: "classify", Attempt: attemptA},
					func(context.Context, string) error {
						ginkgo.Fail("Checkpoint ran seal without a tenant")
						return nil
					})
			}),
		)

		ginkgo.It("returns a stored result with its producer and the producer's write digest", func() {
			e := entry(producer, "t1", 1, `{"model":"m","response":"x"}`)
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())

			gomega.Expect(producersA(key(1), key(2))).To(gomega.Equal(map[string]string{key(1): producer}))
			h, err := verify(ctxA, key(1), sign(ctxA, ref(producer, attemptA)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(h).To(gomega.Equal(resultcache.Hit{CacheKey: key(1), Result: e.Result, ResponseModel: "qwen2.5:14b",
				ProducerJobID: producer, ProducerAttempt: attemptA}))
		})

		ginkgo.It("returns an empty map for no keys and no hit for a key nobody stored", func() {
			gomega.Expect(producersA()).To(gomega.BeEmpty())
			gomega.Expect(producersA(key(9))).To(gomega.BeEmpty())
		})

		ginkgo.It("replaces the result, model and producer on a later put, and the hit verifies only against the new producer's write", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m","response":"old"}`))).To(gomega.Succeed())
			newer := entry(consumer, "t9", 1, `{"model":"m2","response":"new"}`)
			newer.ResponseModel = "qwen2.5:32b"
			gomega.Expect(env.Store.Put(ctxA, newer)).To(gomega.Succeed())

			gomega.Expect(producersA(key(1))).To(gomega.Equal(map[string]string{key(1): consumer}))
			h, err := verify(ctxA, key(1), sign(ctxA, ref(consumer, attemptA)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(string(h.Result)).To(gomega.Equal(`{"model":"m2","response":"new"}`))
			gomega.Expect(h.ResponseModel).To(gomega.Equal("qwen2.5:32b"))
			_, err = verify(ctxA, key(1), sign(ctxA, ref(producer, attemptA)))
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrUntrusted), "only the row's producer can vouch for it")
			gomega.Expect(manifestA(producer).Entries).To(gomega.HaveLen(1), "the old producer's write stays as a fact")
		})

		ginkgo.It("records one write entry however often the same result is put", func() {
			e := entry(producer, "t1", 1, `{"model":"m"}`)
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())
			gomega.Expect(manifestA(producer).Entries).To(gomega.Equal([]resultcache.ManifestEntry{{
				TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key(1), ResultDigest: resultcache.ResultDigest(e.Result),
			}}))
		})

		ginkgo.DescribeTable("refuses an invalid entry, storing nothing",
			func(mutate func(*resultcache.Entry)) {
				e := entry(producer, "t1", 1, `{"model":"m"}`)
				mutate(&e)
				gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.MatchError(resultcache.ErrInvalidEntry))
				gomega.Expect(producersA(key(1))).To(gomega.BeEmpty())
				gomega.Expect(manifestA(producer).Entries).To(gomega.BeEmpty())
			},
			ginkgo.Entry("no job", func(e *resultcache.Entry) { e.JobID = "" }),
			ginkgo.Entry("no attempt", func(e *resultcache.Entry) { e.Attempt = "" }),
			ginkgo.Entry("a malformed key", func(e *resultcache.Entry) { e.CacheKey = "XYZ" }),
			ginkgo.Entry("no result", func(e *resultcache.Entry) { e.Result = nil }),
			ginkgo.Entry("a result that is not JSON", func(e *resultcache.Entry) { e.Result = []byte(`{"model":`) }),
			ginkgo.Entry("a result with invalid UTF-8", func(e *resultcache.Entry) { e.Result = []byte("{\"model\":\"\xff\"}") }),
			ginkgo.Entry("a result with a raw NUL byte", func(e *resultcache.Entry) { e.Result = []byte("{\"model\":\"m\x00\"}") }),
			ginkgo.Entry("no response model", func(e *resultcache.Entry) { e.ResponseModel = "" }),
			ginkgo.Entry("no catalog", func(e *resultcache.Entry) { e.TargetCatalogID = "" }),
		)

		ginkgo.DescribeTable("refuses a put naming a job or catalog the tenant does not have, storing nothing",
			func(mutate func(*resultcache.Entry)) {
				e := entry(producer, "t1", 1, `{"model":"m"}`)
				mutate(&e)
				gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.HaveOccurred())
				gomega.Expect(producersA(key(1))).To(gomega.BeEmpty())
				gomega.Expect(manifestA(e.JobID).Entries).To(gomega.BeEmpty())
				m, err := env.Store.Manifest(ctxB, e.JobID, "classify", attemptA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(m.Entries).To(gomega.BeEmpty(), "the other tenant's job must gain no write entry")
			},
			ginkgo.Entry("an unknown job", func(e *resultcache.Entry) { e.JobID = "job-none-" + e.JobID }),
			ginkgo.Entry("another tenant's job", func(e *resultcache.Entry) { e.JobID = jobOfB }),
			ginkgo.Entry("another tenant's source catalog", func(e *resultcache.Entry) { e.SourceCatalogID = catOfB }),
			ginkgo.Entry("another tenant's target catalog", func(e *resultcache.Entry) { e.TargetCatalogID = catOfB }),
		)

		ginkgo.It("lets two tenants store different results under the same key, each seeing only its own", func() {
			jobB, catB1, catB2 := jobOfB, catOfB, "cat-y-"+catOfB
			env.AddCatalog(tenB, catB2)
			a := entry(producer, "t1", 1, `{"model":"m","response":"from A"}`)
			b := resultcache.Entry{JobID: jobB, Analyzer: "classify", Attempt: attemptA, TaskID: "t1", CacheKey: key(1),
				Result: []byte(`{"model":"m","response":"from B"}`), ResponseModel: "qwen2.5:32b",
				SourceCatalogID: catB1, TargetCatalogID: catB2}
			gomega.Expect(env.Store.Put(ctxA, a)).To(gomega.Succeed())
			gomega.Expect(env.Store.Put(ctxB, b)).To(gomega.Succeed())

			hitA, err := verify(ctxA, key(1), sign(ctxA, ref(producer, attemptA)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(string(hitA.Result)).To(gomega.Equal(string(a.Result)))
			gomega.Expect(hitA.ProducerJobID).To(gomega.Equal(producer))

			hitB, err := verify(ctxB, key(1), sign(ctxB, ref(jobB, attemptA)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(string(hitB.Result)).To(gomega.Equal(string(b.Result)))
			gomega.Expect(hitB.ResponseModel).To(gomega.Equal("qwen2.5:32b"))
			gomega.Expect(hitB.ProducerJobID).To(gomega.Equal(jobB))
		})

		ginkgo.It("does not return a row stored for one analyzer when another analyzer asks for the key", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			producers, err := env.Store.Producers(ctxA, "artifacts", []string{key(1)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(producers).To(gomega.BeEmpty())
		})

		ginkgo.It("never shows one tenant's rows to another, nor lets it record against them", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 1)})).To(gomega.Succeed())

			producers, err := env.Store.Producers(ctxB, "classify", []string{key(1)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(producers).To(gomega.BeEmpty())
			m, err := env.Store.Manifest(ctxB, producer, "classify", attemptA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(m.Entries).To(gomega.BeEmpty())
			gomega.Expect(m.Stats).To(gomega.BeEmpty())
			signedA := signedSummary(ctxA, producer, attemptA)
			sums, err := env.Store.VerifiedSummaries(ctxB, producer, signedA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(sums).To(gomega.Equal([]resultcache.Summary{{Analyzer: "classify", Attempt: attemptA}}),
				"tenant B reads none of A's rows, so A's signature attests nothing to B")
			snap, err := env.Store.LookupVerifiable(ctxB, "classify", []string{key(1)}, []resultcache.AttemptRef{ref(producer, attemptA)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(snap.Keys()).To(gomega.BeEmpty())
			gomega.Expect(env.Store.Checkpoint(ctxB, resultcache.Manifest{TenantID: tenB, JobID: producer, Analyzer: "classify", Attempt: attemptA},
				func(context.Context, string) error {
					ginkgo.Fail("Checkpoint ran seal for another tenant's job")
					return nil
				})).To(gomega.MatchError(resultcache.ErrUnknownJob))
			gomega.Expect(env.Store.RecordStage(ctxB, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 1)})).
				To(gomega.MatchError(resultcache.ErrUnknownJob))
			gomega.Expect(manifestA(producer).Stats).To(gomega.HaveLen(1))
		})

		ginkgo.It("records hit entries once and appends a stats row per call", func() {
			hit := resultcache.HitEntry{TaskID: "t1", CacheKey: key(1), ResultDigest: resultcache.ResultDigest([]byte(`{"model":"m"}`)), ProducerJobID: producer}
			first := resultcache.StageRecord{JobID: consumer, Analyzer: "classify", Attempt: attemptA, Hits: []resultcache.HitEntry{hit}, Stats: stats(1, 0)}
			second := first
			second.Stats = stats(1, 2)
			gomega.Expect(env.Store.RecordStage(ctxA, first)).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, second)).To(gomega.Succeed())

			m := manifestA(consumer)
			gomega.Expect(m.Entries).To(gomega.Equal([]resultcache.ManifestEntry{{
				TaskID: "t1", Role: resultcache.RoleHit, CacheKey: key(1), ResultDigest: hit.ResultDigest, ProducerJobID: producer,
			}}))
			gomega.Expect(m.Stats).To(gomega.Equal([]resultcache.Stats{first.Stats, second.Stats}))
		})

		ginkgo.DescribeTable("refuses an invalid stage record, storing nothing",
			func(mutate func(*resultcache.StageRecord)) {
				rec := resultcache.StageRecord{JobID: consumer, Analyzer: "classify", Attempt: attemptA, Stats: stats(1, 0),
					Hits: []resultcache.HitEntry{{TaskID: "t1", CacheKey: key(1), ResultDigest: resultcache.ResultDigest([]byte("x")), ProducerJobID: producer}}}
				mutate(&rec)
				gomega.Expect(env.Store.RecordStage(ctxA, rec)).To(gomega.MatchError(resultcache.ErrInvalidEntry))
				m := manifestA(consumer)
				gomega.Expect(m.Entries).To(gomega.BeEmpty())
				gomega.Expect(m.Stats).To(gomega.BeEmpty())
			},
			ginkgo.Entry("a hit without its producer", func(r *resultcache.StageRecord) { r.Hits[0].ProducerJobID = "" }),
			ginkgo.Entry("no attempt", func(r *resultcache.StageRecord) { r.Attempt = "" }),
			ginkgo.Entry("hits and dispatches over tasks", func(r *resultcache.StageRecord) { r.Stats.Dispatched = 1 }),
			ginkgo.Entry("an unknown mode", func(r *resultcache.StageRecord) { r.Stats.Mode = "sometimes" }),
			ginkgo.Entry("hit entries that disagree with the hit count", func(r *resultcache.StageRecord) { r.Hits = nil }),
		)

		// A job's record is frozen once it stops running: nobody can add a
		// write digest that would make an edited result verify, or a stats row
		// that would make a finished job look fresh.
		ginkgo.DescribeTable("refuses records for a job that is not running, changing nothing",
			func(status string, record func() error) {
				gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
				gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 1)})).
					To(gomega.Succeed())
				before := manifestA(producer)
				signedA := signedSummary(ctxA, producer, attemptA)
				sumsBefore, err := env.Store.VerifiedSummaries(ctxA, producer, signedA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(sumsBefore[0].Attested).To(gomega.BeTrue())
				env.SetJobStatus(tenA, producer, status)

				err = record()
				gomega.Expect(err).To(gomega.MatchError(resultcache.ErrJobNotRunning))
				gomega.Expect(err).To(gomega.MatchError(gomega.And(
					gomega.ContainSubstring(fmt.Sprintf("job %s: its status is %q", producer, status)),
					gomega.ContainSubstring("only while the job runs"))))
				gomega.Expect(manifestA(producer)).To(gomega.Equal(before))
				sums, err := env.Store.VerifiedSummaries(ctxA, producer, signedA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(sums).To(gomega.Equal(sumsBefore))
				gomega.Expect(producersA(key(2))).To(gomega.BeEmpty(), "no cache row is stored without its write entry")
			},
			ginkgo.Entry("a write entry for a completed job", "completed", func() error {
				return env.Store.Put(ctxA, entry(producer, "t2", 2, `{"model":"m"}`))
			}),
			ginkgo.Entry("a write entry for a failed job", "failed", func() error {
				return env.Store.Put(ctxA, entry(producer, "t2", 2, `{"model":"m"}`))
			}),
			ginkgo.Entry("a write entry for a cancelled job", "cancelled", func() error {
				return env.Store.Put(ctxA, entry(producer, "t2", 2, `{"model":"m"}`))
			}),
			ginkgo.Entry("a stats row for a completed job", "completed", func() error {
				return env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 9)})
			}),
			ginkgo.Entry("a stats row for a failed job", "failed", func() error {
				return env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 9)})
			}),
			ginkgo.Entry("a stats row for a cancelled job", "cancelled", func() error {
				return env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 9)})
			}),
		)

		ginkgo.It("scopes a manifest to its attempt: attempt A's excludes attempt B's rows", func() {
			inA := entry(producer, "t1", 1, `{"model":"m","response":"a"}`)
			inB := entry(producer, "t2", 2, `{"model":"m","response":"b"}`)
			inB.Attempt = attemptB
			gomega.Expect(env.Store.Put(ctxA, inA)).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 1)})).To(gomega.Succeed())
			gomega.Expect(env.Store.Put(ctxA, inB)).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptB, Stats: stats(0, 2)})).To(gomega.Succeed())

			gomega.Expect(manifestAt(producer, attemptA)).To(gomega.Equal(resultcache.Manifest{
				TenantID: tenA, JobID: producer, Analyzer: "classify", Attempt: attemptA,
				Entries: []resultcache.ManifestEntry{{TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key(1), ResultDigest: resultcache.ResultDigest(inA.Result)}},
				Stats:   []resultcache.Stats{stats(0, 1)},
			}))
			gomega.Expect(manifestAt(producer, attemptB)).To(gomega.Equal(resultcache.Manifest{
				TenantID: tenA, JobID: producer, Analyzer: "classify", Attempt: attemptB,
				Entries: []resultcache.ManifestEntry{{TaskID: "t2", Role: resultcache.RoleWrite, CacheKey: key(2), ResultDigest: resultcache.ResultDigest(inB.Result)}},
				Stats:   []resultcache.Stats{stats(0, 2)},
			}))
		})

		ginkgo.It("refuses a manifest for an invalid attempt", func() {
			_, err := env.Store.Manifest(ctxA, producer, "classify", "")
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrInvalidEntry))
		})

		// The producer sealed attempt A, then re-stored the key with another
		// result in attempt B, which was never sealed (it crashed, or the
		// row was planted). The row now names attempt B, which nothing
		// signed: that is a miss to recompute, not tampering with A.
		ginkgo.It("treats a result its producer re-stored in an unsealed later attempt as a miss, not corrupt", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m","response":"sealed"}`))).To(gomega.Succeed())
			signed := sign(ctxA, ref(producer, attemptA))
			again := entry(producer, "t1", 1, `{"model":"m","response":"unsealed"}`)
			again.Attempt = attemptB
			gomega.Expect(env.Store.Put(ctxA, again)).To(gomega.Succeed())

			_, err := verify(ctxA, key(1), signed)
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrUntrusted))
			gomega.Expect(err).NotTo(gomega.MatchError(resultcache.ErrCorrupt))

			h, err := verify(ctxA, key(1), sign(ctxA, ref(producer, attemptA), ref(producer, attemptB)))
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "once attempt B is sealed, it vouches for its result")
			gomega.Expect(string(h.Result)).To(gomega.Equal(`{"model":"m","response":"unsealed"}`))
			gomega.Expect(h.ProducerAttempt).To(gomega.Equal(attemptB))
		})

		// Only the attempt that stored the row vouches for it: another
		// signed attempt of the producer, whatever it recorded, does not.
		ginkgo.It("does not trust a result through another signed attempt of its producer", func() {
			e := entry(producer, "t1", 1, `{"model":"m","response":"x"}`)
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())
			selfHit := resultcache.HitEntry{TaskID: "t1", CacheKey: key(1), ResultDigest: resultcache.ResultDigest(e.Result), ProducerJobID: producer}
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptB,
				Hits: []resultcache.HitEntry{selfHit}, Stats: stats(1, 0)})).To(gomega.Succeed())

			_, err := verify(ctxA, key(1), sign(ctxA, ref(producer, attemptB)))
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrUntrusted))
		})

		// A row the signed attempt that stored it does not vouch for was
		// edited after signing.
		ginkgo.It("reports a result its signed storing attempt does not vouch for as corrupt", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m","response":"genuine"}`))).To(gomega.Succeed())
			signed := sign(ctxA, ref(producer, attemptA))
			// A put of another task under the same attempt re-stores the key
			// and adds a write the signature does not cover.
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t2", 1, `{"model":"m","response":"planted"}`))).To(gomega.Succeed())

			_, err := verify(ctxA, key(1), signed)
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrCorrupt))
		})

		ginkgo.It("treats a result whose producer has no signed attempt as a miss", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			_, err := verify(ctxA, key(1), map[resultcache.AttemptRef]string{})
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrUntrusted))
		})

		// A row added to an attempt after its checkpoint was signed (the job
		// still runs, so the freeze allows it) changes that attempt's
		// manifest, which then no longer matches the signature.
		ginkgo.It("reports a signed attempt that gained a row after signing as corrupt", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			signed := sign(ctxA, ref(producer, attemptA))
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t2", 2, `{"model":"m"}`))).To(gomega.Succeed())
			_, err := verify(ctxA, key(1), signed)
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrCorrupt))
		})

		ginkgo.It("reads the hits and only the requested attempts of their producers", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptB, Stats: stats(0, 1)})).To(gomega.Succeed())
			signed := sign(ctxA, ref(producer, attemptA))

			snap, err := env.Store.LookupVerifiable(ctxA, "classify", []string{key(1), key(9)},
				[]resultcache.AttemptRef{ref(producer, attemptA), ref(consumer, attemptA)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(snap.Keys()).To(gomega.Equal([]string{key(1)}))
			_, err = snap.Verify(key(1), signed)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			unread, err := env.Store.LookupVerifiable(ctxA, "classify", []string{key(1)}, []resultcache.AttemptRef{ref(consumer, attemptA)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = unread.Verify(key(1), signed)
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrInvalidEntry), "the producer's attempt was not requested, so not read")
		})

		ginkgo.It("counts a repeated attempt once", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			signed := sign(ctxA, ref(producer, attemptA))
			snap, err := env.Store.LookupVerifiable(ctxA, "classify", []string{key(1)},
				[]resultcache.AttemptRef{ref(producer, attemptA), ref(producer, attemptA)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = snap.Verify(key(1), signed)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("refuses an attempt for another analyzer rather than skipping it", func() {
			other := ref(producer, attemptA)
			other.Analyzer = "artifacts"
			_, err := env.Store.LookupVerifiable(ctxA, "classify", []string{key(1)}, []resultcache.AttemptRef{other})
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrInvalidEntry))
		})

		ginkgo.It("refuses a stage record for a job the tenant does not have with ErrUnknownJob", func() {
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: "job-none-" + producer, Analyzer: "classify", Attempt: attemptA,
				Stats: stats(0, 1)})).To(gomega.MatchError(resultcache.ErrUnknownJob))
		})

		ginkgo.It("stores the same fact once per attempt: twice across two attempts, once within one", func() {
			e := entry(producer, "t1", 1, `{"model":"m"}`)
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())
			again := e
			again.Attempt = attemptB
			gomega.Expect(env.Store.Put(ctxA, again)).To(gomega.Succeed())

			write := resultcache.ManifestEntry{TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key(1), ResultDigest: resultcache.ResultDigest(e.Result)}
			gomega.Expect(manifestAt(producer, attemptA).Entries).To(gomega.Equal([]resultcache.ManifestEntry{write}))
			gomega.Expect(manifestAt(producer, attemptB).Entries).To(gomega.Equal([]resultcache.ManifestEntry{write}))

			hit := resultcache.HitEntry{TaskID: "t2", CacheKey: key(2), ResultDigest: resultcache.ResultDigest([]byte("x")), ProducerJobID: producer}
			for _, attempt := range []string{attemptA, attemptA, attemptB} {
				gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: consumer, Analyzer: "classify", Attempt: attempt,
					Hits: []resultcache.HitEntry{hit}, Stats: stats(1, 0)})).To(gomega.Succeed())
			}
			hitEntry := resultcache.ManifestEntry{TaskID: "t2", Role: resultcache.RoleHit, CacheKey: key(2), ResultDigest: hit.ResultDigest, ProducerJobID: producer}
			gomega.Expect(manifestAt(consumer, attemptA).Entries).To(gomega.Equal([]resultcache.ManifestEntry{hitEntry}))
			gomega.Expect(manifestAt(consumer, attemptA).Stats).To(gomega.HaveLen(2), "a stats row per call")
			gomega.Expect(manifestAt(consumer, attemptB).Entries).To(gomega.Equal([]resultcache.ManifestEntry{hitEntry}))
		})

		ginkgo.It("summarizes a signed attempt from its latest stats and the other jobs whose results it reused", func() {
			digest := resultcache.ResultDigest([]byte("x"))
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: consumer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 2)})).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: consumer, Analyzer: "classify", Attempt: attemptA, Stats: stats(2, 0),
				Hits: []resultcache.HitEntry{
					{TaskID: "t1", CacheKey: key(1), ResultDigest: digest, ProducerJobID: producer},
					{TaskID: "t2", CacheKey: key(2), ResultDigest: digest, ProducerJobID: consumer},
				}})).To(gomega.Succeed())

			sums, err := env.Store.VerifiedSummaries(ctxA, consumer, signedSummary(ctxA, consumer, attemptA))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(sums).To(gomega.Equal([]resultcache.Summary{{Analyzer: "classify", Attempt: attemptA, Attested: true,
				Stats: stats(2, 0), ProducerJobIDs: []string{producer}}}))
		})

		ginkgo.It("reads only the signed attempt: rows planted under another attempt change nothing", func() {
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 2)})).To(gomega.Succeed())
			signedA := signedSummary(ctxA, producer, attemptA)
			before, err := env.Store.VerifiedSummaries(ctxA, producer, signedA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: "planted",
				Hits:  []resultcache.HitEntry{{TaskID: "t1", CacheKey: key(1), ResultDigest: resultcache.ResultDigest([]byte("x")), ProducerJobID: consumer}},
				Stats: stats(1, 0)})).To(gomega.Succeed())
			after, err := env.Store.VerifiedSummaries(ctxA, producer, signedA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(after).To(gomega.Equal(before))
			gomega.Expect(after[0].Attested).To(gomega.BeTrue())
		})

		ginkgo.It("shows a signed attempt that gained a row after signing as unattested", func() {
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 2)})).To(gomega.Succeed())
			signedA := signedSummary(ctxA, producer, attemptA)
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 9)})).To(gomega.Succeed())
			sums, err := env.Store.VerifiedSummaries(ctxA, producer, signedA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(sums).To(gomega.Equal([]resultcache.Summary{{Analyzer: "classify", Attempt: attemptA}}))
		})

		ginkgo.It("refuses summaries for an invalid attempt", func() {
			_, err := env.Store.VerifiedSummaries(ctxA, producer, map[string]resultcache.SignedAttempt{"classify": {}})
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrInvalidEntry))
		})

		ginkgo.It("checkpoints a matching record by sealing its digest, returning seal's error", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: producer, Analyzer: "classify", Attempt: attemptA, Stats: stats(0, 1)})).To(gomega.Succeed())
			record := resultcache.Manifest{TenantID: tenA, JobID: producer, Analyzer: "classify", Attempt: attemptA,
				Entries: []resultcache.ManifestEntry{{TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key(1), ResultDigest: resultcache.ResultDigest([]byte(`{"model":"m"}`))}},
				Stats:   []resultcache.Stats{stats(0, 1)}}
			var sealed string
			gomega.Expect(env.Store.Checkpoint(ctxA, record, func(_ context.Context, digest string) error {
				sealed = digest
				return nil
			})).To(gomega.Succeed())
			gomega.Expect(sealed).To(gomega.Equal(record.Digest()))

			errSeal := errors.New("signing key unavailable")
			gomega.Expect(env.Store.Checkpoint(ctxA, record, func(context.Context, string) error { return errSeal })).To(gomega.MatchError(errSeal))
		})

		// ADV-9: a row the stage did not record (planted while the job runs)
		// stops the checkpoint before anything is signed.
		ginkgo.It("refuses a record the rows do not match as corrupt, without sealing or changing anything", func() {
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t1", 1, `{"model":"m"}`))).To(gomega.Succeed())
			record := manifestA(producer)
			gomega.Expect(env.Store.Put(ctxA, entry(producer, "t2", 2, `{"model":"planted"}`))).To(gomega.Succeed())
			stored := manifestA(producer)

			err := env.Store.Checkpoint(ctxA, record, func(context.Context, string) error {
				ginkgo.Fail("seal ran for a record the rows do not match")
				return nil
			})
			gomega.Expect(err).To(gomega.MatchError(resultcache.ErrCorrupt))
			gomega.Expect(err).To(gomega.MatchError(gomega.And(
				gomega.ContainSubstring("unexpected rows: entry(task=\"t2\" role=write key="+key(2)),
				gomega.ContainSubstring("missing rows: none"),
				gomega.Not(gomega.ContainSubstring("planted")))), "rows are named by identity, never by their bytes")
			gomega.Expect(manifestA(producer)).To(gomega.Equal(stored))
		})

		ginkgo.DescribeTable("refuses a checkpoint it cannot take, before seal runs",
			func(mutate func(*resultcache.Manifest), want error) {
				record := resultcache.Manifest{TenantID: tenA, JobID: producer, Analyzer: "classify", Attempt: attemptA}
				mutate(&record)
				err := env.Store.Checkpoint(ctxA, record, func(context.Context, string) error {
					ginkgo.Fail("seal ran")
					return nil
				})
				gomega.Expect(err).To(gomega.MatchError(want))
			},
			ginkgo.Entry("an invalid attempt", func(m *resultcache.Manifest) { m.Attempt = "" }, resultcache.ErrInvalidEntry),
			ginkgo.Entry("no analyzer", func(m *resultcache.Manifest) { m.Analyzer = "" }, resultcache.ErrInvalidEntry),
			ginkgo.Entry("a record of another tenant", func(m *resultcache.Manifest) { m.TenantID = tenB }, resultcache.ErrInvalidEntry),
			ginkgo.Entry("an unknown job", func(m *resultcache.Manifest) { m.JobID = "job-none" }, resultcache.ErrUnknownJob),
			ginkgo.Entry("another tenant's job", func(m *resultcache.Manifest) { m.JobID = jobOfB }, resultcache.ErrUnknownJob),
		)

		ginkgo.It("returns a manifest whose digest is the one built from the same facts", func() {
			e := entry(consumer, "t2", 2, `{"model":"m"}`)
			gomega.Expect(env.Store.Put(ctxA, e)).To(gomega.Succeed())
			hit := resultcache.HitEntry{TaskID: "t1", CacheKey: key(1), ResultDigest: resultcache.ResultDigest([]byte("x")), ProducerJobID: producer}
			st := stats(1, 1)
			gomega.Expect(env.Store.RecordStage(ctxA, resultcache.StageRecord{JobID: consumer, Analyzer: "classify", Attempt: attemptA,
				Hits: []resultcache.HitEntry{hit}, Stats: st})).To(gomega.Succeed())

			want := resultcache.Manifest{
				TenantID: tenA, JobID: consumer, Analyzer: "classify", Attempt: attemptA,
				Entries: []resultcache.ManifestEntry{
					{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: key(1), ResultDigest: hit.ResultDigest, ProducerJobID: producer},
					{TaskID: "t2", Role: resultcache.RoleWrite, CacheKey: key(2), ResultDigest: resultcache.ResultDigest(e.Result)},
				},
				Stats: []resultcache.Stats{st},
			}
			got := manifestA(consumer)
			gomega.Expect(got.Entries).To(gomega.Equal(want.Entries))
			gomega.Expect(got.Digest()).To(gomega.Equal(want.Digest()))
		})
	})
}
