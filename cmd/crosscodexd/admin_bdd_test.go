//go:build !integration

package main

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

	It("requires --tenant", func() {
		Expect(runAdmin([]string{"retention", "scan"})).To(Equal(2))
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
		origFn    func(context.Context, *config.Config, string, retention.ScanOptions) (retention.Report, error)
	)

	BeforeEach(func() {
		origFn = runRetentionScanFn
		gotTenant = ""
		gotDryRun = false
		runRetentionScanFn = func(ctx context.Context, _ *config.Config, tenantID string, opts retention.ScanOptions) (retention.Report, error) {
			// The scan ctx must carry the tenant that runRetentionScan set via
			// tenant.WithTenant, so the RLS-scoped collectors resolve it.
			t, err := tenant.FromContext(ctx)
			Expect(err).NotTo(HaveOccurred(), "scan ctx must carry the tenant set by tenant.WithTenant")
			gotTenant = t
			gotDryRun = opts.DryRun
			return retention.Report{}, nil
		}
		// Point config.NewLoader().Load at an empty XDG dir so it resolves to
		// built-in defaults, keeping this wiring test free of any DB or on-disk
		// config.
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	AfterEach(func() {
		runRetentionScanFn = origFn
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
		runRetentionScanFn = func(context.Context, *config.Config, string, retention.ScanOptions) (retention.Report, error) {
			return retention.Report{}, errors.New("boom")
		}
		Expect(runAdmin([]string{"retention", "scan", "--tenant", "acme"})).To(Equal(1))
	})

	It("rejects a malformed --tenant before running the scan", func() {
		called := false
		runRetentionScanFn = func(context.Context, *config.Config, string, retention.ScanOptions) (retention.Report, error) {
			called = true
			return retention.Report{}, nil
		}
		Expect(runAdmin([]string{"retention", "scan", "--tenant", "Bad_Tenant"})).To(Equal(1))
		Expect(called).To(BeFalse(), "an invalid tenant must fail before the scan runs")
	})
})
