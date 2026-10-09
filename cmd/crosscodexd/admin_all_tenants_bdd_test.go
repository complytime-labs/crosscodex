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
	)

	DescribeTable("names both flags when neither is given",
		func(args ...string) {
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring("--tenant or --all-tenants is required"))
		},
		Entry("retention scan", "retention", "scan"),
		Entry("reconcile artifacts", "reconcile", "artifacts"),
	)
})

var _ = Describe("runAdmin --all-tenants", func() {
	var (
		origList      func(context.Context, string) ([]dbpkg.TenantRecord, error)
		origScan      func(context.Context, *config.Config) (retentionScanner, func(), error)
		origReconcile func(context.Context, *config.Config) (artifactReconciler, func(), error)

		records   []dbpkg.TenantRecord
		listErr   error
		listDSN   string
		listCalls int
		openErr   error
		opens     int
		closes    int
		ran       []string
		dryRuns   []bool
		failFor   map[string]bool
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
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", testTenantAdminDSN)
	})

	AfterEach(func() {
		runTenantListFn, openRetentionScanFn, openReconcileArtifactsFn = origList, origScan, origReconcile
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
		Entry("retention scan", "retention", "scan", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
	)

	DescribeTable("opens the command's resources once for every tenant and closes them",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(0))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(opens).To(Equal(1))
			Expect(closes).To(Equal(1))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants"),
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
	)

	DescribeTable("passes --dry-run to every tenant",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(0))
			Expect(ran).To(Equal([]string{"acme", "initech"}))
			Expect(dryRuns).To(Equal([]bool{true, true}))
		},
		Entry("retention scan", "retention", "scan", "--all-tenants", "--dry-run"),
		Entry("reconcile artifacts", "reconcile", "artifacts", "--all-tenants", "--dry-run"),
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
		Expect(opens).To(BeZero())
		Expect(ran).To(BeEmpty())
	})

	It("exits 0 and says so when no tenant is active", func() {
		records = []dbpkg.TenantRecord{{ID: "globex", Status: dbpkg.TenantStatusSuspended}}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"retention", "scan", "--all-tenants"}) })
		Expect(code).To(Equal(0))
		Expect(stderr).To(ContainSubstring("admin retention scan: no active tenants"))
		Expect(opens).To(BeZero())
		Expect(ran).To(BeEmpty())
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
	)
})
