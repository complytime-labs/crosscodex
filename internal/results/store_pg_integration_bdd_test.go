//go:build integration

package results_test

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/results"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// storeEnv is one isolated PostgreSQL fixture: two fresh tenants, a store on
// a restricted app_user pool (so RLS applies) and a superuser pool for seeding.
type storeEnv struct {
	store            *results.PGStore
	appPool          db.Pool
	su               db.Pool
	tenantA, tenantB string
	ctxA, ctxB       context.Context
}

func setupStore() storeEnv {
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
	Expect(err).NotTo(HaveOccurred(), "app_user password")
	Expect(dbtest.SetRolePassword(ctx, suDSN, "app_user", appPassword)).To(Succeed())

	su, err := db.NewPool(db.PoolConfig{DSN: suDSN, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { su.Close() })

	appDSN, err := dbtest.RoleDSN(suDSN, "app_user", appPassword)
	Expect(err).NotTo(HaveOccurred())
	appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 10})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { appPool.Close() })

	env := storeEnv{
		store: results.NewPGStore(db.NewTenantPool(appPool)), appPool: appPool, su: su,
		tenantA: "results-a-" + uuid.NewString(), tenantB: "results-b-" + uuid.NewString(),
	}
	for _, id := range []string{env.tenantA, env.tenantB} {
		Expect(db.EnsureTenant(ctx, su, id, "results store it")).To(Succeed())
	}
	DeferCleanup(func() {
		for _, id := range []string{env.tenantA, env.tenantB} {
			stmt, err := dbtest.HardDeleteTenantSQL(id)
			Expect(err).NotTo(HaveOccurred())
			Expect(su.Exec(ctx, stmt)).To(Succeed())
		}
	})
	env.ctxA, err = tenant.WithTenant(ctx, env.tenantA)
	Expect(err).NotTo(HaveOccurred())
	env.ctxB, err = tenant.WithTenant(ctx, env.tenantB)
	Expect(err).NotTo(HaveOccurred())
	return env
}

func (e storeEnv) seedJob(tenantID, status string) string {
	id := "job-" + uuid.NewString()
	Expect(e.su.Exec(context.Background(), `INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, $3, 'test')`,
		id, tenantID, status)).To(Succeed())
	return id
}

// seedVote inserts a vote_summaries row with the contribution the pipeline
// writes for consensus: "requires" rows come from the requires analyzer,
// every other consensus from the relationship analyzer.
func (e storeEnv) seedVote(tenantID, jobID, src, tgt, consensus string, confidence, viability float64) {
	contribution := "relationship"
	if consensus == "requires" {
		contribution = "requires"
	}
	Expect(e.su.Exec(context.Background(), `INSERT INTO vote_summaries (job_id, source_id, target_id, analyzer_name, consensus, confidence, viability, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, jobID, src, tgt, contribution, consensus, confidence, viability, tenantID)).To(Succeed())
}

func (e storeEnv) seedAnalysis(tenantID, jobID, data string) {
	Expect(e.su.Exec(context.Background(), `INSERT INTO analysis_results (tenant_id, job_id, analyzer_name, result_data)
		VALUES ($1, $2, 'relationship', $3::jsonb)`, tenantID, jobID, data)).To(Succeed())
}

func keys(p results.Page) []string {
	out := make([]string, 0, len(p.Mappings))
	for _, m := range p.Mappings {
		out = append(out, m.SourceID+">"+m.TargetID)
	}
	return out
}

var allTypes = []string{"EQUIVALENT", "SUPPORTS", "SUBSET_OF"}

var _ = Describe("PGStore", func() {
	var env storeEnv

	BeforeEach(func() { env = setupStore() })

	filter := func(tenantID, jobID string, stage results.Stage) results.Filter {
		return results.Filter{TenantID: tenantID, JobID: jobID, Stage: stage, RelationshipTypes: allTypes, PageSize: 100}
	}

	Describe("synthesized stage", func() {
		var job string

		BeforeEach(func() {
			job = env.seedJob(env.tenantA, "completed")
			env.seedVote(env.tenantA, job, "AC-2", "B-1", "EQUIVALENT", 0.9, 0.8)
			env.seedVote(env.tenantA, job, "AC-1", "B-1", "SUPPORTS", 0.5, 0.4)
			env.seedVote(env.tenantA, job, "AC-3", "B-3", "EQUIVALENT", 0.7, 0)
			env.seedVote(env.tenantA, job, "AC-4", "B-4", "requires", 0.9, 0.9)
		})

		It("returns viable rows only, ordered, with viability", func() {
			page, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, job, results.StageSynthesized))
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-1>B-1", "AC-2>B-1"}))
			Expect(page.Mappings[0]).To(Equal(results.Mapping{JobID: job, SourceID: "AC-1", TargetID: "B-1",
				RelationshipType: "SUPPORTS", Confidence: 0.5, Viability: 0.4, HasViability: true}))
			Expect(page.Next).To(BeNil())
		})

		It("adds non-viable rows on request but never requires rows", func() {
			f := filter(env.tenantA, job, results.StageSynthesized)
			f.IncludeNonViable = true
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-1>B-1", "AC-2>B-1", "AC-3>B-3"}))
		})

		It("returns one mapping, from the relationship row, for a pair that also has a requires row", func() {
			env.seedVote(env.tenantA, job, "AC-1", "B-1", "requires", 0.95, 0.95)
			f := filter(env.tenantA, job, results.StageSynthesized)
			// Naming requires as a type must not surface requires rows either:
			// mappings are the relationship contribution only.
			f.RelationshipTypes = append(append([]string(nil), allTypes...), "requires")
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-1>B-1", "AC-2>B-1"}))
			Expect(page.Mappings[0]).To(Equal(results.Mapping{JobID: job, SourceID: "AC-1", TargetID: "B-1",
				RelationshipType: "SUPPORTS", Confidence: 0.5, Viability: 0.4, HasViability: true}))
		})

		It("applies an inclusive confidence bound", func() {
			f := filter(env.tenantA, job, results.StageSynthesized)
			f.MinConfidence = 0.9
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-2>B-1"}))
		})

		It("includes a row whose confidence equals the bound exactly", func() {
			exact := env.seedJob(env.tenantA, "completed")
			env.seedVote(env.tenantA, exact, "AC-5", "B-5", "EQUIVALENT", 0.8, 0.8)
			f := filter(env.tenantA, exact, results.StageSynthesized)
			f.MinConfidence = 0.8
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-5>B-5"}))
		})

		It("filters by relationship type", func() {
			f := filter(env.tenantA, job, results.StageSynthesized)
			f.RelationshipTypes = []string{"SUPPORTS"}
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-1>B-1"}))
		})

		It("matches a control as source or target", func() {
			f := filter(env.tenantA, job, results.StageSynthesized)
			f.ControlID = "B-1"
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-1>B-1", "AC-2>B-1"}))
			f.ControlID = "AC-2"
			page, err = env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-2>B-1"}))
		})

		It("spans completed jobs only when JobID is empty", func() {
			other := env.seedJob(env.tenantA, "completed")
			env.seedVote(env.tenantA, other, "AC-9", "B-1", "EQUIVALENT", 0.9, 0.9)
			running := env.seedJob(env.tenantA, "running")
			env.seedVote(env.tenantA, running, "AC-8", "B-1", "EQUIVALENT", 0.9, 0.9)
			f := filter(env.tenantA, "", results.StageSynthesized)
			f.ControlID = "B-1"
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			jobs := map[string]bool{}
			for _, m := range page.Mappings {
				jobs[m.JobID] = true
			}
			Expect(jobs).To(Equal(map[string]bool{job: true, other: true}))
			Expect(page.Mappings).To(HaveLen(3))
		})

		It("does not expose another tenant's rows on the all-jobs path", func() {
			f := filter(env.tenantB, "", results.StageSynthesized)
			f.ControlID = "B-1"
			page, err := env.store.ListMappings(env.ctxB, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Mappings).To(BeEmpty())
		})
	})

	Describe("analyzer stage", func() {
		var job string

		BeforeEach(func() {
			job = env.seedJob(env.tenantA, "completed")
			env.seedAnalysis(env.tenantA, job, `[
				{"source_id":"AC-2","target_id":"B-1","relationship_type":"EQUIVALENT","confidence":0.9},
				{"source_id":"AC-1","target_id":"B-1","relationship_type":"SUPPORTS","confidence":0.5,"properties":{"k":"v"}},
				{"source_id":"AC-3","target_id":"B-3","relationship_type":"NO_RELATIONSHIP","confidence":0.8}]`)
		})

		It("decodes, orders and excludes types not requested", func() {
			page, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, job, results.StageAnalyzer))
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-1>B-1", "AC-2>B-1"}))
			Expect(page.Mappings[0]).To(Equal(results.Mapping{JobID: job, SourceID: "AC-1", TargetID: "B-1",
				RelationshipType: "SUPPORTS", Confidence: 0.5}))
			Expect(page.Mappings[0].HasViability).To(BeFalse())
		})

		It("filters by control, confidence and type", func() {
			f := filter(env.tenantA, job, results.StageAnalyzer)
			f.ControlID = "B-1"
			f.MinConfidence = 0.9
			f.RelationshipTypes = []string{"EQUIVALENT"}
			page, err := env.store.ListMappings(env.ctxA, f)
			Expect(err).NotTo(HaveOccurred())
			Expect(keys(page)).To(Equal([]string{"AC-2>B-1"}))
		})

		It("returns an empty page when the relationship analyzer produced nothing", func() {
			empty := env.seedJob(env.tenantA, "completed")
			page, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, empty, results.StageAnalyzer))
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Mappings).To(BeEmpty())
			Expect(page.Next).To(BeNil())
		})

		It("names the job and analyzer when result_data has the wrong shape", func() {
			bad := env.seedJob(env.tenantA, "completed")
			env.seedAnalysis(env.tenantA, bad, `{"x":1}`)
			_, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, bad, results.StageAnalyzer))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(bad))
			Expect(err.Error()).To(ContainSubstring("relationship"))
		})

		It("fails closed, naming the job, analyzer and pair, when result_data repeats a pair", func() {
			dup := env.seedJob(env.tenantA, "completed")
			env.seedAnalysis(env.tenantA, dup, `[
				{"source_id":"AC-1","target_id":"B-1","relationship_type":"EQUIVALENT","confidence":0.9},
				{"source_id":"AC-1","target_id":"B-1","relationship_type":"SUPPORTS","confidence":0.5}]`)
			page, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, dup, results.StageAnalyzer))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(dup))
			Expect(err.Error()).To(ContainSubstring("relationship"))
			Expect(err.Error()).To(ContainSubstring("AC-1"))
			Expect(err.Error()).To(ContainSubstring("B-1"))
			Expect(page.Mappings).To(BeEmpty())
		})

		It("refuses the analyzer stage without a job, saying what to pass instead", func() {
			page, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, "", results.StageAnalyzer))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("the analyzer stage needs a job ID"))
			Expect(err.Error()).To(ContainSubstring("request the synthesized stage"))
			Expect(page.Mappings).To(BeEmpty())
		})

		It("records the decoded result_data size on a gauge", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			store := results.NewPGStore(db.NewTenantPool(env.appPool),
				results.WithStoreTelemetry(tp.TracerProvider(), tp.MeterProvider()))
			_, err = store.ListMappings(env.ctxA, filter(env.tenantA, job, results.StageAnalyzer))
			Expect(err).NotTo(HaveOccurred())
			m := telemetrytest.FindMetric(tp.GetMetrics(), "results.store.analyzer_result_bytes")
			Expect(m).NotTo(BeNil())
			g, ok := m.Data.(metricdata.Gauge[float64])
			Expect(ok).To(BeTrue(), "metric data is %T", m.Data)
			Expect(g.DataPoints).To(HaveLen(1))
			Expect(g.DataPoints[0].Value).To(BeNumerically(">", 100))
		})
	})

	Describe("paging", func() {
		collect := func(ctx context.Context, f results.Filter) ([]string, int) {
			var all []string
			pages := 0
			for {
				page, err := env.store.ListMappings(ctx, f)
				Expect(err).NotTo(HaveOccurred())
				pages++
				all = append(all, keys(page)...)
				if page.Next == nil {
					return all, pages
				}
				f.After = page.Next
			}
		}
		want := []string{"A1>B", "A2>B", "A3>B", "A4>B", "A5>B"}

		It("walks synthesized rows in three pages without gaps or duplicates", func() {
			job := env.seedJob(env.tenantA, "completed")
			for i := 5; i >= 1; i-- {
				env.seedVote(env.tenantA, job, fmt.Sprintf("A%d", i), "B", "EQUIVALENT", 0.9, 0.9)
			}
			f := filter(env.tenantA, job, results.StageSynthesized)
			f.PageSize = 2
			got, pages := collect(env.ctxA, f)
			Expect(got).To(Equal(want))
			Expect(pages).To(Equal(3))
		})

		It("walks analyzer rows in three pages without gaps or duplicates", func() {
			job := env.seedJob(env.tenantA, "completed")
			env.seedAnalysis(env.tenantA, job, `[
				{"source_id":"A5","target_id":"B","relationship_type":"EQUIVALENT","confidence":0.9},
				{"source_id":"A3","target_id":"B","relationship_type":"EQUIVALENT","confidence":0.9},
				{"source_id":"A1","target_id":"B","relationship_type":"EQUIVALENT","confidence":0.9},
				{"source_id":"A4","target_id":"B","relationship_type":"EQUIVALENT","confidence":0.9},
				{"source_id":"A2","target_id":"B","relationship_type":"EQUIVALENT","confidence":0.9}]`)
			f := filter(env.tenantA, job, results.StageAnalyzer)
			f.PageSize = 2
			got, pages := collect(env.ctxA, f)
			Expect(got).To(Equal(want))
			Expect(pages).To(Equal(3))
		})
	})

	Describe("refusals", func() {
		It("reports an unknown job", func() {
			_, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, "no-such-job", results.StageSynthesized))
			Expect(errors.Is(err, results.ErrJobNotFound)).To(BeTrue())
		})

		It("reports a job that has not completed, with its status", func() {
			job := env.seedJob(env.tenantA, "running")
			env.seedVote(env.tenantA, job, "A1", "B", "EQUIVALENT", 0.9, 0.9)
			_, err := env.store.ListMappings(env.ctxA, filter(env.tenantA, job, results.StageSynthesized))
			var notDone *results.JobNotCompletedError
			Expect(errors.As(err, &notDone)).To(BeTrue())
			Expect(notDone.JobID).To(Equal(job))
			Expect(notDone.Status).To(Equal("running"))
		})

		It("hides one tenant's job from another", func() {
			job := env.seedJob(env.tenantA, "completed")
			env.seedVote(env.tenantA, job, "A1", "B", "EQUIVALENT", 0.9, 0.9)
			_, err := env.store.ListMappings(env.ctxB, filter(env.tenantB, job, results.StageSynthesized))
			Expect(errors.Is(err, results.ErrJobNotFound)).To(BeTrue())
		})

		It("refuses a filter tenant that differs from the context tenant and reads nothing", func() {
			job := env.seedJob(env.tenantA, "completed")
			env.seedVote(env.tenantA, job, "A1", "B", "EQUIVALENT", 0.9, 0.9)
			page, err := env.store.ListMappings(env.ctxB, filter(env.tenantA, job, results.StageSynthesized))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(env.tenantA))
			Expect(page.Mappings).To(BeEmpty())
		})
	})
})
