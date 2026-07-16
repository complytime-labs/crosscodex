//go:build integration

package synthesis_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ---------------------------------------------------------------------------
// Suite bootstrap
// ---------------------------------------------------------------------------

func TestSynthesisIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Synthesis Integration BDD Suite")
}

// ---------------------------------------------------------------------------
// Suite-level DB state
// ---------------------------------------------------------------------------

var (
	intSuDSN  string
	intSuPool db.Pool
)

var _ = BeforeEach(func() {
	// Redirect slog output to GinkgoWriter so log noise only appears on failure.
})

var _ = SynchronizedBeforeSuite(func() []byte {
	ctx := context.Background()

	intSuDSN = os.Getenv("TEST_DATABASE_DSN")
	if intSuDSN == "" {
		Fail("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	migrator, err := db.NewMigrator(intSuDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
	Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

	adminDB, err := sql.Open("pgx", intSuDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'")
	Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
	Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

	return []byte(intSuDSN)
}, func(data []byte) {
	ctx := context.Background()
	intSuDSN = string(data)

	var err error
	intSuPool, err = db.NewPool(db.PoolConfig{
		DSN:          intSuDSN,
		MaxOpenConns: 5,
	})
	Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")

	// Verify migrations applied (vote_summaries must exist).
	err = intSuPool.Exec(ctx, "SELECT 1 FROM vote_summaries LIMIT 0")
	Expect(err).NotTo(HaveOccurred(),
		"vote_summaries table does not exist — migration 005 may not have applied.\n"+
			"If the schema_migrations table is dirty, run: task test:integration:clean")
})

var _ = SynchronizedAfterSuite(func() {
	// per-process cleanup (nothing needed)
}, func() {
	if intSuPool != nil {
		intSuPool.Close()
	}
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func synthAppUserDSN() string {
	u, err := url.Parse(intSuDSN)
	Expect(err).NotTo(HaveOccurred(), "bad intSuDSN")
	u.User = url.UserPassword("app_user", "apppass")
	return u.String()
}

// setupSynthTenant creates a tenant row via superuser (bypasses RLS).
func setupSynthTenant(tenantID, displayName string) {
	ctx := context.Background()
	err := intSuPool.Exec(ctx,
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	Expect(err).NotTo(HaveOccurred(), "setupSynthTenant: "+tenantID)
}

// setupSynthJob inserts a job row for the given tenant and returns the job_id.
// Uses a superuser connection so it bypasses RLS during setup.
func setupSynthJob(tenantID, jobID, status string) {
	ctx := context.Background()
	err := intSuPool.Exec(ctx,
		`INSERT INTO jobs (job_id, tenant_id, status, created_by)
		 VALUES ($1, $2, $3, 'test-runner')
		 ON CONFLICT (job_id) DO NOTHING`,
		jobID, tenantID, status)
	Expect(err).NotTo(HaveOccurred(), "setupSynthJob: "+jobID)
}

// setupVoteSummaries inserts vote_summary rows via superuser.
func setupVoteSummaries(tenantID, jobID string, pairs [][2]string) {
	ctx := context.Background()
	for _, pair := range pairs {
		err := intSuPool.Exec(ctx,
			`INSERT INTO vote_summaries
			   (job_id, source_id, target_id, consensus, confidence, viability, tenant_id)
			 VALUES ($1, $2, $3, 'EQUIVALENT', 0.9, 0.0, $4)
			 ON CONFLICT (job_id, source_id, target_id) DO NOTHING`,
			jobID, pair[0], pair[1], tenantID)
		Expect(err).NotTo(HaveOccurred(), "setupVoteSummaries: "+pair[0]+"->"+pair[1])
	}
}

// newAppTenantConnection returns a TenantConnection using the restricted app_user role.
func newAppTenantConnection() db.TenantConnection {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          synthAppUserDSN(),
		MaxOpenConns: 2,
	})
	Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
	DeferCleanup(func() { pool.Close() })
	return db.NewTenantPool(pool)
}

// defaultSynthCfg returns a minimal SynthesisConfig for integration tests.
func defaultSynthCfg() config.SynthesisConfig {
	return config.SynthesisConfig{
		ConfidenceThreshold:   0.5,
		MaxMappingsPerControl: 10,
		Viability: config.ViabilityConfig{
			TypeMismatchFactor: 0.8,
			SkipLevelFactor:    0.7,
			IntegralToFactor:   1.1,
		},
		Assessment: config.AssessmentConfig{
			IQRGood:        20.0,
			IQRPoor:        10.0,
			NoRelHigh:      0.97,
			NoRelLow:       0.80,
			ContestedWarn:  0.20,
			ActionableWarn: 0.30,
		},
	}
}

// buildInputs builds SynthesisInput slices from source/target pairs.
func buildInputs(pairs [][2]string, relationship, contribution string, confidence float64) []synthesis.SynthesisInput {
	inputs := make([]synthesis.SynthesisInput, len(pairs))
	for i, pair := range pairs {
		inputs[i] = synthesis.SynthesisInput{
			SourceID:              pair[0],
			TargetID:              pair[1],
			SimilarityScore:       75.0,
			SimilarityMedian:      75.0,
			SimilarityVar:         5.0,
			SimilarityCount:       3,
			ConsensusRelationship: relationship,
			ContributionType:      contribution,
			ConfidenceFraction:    confidence,
			Unanimous:             true,
		}
	}
	return inputs
}

// ---------------------------------------------------------------------------
// Integration specs
// ---------------------------------------------------------------------------

var _ = Describe("Service.Execute — persistViability integration", Ordered, func() {
	const (
		intTenantID    = "synth-int-test"
		intDisplayName = "Synthesis Integration Test Tenant"
	)

	BeforeAll(func() {
		setupSynthTenant(intTenantID, intDisplayName)
	})

	Context("when vote_summaries rows exist for a pending job", func() {
		const jobID = "synth-int-job-pending"

		BeforeAll(func() {
			setupSynthJob(intTenantID, jobID, "pending")
			setupVoteSummaries(intTenantID, jobID, [][2]string{
				{"ctrl-A", "ctrl-X"},
				{"ctrl-A", "ctrl-Y"},
				{"ctrl-B", "ctrl-X"},
			})
		})

		It("updates viability weights and returns updated count", func() {
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"EQUIVALENT", "SUPERSET_OF"})

			inputs := buildInputs([][2]string{
				{"ctrl-A", "ctrl-X"},
				{"ctrl-A", "ctrl-Y"},
				{"ctrl-B", "ctrl-X"},
			}, "EQUIVALENT", "INTEGRAL_TO", 0.9)

			result, err := svc.Execute(ctx, jobID, inputs, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Rows).To(HaveLen(3))

			// Verify viability weights were actually written to the database.
			ctx2 := context.Background()
			rows, queryErr := intSuPool.Query(ctx2,
				"SELECT source_id, target_id, viability FROM vote_summaries WHERE job_id = $1 ORDER BY source_id, target_id",
				jobID)
			Expect(queryErr).NotTo(HaveOccurred())
			defer rows.Close()

			type row struct {
				src, tgt  string
				viability float64
			}
			var found []row
			for rows.Next() {
				var r row
				Expect(rows.Scan(&r.src, &r.tgt, &r.viability)).To(Succeed())
				found = append(found, r)
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(found).To(HaveLen(3))
			for _, r := range found {
				Expect(r.viability).To(BeNumerically(">", 0.0),
					"viability should be non-zero after Execute for %s->%s", r.src, r.tgt)
			}
		})
	})

	Context("when vote_summaries rows do not exist for the given job", func() {
		const jobID = "synth-int-job-missing-rows"

		BeforeAll(func() {
			setupSynthJob(intTenantID, jobID, "pending")
			// No vote_summaries inserted — all updates will affect 0 rows.
		})

		It("returns ErrDBNoRowsAffected and rolls back", func() {
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"EQUIVALENT"})

			inputs := buildInputs([][2]string{
				{"ctrl-MISSING", "ctrl-ALSO-MISSING"},
			}, "EQUIVALENT", "INTEGRAL_TO", 0.9)

			_, err = svc.Execute(ctx, jobID, inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBNoRowsAffected)).To(BeTrue(),
				"expected ErrDBNoRowsAffected, got: %v", err)
		})
	})

	Context("when the parent job is completed (immutability trigger)", func() {
		const jobID = "synth-int-job-completed"

		BeforeAll(func() {
			setupSynthJob(intTenantID, jobID, "completed")
			setupVoteSummaries(intTenantID, jobID, [][2]string{
				{"ctrl-C", "ctrl-Z"},
			})
		})

		It("returns ErrImmutabilityViolation and rolls back", func() {
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"EQUIVALENT"})

			inputs := buildInputs([][2]string{
				{"ctrl-C", "ctrl-Z"},
			}, "EQUIVALENT", "INTEGRAL_TO", 0.9)

			_, err = svc.Execute(ctx, jobID, inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrImmutabilityViolation)).To(BeTrue(),
				"expected ErrImmutabilityViolation, got: %v", err)

			// Verify the row was NOT updated (rollback was effective).
			ctx2 := context.Background()
			var viability float64
			queryErr := intSuPool.QueryRow(ctx2,
				"SELECT viability FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
				jobID, "ctrl-C", "ctrl-Z").Scan(&viability)
			Expect(queryErr).NotTo(HaveOccurred())
			Expect(viability).To(Equal(0.0),
				"viability must remain 0 after rolled-back immutability violation")
		})
	})

	Context("tenant isolation (RLS enforcement)", func() {
		const jobID = "synth-int-job-rls"
		const otherTenantID = "synth-int-other-tenant"

		BeforeAll(func() {
			setupSynthTenant(otherTenantID, "Other Tenant")
			// Job and rows belong to otherTenantID.
			setupSynthJob(otherTenantID, jobID, "pending")
			setupVoteSummaries(otherTenantID, jobID, [][2]string{
				{"ctrl-RLS", "ctrl-OTHER"},
			})
		})

		It("returns ErrDBNoRowsAffected when tenant context does not match row tenant", func() {
			// intTenantID has no access to rows owned by otherTenantID.
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"EQUIVALENT"})

			inputs := buildInputs([][2]string{
				{"ctrl-RLS", "ctrl-OTHER"},
			}, "EQUIVALENT", "INTEGRAL_TO", 0.9)

			_, err = svc.Execute(ctx, jobID, inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBNoRowsAffected)).To(BeTrue(),
				"RLS should prevent cross-tenant update; expected ErrDBNoRowsAffected, got: %v", err)

			// Verify the row was NOT updated in the other tenant's data.
			ctx2 := context.Background()
			var viability float64
			queryErr := intSuPool.QueryRow(ctx2,
				"SELECT viability FROM vote_summaries WHERE job_id = $1 AND source_id = $2",
				jobID, "ctrl-RLS").Scan(&viability)
			Expect(queryErr).NotTo(HaveOccurred())
			Expect(viability).To(Equal(0.0), "cross-tenant update must be a no-op")
		})
	})
})
