//go:build integration

package retention_test

// End-to-end retention engine integration specs. These exercise the full
// Collector → HoldChecker → Archiver → Purger → AuditPublisher pipeline against
// a real PostgreSQL container (started by `task test:integration:db`) and real
// on-disk object stores, proving the guarantees that unit fakes cannot:
//
//   - archive-before-purge with a real content-hash read-back verification,
//   - the role-based purge carve-out (purge_user / retention_purge) over a real
//     purge_user connection distinct from the app_user pool,
//   - legal holds preventing deletion of expired data,
//   - and — closing the purge-path integration deferred from R1/R2 — that RLS
//     tenant isolation holds through the entire engine, not just the hold store.
//
// This file lives in pkg/retention (not test/integration) deliberately: the
// package already carries the DB BeforeSuite (holdIntegPool + migrations) in
// hold_store_integration_bdd_test.go and is already wired into
// `task test:integration:db`. The test/integration package has no DB harness and
// its trace-propagation target starts no containers, so a DB-dependent suite
// there would either break that target or never receive a database.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// enginePurgePool is the purge_user pool used by the engine integration specs.
// The purger MUST authenticate as purge_user (member of retention_purge); the
// app_user holdIntegPool is rejected by the delete-path immutability triggers.
var enginePurgePool db.Pool

// engineSuPool is a superuser pool used only to provision tenants. Tenant
// insertion fires the AGE graph-create trigger, which requires privileges the
// RLS-scoped app_user role does not have — so provisioning is deliberately a
// privileged operation (see pkg/db.EnsureTenant).
var engineSuPool db.Pool

// integPurgeUserDSN rewrites the superuser DSN to authenticate as purge_user,
// preserving all TLS parameters (only the userinfo component changes).
func integPurgeUserDSN(suDSN string) string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad TEST_DATABASE_DSN: %v", err))
	}
	u.User = url.UserPassword("purge_user", "purgepass")
	return u.String()
}

// integCaptureAudit is an in-process AuditPublisher that records every emitted
// record, so specs can assert the archive/purge audit trail without a live NATS
// bus. It fully honors the AuditPublisher contract (Publish never fails).
type integCaptureAudit struct {
	records []retention.AuditRecord
}

func (c *integCaptureAudit) Publish(_ context.Context, r retention.AuditRecord) error {
	c.records = append(c.records, r)
	return nil
}

func (c *integCaptureAudit) actions() []string {
	out := make([]string, 0, len(c.records))
	for _, r := range c.records {
		out = append(out, r.Action)
	}
	return out
}

// The corruptingProvider used by the corrupting-archive spec is defined in
// archive_bdd_test.go (same package): it accepts every Put but returns mangled
// bytes on Get, so the Archiver's read-back hash never matches and archival
// fails with ErrArchiveVerify.

// integKit bundles a per-tenant engine and the concrete stores behind it so a
// spec can both drive Engine.Scan and assert against the real backends.
type integKit struct {
	engine  *retention.Engine
	primary storage.Provider
	archive storage.Provider // the REAL archive store (unwrapped), for read-back assertions
	audit   *integCaptureAudit
	tenant  string
}

// expiredTiers returns default retention tiers under which data created "now" is
// already expired relative to the far-future scan clock used by these specs.
func expiredTiers() config.RetentionTiers {
	return config.RetentionTiers{
		JobResults:  "24h",
		Catalogs:    "24h",
		Attestation: "24h",
	}
}

// integScanTime is the scan clock: far enough ahead of seed time that 24h-tier
// data is unambiguously expired, while any hold (ExpiresAt nil) stays active.
func integScanTime() time.Time {
	return time.Now().UTC().Add(72 * time.Hour)
}

// newIntegKit builds a per-tenant engine wired to real stores. wrapArchive, when
// non-nil, decorates the archive provider (used to inject the corrupting backend);
// the returned kit always exposes the underlying real archive store for assertions.
func newIntegKit(tenantID string, wrapArchive func(storage.Provider) storage.Provider) integKit {
	primaryRoot := GinkgoT().TempDir()
	archiveRoot := GinkgoT().TempDir()

	primary, err := storage.NewLocal(primaryRoot, tenantID)
	Expect(err).NotTo(HaveOccurred(), "create primary store")
	realArchive, err := storage.NewLocal(archiveRoot, tenantID)
	Expect(err).NotTo(HaveOccurred(), "create archive store")

	archiveForEngine := storage.Provider(realArchive)
	if wrapArchive != nil {
		archiveForEngine = wrapArchive(realArchive)
	}

	policy, err := retention.NewPolicy(config.RetentionConfig{Defaults: expiredTiers()})
	Expect(err).NotTo(HaveOccurred(), "build policy")

	appConn := db.NewTenantPool(holdIntegPool)
	purgeConn := db.NewTenantPool(enginePurgePool)

	collectors := []retention.Collector{
		retention.NewDBCollector(appConn),
		retention.NewCatalogCollector(appConn),
		retention.NewObjectCollector(primary, retention.ClassAttestation, tenantID),
	}
	holds := retention.NewPgHoldStore(appConn)
	arch := retention.NewArchiver(appConn, primary, archiveForEngine)
	purger := retention.NewPurger(purgeConn, primary)
	audit := &integCaptureAudit{}

	engine := retention.NewEngine(collectors, holds, arch, purger, audit, policy)

	return integKit{engine: engine, primary: primary, archive: realArchive, audit: audit, tenant: tenantID}
}

// integSeedJob provisions the tenant (jobs.tenant_id is a FK to tenants) and
// inserts a completed job for tenantID as app_user under RLS.
func integSeedJob(tenantID, jobID string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	// Provision the tenant via the superuser pool: the tenant-insert trigger
	// creates the per-tenant AGE graph, which app_user is not privileged to do.
	Expect(db.EnsureTenant(context.Background(), engineSuPool, tenantID, tenantID)).
		To(Succeed(), "provision tenant")

	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin seed tx")
	defer func() { _ = tx.Rollback() }()
	Expect(tx.Exec(ctx,
		`INSERT INTO jobs (job_id, tenant_id, status, created_by)
		 VALUES ($1, $2, 'completed', 'retention-integ')
		 ON CONFLICT DO NOTHING`,
		jobID, tenantID,
	)).To(Succeed(), "insert completed job")
	Expect(tx.Commit()).To(Succeed(), "commit seed")
}

// integSeedJobChildren provisions the tenant, inserts a completed job, and adds
// one row to each of the four tables that FK-reference jobs(job_id): job_stages,
// vote_summaries, analysis_results, relationship_candidates. All inserts run as
// app_user in a single tenant-scoped tx (RLS WITH CHECK requires tenant_id to
// match app.current_tenant). The child immutability triggers fire only on
// UPDATE/DELETE, so seeding a completed job's children is permitted.
func integSeedJobChildren(tenantID, jobID string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	Expect(db.EnsureTenant(context.Background(), engineSuPool, tenantID, tenantID)).
		To(Succeed(), "provision tenant")

	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin seed tx")
	defer func() { _ = tx.Rollback() }()

	Expect(tx.Exec(ctx,
		`INSERT INTO jobs (job_id, tenant_id, status, created_by)
		 VALUES ($1, $2, 'completed', 'retention-integ')
		 ON CONFLICT DO NOTHING`,
		jobID, tenantID,
	)).To(Succeed(), "insert completed job")
	Expect(tx.Exec(ctx,
		`INSERT INTO job_stages (job_id, stage_name, status, tenant_id)
		 VALUES ($1, 'collect', 'completed', $2)`,
		jobID, tenantID,
	)).To(Succeed(), "insert job_stage")
	Expect(tx.Exec(ctx,
		`INSERT INTO vote_summaries (job_id, source_id, target_id, consensus, confidence, viability, tenant_id)
		 VALUES ($1, 'src-1', 'tgt-1', 'accept', 0.9, 0.8, $2)`,
		jobID, tenantID,
	)).To(Succeed(), "insert vote_summary")
	Expect(tx.Exec(ctx,
		`INSERT INTO analysis_results (tenant_id, job_id, analyzer_name, result_data)
		 VALUES ($1, $2, 'similarity', '{"score":0.5}'::jsonb)`,
		tenantID, jobID,
	)).To(Succeed(), "insert analysis_result")
	Expect(tx.Exec(ctx,
		`INSERT INTO relationship_candidates (tenant_id, job_id, source_id, target_id, similarity_score)
		 VALUES ($1, $2, 'src-1', 'tgt-1', 42.0)`,
		tenantID, jobID,
	)).To(Succeed(), "insert relationship_candidate")
	// requires_candidates has no FK to jobs but is job-scoped by job_id; it is
	// folded into the job aggregate so it is archived and purged with the job.
	Expect(tx.Exec(ctx,
		`INSERT INTO requires_candidates (tenant_id, job_id, source_id, target_id, aggregate_score, provenance)
		 VALUES ($1, $2, 'src-1', 'tgt-1', 0.75, '{"reason":"seed"}'::jsonb)`,
		tenantID, jobID,
	)).To(Succeed(), "insert requires_candidate")

	Expect(tx.Commit()).To(Succeed(), "commit seed")
}

// integCountRequiresCandidates returns the number of requires_candidates rows for
// jobID, visible to tenantID as app_user. requires_candidates has no FK to jobs
// (it is counted separately from the FK children) but is job-scoped by job_id.
func integCountRequiresCandidates(tenantID, jobID string) int {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin requires-candidate-count tx")
	defer func() { _ = tx.Rollback() }()
	var n int
	Expect(tx.QueryRow(ctx,
		`SELECT count(*) FROM requires_candidates WHERE job_id = $1`,
		jobID,
	).Scan(&n)).To(Succeed())
	Expect(tx.Commit()).To(Succeed())
	return n
}

// integCountJobChildren returns the total number of rows across the four FK-child
// tables for jobID, visible to tenantID as app_user.
func integCountJobChildren(tenantID, jobID string) int {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin child-count tx")
	defer func() { _ = tx.Rollback() }()
	var n int
	Expect(tx.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM job_stages              WHERE job_id = $1)
		 + (SELECT count(*) FROM vote_summaries          WHERE job_id = $1)
		 + (SELECT count(*) FROM analysis_results        WHERE job_id = $1)
		 + (SELECT count(*) FROM relationship_candidates WHERE job_id = $1)`,
		jobID,
	).Scan(&n)).To(Succeed())
	Expect(tx.Commit()).To(Succeed())
	return n
}

// integPurgeJobChildren deletes jobID and its FK children for tenantID as
// purge_user (children first, then the parent — FK-safe). Best-effort cleanup.
func integPurgeJobChildren(tenantID, jobID string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		return
	}
	tx, err := db.NewTenantPool(enginePurgePool).Begin(ctx)
	if err != nil {
		return
	}
	for _, table := range []string{
		"job_stages", "vote_summaries", "analysis_results", "relationship_candidates",
		"requires_candidates",
	} {
		_ = tx.Exec(ctx, "DELETE FROM "+table+" WHERE job_id = $1", jobID)
	}
	_ = tx.Exec(ctx, `DELETE FROM jobs WHERE job_id = $1`, jobID)
	_ = tx.Commit()
}

// integJobExists reports whether jobID is visible to tenantID as app_user.
func integJobExists(tenantID, jobID string) bool {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin exists tx")
	defer func() { _ = tx.Rollback() }()
	var n int
	Expect(tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE job_id = $1`, jobID).Scan(&n)).To(Succeed())
	Expect(tx.Commit()).To(Succeed())
	return n > 0
}

// integPurgeJob deletes jobID for tenantID as purge_user. Best-effort cleanup:
// completed jobs are undeletable by app_user (immutability trigger), so tests
// that leave a job in place must clean it up through the sanctioned purge role.
func integPurgeJob(tenantID, jobID string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		return
	}
	tx, err := db.NewTenantPool(enginePurgePool).Begin(ctx)
	if err != nil {
		return
	}
	_ = tx.Exec(ctx, `DELETE FROM jobs WHERE job_id = $1`, jobID)
	_ = tx.Commit()
}

// integDeleteHold removes a hold row for tenantID as app_user (no immutability
// trigger on retention_holds).
func integDeleteHold(tenantID, name string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		return
	}
	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	if err != nil {
		return
	}
	_ = tx.Exec(ctx, `DELETE FROM public.retention_holds WHERE name = $1`, name)
	_ = tx.Commit()
}

// integSeedCatalogAggregate provisions the tenant, inserts a catalog, and adds
// one row to each catalog dependent that FK-references catalogs(catalog_id):
// controls, classifications, embeddings. All inserts run as app_user in a single
// tenant-scoped tx (RLS WITH CHECK requires tenant_id to match
// app.current_tenant). The classifications immutability trigger fires only on
// UPDATE/DELETE, so seeding is permitted. The embedding uses a small
// dimensionless vector literal (the column is model-agnostic).
func integSeedCatalogAggregate(tenantID, catalogID string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	Expect(db.EnsureTenant(context.Background(), engineSuPool, tenantID, tenantID)).
		To(Succeed(), "provision tenant")

	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin catalog seed tx")
	defer func() { _ = tx.Rollback() }()

	Expect(tx.Exec(ctx,
		`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
		 VALUES ($1, $2, 'NIST 800-53', 'rev5', 'oscal', 'catalogs/seed.json')
		 ON CONFLICT DO NOTHING`,
		catalogID, tenantID,
	)).To(Succeed(), "insert catalog")
	Expect(tx.Exec(ctx,
		`INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
		 VALUES ($1, 'ac-1', $2, 'AC-1', 'Access Control Policy', 'The organization develops a policy.', 'SP800-53')`,
		tenantID, catalogID,
	)).To(Succeed(), "insert control")
	Expect(tx.Exec(ctx,
		`INSERT INTO classifications (catalog_id, control_id, type, level, tenant_id)
		 VALUES ($1, 'ac-1', 'Technical', 'Operational', $2)`,
		catalogID, tenantID,
	)).To(Succeed(), "insert classification")
	Expect(tx.Exec(ctx,
		`INSERT INTO embeddings (catalog_id, control_id, model, vector, tenant_id)
		 VALUES ($1, 'ac-1', 'test-model', $2::vector, $3)`,
		catalogID, "[0.1,0.2,0.3]", tenantID,
	)).To(Succeed(), "insert embedding")

	Expect(tx.Commit()).To(Succeed(), "commit catalog seed")
}

// integCountCatalogDependents returns the total number of rows across the three
// catalog dependent tables for catalogID, visible to tenantID as app_user.
func integCountCatalogDependents(tenantID, catalogID string) int {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin dependent-count tx")
	defer func() { _ = tx.Rollback() }()
	var n int
	Expect(tx.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM classifications WHERE catalog_id = $1)
		 + (SELECT count(*) FROM embeddings      WHERE catalog_id = $1)
		 + (SELECT count(*) FROM controls        WHERE catalog_id = $1)`,
		catalogID,
	).Scan(&n)).To(Succeed())
	Expect(tx.Commit()).To(Succeed())
	return n
}

// integCatalogExists reports whether catalogID is visible to tenantID as app_user.
func integCatalogExists(tenantID, catalogID string) bool {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	tx, err := db.NewTenantPool(holdIntegPool).Begin(ctx)
	Expect(err).NotTo(HaveOccurred(), "begin catalog-exists tx")
	defer func() { _ = tx.Rollback() }()
	var n int
	Expect(tx.QueryRow(ctx, `SELECT count(*) FROM catalogs WHERE catalog_id = $1`, catalogID).Scan(&n)).To(Succeed())
	Expect(tx.Commit()).To(Succeed())
	return n > 0
}

// integPurgeCatalog deletes catalogID and its dependents for tenantID as
// purge_user (children first, then the parent — FK-safe). Best-effort cleanup.
func integPurgeCatalog(tenantID, catalogID string) {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		return
	}
	tx, err := db.NewTenantPool(enginePurgePool).Begin(ctx)
	if err != nil {
		return
	}
	for _, table := range []string{"classifications", "embeddings", "controls"} {
		_ = tx.Exec(ctx, "DELETE FROM "+table+" WHERE catalog_id = $1", catalogID)
	}
	_ = tx.Exec(ctx, `DELETE FROM catalogs WHERE catalog_id = $1`, catalogID)
	_ = tx.Commit()
}

var _ = Describe("Retention engine end-to-end", Ordered, func() {
	BeforeAll(func() {
		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		// Grant purge_user a known password so the engine can open its dedicated
		// pool. Scoped here to avoid touching the shared BeforeSuite harness.
		ctx := context.Background()
		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "open admin connection")
		_, err = adminDB.ExecContext(ctx, "ALTER ROLE purge_user WITH PASSWORD 'purgepass'")
		Expect(err).NotTo(HaveOccurred(), "set purge_user password")
		Expect(adminDB.Close()).To(Succeed())

		enginePurgePool, err = db.NewPool(db.PoolConfig{
			DSN:          integPurgeUserDSN(suDSN),
			MaxOpenConns: 3,
		})
		Expect(err).NotTo(HaveOccurred(), "create purge_user pool")

		engineSuPool, err = db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 3,
		})
		Expect(err).NotTo(HaveOccurred(), "create superuser pool")
	})

	AfterAll(func() {
		// Drop every tenant these specs provision. Deleting a tenant fires the
		// tenant_graph_drop trigger, removing the per-tenant graph schema owned by
		// graph_user — otherwise those schemas leak into later packages in the same
		// `ginkgo run` and block 001.down's DROP ROLE graph_user (2BP01). Must run
		// before engineSuPool is closed. Best-effort. Tenant B in the isolation
		// spec is never provisioned (no seed), so it is not listed here.
		if engineSuPool != nil {
			ctx := context.Background()
			for _, tenantID := range []string{
				"ret-e2e-purge", "ret-e2e-hold", "ret-e2e-corrupt", "ret-e2e-iso-a",
				"ret-e2e-children", "ret-e2e-catalog",
			} {
				// Delete FK children before the parent jobs/catalogs so a spec that
				// leaked a row cannot block the tenant delete below.
				for _, table := range []string{
					"job_stages", "vote_summaries", "analysis_results", "relationship_candidates",
					"requires_candidates",
					"classifications", "embeddings", "controls",
				} {
					_ = engineSuPool.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tenantID)
				}
				_ = engineSuPool.Exec(ctx, "DELETE FROM jobs WHERE tenant_id = $1", tenantID)
				_ = engineSuPool.Exec(ctx, "DELETE FROM catalogs WHERE tenant_id = $1", tenantID)
				_ = engineSuPool.Exec(ctx, "DELETE FROM tenants WHERE tenant_id = $1", tenantID)
			}
			engineSuPool.Close() //nolint:errcheck
		}
		if enginePurgePool != nil {
			enginePurgePool.Close() //nolint:errcheck
		}
	})

	It("archives then purges expired data with no hold (primary gone, archive verified)", func() {
		const tid = "ret-e2e-purge"
		const jobID = "job-e2e-purge-1"
		const objKey = "attestation/e2e-purge-1.json"
		objBody := []byte(`{"attestation":"e2e-purge"}`)

		kit := newIntegKit(tid, nil)
		ctx, err := tenant.WithTenant(context.Background(), tid)
		Expect(err).NotTo(HaveOccurred())

		integSeedJob(tid, jobID)
		DeferCleanup(func() { integPurgeJob(tid, jobID) })
		Expect(kit.primary.Put(ctx, objKey, bytes.NewReader(objBody))).To(Succeed())

		rep, err := kit.engine.Scan(ctx, retention.ScanOptions{Now: integScanTime()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Errors).To(BeEmpty(), "no archive/purge errors expected")
		Expect(rep.Scanned).To(Equal(2))
		Expect(rep.Archived).To(Equal(2))
		Expect(rep.Purged).To(Equal(2))

		// DB job: row gone from the primary store, JSON serialization present in
		// the archive at the deterministic db/<class>/<id>.json key.
		Expect(integJobExists(tid, jobID)).To(BeFalse(), "purged job must be gone")
		dbArchiveKey := fmt.Sprintf("db/%s/%s.json", retention.ClassJobResults, jobID)
		archived, err := kit.archive.Exists(ctx, dbArchiveKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(archived).To(BeTrue(), "job row must be serialized to the archive")

		// Object: primary copy deleted, archive copy present and byte-identical
		// (the engine hash-verifies before purge; equal bytes prove the copy).
		primaryHas, err := kit.primary.Exists(ctx, objKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(primaryHas).To(BeFalse(), "purged object must be gone from primary")
		rc, err := kit.archive.Get(ctx, objKey)
		Expect(err).NotTo(HaveOccurred())
		archivedBody, err := io.ReadAll(rc)
		Expect(rc.Close()).To(Succeed())
		Expect(err).NotTo(HaveOccurred())
		Expect(archivedBody).To(Equal(objBody), "archived object must match source bytes")

		Expect(kit.audit.actions()).To(ContainElements(retention.ActionArchive, retention.ActionPurge))
	})

	It("archives then purges a completed job together with all its FK children", func() {
		const tid = "ret-e2e-children"
		const jobID = "job-e2e-children-1"

		kit := newIntegKit(tid, nil)
		ctx, err := tenant.WithTenant(context.Background(), tid)
		Expect(err).NotTo(HaveOccurred())

		integSeedJobChildren(tid, jobID)
		DeferCleanup(func() { integPurgeJobChildren(tid, jobID) })

		// Precondition: all four FK child sets and the job-scoped
		// requires_candidates row are present before the scan.
		Expect(integCountJobChildren(tid, jobID)).To(Equal(4), "seed must create one row per child table")
		Expect(integCountRequiresCandidates(tid, jobID)).To(Equal(1), "seed must create one requires_candidates row")

		rep, err := kit.engine.Scan(ctx, retention.ScanOptions{Now: integScanTime()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Errors).To(BeEmpty(), "no archive/purge errors expected")
		Expect(rep.Purged).To(BeNumerically(">=", 1), "the completed job must be purged")

		// The parent job and every FK child must be gone: the single-statement
		// DELETE FROM jobs would have failed with foreign_key_violation.
		Expect(integJobExists(tid, jobID)).To(BeFalse(), "purged job must be gone")
		Expect(integCountJobChildren(tid, jobID)).To(Equal(0), "all FK children must be purged with the parent")
		// requires_candidates (no FK, job-scoped by column) must also be purged.
		Expect(integCountRequiresCandidates(tid, jobID)).To(Equal(0), "requires_candidates must be purged with the job")

		// The aggregate archive doc exists and carries the parent plus every child
		// set — proving json_build_object/json_agg serialized the children.
		dbArchiveKey := fmt.Sprintf("db/%s/%s.json", retention.ClassJobResults, jobID)
		rc, err := kit.archive.Get(ctx, dbArchiveKey)
		Expect(err).NotTo(HaveOccurred(), "aggregate archive doc must exist")
		body, err := io.ReadAll(rc)
		Expect(rc.Close()).To(Succeed())
		Expect(err).NotTo(HaveOccurred())

		var doc struct {
			Job                    map[string]any   `json:"job"`
			JobStages              []map[string]any `json:"job_stages"`
			VoteSummaries          []map[string]any `json:"vote_summaries"`
			AnalysisResults        []map[string]any `json:"analysis_results"`
			RelationshipCandidates []map[string]any `json:"relationship_candidates"`
			RequiresCandidates     []map[string]any `json:"requires_candidates"`
		}
		Expect(json.Unmarshal(body, &doc)).To(Succeed(), "aggregate archive doc must be valid JSON")
		Expect(doc.Job).NotTo(BeEmpty(), "archived doc must contain the parent job")
		Expect(doc.Job["job_id"]).To(Equal(jobID))
		Expect(doc.JobStages).To(HaveLen(1), "archived doc must contain the job_stages child")
		Expect(doc.VoteSummaries).To(HaveLen(1), "archived doc must contain the vote_summaries child")
		Expect(doc.AnalysisResults).To(HaveLen(1), "archived doc must contain the analysis_results child")
		Expect(doc.RelationshipCandidates).To(HaveLen(1), "archived doc must contain the relationship_candidates child")
		Expect(doc.RequiresCandidates).To(HaveLen(1), "archived doc must contain the requires_candidates set")

		Expect(kit.audit.actions()).To(ContainElements(retention.ActionArchive, retention.ActionPurge))
	})

	It("archives then purges an expired catalog together with its classifications, controls, and embeddings", func() {
		const tid = "ret-e2e-catalog"
		const catalogID = "cat-e2e-1"

		kit := newIntegKit(tid, nil)
		ctx, err := tenant.WithTenant(context.Background(), tid)
		Expect(err).NotTo(HaveOccurred())

		integSeedCatalogAggregate(tid, catalogID)
		DeferCleanup(func() { integPurgeCatalog(tid, catalogID) })

		// Precondition: all three dependent rows are present before the scan.
		Expect(integCountCatalogDependents(tid, catalogID)).To(Equal(3), "seed must create one row per dependent table")

		rep, err := kit.engine.Scan(ctx, retention.ScanOptions{Now: integScanTime()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Errors).To(BeEmpty(), "no archive/purge errors expected")
		Expect(rep.Purged).To(BeNumerically(">=", 1), "the catalog must be purged")

		// The parent catalog and every dependent must be gone: the single-statement
		// DELETE FROM catalogs would have failed with foreign_key_violation
		// otherwise.
		Expect(integCatalogExists(tid, catalogID)).To(BeFalse(), "purged catalog must be gone")
		Expect(integCountCatalogDependents(tid, catalogID)).To(Equal(0), "all catalog dependents must be purged")

		// The aggregate archive doc exists and carries the parent plus its
		// classifications and controls — proving json_build_object/json_agg
		// serialized the children.
		dbArchiveKey := fmt.Sprintf("db/%s/%s.json", retention.ClassCatalogs, catalogID)
		rc, err := kit.archive.Get(ctx, dbArchiveKey)
		Expect(err).NotTo(HaveOccurred(), "aggregate archive doc must exist")
		body, err := io.ReadAll(rc)
		Expect(rc.Close()).To(Succeed())
		Expect(err).NotTo(HaveOccurred())

		var doc struct {
			Catalog         map[string]any   `json:"catalog"`
			Classifications []map[string]any `json:"classifications"`
			Controls        []map[string]any `json:"controls"`
		}
		Expect(json.Unmarshal(body, &doc)).To(Succeed(), "aggregate archive doc must be valid JSON")
		Expect(doc.Catalog).NotTo(BeEmpty(), "archived doc must contain the parent catalog")
		Expect(doc.Catalog["catalog_id"]).To(Equal(catalogID))
		Expect(doc.Classifications).To(HaveLen(1), "archived doc must contain the classifications child")
		Expect(doc.Controls).To(HaveLen(1), "archived doc must contain the controls child")

		// embeddings are purged as a catalog dependent but deliberately OMITTED
		// from the archive doc (derived/regenerable, purge-only). Assert the key is
		// absent so a future change that leaks embeddings into the archive is caught.
		var raw map[string]json.RawMessage
		Expect(json.Unmarshal(body, &raw)).To(Succeed())
		_, hasEmbeddings := raw["embeddings"]
		Expect(hasEmbeddings).To(BeFalse(), "archive doc must omit embeddings (derived/regenerable, purge-only)")

		Expect(kit.audit.actions()).To(ContainElements(retention.ActionArchive, retention.ActionPurge))
	})

	It("retains expired data covered by an active hold", func() {
		const tid = "ret-e2e-hold"
		const jobID = "job-e2e-hold-1"
		const holdName = "e2e-hold-1"

		kit := newIntegKit(tid, nil)
		ctx, err := tenant.WithTenant(context.Background(), tid)
		Expect(err).NotTo(HaveOccurred())

		integSeedJob(tid, jobID)
		DeferCleanup(func() { integPurgeJob(tid, jobID) })

		// A tenant-scoped hold with wildcard age/job covers every candidate for
		// this tenant. Persisted through the real RLS-backed hold store.
		holds := retention.NewPgHoldStore(db.NewTenantPool(holdIntegPool))
		_, err = holds.Create(ctx, retention.Hold{
			Name:      holdName,
			CreatedBy: "retention-integ",
			Scope:     retention.HoldScope{TenantID: tid},
		})
		Expect(err).NotTo(HaveOccurred(), "create covering hold")
		DeferCleanup(func() { integDeleteHold(tid, holdName) })

		rep, err := kit.engine.Scan(ctx, retention.ScanOptions{Now: integScanTime()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Errors).To(BeEmpty())
		Expect(rep.Held).To(BeNumerically(">=", 1), "held candidate expected")
		Expect(rep.Purged).To(Equal(0), "held data must never be purged")

		Expect(integJobExists(tid, jobID)).To(BeTrue(), "held job must remain")
	})

	It("retains data and records an error when the archive backend corrupts the copy", func() {
		const tid = "ret-e2e-corrupt"
		const jobID = "job-e2e-corrupt-1"

		kit := newIntegKit(tid, func(_ storage.Provider) storage.Provider {
			return &corruptingProvider{}
		})
		ctx, err := tenant.WithTenant(context.Background(), tid)
		Expect(err).NotTo(HaveOccurred())

		integSeedJob(tid, jobID)
		DeferCleanup(func() { integPurgeJob(tid, jobID) })

		rep, err := kit.engine.Scan(ctx, retention.ScanOptions{Now: integScanTime()})
		Expect(err).NotTo(HaveOccurred(), "Scan returns no fatal error; per-candidate failures are non-fatal")
		Expect(rep.Purged).To(Equal(0), "a failed archive must block purge")
		Expect(rep.Errors).NotTo(BeEmpty(), "the archive-verify failure must be recorded")

		Expect(integJobExists(tid, jobID)).To(BeTrue(), "primary data must survive a failed archive")
	})

	It("does not touch another tenant's data when scoped to a different tenant", func() {
		const tenantA = "ret-e2e-iso-a"
		const tenantB = "ret-e2e-iso-b"
		const jobA = "job-e2e-iso-a-1"

		// Seed expired data under tenant A.
		integSeedJob(tenantA, jobA)
		DeferCleanup(func() { integPurgeJob(tenantA, jobA) })

		// Scan scoped to tenant B: RLS makes tenant A's completed job invisible to
		// the collector, so the whole engine (collect → purge) sees nothing.
		kitB := newIntegKit(tenantB, nil)
		ctxB, err := tenant.WithTenant(context.Background(), tenantB)
		Expect(err).NotTo(HaveOccurred())

		rep, err := kitB.engine.Scan(ctxB, retention.ScanOptions{Now: integScanTime()})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Errors).To(BeEmpty())
		Expect(rep.Scanned).To(Equal(0), "tenant B must not see tenant A's data")
		Expect(rep.Purged).To(Equal(0))

		// Tenant A's data is untouched.
		Expect(integJobExists(tenantA, jobA)).To(BeTrue(), "tenant A's data must survive tenant B's scan")
	})
})
