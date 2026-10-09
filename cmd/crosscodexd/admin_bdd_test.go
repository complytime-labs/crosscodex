//go:build !integration

package main

import (
	"bytes"
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("runAdmin arg parsing", func() {
	It("rejects a non-retention subcommand", func() {
		Expect(runAdmin([]string{"bogus"})).To(Equal(2))
	})

	It("rejects retention without scan", func() {
		Expect(runAdmin([]string{"retention"})).To(Equal(2))
	})

	It("requires a non-empty --tenant value", func() {
		Expect(runAdmin([]string{"retention", "scan", "--tenant", ""})).To(Equal(2))
	})

	It("rejects an unknown flag", func() {
		Expect(runAdmin([]string{"retention", "scan", "--nope"})).To(Equal(2))
	})
})

var _ = Describe("runAdmin retention scan wiring", func() {
	var (
		gotTenant string
		gotDryRun bool
		origFn    func(context.Context, *config.Config) (retentionScanner, func(), error)
	)

	// openWith installs scan as the scanner the one-shot opens.
	openWith := func(scan retentionScanner) {
		openRetentionScanFn = func(context.Context, *config.Config) (retentionScanner, func(), error) {
			return scan, func() {}, nil
		}
	}

	BeforeEach(func() {
		origFn = openRetentionScanFn
		gotTenant = ""
		gotDryRun = false
		openWith(func(ctx context.Context, tenantID string, opts retention.ScanOptions) (retention.Report, error) {
			// The scan ctx must carry the tenant that runRetentionScan set via
			// tenant.WithTenant, so the RLS-scoped collectors resolve it.
			t, err := tenant.FromContext(ctx)
			Expect(err).NotTo(HaveOccurred(), "scan ctx must carry the tenant set by tenant.WithTenant")
			gotTenant = t
			gotDryRun = opts.DryRun
			return retention.Report{}, nil
		})
		// Point config.NewLoader().Load at an empty XDG dir so it resolves to
		// built-in defaults, keeping this wiring test free of any DB or on-disk
		// config.
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	AfterEach(func() {
		openRetentionScanFn = origFn
	})

	It("scopes the scan ctx to --tenant and passes --dry-run through", func() {
		Expect(runAdmin([]string{"retention", "scan", "--tenant", "acme", "--dry-run"})).To(Equal(0))
		Expect(gotTenant).To(Equal("acme"))
		Expect(gotDryRun).To(BeTrue())
	})

	It("defaults --dry-run to false", func() {
		Expect(runAdmin([]string{"retention", "scan", "--tenant", "acme"})).To(Equal(0))
		Expect(gotTenant).To(Equal("acme"))
		Expect(gotDryRun).To(BeFalse())
	})

	It("returns 1 when the scan fails", func() {
		openWith(func(context.Context, string, retention.ScanOptions) (retention.Report, error) {
			return retention.Report{}, errors.New("boom")
		})
		Expect(runAdmin([]string{"retention", "scan", "--tenant", "acme"})).To(Equal(1))
	})

	It("rejects a malformed --tenant before opening any resource", func() {
		opened := false
		openRetentionScanFn = func(context.Context, *config.Config) (retentionScanner, func(), error) {
			opened = true
			return nil, nil, errors.New("must not open")
		}
		Expect(runAdmin([]string{"retention", "scan", "--tenant", "Bad_Tenant"})).To(Equal(1))
		Expect(opened).To(BeFalse(), "an invalid tenant must fail before any pool is opened")
	})
})

var _ = Describe("runAdmin reconcile artifacts arg parsing", func() {
	DescribeTable("returns 2 for usage errors",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(2))
		},
		Entry("reconcile without a target", "reconcile"),
		Entry("unknown reconcile target", "reconcile", "bogus", "--tenant", "acme"),
		Entry("empty --tenant", "reconcile", "artifacts", "--tenant", ""),
		Entry("unknown flag", "reconcile", "artifacts", "--tenant", "acme", "--nope"),
		Entry("zero --max-edges", "reconcile", "artifacts", "--tenant", "acme", "--max-edges", "0"),
		Entry("negative --max-edges", "reconcile", "artifacts", "--tenant", "acme", "--max-edges", "-1"),
	)
})

var _ = Describe("writeReconcileReport", func() {
	It("prints every metric, including closed, with its value", func() {
		var buf bytes.Buffer
		writeReconcileReport(&buf, artifacts.ReconcileResult{Artifacts: 1, Skipped: 2, Groups: 3, NewGroups: 4,
			NewMemberships: 5, Matches: 6, Closed: 7, NewMatches: 8})
		Expect(buf.String()).To(MatchRegexp(`^METRIC +VALUE\n`))
		for metric, value := range map[string]int{"artifacts": 1, "skipped": 2, "groups": 3, "new_groups": 4,
			"new_memberships": 5, "matches": 6, "closed": 7, "new_matches": 8} {
			Expect(buf.String()).To(MatchRegexp(`(?m)^%s +%d$`, metric, value))
		}
	})
})

var _ = Describe("runAdmin reconcile artifacts wiring", func() {
	var (
		gotTenant string
		gotOpts   artifacts.ReconcileOptions
		fnErr     error
		opened    bool
		origFn    func(context.Context, *config.Config) (artifactReconciler, func(), error)
	)

	BeforeEach(func() {
		origFn = openReconcileArtifactsFn
		gotTenant, gotOpts, fnErr, opened = "", artifacts.ReconcileOptions{}, nil, false
		openReconcileArtifactsFn = func(context.Context, *config.Config) (artifactReconciler, func(), error) {
			opened = true
			return func(ctx context.Context, tenantID string, opts artifacts.ReconcileOptions) (artifacts.ReconcileResult, error) {
				t, err := tenant.FromContext(ctx)
				Expect(err).NotTo(HaveOccurred(), "reconcile ctx must carry the tenant set by tenant.WithTenant")
				Expect(t).To(Equal(tenantID))
				gotTenant, gotOpts = tenantID, opts
				return artifacts.ReconcileResult{}, fnErr
			}, func() {}, nil
		}
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	AfterEach(func() {
		openReconcileArtifactsFn = origFn
	})

	It("passes the tenant and defaults through", func() {
		Expect(runAdmin([]string{"reconcile", "artifacts", "--tenant", "acme"})).To(Equal(0))
		Expect(gotTenant).To(Equal("acme"))
		Expect(gotOpts.DryRun).To(BeFalse())
		Expect(gotOpts.MaxEdges).To(Equal(artifacts.DefaultReconcileMaxEdges))
		Expect(gotOpts.ChunkSize).To(Equal(config.DefaultGraphMaxBulkEdges))
		Expect(gotOpts.Now).NotTo(BeZero())
	})

	It("passes --dry-run and --max-edges through", func() {
		Expect(runAdmin([]string{"reconcile", "artifacts", "--tenant", "acme", "--dry-run", "--max-edges", "7"})).To(Equal(0))
		Expect(gotOpts.DryRun).To(BeTrue())
		Expect(gotOpts.MaxEdges).To(Equal(7))
	})

	It("returns 1 when the reconcile fails", func() {
		fnErr = errors.New("boom")
		Expect(runAdmin([]string{"reconcile", "artifacts", "--tenant", "acme"})).To(Equal(1))
	})

	It("rejects a malformed --tenant before opening the graph connection", func() {
		Expect(runAdmin([]string{"reconcile", "artifacts", "--tenant", "Not_Valid"})).To(Equal(1))
		Expect(opened).To(BeFalse())
		Expect(gotTenant).To(BeEmpty())
	})
})
