//go:build integration

package artifacts_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// verdictStoreEnv is one isolated PostgreSQL fixture: two fresh tenants, a
// store on an app_user pool (so RLS applies), a raw app_user connection for
// policy probes and a superuser pool for setup and inspection.
type verdictStoreEnv struct {
	store            *artifacts.PGVerdictStore
	su               db.Pool
	app              *sql.DB
	tenantA, tenantB string
	ctxA, ctxB       context.Context
}

func setupVerdictStore() verdictStoreEnv {
	ctx := context.Background()
	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	migrator, err := db.NewMigrator(suDSN)
	Expect(err).NotTo(HaveOccurred())
	Expect(migrator.Up(ctx)).To(Succeed())
	Expect(migrator.Close()).To(Succeed())

	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred())
	stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
	Expect(err).NotTo(HaveOccurred())
	adminDB, err := sql.Open("pgx", suDSN)
	Expect(err).NotTo(HaveOccurred())
	_, err = adminDB.ExecContext(ctx, stmt)
	Expect(err).NotTo(HaveOccurred())
	Expect(adminDB.Close()).To(Succeed())

	su, err := db.NewPool(db.PoolConfig{DSN: suDSN, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { su.Close() })

	u, err := url.Parse(suDSN)
	Expect(err).NotTo(HaveOccurred())
	u.User = url.UserPassword("app_user", appPassword)
	appPool, err := db.NewPool(db.PoolConfig{DSN: u.String(), MaxOpenConns: 10})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { appPool.Close() })
	app, err := sql.Open("pgx", u.String())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { app.Close() })

	env := verdictStoreEnv{
		store: artifacts.NewPGVerdictStore(db.NewTenantPool(appPool)), su: su, app: app,
		tenantA: "verdicts-a-" + uuid.NewString(), tenantB: "verdicts-b-" + uuid.NewString(),
	}
	for _, id := range []string{env.tenantA, env.tenantB} {
		Expect(db.EnsureTenant(ctx, su, id, "verdict store it")).To(Succeed())
	}
	DeferCleanup(func() {
		for _, id := range []string{env.tenantA, env.tenantB} {
			Expect(su.Exec(ctx, "DELETE FROM artifact_pair_verdicts WHERE tenant_id = $1", id)).To(Succeed())
			Expect(su.Exec(ctx, "DELETE FROM tenants WHERE tenant_id = $1", id)).To(Succeed())
		}
	})
	env.ctxA, err = tenant.WithTenant(ctx, env.tenantA)
	Expect(err).NotTo(HaveOccurred())
	env.ctxB, err = tenant.WithTenant(ctx, env.tenantB)
	Expect(err).NotTo(HaveOccurred())
	return env
}

type storedVerdict struct {
	status, determination, determinedBy string
	candidateEdgeID                     string
	similarityScore                     float64
	context                             []byte
	createdAt                           time.Time
	confidence                          sql.NullFloat64
	attempts                            int
	nextAttemptAt                       time.Time
	leasedUntil, decidedAt              sql.NullTime
	lastError                           sql.NullString
	graphApplied                        bool
	dissent                             []byte
	claimedUntil                        sql.NullTime
}

// stored reads a row as the superuser, bypassing RLS.
func (e verdictStoreEnv) stored(tenantID, key string) storedVerdict {
	var v storedVerdict
	Expect(e.su.QueryRow(context.Background(), `
		SELECT status, determination_type, determined_by, candidate_edge_id, similarity_score, context, created_at,
		       confidence, attempts, next_attempt_at, leased_until, decided_at, last_error, graph_applied, dissent,
		       projection_claimed_until
		FROM artifact_pair_verdicts WHERE tenant_id = $1 AND pair_key = $2`, tenantID, key).Scan(
		&v.status, &v.determination, &v.determinedBy, &v.candidateEdgeID, &v.similarityScore, &v.context, &v.createdAt,
		&v.confidence, &v.attempts, &v.nextAttemptAt, &v.leasedUntil, &v.decidedAt, &v.lastError, &v.graphApplied, &v.dissent,
		&v.claimedUntil)).To(Succeed())
	return v
}

// count returns how many pairs the tenant has stored, read as the superuser.
func (e verdictStoreEnv) count(tenantID string) int {
	var n int
	Expect(e.su.QueryRow(context.Background(), `SELECT count(*) FROM artifact_pair_verdicts WHERE tenant_id = $1`,
		tenantID).Scan(&n)).To(Succeed())
	return n
}

func verdictCandidate(i int) artifacts.PairCandidate {
	return artifacts.PairCandidate{
		LowGroupID: fmt.Sprintf("g-%03d-a", i), HighGroupID: fmt.Sprintf("g-%03d-b", i),
		CandidateEdgeID: fmt.Sprintf("edge-%03d", i), SimilarityScore: 0.75,
		Context: artifacts.PairContext{
			Type: "document", LowName: "access policy", HighName: "access control policy",
			LowSamples:  []artifacts.MemberSample{{Name: "Access Policy", OwnerRole: "CISO", Frequency: "annual"}},
			HighSamples: []artifacts.MemberSample{{Name: "Access Control Policy"}},
		},
	}
}

func keyOf(c artifacts.PairCandidate) string { return artifacts.PairKey(c.LowGroupID, c.HighGroupID) }

var _ = Describe("PGVerdictStore", func() {
	var (
		env verdictStoreEnv
		t0  time.Time
		ev  artifacts.PanelEvidence
	)

	BeforeEach(func() {
		env = setupVerdictStore()
		t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		yes := true
		ev = artifacts.PanelEvidence{PromptName: "artifact_same_as", PromptVersion: "1.0.0", Models: []string{"m1"},
			Votes:      []artifacts.VoteRecord{{VoterID: "m1__s0", Decision: &yes, Confidence: "HIGH", Justification: "same"}},
			ValidVotes: 1, TotalVotes: 1}
	})

	enqueue := func(cs ...artifacts.PairCandidate) {
		Expect(env.store.Enqueue(env.ctxA, env.tenantA, cs, t0)).To(Succeed())
	}
	// project claims p at t0 for a minute and releases the claim with
	// MarkProjected, returning MarkProjected's report. A pair that cannot be
	// claimed is reported false.
	project := func(p artifacts.Projection) bool {
		out, err := env.store.ClaimProjection(env.ctxA, env.tenantA, p, t0.Add(time.Minute), t0)
		Expect(err).NotTo(HaveOccurred())
		if out != artifacts.ClaimAcquired {
			return false
		}
		ok, err := env.store.MarkProjected(env.ctxA, env.tenantA, p, t0.Add(time.Minute))
		Expect(err).NotTo(HaveOccurred())
		return ok
	}

	Describe("Enqueue and Lease", func() {
		It("enqueues a pair once and leases it with its context", func() {
			c := verdictCandidate(1)
			enqueue(c)
			Expect(env.store.Enqueue(env.ctxA, env.tenantA, []artifacts.PairCandidate{c}, t0.Add(time.Hour))).To(Succeed())

			pairs, err := env.store.Lease(env.ctxA, env.tenantA, 10, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(HaveLen(1))
			Expect(pairs[0].LeasedUntil).To(BeTemporally("==", t0.Add(time.Minute)))
			pairs[0].LeasedUntil = time.Time{}
			Expect(pairs).To(Equal([]artifacts.LeasedPair{{PairCandidate: c, Key: keyOf(c)}}))
			row := env.stored(env.tenantA, keyOf(c))
			Expect(row.status).To(Equal("pending"))
			Expect(row.determination).To(Equal("token_overlap"))
			Expect(row.determinedBy).To(Equal("artifact-reconciler"))
		})

		It("moves a re-enqueued pending pair to the new candidate edge and score, keeping the rest of its row", func() {
			c := verdictCandidate(1)
			enqueue(c)
			first, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(first).To(HaveLen(1))
			Expect(env.store.RecordFailure(env.ctxA, env.tenantA, keyOf(c), artifacts.Failure{
				Err: "boom", At: t0, NextAttemptAt: t0.Add(2 * time.Minute), LeasedUntil: first[0].LeasedUntil})).To(Succeed())
			_, err = env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0.Add(2*time.Minute))
			Expect(err).NotTo(HaveOccurred())
			before := env.stored(env.tenantA, keyOf(c))

			bumped := c
			bumped.CandidateEdgeID, bumped.SimilarityScore = "edge-001-v2", 0.9
			bumped.Context.LowName = "a newer snapshot"
			Expect(env.store.Enqueue(env.ctxA, env.tenantA, []artifacts.PairCandidate{bumped}, t0.Add(time.Hour))).To(Succeed())

			after := env.stored(env.tenantA, keyOf(c))
			Expect(after.candidateEdgeID).To(Equal("edge-001-v2"))
			Expect(after.similarityScore).To(Equal(0.9))
			after.candidateEdgeID, after.similarityScore = before.candidateEdgeID, before.similarityScore
			Expect(after).To(Equal(before), "context, created_at, attempts, next_attempt_at and lease are kept")
			Expect(before.attempts).To(Equal(1))
			Expect(before.nextAttemptAt).To(BeTemporally("==", t0.Add(2*time.Minute)))
			Expect(before.leasedUntil.Time).To(BeTemporally("==", t0.Add(3*time.Minute)))
		})

		It("leaves a decided or human-owned pair's candidate alone when it is re-enqueued", func() {
			decided, human := verdictCandidate(1), verdictCandidate(2)
			enqueue(decided, human)
			out, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(decided), artifacts.Verdict{
				Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.Outcome).To(Equal(artifacts.DecideApplied))
			Expect(env.su.Exec(context.Background(),
				`UPDATE artifact_pair_verdicts SET determination_type = 'human' WHERE tenant_id = $1 AND pair_key = $2`,
				env.tenantA, keyOf(human))).To(Succeed())
			before := map[string]storedVerdict{
				keyOf(decided): env.stored(env.tenantA, keyOf(decided)),
				keyOf(human):   env.stored(env.tenantA, keyOf(human)),
			}

			bumped := []artifacts.PairCandidate{decided, human}
			for i := range bumped {
				bumped[i].CandidateEdgeID, bumped[i].SimilarityScore = bumped[i].CandidateEdgeID+"-v2", 0.9
			}
			Expect(env.store.Enqueue(env.ctxA, env.tenantA, bumped, t0.Add(time.Hour))).To(Succeed())

			for key, want := range before {
				Expect(env.stored(env.tenantA, key)).To(Equal(want), key)
			}
			Expect(before[keyOf(decided)].candidateEdgeID).To(Equal("edge-001"))
			Expect(before[keyOf(human)].candidateEdgeID).To(Equal("edge-002"))
		})

		It("holds a leased pair until the lease expires", func() {
			enqueue(verdictCandidate(1))
			first, err := env.store.Lease(env.ctxA, env.tenantA, 10, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(first).To(HaveLen(1))

			during, err := env.store.Lease(env.ctxA, env.tenantA, 10, time.Minute, t0.Add(30*time.Second))
			Expect(err).NotTo(HaveOccurred())
			Expect(during).To(BeEmpty())

			after, err := env.store.Lease(env.ctxA, env.tenantA, 10, time.Minute, t0.Add(2*time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(HaveLen(1))
		})

		It("does not lease a pair before it is due", func() {
			enqueue(verdictCandidate(1))
			pairs, err := env.store.Lease(env.ctxA, env.tenantA, 10, time.Minute, t0.Add(-time.Second))
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(BeEmpty())
		})

		It("never leases a human-owned pair, even one still pending", func() {
			c := verdictCandidate(1)
			enqueue(c)
			Expect(env.su.Exec(context.Background(),
				`UPDATE artifact_pair_verdicts SET determination_type = 'human' WHERE tenant_id = $1 AND pair_key = $2`,
				env.tenantA, keyOf(c))).To(Succeed())
			pairs, err := env.store.Lease(env.ctxA, env.tenantA, 10, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(BeEmpty())
		})

		It("gives concurrent leases disjoint pairs", func() {
			var cs []artifacts.PairCandidate
			for i := range 40 {
				cs = append(cs, verdictCandidate(i))
			}
			enqueue(cs...)

			var (
				mu   sync.Mutex
				seen = map[string]int{}
				wg   sync.WaitGroup
				errs []error
			)
			for range 8 {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					pairs, err := env.store.Lease(env.ctxA, env.tenantA, 5, time.Minute, t0)
					mu.Lock()
					defer mu.Unlock()
					errs = append(errs, err)
					for _, p := range pairs {
						seen[p.Key]++
					}
				}()
			}
			wg.Wait()
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(seen).To(HaveLen(40))
			for key, n := range seen {
				Expect(n).To(Equal(1), "pair %s leased %d times", key, n)
			}
		})

		It("refuses a non-positive batch size or lease duration", func() {
			enqueue(verdictCandidate(1))
			_, err := env.store.Lease(env.ctxA, env.tenantA, 0, time.Minute, t0)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
			_, err = env.store.Lease(env.ctxA, env.tenantA, 10, 0, t0)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
			_, err = env.store.Lease(env.ctxA, env.tenantA, 10, -time.Minute, t0)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
			Expect(env.stored(env.tenantA, keyOf(verdictCandidate(1))).leasedUntil.Valid).To(BeFalse())
		})

		It("rejects a pair whose group IDs are out of byte order", func() {
			c := verdictCandidate(1)
			c.LowGroupID, c.HighGroupID = c.HighGroupID, c.LowGroupID
			err := env.store.Enqueue(env.ctxA, env.tenantA, []artifacts.PairCandidate{c}, t0)
			Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "expected a check violation, got %v", err)
			Expect(env.count(env.tenantA)).To(BeZero())
		})

		It("rolls back every chunk when a later chunk holds an out-of-order pair", func() {
			cs := make([]artifacts.PairCandidate, artifacts.ExportEnqueueChunk+1)
			for i := range cs {
				cs[i] = verdictCandidate(i)
			}
			last := &cs[len(cs)-1]
			last.LowGroupID, last.HighGroupID = last.HighGroupID, last.LowGroupID
			err := env.store.Enqueue(env.ctxA, env.tenantA, cs, t0)
			Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "expected a check violation, got %v", err)
			Expect(env.count(env.tenantA)).To(BeZero(), "chunk 1 was inserted before chunk 2 failed and must be rolled back")
		})

		It("enqueues more pairs than one INSERT chunk carries", func() {
			n := artifacts.ExportEnqueueChunk + 1
			cs := make([]artifacts.PairCandidate, n)
			keys := make([]string, n)
			for i := range cs {
				cs[i] = verdictCandidate(i)
				keys[i] = keyOf(cs[i])
			}
			enqueue(cs...)
			var present int
			Expect(env.su.QueryRow(context.Background(), `SELECT count(*) FROM artifact_pair_verdicts
				WHERE tenant_id = $1 AND pair_key = ANY($2::text[])`, env.tenantA, keys).Scan(&present)).To(Succeed())
			Expect(env.count(env.tenantA)).To(Equal(n))
			Expect(present).To(Equal(n))
		})

		It("rejects context, evidence or dissent that is not a JSON object", func() {
			for _, col := range []struct{ context, evidence, dissent string }{
				{context: `[]`},
				{context: `{}`, evidence: `"x"`},
				{context: `{}`, dissent: `1`},
			} {
				var evidence, dissent any
				if col.evidence != "" {
					evidence = col.evidence
				}
				if col.dissent != "" {
					dissent = col.dissent
				}
				err := env.su.Exec(context.Background(), `INSERT INTO artifact_pair_verdicts
					(tenant_id, pair_key, low_group_id, high_group_id, candidate_edge_id, similarity_score, context,
					 evidence, dissent, determined_by, next_attempt_at, created_at)
					VALUES ($1, 'k', 'a', 'b', 'e', 0.5, $2::jsonb, $3::jsonb, $4::jsonb, 'x', now(), now())`,
					env.tenantA, col.context, evidence, dissent)
				Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "expected a check violation for %+v, got %v", col, err)
			}
		})

		It("orders group IDs by bytes, not by the database collation", func() {
			c := verdictCandidate(1)
			c.LowGroupID, c.HighGroupID = "G-1", "g-1" // 'G' (0x47) < 'g' (0x67)
			enqueue(c)
		})
	})

	Describe("Decide and RecordDissent", func() {
		It("records a panel verdict once and reports a second decision as stale", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())

			v := artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 0.75, Evidence: ev, DecidedAt: t0}
			out, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), v)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideApplied, CandidateEdgeID: c.CandidateEdgeID}))
			row := env.stored(env.tenantA, keyOf(c))
			Expect(row.status).To(Equal("confirmed"))
			Expect(row.determination).To(Equal("llm_panel"))
			Expect(row.determinedBy).To(Equal("artifact-adjudicator"))
			Expect(row.confidence.Float64).To(Equal(0.75))
			Expect(row.leasedUntil.Valid).To(BeFalse())
			Expect(row.graphApplied).To(BeFalse())

			out, err = env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictRejected, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideStale}))
			Expect(env.stored(env.tenantA, keyOf(c)).status).To(Equal("confirmed"))
		})

		It("returns the candidate edge a re-enqueue moved the pair to while its lease was held", func() {
			c := verdictCandidate(1)
			enqueue(c)
			leased, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(leased).To(HaveLen(1))
			bumped := c
			bumped.CandidateEdgeID = "edge-001-v2"
			Expect(env.store.Enqueue(env.ctxA, env.tenantA, []artifacts.PairCandidate{bumped}, t0)).To(Succeed())

			out, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(leased[0].CandidateEdgeID).To(Equal(c.CandidateEdgeID))
			Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideApplied, CandidateEdgeID: "edge-001-v2"}))
			Expect(env.stored(env.tenantA, keyOf(c)).candidateEdgeID).To(Equal("edge-001-v2"))
		})

		It("leaves a human verdict alone and stores dissent only on human rows", func() {
			human, auto := verdictCandidate(1), verdictCandidate(2)
			enqueue(human, auto)
			Expect(env.su.Exec(context.Background(),
				`UPDATE artifact_pair_verdicts SET status = 'rejected', determination_type = 'human', determined_by = 'alice'
				 WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(human))).To(Succeed())

			v := artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0}
			out, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(human), v)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(artifacts.Decision{Outcome: artifacts.DecideHumanOwned}))
			Expect(env.stored(env.tenantA, keyOf(human)).status).To(Equal("rejected"))

			Expect(env.store.RecordDissent(env.ctxA, env.tenantA, keyOf(human), v)).To(Succeed())
			Expect(env.store.RecordDissent(env.ctxA, env.tenantA, keyOf(auto), v)).To(Succeed())
			var dissent map[string]any
			Expect(json.Unmarshal(env.stored(env.tenantA, keyOf(human)).dissent, &dissent)).To(Succeed())
			Expect(dissent).To(HaveKeyWithValue("status", "confirmed"))
			Expect(dissent).To(HaveKeyWithValue("determined_by", "artifact-adjudicator"))
			Expect(env.stored(env.tenantA, keyOf(auto)).dissent).To(BeNil())
		})

		It("refuses a status that is not a panel verdict", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictPending, DecidedAt: t0})
			Expect(err).To(MatchError(ContainSubstring("not a panel verdict")))
		})

		It("reports a pair that was never queued", func() {
			_, err := env.store.Decide(env.ctxA, env.tenantA, "nope", artifacts.Verdict{Status: artifacts.VerdictRejected, Evidence: ev, DecidedAt: t0})
			Expect(err).To(MatchError(ContainSubstring("not queued")))
		})
	})

	Describe("RecordFailure", func() {
		It("backs a failed pair off and abandons it when told", func() {
			c := verdictCandidate(1)
			enqueue(c)
			leased, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(leased).To(HaveLen(1))
			Expect(env.store.RecordFailure(env.ctxA, env.tenantA, keyOf(c), artifacts.Failure{
				Err: "boom", At: t0, NextAttemptAt: t0.Add(2 * time.Minute), LeasedUntil: leased[0].LeasedUntil})).To(Succeed())
			row := env.stored(env.tenantA, keyOf(c))
			Expect(row.attempts).To(Equal(1))
			Expect(row.lastError.String).To(Equal("boom"))
			Expect(row.nextAttemptAt).To(BeTemporally("==", t0.Add(2*time.Minute)))
			Expect(row.status).To(Equal("pending"))
			Expect(row.leasedUntil.Valid).To(BeFalse())

			early, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(early).To(BeEmpty())
			due, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0.Add(2*time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(due).To(HaveLen(1))
			Expect(due[0].Attempts).To(Equal(1))

			Expect(env.store.RecordFailure(env.ctxA, env.tenantA, keyOf(c), artifacts.Failure{
				Err: "boom again", At: t0.Add(3 * time.Minute), NextAttemptAt: t0.Add(time.Hour), Abandon: true,
				LeasedUntil: due[0].LeasedUntil})).To(Succeed())
			row = env.stored(env.tenantA, keyOf(c))
			Expect(row.status).To(Equal("abandoned"))
			Expect(row.attempts).To(Equal(2))
			Expect(row.decidedAt.Time).To(BeTemporally("==", t0.Add(3*time.Minute)))
			Expect(row.leasedUntil.Valid).To(BeFalse())
		})

		It("leaves a human-owned pair untouched", func() {
			c := verdictCandidate(1)
			enqueue(c)
			leased, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(leased).To(HaveLen(1))
			Expect(env.su.Exec(context.Background(),
				`UPDATE artifact_pair_verdicts SET determination_type = 'human' WHERE tenant_id = $1 AND pair_key = $2`,
				env.tenantA, keyOf(c))).To(Succeed())
			Expect(env.store.RecordFailure(env.ctxA, env.tenantA, keyOf(c), artifacts.Failure{
				Err: "boom", At: t0, NextAttemptAt: t0.Add(time.Minute), Abandon: true,
				LeasedUntil: leased[0].LeasedUntil})).To(Succeed())
			row := env.stored(env.tenantA, keyOf(c))
			Expect(row.attempts).To(Equal(0))
			Expect(row.status).To(Equal("pending"))
		})

		It("ignores a failure recorded under an expired lease another caller has replaced", func() {
			c := verdictCandidate(1)
			enqueue(c)
			first, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(first).To(HaveLen(1))
			second, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0.Add(2*time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(HaveLen(1))
			Expect(second[0].LeasedUntil).To(BeTemporally("==", t0.Add(3*time.Minute)))

			Expect(env.store.RecordFailure(env.ctxA, env.tenantA, keyOf(c), artifacts.Failure{
				Err: "late", At: t0.Add(2 * time.Minute), NextAttemptAt: t0.Add(2 * time.Minute),
				LeasedUntil: first[0].LeasedUntil})).To(Succeed())
			row := env.stored(env.tenantA, keyOf(c))
			Expect(row.attempts).To(Equal(0))
			Expect(row.lastError.Valid).To(BeFalse())
			Expect(row.leasedUntil.Time).To(BeTemporally("==", second[0].LeasedUntil))
			concurrent, err := env.store.Lease(env.ctxA, env.tenantA, 1, time.Minute, t0.Add(2*time.Minute+time.Second))
			Expect(err).NotTo(HaveOccurred())
			Expect(concurrent).To(BeEmpty())

			Expect(env.store.RecordFailure(env.ctxA, env.tenantA, keyOf(c), artifacts.Failure{
				Err: "current", At: t0.Add(2 * time.Minute), NextAttemptAt: t0.Add(4 * time.Minute),
				LeasedUntil: second[0].LeasedUntil})).To(Succeed())
			row = env.stored(env.tenantA, keyOf(c))
			Expect(row.attempts).To(Equal(1))
			Expect(row.lastError.String).To(Equal("current"))
			Expect(row.leasedUntil.Valid).To(BeFalse())
		})
	})

	Describe("projection bookkeeping", func() {
		It("lists decided verdicts until they are marked projected, and counts the queue", func() {
			confirmed, undecided, pending, human := verdictCandidate(1), verdictCandidate(2), verdictCandidate(3), verdictCandidate(4)
			enqueue(confirmed, undecided, pending, human)
			_, err := decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(human)),
				artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			_, err = env.store.Decide(env.ctxA, env.tenantA, keyOf(confirmed),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			_, err = env.store.Decide(env.ctxA, env.tenantA, keyOf(undecided),
				artifacts.Verdict{Status: artifacts.VerdictUndecided, Confidence: 0.5, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())

			counts, err := env.store.Counts(env.ctxA, env.tenantA, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(counts).To(Equal(artifacts.QueueCounts{Pending: 1, Due: 1, Unprojected: 2}))

			proj, err := env.store.Unprojected(env.ctxA, env.tenantA, 10, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(proj).To(HaveLen(2))
			Expect(proj[0].Key).To(Equal(keyOf(confirmed)), "oldest decision first")
			Expect(proj[0].LowGroupID).To(Equal(confirmed.LowGroupID))
			Expect(proj[0].HighGroupID).To(Equal(confirmed.HighGroupID))
			Expect(proj[0].CandidateEdgeID).To(Equal(confirmed.CandidateEdgeID))
			Expect(proj[0].Status).To(Equal(artifacts.VerdictConfirmed))
			Expect(proj[0].DeterminationType).To(Equal(artifacts.DeterminationLLMPanel))
			Expect(proj[0].DeterminedBy).To(Equal("artifact-adjudicator"))
			Expect(proj[0].Evidence).To(Equal(ev))
			Expect(proj[0].DecidedAt).To(BeTemporally("==", t0))
			Expect(proj[1]).To(Equal(artifacts.Projection{Key: keyOf(human), LowGroupID: human.LowGroupID,
				HighGroupID: human.HighGroupID, CandidateEdgeID: human.CandidateEdgeID, Status: artifacts.VerdictConfirmed,
				Confidence: 1, DecidedAt: proj[1].DecidedAt, DeterminationType: artifacts.DeterminationHuman, DeterminedBy: "alice"}))
			Expect(proj[1].DecidedAt).To(BeTemporally("==", t0.Add(time.Hour)))

			closed, err := env.store.ClosedPairs(env.ctxA, env.tenantA)
			Expect(err).NotTo(HaveOccurred())
			Expect(closed).To(Equal(map[string]bool{keyOf(confirmed): true, keyOf(human): true}))

			for _, c := range []artifacts.PairCandidate{pending, undecided} {
				ok := project(artifacts.Projection{Key: keyOf(c), DeterminationType: artifacts.DeterminationLLMPanel,
					Status: artifacts.VerdictConfirmed, DecidedAt: t0})
				Expect(ok).To(BeFalse())
				Expect(env.stored(env.tenantA, keyOf(c)).graphApplied).To(BeFalse())
			}

			ok := project(artifacts.Projection{Key: keyOf(confirmed), DeterminationType: artifacts.DeterminationLLMPanel,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0})
			Expect(ok).To(BeTrue())
			ok = project(artifacts.Projection{Key: keyOf(human), DeterminationType: artifacts.DeterminationHuman,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0.Add(time.Hour)})
			Expect(ok).To(BeTrue())
			proj, err = env.store.Unprojected(env.ctxA, env.tenantA, 10, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(proj).To(BeEmpty())
		})

		It("grants a projection claim to exactly one of several concurrent callers", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			p := artifacts.Projection{Key: keyOf(c), DeterminationType: artifacts.DeterminationLLMPanel,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0}
			var (
				mu       sync.Mutex
				outcomes = map[artifacts.ClaimOutcome]int{}
				errs     []error
				wg       sync.WaitGroup
			)
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					out, err := env.store.ClaimProjection(env.ctxA, env.tenantA, p, t0.Add(time.Minute+time.Duration(i)*time.Second), t0)
					mu.Lock()
					defer mu.Unlock()
					errs = append(errs, err)
					outcomes[out]++
				}()
			}
			wg.Wait()
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(outcomes).To(Equal(map[artifacts.ClaimOutcome]int{artifacts.ClaimAcquired: 1, artifacts.ClaimBusy: 7}))
		})

		It("stores a claim at microsecond precision, so MarkProjected matches the time ClaimProjection was given", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			p := artifacts.Projection{Key: keyOf(c), DeterminationType: artifacts.DeterminationLLMPanel,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0}
			until := t0.Add(time.Minute + 123456789*time.Nanosecond)
			out, err := env.store.ClaimProjection(env.ctxA, env.tenantA, p, until, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(artifacts.ClaimAcquired))
			Expect(env.stored(env.tenantA, keyOf(c)).claimedUntil.Time).To(BeTemporally("==", until.Truncate(time.Microsecond)))
			ok, err := env.store.MarkProjected(env.ctxA, env.tenantA, p, until)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(env.stored(env.tenantA, keyOf(c)).claimedUntil.Valid).To(BeFalse())
		})

		It("refuses a claim in another tenant's context and changes nothing", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			p := artifacts.Projection{Key: keyOf(c), DeterminationType: artifacts.DeterminationLLMPanel,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0}
			_, err = env.store.ClaimProjection(env.ctxB, env.tenantB, p, t0.Add(time.Minute), t0)
			Expect(err).To(MatchError(ContainSubstring("pair is not queued")), "tenant B cannot see tenant A's pair")
			_, err = env.store.ClaimProjection(env.ctxB, env.tenantA, p, t0.Add(time.Minute), t0)
			Expect(err).To(MatchError(ContainSubstring("does not match")))
			Expect(env.stored(env.tenantA, keyOf(c)).claimedUntil.Valid).To(BeFalse())
		})

		It("reports a pair that is not queued as not projected", func() {
			ok, err := env.store.MarkProjected(env.ctxA, env.tenantA, artifacts.Projection{Key: "nope", DeterminationType: artifacts.DeterminationLLMPanel,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0}, t0.Add(time.Minute))
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("refuses a non-positive batch size", func() {
			_, err := env.store.Unprojected(env.ctxA, env.tenantA, 0, t0)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
			_, err = env.store.Unprojected(env.ctxA, env.tenantA, -1, t0)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
		})

		It("fails Unprojected on a decided row without confidence and leaves the row unprojected", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts SET confidence = NULL
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c))).To(Succeed())
			before := env.stored(env.tenantA, keyOf(c))
			_, err = env.store.Unprojected(env.ctxA, env.tenantA, 10, t0)
			Expect(err).To(MatchError(ContainSubstring("pair " + artifacts.PairRef(keyOf(c)) + " is decided but lacks confidence, evidence or decided_at")))
			Expect(err).NotTo(MatchError(ContainSubstring(keyOf(c))), "errors name pairs by ref, never by raw key")
			Expect(env.stored(env.tenantA, keyOf(c))).To(Equal(before))
		})
	})

	Describe("ResetProjection", func() {
		It("marks projected llm_panel and human verdicts unprojected, and nothing else", func() {
			confirmed, rejected, undecided, human := verdictCandidate(1), verdictCandidate(2), verdictCandidate(3), verdictCandidate(4)
			enqueue(confirmed, rejected, undecided, human)
			for _, d := range []struct {
				c artifacts.PairCandidate
				s artifacts.VerdictStatus
			}{{confirmed, artifacts.VerdictConfirmed}, {rejected, artifacts.VerdictRejected}, {undecided, artifacts.VerdictUndecided}} {
				_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(d.c), artifacts.Verdict{Status: d.s, Confidence: 1, Evidence: ev, DecidedAt: t0})
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(human)),
				artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			for _, p := range []struct {
				c             artifacts.PairCandidate
				determination string
				status        artifacts.VerdictStatus
			}{{confirmed, artifacts.DeterminationLLMPanel, artifacts.VerdictConfirmed}, {rejected, artifacts.DeterminationLLMPanel, artifacts.VerdictRejected},
				{human, artifacts.DeterminationHuman, artifacts.VerdictConfirmed}} {
				ok := project(artifacts.Projection{Key: keyOf(p.c), DeterminationType: p.determination,
					Status: p.status, DecidedAt: t0})
				Expect(ok).To(BeTrue())
			}
			Expect(env.su.Exec(context.Background(),
				`UPDATE artifact_pair_verdicts SET graph_applied = true WHERE tenant_id = $1 AND pair_key = $2`,
				env.tenantA, keyOf(undecided))).To(Succeed())

			n, err := env.store.ResetProjection(env.ctxB, env.tenantB)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero(), "tenant B sees none of tenant A's rows")
			Expect(env.stored(env.tenantA, keyOf(confirmed)).graphApplied).To(BeTrue())

			n, err = env.store.ResetProjection(env.ctxA, env.tenantA)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(3))
			Expect(env.stored(env.tenantA, keyOf(confirmed)).graphApplied).To(BeFalse())
			Expect(env.stored(env.tenantA, keyOf(rejected)).graphApplied).To(BeFalse())
			Expect(env.stored(env.tenantA, keyOf(human)).graphApplied).To(BeFalse())
			Expect(env.stored(env.tenantA, keyOf(undecided)).graphApplied).To(BeTrue())
		})

		It("refuses a tenant argument that differs from the context and changes nothing", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			ok := project(artifacts.Projection{Key: keyOf(c), DeterminationType: artifacts.DeterminationLLMPanel,
				Status: artifacts.VerdictConfirmed, DecidedAt: t0})
			Expect(ok).To(BeTrue())
			_, err = env.store.ResetProjection(env.ctxB, env.tenantA)
			Expect(err).To(MatchError(ContainSubstring("does not match")))
			Expect(env.stored(env.tenantA, keyOf(c)).graphApplied).To(BeTrue())
		})
	})

	Describe("vote split", func() {
		It("marks a pair whose stored evidence cannot be decoded as unreadable, and still lists the queue", func() {
			bad, good := verdictCandidate(1), verdictCandidate(2)
			enqueue(bad, good)
			for _, c := range []artifacts.PairCandidate{bad, good} {
				_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictUndecided,
					Confidence: 0.5, Evidence: artifacts.PanelEvidence{ValidVotes: 1, TotalVotes: 1}, DecidedAt: t0})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts SET evidence = '{"votes": "x"}'
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(bad))).To(Succeed())
			items, err := env.store.ReviewQueue(env.ctxA, env.tenantA, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(2))
			byKey := map[string]artifacts.ReviewItem{items[0].Key: items[0], items[1].Key: items[1]}
			Expect(byKey[keyOf(bad)].VotesUnreadable).To(BeTrue())
			Expect(byKey[keyOf(bad)].Votes).To(BeNil())
			Expect(byKey[keyOf(good)].Votes).To(Equal(&artifacts.VoteSplit{Valid: 1, Total: 1}))

			found, err := env.store.SearchPairs(env.ctxA, env.tenantA, bad.Context.LowName, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(HaveLen(2))
			for _, it := range found {
				if it.Key == keyOf(bad) {
					Expect(it.VotesUnreadable).To(BeTrue())
					Expect(it.Votes).To(BeNil())
				}
			}
		})

		It("marks an llm_panel pair whose stored evidence is missing as unreadable", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictUndecided,
				Confidence: 0.5, Evidence: artifacts.PanelEvidence{ValidVotes: 1, TotalVotes: 1}, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts SET evidence = NULL
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c))).To(Succeed())
			items, err := env.store.ReviewQueue(env.ctxA, env.tenantA, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].VotesUnreadable).To(BeTrue())
			Expect(items[0].Votes).To(BeNil())
		})
	})

	Describe("PairDetail", func() {
		panel := artifacts.PanelEvidence{PromptName: "p", PromptVersion: "1", ValidVotes: 1, TotalVotes: 1}

		It("reports undecodable current evidence in EvidenceErr and still returns the expect token", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictUndecided,
				Confidence: 0.5, Evidence: panel, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts SET evidence = '{"votes": "x"}'
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c))).To(Succeed())
			d, err := env.store.PairDetail(env.ctxA, env.tenantA, artifacts.PairRef(keyOf(c)), 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.EvidenceErr).To(MatchError(ContainSubstring("decode evidence")))
			Expect(d.Evidence).To(BeNil())
			Expect(d.VotesUnreadable).To(BeTrue())
			Expect(artifacts.ValidateExpect(d.Expect)).To(Succeed())
		})

		It("reports undecodable dissent evidence inside the dissent, keeping its status", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictConfirmed,
				Confidence: 1, Evidence: panel, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			_, err = decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)),
				artifacts.HumanVerdict{Status: artifacts.VerdictRejected, Reviewer: "alice", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts
				SET dissent = jsonb_set(dissent, '{evidence}', '{"votes": "x"}')
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c))).To(Succeed())
			d, err := env.store.PairDetail(env.ctxA, env.tenantA, artifacts.PairRef(keyOf(c)), 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Dissent.Status).To(Equal(artifacts.VerdictConfirmed))
			Expect(d.Dissent.Evidence).To(BeNil())
			Expect(d.Dissent.EvidenceErr).To(MatchError(ContainSubstring("decode dissent evidence")))
		})

		It("reports an undecodable dissent inside the dissent, keeping its status and the expect token", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c), artifacts.Verdict{Status: artifacts.VerdictRejected,
				Confidence: 1, Evidence: panel, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			_, err = decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)),
				artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: "alice", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts
				SET dissent = '{"status": "confirmed", "decided_at": "not-a-time"}'
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c))).To(Succeed())
			d, err := env.store.PairDetail(env.ctxA, env.tenantA, artifacts.PairRef(keyOf(c)), 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Dissent).NotTo(BeNil())
			Expect(d.Dissent.Status).To(Equal(artifacts.VerdictConfirmed))
			Expect(d.DissentStatus).To(Equal(artifacts.VerdictConfirmed))
			Expect(d.Dissent.Evidence).To(BeNil())
			Expect(d.Dissent.EvidenceErr).To(MatchError(ContainSubstring("decode dissent")))
			Expect(artifacts.ValidateExpect(d.Expect)).To(Succeed())
		})

		It("does not resolve another tenant's pair ref, nor list its pairs as related", func() {
			mine, theirs := verdictCandidate(1), verdictCandidate(2)
			theirs.LowGroupID = mine.LowGroupID
			enqueue(mine)
			Expect(env.store.Enqueue(env.ctxB, env.tenantB, []artifacts.PairCandidate{theirs}, t0)).To(Succeed())
			_, err := env.store.PairDetail(env.ctxA, env.tenantA, artifacts.PairRef(keyOf(theirs)), 10)
			Expect(err).To(MatchError(artifacts.ErrPairNotFound))
			d, err := env.store.PairDetail(env.ctxA, env.tenantA, artifacts.PairRef(keyOf(mine)), 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(d.RelatedTotal).To(BeZero())
		})

		It("changes nothing", func() {
			c := verdictCandidate(1)
			enqueue(c)
			before := env.stored(env.tenantA, keyOf(c))
			_, err := env.store.PairDetail(env.ctxA, env.tenantA, artifacts.PairRef(keyOf(c)), 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(env.stored(env.tenantA, keyOf(c))).To(Equal(before))
		})
	})

	Describe("DecideHuman", func() {
		human := func(by string) artifacts.HumanVerdict {
			return artifacts.HumanVerdict{Status: artifacts.VerdictConfirmed, Reviewer: by, DecidedAt: t0}
		}

		It("computes the same pair ref in SQL as PairRef", func() {
			c := verdictCandidate(1)
			c.LowGroupID, c.HighGroupID = "artifact-group_document_naïve \"quoted\" name", "artifact-group_document_zzz"
			enqueue(c)
			d, err := decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)), human("alice"))
			Expect(err).NotTo(HaveOccurred())
			Expect(d).To(Equal(artifacts.HumanDecision{Outcome: artifacts.HumanApplied, Key: keyOf(c)}))
		})

		It("does not resolve another tenant's pair ref (RLS)", func() {
			c := verdictCandidate(1)
			Expect(env.store.Enqueue(env.ctxB, env.tenantB, []artifacts.PairCandidate{c}, t0)).To(Succeed())
			before := env.stored(env.tenantB, keyOf(c))
			_, err := decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)), human("alice"))
			Expect(err).To(MatchError(artifacts.ErrPairNotFound))
			Expect(env.stored(env.tenantB, keyOf(c))).To(Equal(before))
			Expect(before.status).To(Equal("pending"))
		})

		It("refuses an invalid reviewer and leaves the row unchanged", func() {
			c := verdictCandidate(1)
			enqueue(c)
			before := env.stored(env.tenantA, keyOf(c))
			_, err := decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)), human("alice\u202e"))
			Expect(err).To(MatchError(ContainSubstring("control, format or line-separator")))
			Expect(env.stored(env.tenantA, keyOf(c))).To(Equal(before))
		})

		It("keeps panel evidence it cannot decode, so old evidence never blocks a human verdict", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts SET evidence = '{"votes": "not-a-list"}'
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c))).To(Succeed())

			d, err := decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)),
				artifacts.HumanVerdict{Status: artifacts.VerdictRejected, Reviewer: "bob", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())
			Expect(d.Outcome).To(Equal(artifacts.HumanApplied))
			var dissent map[string]any
			Expect(json.Unmarshal(env.stored(env.tenantA, keyOf(c)).dissent, &dissent)).To(Succeed())
			Expect(dissent).To(HaveKeyWithValue("status", "confirmed"))
			Expect(dissent).To(HaveKeyWithValue("evidence", map[string]any{"votes": "not-a-list"}))
		})

		It("stores the overridden panel verdict as dissent JSON in the RecordDissent shape", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 0.75, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			_, err = decideCurrent(env.ctxA, env.store, env.tenantA, artifacts.PairRef(keyOf(c)),
				artifacts.HumanVerdict{Status: artifacts.VerdictRejected, Reviewer: "bob", DecidedAt: t0.Add(time.Hour)})
			Expect(err).NotTo(HaveOccurred())

			row := env.stored(env.tenantA, keyOf(c))
			Expect(row.status).To(Equal("rejected"))
			Expect(row.determination).To(Equal("human"))
			Expect(row.determinedBy).To(Equal("bob"))
			Expect(row.confidence.Float64).To(Equal(1.0))
			Expect(row.decidedAt.Time).To(BeTemporally("==", t0.Add(time.Hour)))
			var dissent map[string]any
			Expect(json.Unmarshal(row.dissent, &dissent)).To(Succeed())
			Expect(dissent).To(HaveKeyWithValue("status", "confirmed"))
			Expect(dissent).To(HaveKeyWithValue("confidence", 0.75))
			Expect(dissent).To(HaveKeyWithValue("determined_by", "artifact-adjudicator"))
			Expect(dissent).To(HaveKey("decided_at"))
			decidedAt, err := time.Parse(time.RFC3339Nano, dissent["decided_at"].(string))
			Expect(err).NotTo(HaveOccurred())
			Expect(decidedAt).To(BeTemporally("==", t0))
			Expect(dissent).To(HaveKeyWithValue("evidence", HaveKeyWithValue("prompt_name", "artifact_same_as")))
			var evidenceIsNull bool
			Expect(env.su.QueryRow(context.Background(), `SELECT evidence IS NULL FROM artifact_pair_verdicts
				WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(c)).Scan(&evidenceIsNull)).To(Succeed())
			Expect(evidenceIsNull).To(BeTrue())
		})
	})

	Describe("tenant isolation", func() {
		It("confines every operation to the context's tenant", func() {
			c := verdictCandidate(1)
			enqueue(c)
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictRejected, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			enqueue(verdictCandidate(2))

			closed, err := env.store.ClosedPairs(env.ctxB, env.tenantB)
			Expect(err).NotTo(HaveOccurred())
			Expect(closed).To(BeEmpty())
			leased, err := env.store.Lease(env.ctxB, env.tenantB, 10, time.Minute, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(leased).To(BeEmpty())
			counts, err := env.store.Counts(env.ctxB, env.tenantB, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(counts).To(Equal(artifacts.QueueCounts{}))
			proj, err := env.store.Unprojected(env.ctxB, env.tenantB, 10, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(proj).To(BeEmpty())
		})

		It("finds only the context tenant's pairs by name (RLS)", func() {
			a, b := verdictCandidate(1), verdictCandidate(2)
			enqueue(a)
			Expect(env.store.Enqueue(env.ctxB, env.tenantB, []artifacts.PairCandidate{b}, t0)).To(Succeed())
			for _, tc := range []struct {
				ctx      context.Context
				tenantID string
				wantKey  string
			}{{env.ctxA, env.tenantA, keyOf(a)}, {env.ctxB, env.tenantB, keyOf(b)}} {
				items, err := env.store.SearchPairs(tc.ctx, tc.tenantID, "ACCESS", 10)
				Expect(err).NotTo(HaveOccurred())
				Expect(items).To(ConsistOf(HaveField("Key", tc.wantKey)))
			}
		})

		It("refuses a tenant argument that differs from the context", func() {
			_, err := env.store.ClosedPairs(env.ctxB, env.tenantA)
			Expect(err).To(MatchError(ContainSubstring("does not match")))
		})

		It("leaves a pair pending when Decide names a tenant other than the context's", func() {
			c := verdictCandidate(1)
			enqueue(c)
			before := env.stored(env.tenantA, keyOf(c))
			_, err := env.store.Decide(env.ctxB, env.tenantA, keyOf(c),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).To(MatchError(ContainSubstring("does not match")))
			Expect(env.stored(env.tenantA, keyOf(c))).To(Equal(before))
			Expect(before.status).To(Equal("pending"))
		})

		It("refuses Enqueue from a context without a tenant and writes nothing", func() {
			err := env.store.Enqueue(context.Background(), env.tenantA, []artifacts.PairCandidate{verdictCandidate(1)}, t0)
			Expect(err).To(MatchError(tenant.ErrNoTenant))
			Expect(env.count(env.tenantA)).To(BeZero())
		})

		It("blocks cross-tenant reads and writes at the database", func() {
			enqueue(verdictCandidate(1))
			ctx := context.Background()
			tx, err := env.app.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
			_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", env.tenantB)
			Expect(err).NotTo(HaveOccurred())

			var visible int
			Expect(tx.QueryRowContext(ctx, "SELECT count(*) FROM artifact_pair_verdicts WHERE tenant_id = $1", env.tenantA).Scan(&visible)).To(Succeed())
			Expect(visible).To(BeZero())

			res, err := tx.ExecContext(ctx, "UPDATE artifact_pair_verdicts SET status = 'rejected' WHERE tenant_id = $1", env.tenantA)
			Expect(err).NotTo(HaveOccurred())
			n, err := res.RowsAffected()
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(BeZero())

			_, err = tx.ExecContext(ctx, `INSERT INTO artifact_pair_verdicts
				(tenant_id, pair_key, low_group_id, high_group_id, candidate_edge_id, similarity_score, context,
				 determined_by, next_attempt_at, created_at)
				VALUES ($1, 'k', 'a', 'b', 'e', 0.5, '{}', 'x', now(), now())`, env.tenantA)
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "expected an RLS violation, got %v", err)
		})

		It("denies app_user DELETE even within its own tenant", func() {
			enqueue(verdictCandidate(1))
			ctx := context.Background()
			tx, err := env.app.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
			_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", env.tenantA)
			Expect(err).NotTo(HaveOccurred())

			_, err = tx.ExecContext(ctx, "DELETE FROM artifact_pair_verdicts")
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "expected a privilege violation, got %v", err)
		})
	})
})

var _ = Describe("PGVerdictStore contract", func() {
	newEnv := func() verdictStoreContractEnv {
		env := setupVerdictStore()
		return verdictStoreContractEnv{store: env.store, ctx: env.ctxA, tenantID: env.tenantA,
			setHuman: func(key string, status artifacts.VerdictStatus) {
				Expect(env.su.Exec(context.Background(), `UPDATE artifact_pair_verdicts
					SET status = $3, determination_type = 'human', determined_by = 'test-reviewer', graph_applied = false
					WHERE tenant_id = $1 AND pair_key = $2`,
					env.tenantA, key, string(status))).To(Succeed())
			},
			row: func(key string) contractRow {
				v := env.stored(env.tenantA, key)
				row := contractRow{status: artifacts.VerdictStatus(v.status), determination: v.determination,
					determinedBy: v.determinedBy, graphApplied: v.graphApplied, attempts: v.attempts,
					leased: v.leasedUntil.Valid, hasDissent: v.dissent != nil}
				if v.claimedUntil.Valid {
					row.claimedUntil = v.claimedUntil.Time
				}
				if v.dissent != nil {
					var d struct {
						Status artifacts.VerdictStatus `json:"status"`
					}
					Expect(json.Unmarshal(v.dissent, &d)).To(Succeed())
					row.dissentStatus = d.Status
				}
				return row
			}}
	}
	verdictStoreContract(newEnv)
	reviewDetailContract(newEnv)
})
