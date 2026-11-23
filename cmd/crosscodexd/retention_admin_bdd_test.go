//go:build !integration

package main

import (
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/retention/retentiontest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// purgePoolSentinel stands in for the dedicated purge_user pool:
// buildRetentionAdmin only tests it for nil and never calls it.
type purgePoolSentinel struct{ dbpkg.Pool }

var _ = Describe("buildRetentionAdmin purge selection", func() {
	var (
		built  int
		wiring retentionWiring
		ctx    context.Context
	)

	BeforeEach(func() {
		built = 0
		policy, err := retention.NewPolicy(config.RetentionConfig{})
		Expect(err).NotTo(HaveOccurred())
		holds := &retentiontest.Fake{}
		wiring = retentionWiring{
			engineFor: func(_ string, p retention.Policy) (*retention.Engine, error) {
				built++
				return retention.NewEngine(nil, holds, nil, nil, nil, p, nil), nil
			},
			defaultPolicy: policy,
			holds:         holds,
		}
		ctx, err = tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		ctx = authn.WithIdentity(ctx, &authn.Identity{Subject: "admin-user", TenantID: "tenant-a", Roles: []string{authn.RoleAdmin}})
	})

	scan := func(dryRun bool) error {
		_, err := buildRetentionAdmin(&config.Config{}, &sharedResources{}, wiring).ScanRetention(ctx,
			connect.NewRequest(&pb.ScanRetentionRequest{TenantContext: &pb.TenantContext{TenantId: "tenant-a"}, DryRun: dryRun}))
		return err
	}
	apply := func(dryRun bool) error {
		_, err := buildRetentionAdmin(&config.Config{}, &sharedResources{}, wiring).ApplyRetentionPolicy(ctx,
			connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"}, Policy: &pb.RetentionPolicy{JobResults: "30d"}, DryRun: dryRun,
			}))
		return err
	}

	Context("without a dedicated purge pool (database.purge_dsn unset)", func() {
		DescribeTable("refuses a non-dry-run call up front, naming database.purge_dsn, and builds no engine",
			func(call func(bool) error) {
				err := call(false)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
				Expect(err).To(MatchError(ContainSubstring("database.purge_dsn is unset")))
				Expect(err).To(MatchError(ContainSubstring("purge_user")))
				Expect(built).To(BeZero(), "no engine, so nothing was archived")
			},
			Entry("ScanRetention", scan),
			Entry("ApplyRetentionPolicy", apply),
		)

		DescribeTable("still serves a dry run",
			func(call func(bool) error) {
				Expect(call(true)).To(Succeed())
				Expect(built).To(Equal(1))
			},
			Entry("ScanRetention", scan),
			Entry("ApplyRetentionPolicy", apply),
		)
	})

	Context("with a dedicated purge pool", func() {
		It("does not refuse a non-dry-run call before building an engine", func() {
			wiring.purgePool = purgePoolSentinel{}
			// The engine refuses a non-dry-run scan without a lock; that is
			// the engine's own error, not the up-front purge_dsn refusal.
			err := scan(false)
			Expect(built).To(Equal(1), "the call reached engine construction")
			if err != nil {
				Expect(err).NotTo(MatchError(ContainSubstring("database.purge_dsn is unset")))
			}
		})
	})
})
