//go:build !integration

package main

import (
	"context"
	"errors"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("runAdmin --all-tenants arg parsing", func() {
	DescribeTable("returns 2 when --tenant and --all-tenants are combined",
		func(args ...string) {
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring("--tenant and --all-tenants are mutually exclusive"))
		},
		Entry("retention scan", "retention", "scan", "--tenant", "acme", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--tenant", "acme", "--all-tenants"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--tenant", "acme", "--all-tenants"),
	)

	DescribeTable("names both flags when neither is given",
		func(args ...string) {
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring("--tenant or --all-tenants is required"))
		},
		Entry("retention scan", "retention", "scan"),
		Entry("reconcile artifacts", "reconcile", "artifacts"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts"),
	)
})

var _ = Describe("runAdmin --all-tenants", func() {
	var (
		origList       func(context.Context, string) ([]dbpkg.TenantRecord, error)
		origRetList    func(context.Context, string) ([]dbpkg.RetentionTenant, error)
		origScan       func(context.Context, *config.Config) (retentionScanner, func(), error)
		origReconcile  func(context.Context, *config.Config) (artifactReconciler, func(), error)
		origAdjudicate func(context.Context, *config.Config, bool) (artifactAdjudicator, func(), error)

		records []dbpkg.TenantRecord
		// retentionTenants overrides what list_retention_tenants returns. When
		// nil the shared fixture lists the non-suspended records, so the tables
		// shared with reconcile and adjudicate see the same two tenants; the
		// dedicated retention specs set it to cover suspended, deleted and hidden.
		retentionTenants []dbpkg.RetentionTenant
		listErr          error
		listDSN          string
		listCalls        int
		openErr          error
		opens            int
		closes           int
		ran              []string
		dryRuns          []bool
		failFor          map[string]bool
	)

	// record asserts the run ctx is scoped to tenantID, then records the run.
	record := func(ctx context.Context, tenantID string, dryRun bool) error {
		t, err := tenant.FromContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(t).To(Equal(tenantID))
		ran = append(ran, tenantID)
		dryRuns = append(dryRuns, dryRun)
		if failFor[tenantID] {
			return errors.New("boom")
		}
		return nil
	}

	BeforeEach(func() {
		origList, origScan, origReconcile = runTenantListFn, openRetentionScanFn, openReconcileArtifactsFn
		origAdjudicate, origRetList = openAdjudicateArtifactsFn, runRetentionTenantListFn
		retentionTenants = nil
		records = []dbpkg.TenantRecord{
			{ID: "acme", Status: dbpkg.TenantStatusActive},
			{ID: "globex", Status: dbpkg.TenantStatusSuspended},
			{ID: "initech", Status: dbpkg.TenantStatusActive},
		}
		listErr, listDSN, listCalls = nil, "", 0
		openErr, opens, closes = nil, 0, 0
		ran, dryRuns, failFor = nil, nil, map[string]bool{}

		runTenantListFn = func(_ context.Context, dsn string) ([]dbpkg.TenantRecord, error) {
			listCalls++
			listDSN = dsn
			return records, listErr
		}
		runRetentionTenantListFn = func(_ context.Context, dsn string) ([]dbpkg.RetentionTenant, error) {
			listCalls++
			listDSN = dsn
			if retentionTenants != nil || listErr != nil {
				return retentionTenants, listErr
			}
			var all []dbpkg.RetentionTenant
			for _, r := range records {
				if r.Status == dbpkg.TenantStatusSuspended {
					continue
				}
				all = append(all, dbpkg.RetentionTenant{ID: r.ID, DisplayID: r.ID, Status: r.Status})
			}
			return all, nil
		}
		openRetentionScanFn = func(context.Context, *config.Config) (retentionScanner, func(), error) {
			opens++
			if openErr != nil {
				return nil, nil, openErr
			}
			return func(ctx context.Context, tenantID string, opts retention.ScanOptions) (retention.Report, error) {
				return retention.Report{}, record(ctx, tenantID, opts.DryRun)
			}, func() { closes++ }, nil
		}
		openReconcileArtifactsFn = func(context.Context, *config.Config) (artifactReconciler, func(), error) {
			opens++
			if openErr != nil {
				return nil, nil, openErr
			}
			return func(ctx context.Context, tenantID string, opts artifacts.ReconcileOptions) (artifacts.ReconcileResult, error) {
				return artifacts.ReconcileResult{}, record(ctx, tenantID, opts.DryRun)
			}, func() { closes++ }, nil
		}
		openAdjudicateArtifactsFn = func(context.Context, *config.Config, bool) (artifactAdjudicator, func(), error) {
			opens++
			if openErr != nil {
				return nil, nil, openErr
			}
			return func(ctx context.Context, tenantID string, opts artifacts.AdjudicateOptions) (artifacts.AdjudicateResult, error) {
				return artifacts.AdjudicateResult{}, record(ctx, tenantID, opts.DryRun)
			}, func() { closes++ }, nil
		}
		writeAdjudicationConfig(true)
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", testTenantAdminDSN)
	})

	AfterEach(func() {
		runTenantListFn, openRetentionScanFn, openReconcileArtifactsFn = origList, origScan, origReconcile
		openAdjudicateArtifactsFn, runRetentionTenantListFn = origAdjudicate, origRetList
	})

	DescribeTable("runs once per active tenant, in list order, skipping suspended tenants",
		func(args ...string) {
			code, stdout := captureStream(&os.Stdout, func() int { return runAdmin(args) })
			Expect(code).To(Equal(0))
			Expect(listDSN).To(Equal(testTenantAdminDSN))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(stdout).To(MatchRegexp(`(?s)tenant: acme\n.*METRIC.*\n\ntenant: initech\n.*METRIC`))
			Expect(stdout).NotTo(ContainSubstring("globex"))
		},
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--all-tenants"),
	)

	It("retention scan runs every tenant, deleted and suspended included, showing the display ID", func() {
		retentionTenants = []dbpkg.RetentionTenant{
			{ID: "acme", DisplayID: "acme", Status: dbpkg.TenantStatusActive},
			{ID: "globex", DisplayID: "globex", Status: dbpkg.TenantStatusSuspended},
			{ID: "gone", DisplayID: "gone", Status: "deleted"},
			{ID: "secret-co", DisplayID: "hidden-tenant-0123456789abcdef", Status: "deleted"},
		}
		code, stdout := captureStream(&os.Stdout, func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(0))
		Expect(listDSN).To(Equal(testTenantAdminDSN))
		Expect(ran).To(Equal([]string{"acme", "globex", "gone", "secret-co"}))
		Expect(stdout).To(ContainSubstring("tenant: hidden-tenant-0123456789abcdef\n"))
		Expect(stdout).NotTo(ContainSubstring("secret-co"))
	})

	It("retention scan names a hidden tenant by its display ID in failures", func() {
		retentionTenants = []dbpkg.RetentionTenant{{ID: "secret-co", DisplayID: "hidden-tenant-0123456789abcdef", Status: "deleted"}}
		failFor["secret-co"] = true
		code, stderr := captureStderr(func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("tenant hidden-tenant-0123456789abcdef: boom"))
		Expect(stderr).To(ContainSubstring("1 of 1 tenants failed: hidden-tenant-0123456789abcdef"))
		Expect(stderr).NotTo(ContainSubstring("secret-co"))
	})

	It("reconcile and adjudicate never list retention tenants", func() {
		Expect(runAdmin([]string{"reconcile", "artifacts", "--all-tenants"})).To(Equal(0))
		Expect(runAdmin([]string{"adjudicate", "artifacts", "--all-tenants"})).To(Equal(0))
		Expect(ran).To(Equal([]string{"acme", "initech", "acme", "initech"}))
	})

	DescribeTable("opens the command's resources once for every tenant and closes them",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(0))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(opens).To(Equal(1))
			Expect(closes).To(Equal(1))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--all-tenants"),
	)

	DescribeTable("stops with one error when the resources cannot be opened",
		func(args ...string) {
			openErr = errors.New("connection refused")
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(1))
			Expect(opens).To(Equal(1))
			Expect(ran).To(BeEmpty())
			Expect(stderr).To(ContainSubstring(": connection refused"))
			Expect(stderr).NotTo(ContainSubstring("tenant acme"))
			Expect(stderr).NotTo(ContainSubstring("tenants failed"))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--all-tenants"),
	)

	DescribeTable("passes --dry-run to every tenant",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(0))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(dryRuns).To(Equal([]bool{true, true}))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants", "--dry-run"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants", "--dry-run"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--all-tenants", "--dry-run"),
	)

	DescribeTable("continues past a failing tenant and exits 1 naming it",
		func(args ...string) {
			failFor["acme"] = true
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(1))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(stderr).To(ContainSubstring("tenant acme: boom"))
			Expect(stderr).To(ContainSubstring("1 of 2 tenants failed: acme"))
			Expect(closes).To(Equal(1))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--all-tenants"),
	)

	DescribeTable("fails a malformed listed tenant without running it, quoting its ID",
		func(args ...string) {
			records = []dbpkg.TenantRecord{
				{ID: "bad\x1bid", Status: dbpkg.TenantStatusActive},
				{ID: "acme", Status: dbpkg.TenantStatusActive},
				{ID: "initech", Status: dbpkg.TenantStatusActive},
			}
			failFor["initech"] = true
			var stderr string
			code, stdout := captureStream(&os.Stdout, func() int {
				var code int
				code, stderr = captureStderr(func() int { return runAdmin(args) })
				return code
			})
			Expect(code).To(Equal(1))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(closes).To(Equal(1))
			Expect(stdout).To(ContainSubstring("tenant: \"bad\\x1bid\"\n"))
			Expect(stdout).NotTo(ContainSubstring("bad\x1bid"))
			Expect(stderr).To(ContainSubstring(`tenant "bad\x1bid": `))
			Expect(stderr).NotTo(ContainSubstring("bad\x1bid"))
			Expect(stderr).To(ContainSubstring(`2 of 3 tenants failed: "bad\x1bid", initech`))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--all-tenants"),
	)

	It("opens nothing when every listed tenant is malformed", func() {
		records = []dbpkg.TenantRecord{{ID: "Not_Valid", Status: dbpkg.TenantStatusActive}}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(1))
		Expect(opens).To(BeZero())
		Expect(stderr).To(ContainSubstring(`admin retention scan: tenant Not_Valid: tenant ID "Not_Valid" is invalid`))
		Expect(stderr).To(ContainSubstring("admin retention scan: 1 of 1 tenants failed: Not_Valid"))
	})

	It("refuses without tenant_admin_dsn and runs nothing", func() {
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "")
		code, stderr := captureStderr(func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin retention scan: database.tenant_admin_dsn is not set"))
		Expect(listCalls).To(BeZero())
		Expect(opens).To(BeZero())
		Expect(ran).To(BeEmpty())
	})

	It("exits 1 and runs nothing when listing tenants fails", func() {
		listErr = errors.New("connection refused")
		code, stderr := captureStderr(func() int { return runAdmin([]string{"reconcile", "artifacts", "--all-tenants"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin reconcile artifacts: list tenants: connection refused"))
		code, stderr = captureStderr(func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin retention scan: list tenants: connection refused"))
		Expect(opens).To(BeZero())
		Expect(ran).To(BeEmpty())
	})

	It("exits 0 and says so when no tenant is active", func() {
		records = []dbpkg.TenantRecord{{ID: "globex", Status: dbpkg.TenantStatusSuspended}}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"reconcile", "artifacts", "--all-tenants"}) })
		Expect(code).To(Equal(0))
		Expect(stderr).To(ContainSubstring("admin reconcile artifacts: no active tenants"))
		Expect(opens).To(BeZero())
		Expect(ran).To(BeEmpty())
	})

	It("retention scan exits 0 and says so when there is no tenant at all", func() {
		records = []dbpkg.TenantRecord{}
		retentionTenants = []dbpkg.RetentionTenant{}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(0))
		Expect(stderr).To(ContainSubstring("admin retention scan: no tenants"))
		Expect(opens).To(BeZero())
	})

	DescribeTable("keeps single-tenant output free of the tenant header",
		func(args ...string) {
			code, stdout := captureStream(&os.Stdout, func() int { return runAdmin(args) })
			Expect(code).To(Equal(0))
			Expect(ran).To(Equal([]string{"acme"}))
			Expect(stdout).To(ContainSubstring("METRIC"))
			Expect(stdout).NotTo(ContainSubstring("tenant:"))
			Expect(listCalls).To(BeZero())
		},
		Entry("retention scan", "retention", "scan", "--tenant", "acme"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--tenant", "acme"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--tenant", "acme"),
	)

	DescribeTable("closes the resources when a single tenant fails",
		func(args ...string) {
			failFor["acme"] = true
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring(": boom"))
			Expect(opens).To(Equal(1))
			Expect(closes).To(Equal(1))
		},
		Entry("retention scan", "retention", "scan", "--tenant", "acme"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--tenant", "acme"),
		Entry("adjudicate artifacts", "adjudicate", "artifacts", "--tenant", "acme"),
	)
})
