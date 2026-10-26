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
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
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
	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred(), "app_user password")
	stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
	Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
	_, err = adminDB.ExecContext(ctx, stmt)
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
		"vote_summaries table does not exist — the initial schema may not have applied.\n"+
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
	pw, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred(), "app_user password")
	u.User = url.UserPassword("app_user", pw)
	return u.String()
}

// setupSynthTenant creates a tenant row via superuser (bypasses RLS).
func setupSynthTenant(tenantID, displayName string) {
	err := db.EnsureTenant(context.Background(), intSuPool, tenantID, displayName)
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

// setupVoteSummaries inserts one vote_summaries row per pair for the given
// analyzer via superuser.
func setupVoteSummaries(tenantID, jobID, analyzerName string, pairs [][2]string) {
	ctx := context.Background()
	for _, pair := range pairs {
		err := intSuPool.Exec(ctx,
			`INSERT INTO vote_summaries
			   (job_id, source_id, target_id, analyzer_name, consensus, confidence, viability, tenant_id)
			 VALUES ($1, $2, $3, $4, 'EQUIVALENT', 0.9, 0.0, $5)
			 ON CONFLICT (job_id, source_id, target_id, analyzer_name) DO NOTHING`,
			jobID, pair[0], pair[1], analyzerName, tenantID)
		Expect(err).NotTo(HaveOccurred(), "setupVoteSummaries: "+pair[0]+"->"+pair[1]+" ("+analyzerName+")")
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

// buildInputs builds SynthesisInput slices from source/target pairs produced
// by the named analyzer, with no CONTRIBUTES_TO sub-classification.
func buildInputs(pairs [][2]string, relationship, analyzerName string, confidence float64) []synthesis.SynthesisInput {
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
			Analyzer:              analyzerName,
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
			setupVoteSummaries(intTenantID, jobID, "relationship", [][2]string{
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
			}, "EQUIVALENT", "relationship", 0.9)

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

	Context("when one pair has both a requires and a relationship contribution", func() {
		const jobID = "synth-int-job-both-contributions"

		BeforeAll(func() {
			setupSynthJob(intTenantID, jobID, "pending")
			setupVoteSummaries(intTenantID, jobID, "requires", [][2]string{{"ctrl-D", "ctrl-W"}})
			setupVoteSummaries(intTenantID, jobID, "relationship", [][2]string{{"ctrl-D", "ctrl-W"}})
		})

		It("succeeds and writes each contribution's own viability to its own row", func() {
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"EQUIVALENT"})

			// Distinct similarity scores give the two contributions distinct
			// viabilities, so a row receiving the other contribution's value
			// is detectable.
			requiresInput := buildInputs([][2]string{{"ctrl-D", "ctrl-W"}}, synthesis.ConsensusRequires, "requires", 0.9)[0]
			requiresInput.SimilarityScore = 60.0
			relationshipInput := buildInputs([][2]string{{"ctrl-D", "ctrl-W"}}, "EQUIVALENT", "relationship", 0.9)[0]
			relationshipInput.SimilarityScore = 90.0

			result, err := svc.Execute(ctx, jobID, []synthesis.SynthesisInput{requiresInput, relationshipInput}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Rows).To(HaveLen(2))

			want := make(map[string]float64, len(result.Rows))
			for _, r := range result.Rows {
				want[r.Analyzer] = r.ViabilityWeight
			}
			Expect(want).To(HaveLen(2))
			Expect(want["requires"]).NotTo(Equal(want["relationship"]),
				"fixture must give the contributions different viabilities")

			rows, queryErr := intSuPool.Query(context.Background(),
				"SELECT analyzer_name, viability FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
				jobID, "ctrl-D", "ctrl-W")
			Expect(queryErr).NotTo(HaveOccurred())
			defer rows.Close()
			got := make(map[string]float64)
			for rows.Next() {
				var contribution string
				var viability float64
				Expect(rows.Scan(&contribution, &viability)).To(Succeed())
				got[contribution] = viability
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		})
	})

	Context("when relationship pairs differ only in their contribution type", func() {
		const jobID = "synth-int-job-integral-to"

		BeforeAll(func() {
			setupSynthJob(intTenantID, jobID, "pending")
			setupVoteSummaries(intTenantID, jobID, "relationship", [][2]string{
				{"ctrl-F", "ctrl-U"},
				{"ctrl-G", "ctrl-U"},
			})
		})

		It("persists the IntegralToFactor-weighted viability for INTEGRAL_TO only", func() {
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"CONTRIBUTES_TO"})

			inputs := buildInputs([][2]string{{"ctrl-F", "ctrl-U"}, {"ctrl-G", "ctrl-U"}}, "CONTRIBUTES_TO", "relationship", 0.9)
			inputs[0].ContributionType = "INTEGRAL_TO"
			inputs[1].ContributionType = "EXAMPLE_OF"

			_, err = svc.Execute(ctx, jobID, inputs, nil)
			Expect(err).NotTo(HaveOccurred())

			viability := func(sourceID string) float64 {
				var v float64
				Expect(intSuPool.QueryRow(context.Background(),
					"SELECT viability FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = 'ctrl-U' AND analyzer_name = 'relationship'",
					jobID, sourceID).Scan(&v)).To(Succeed())
				return v
			}
			// Unclassified controls on both sides: type and level factors are
			// 1, so viability is the similarity score times the contribution
			// factor (IntegralToFactor 1.1 in defaultSynthCfg).
			Expect(viability("ctrl-G")).To(Equal(75.0), "EXAMPLE_OF gets no contribution factor")
			Expect(viability("ctrl-F")).To(Equal(82.5), "INTEGRAL_TO gets IntegralToFactor")
		})
	})

	Context("when a synthesis row's contribution has no vote_summaries row", func() {
		const jobID = "synth-int-job-missing-contribution"

		BeforeAll(func() {
			setupSynthJob(intTenantID, jobID, "pending")
			// Only the requires row exists; the relationship input has no row.
			setupVoteSummaries(intTenantID, jobID, "requires", [][2]string{{"ctrl-E", "ctrl-V"}})
		})

		It("returns an actionable ErrDBNoRowsAffected and leaves the existing row unchanged", func() {
			ctx, err := tenant.WithTenant(context.Background(), intTenantID)
			Expect(err).NotTo(HaveOccurred())
			conn := newAppTenantConnection()
			svc := synthesis.New(conn, defaultSynthCfg(), []string{"EQUIVALENT"})

			inputs := append(
				buildInputs([][2]string{{"ctrl-E", "ctrl-V"}}, synthesis.ConsensusRequires, "requires", 0.9),
				buildInputs([][2]string{{"ctrl-E", "ctrl-V"}}, "EQUIVALENT", "relationship", 0.9)...,
			)

			_, err = svc.Execute(ctx, jobID, inputs, nil)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, synthesis.ErrDBNoRowsAffected)).To(BeTrue(),
				"expected ErrDBNoRowsAffected, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("expected 2 viability updates, got 1"))
			Expect(err.Error()).To(ContainSubstring("transaction rolled back"))

			var viability float64
			Expect(intSuPool.QueryRow(context.Background(),
				"SELECT viability FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = $3 AND analyzer_name = 'requires'",
				jobID, "ctrl-E", "ctrl-V").Scan(&viability)).To(Succeed())
			Expect(viability).To(Equal(0.0), "the matched row's update must be rolled back")
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
			}, "EQUIVALENT", "relationship", 0.9)

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
			setupVoteSummaries(intTenantID, jobID, "relationship", [][2]string{
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
			}, "EQUIVALENT", "relationship", 0.9)

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
			setupVoteSummaries(otherTenantID, jobID, "relationship", [][2]string{
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
			}, "EQUIVALENT", "relationship", 0.9)

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
