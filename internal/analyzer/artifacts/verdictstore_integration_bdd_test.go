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
}

// stored reads a row as the superuser, bypassing RLS.
func (e verdictStoreEnv) stored(tenantID, key string) storedVerdict {
	var v storedVerdict
	Expect(e.su.QueryRow(context.Background(), `
		SELECT status, determination_type, determined_by, candidate_edge_id, similarity_score, context, created_at,
		       confidence, attempts, next_attempt_at, leased_until, decided_at, last_error, graph_applied, dissent
		FROM artifact_pair_verdicts WHERE tenant_id = $1 AND pair_key = $2`, tenantID, key).Scan(
		&v.status, &v.determination, &v.determinedBy, &v.candidateEdgeID, &v.similarityScore, &v.context, &v.createdAt,
		&v.confidence, &v.attempts, &v.nextAttemptAt, &v.leasedUntil, &v.decidedAt, &v.lastError, &v.graphApplied, &v.dissent)).To(Succeed())
	return v
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
			Expect(env.store.Enqueue(env.ctxA, env.tenantA, []artifacts.PairCandidate{c}, t0)).NotTo(Succeed())
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
			Expect(env.su.Exec(context.Background(),
				`UPDATE artifact_pair_verdicts SET status = 'confirmed', determination_type = 'human', graph_applied = false
				 WHERE tenant_id = $1 AND pair_key = $2`, env.tenantA, keyOf(human))).To(Succeed())
			_, err := env.store.Decide(env.ctxA, env.tenantA, keyOf(confirmed),
				artifacts.Verdict{Status: artifacts.VerdictConfirmed, Confidence: 1, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())
			_, err = env.store.Decide(env.ctxA, env.tenantA, keyOf(undecided),
				artifacts.Verdict{Status: artifacts.VerdictUndecided, Confidence: 0.5, Evidence: ev, DecidedAt: t0})
			Expect(err).NotTo(HaveOccurred())

			counts, err := env.store.Counts(env.ctxA, env.tenantA, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(counts).To(Equal(artifacts.QueueCounts{Pending: 1, Due: 1, Unprojected: 1}))

			proj, err := env.store.Unprojected(env.ctxA, env.tenantA, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(proj).To(HaveLen(1))
			Expect(proj[0].Key).To(Equal(keyOf(confirmed)))
			Expect(proj[0].LowGroupID).To(Equal(confirmed.LowGroupID))
			Expect(proj[0].HighGroupID).To(Equal(confirmed.HighGroupID))
			Expect(proj[0].CandidateEdgeID).To(Equal(confirmed.CandidateEdgeID))
			Expect(proj[0].Status).To(Equal(artifacts.VerdictConfirmed))
			Expect(proj[0].Evidence).To(Equal(ev))
			Expect(proj[0].DecidedAt).To(BeTemporally("==", t0))
			Expect(proj).NotTo(ContainElement(HaveField("Key", keyOf(human))))

			closed, err := env.store.ClosedPairs(env.ctxA, env.tenantA)
			Expect(err).NotTo(HaveOccurred())
			Expect(closed).To(Equal(map[string]bool{keyOf(confirmed): true, keyOf(human): true}))

			Expect(env.store.MarkProjected(env.ctxA, env.tenantA, keyOf(human))).To(Succeed())
			Expect(env.stored(env.tenantA, keyOf(human)).graphApplied).To(BeFalse())
			Expect(env.store.MarkProjected(env.ctxA, env.tenantA, keyOf(pending))).To(Succeed())
			Expect(env.stored(env.tenantA, keyOf(pending)).graphApplied).To(BeFalse())
			Expect(env.store.MarkProjected(env.ctxA, env.tenantA, keyOf(undecided))).To(Succeed())
			Expect(env.stored(env.tenantA, keyOf(undecided)).graphApplied).To(BeFalse())

			Expect(env.store.MarkProjected(env.ctxA, env.tenantA, keyOf(confirmed))).To(Succeed())
			proj, err = env.store.Unprojected(env.ctxA, env.tenantA, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(proj).To(BeEmpty())
		})

		It("refuses a non-positive batch size", func() {
			_, err := env.store.Unprojected(env.ctxA, env.tenantA, 0)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
			_, err = env.store.Unprojected(env.ctxA, env.tenantA, -1)
			Expect(err).To(MatchError(ContainSubstring("must be positive")))
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
			proj, err := env.store.Unprojected(env.ctxB, env.tenantB, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(proj).To(BeEmpty())
		})

		It("refuses a tenant argument that differs from the context", func() {
			_, err := env.store.ClosedPairs(env.ctxB, env.tenantA)
			Expect(err).To(MatchError(ContainSubstring("does not match")))
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
