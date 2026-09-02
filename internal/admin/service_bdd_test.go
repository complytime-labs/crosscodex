package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestAdmin(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Admin Service Suite")
}

// stubHoldStore is a minimal in-memory retention.HoldStore for handler tests.
type stubHoldStore struct {
	active       []retention.Hold
	all          []retention.Hold
	createErr    error
	releaseErr   error
	releasedName string
	releasedBy   string
	createdHold  retention.Hold
}

func (s *stubHoldStore) Create(_ context.Context, h retention.Hold) (retention.Hold, error) {
	if s.createErr != nil {
		return retention.Hold{}, s.createErr
	}
	s.createdHold = h
	h.ID = "hold-1"
	h.CreatedAt = time.Unix(1000, 0).UTC()
	return h, nil
}

func (s *stubHoldStore) Release(_ context.Context, name, releasedBy string) error {
	if s.releaseErr != nil {
		return s.releaseErr
	}
	s.releasedName = name
	s.releasedBy = releasedBy
	return nil
}

func (s *stubHoldStore) List(_ context.Context) ([]retention.Hold, error) {
	return s.all, nil
}

func (s *stubHoldStore) Active(_ context.Context, _ time.Time) ([]retention.Hold, error) {
	return s.active, nil
}

// stubDriftSource is a minimal audit-stream drift source for handler tests.
type stubDriftSource struct {
	drift []retention.StreamDrift
	err   error
}

func (s *stubDriftSource) source(_ context.Context) ([]retention.StreamDrift, error) {
	return s.drift, s.err
}

func newTestService(holds retention.HoldStore, drift *stubDriftSource) *Service {
	engineFor := func(_ string, p retention.Policy) (*retention.Engine, error) {
		return retention.NewEngine(nil, holds, nil, nil, nil, p), nil
	}
	policy, err := retention.NewPolicy(config.RetentionConfig{})
	Expect(err).NotTo(HaveOccurred())
	return NewService(engineFor, policy, holds, drift.source)
}

// adminCtx returns a context carrying an authenticated identity with the given
// roles and a matching tenant, mirroring what the gateway auth interceptor sets.
func adminCtx(tenantID string, roles ...string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	return authn.WithIdentity(ctx, &authn.Identity{
		Subject:  "admin-user",
		TenantID: tenantID,
		Roles:    roles,
	})
}

var _ = Describe("Admin retention RPCs RBAC", func() {
	var holds *stubHoldStore
	var drift *stubDriftSource
	var svc *Service

	BeforeEach(func() {
		holds = &stubHoldStore{}
		drift = &stubDriftSource{}
		svc = newTestService(holds, drift)
	})

	Describe("ScanRetention", func() {
		It("rejects a reader with PermissionDenied", func() {
			ctx := adminCtx("tenant-a", "reader")
			resp, err := svc.ScanRetention(ctx, connect.NewRequest(&pb.ScanRetentionRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("rejects an unauthenticated caller with Unauthenticated", func() {
			resp, err := svc.ScanRetention(context.Background(), connect.NewRequest(&pb.ScanRetentionRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("allows an admin and returns a report", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.ScanRetention(ctx, connect.NewRequest(&pb.ScanRetentionRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				DryRun:        true,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetReport()).NotTo(BeNil())
		})

		It("rejects a mismatched tenant with PermissionDenied", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.ScanRetention(ctx, connect.NewRequest(&pb.ScanRetentionRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-b"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})
	})

	Describe("GetRetentionStats", func() {
		It("returns eligible report and active hold count for an admin", func() {
			holds.active = []retention.Hold{{Name: "h1"}, {Name: "h2"}}
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.GetRetentionStats(ctx, connect.NewRequest(&pb.GetRetentionStatsRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetEligible()).NotTo(BeNil())
			Expect(resp.Msg.GetActiveHolds()).To(Equal(int64(2)))
		})

		It("surfaces audit-stream retention drift", func() {
			drift.drift = []retention.StreamDrift{
				{Stream: "AUDIT_LLM", Configured: 90 * 24 * time.Hour, Live: 30 * 24 * time.Hour, OK: false},
				{Stream: "AUDIT_EVENTS", Configured: 30 * 24 * time.Hour, Live: 30 * 24 * time.Hour, OK: true},
			}
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.GetRetentionStats(ctx, connect.NewRequest(&pb.GetRetentionStatsRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetStreamDrifts()).To(HaveLen(2))
			Expect(resp.Msg.GetStreamDrifts()[0].GetStream()).To(Equal("AUDIT_LLM"))
			Expect(resp.Msg.GetStreamDrifts()[0].GetOk()).To(BeFalse())
			Expect(resp.Msg.GetStreamDrifts()[0].GetConfigured().AsDuration()).To(Equal(90 * 24 * time.Hour))
			Expect(resp.Msg.GetStreamDrifts()[0].GetLive().AsDuration()).To(Equal(30 * 24 * time.Hour))
			Expect(resp.Msg.GetStreamDrifts()[1].GetOk()).To(BeTrue())
		})

		It("fails with Internal when the drift source errors", func() {
			drift.err = errors.New("stream unavailable")
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.GetRetentionStats(ctx, connect.NewRequest(&pb.GetRetentionStatsRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		})
	})

	Describe("ApplyRetentionPolicy", func() {
		It("rejects a nil policy with InvalidArgument", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.ApplyRetentionPolicy(ctx, connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects an invalid tier string with InvalidArgument", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.ApplyRetentionPolicy(ctx, connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				Policy:        &pb.RetentionPolicy{JobResults: "not-a-duration"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("runs a scan with the supplied policy for an admin", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.ApplyRetentionPolicy(ctx, connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				Policy:        &pb.RetentionPolicy{JobResults: "30d"},
				DryRun:        true,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetReport()).NotTo(BeNil())
		})
	})

	Describe("hold CRUD", func() {
		It("creates a hold stamped with the caller subject", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.CreateHold(ctx, connect.NewRequest(&pb.CreateHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				Name:          "audit-hold",
				Scope:         &pb.HoldScope{TenantId: "tenant-a"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetHold().GetName()).To(Equal("audit-hold"))
			Expect(holds.createdHold.CreatedBy).To(Equal("admin-user"))
		})

		It("forces the hold scope to the resolved tenant, ignoring a caller-supplied tenant", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.CreateHold(ctx, connect.NewRequest(&pb.CreateHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				Name:          "cross-tenant-hold",
				Scope:         &pb.HoldScope{TenantId: "tenant-b"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetHold().GetScope().GetTenantId()).To(Equal("tenant-a"))
			Expect(holds.createdHold.Scope.TenantID).To(Equal("tenant-a"))
		})

		It("rejects a create with an empty name", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.CreateHold(ctx, connect.NewRequest(&pb.CreateHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("releases a hold by name stamped with the caller subject", func() {
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			_, err := svc.ReleaseHold(ctx, connect.NewRequest(&pb.ReleaseHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				Name:          "audit-hold",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(holds.releasedName).To(Equal("audit-hold"))
			Expect(holds.releasedBy).To(Equal("admin-user"))
		})

		It("lists holds mapped to proto", func() {
			holds.all = []retention.Hold{{ID: "1", Name: "h1", CreatedAt: time.Unix(1, 0).UTC()}}
			ctx := adminCtx("tenant-a", authn.RoleAdmin)
			resp, err := svc.ListHolds(ctx, connect.NewRequest(&pb.ListHoldsRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetHolds()).To(HaveLen(1))
			Expect(resp.Msg.GetHolds()[0].GetName()).To(Equal("h1"))
		})

		It("rejects a non-admin from creating a hold", func() {
			ctx := adminCtx("tenant-a", "reader")
			_, err := svc.CreateHold(ctx, connect.NewRequest(&pb.CreateHoldRequest{
				TenantContext: &pb.TenantContext{TenantId: "tenant-a"},
				Name:          "x",
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})
	})
})

var _ = Describe("Admin mappers", func() {
	It("maps a retention.Report to proto with per-class counts", func() {
		rep := retention.Report{
			Scanned:  5,
			Held:     1,
			Archived: 2,
			Purged:   2,
			PerClass: map[retention.DataClass]retention.ClassCount{
				retention.ClassJobResults: {Scanned: 5, Held: 1, Archived: 2, Purged: 2},
			},
			Errors: []string{"boom"},
		}
		p := reportToProto(rep)
		Expect(p.GetScanned()).To(Equal(int64(5)))
		Expect(p.GetHeld()).To(Equal(int64(1)))
		Expect(p.GetArchived()).To(Equal(int64(2)))
		Expect(p.GetPurged()).To(Equal(int64(2)))
		Expect(p.GetErrors()).To(Equal([]string{"boom"}))
		cc := p.GetPerClass()["job_results"]
		Expect(cc).NotTo(BeNil())
		Expect(cc.GetScanned()).To(Equal(int64(5)))
		Expect(cc.GetPurged()).To(Equal(int64(2)))
	})

	It("round-trips a HoldScope through proto", func() {
		before := time.Unix(5000, 0).UTC()
		scope := retention.HoldScope{CreatedBefore: &before, TenantID: "tenant-a", JobID: "job-9"}
		got := holdScopeFromProto(holdScopeToProto(scope))
		Expect(got.TenantID).To(Equal("tenant-a"))
		Expect(got.JobID).To(Equal("job-9"))
		Expect(got.CreatedBefore).NotTo(BeNil())
		Expect(got.CreatedBefore.Equal(before)).To(BeTrue())
	})

	It("round-trips a Hold through proto", func() {
		expires := time.Unix(9000, 0).UTC()
		released := time.Unix(9500, 0).UTC()
		h := retention.Hold{
			ID:         "h-1",
			Name:       "legal",
			Scope:      retention.HoldScope{TenantID: "tenant-a"},
			CreatedAt:  time.Unix(1000, 0).UTC(),
			CreatedBy:  "alice",
			ExpiresAt:  &expires,
			ReleasedAt: &released,
			ReleasedBy: "bob",
		}
		got := holdFromProto(holdToProto(h))
		Expect(got.ID).To(Equal("h-1"))
		Expect(got.Name).To(Equal("legal"))
		Expect(got.Scope.TenantID).To(Equal("tenant-a"))
		Expect(got.CreatedBy).To(Equal("alice"))
		Expect(got.ReleasedBy).To(Equal("bob"))
		Expect(got.CreatedAt.Equal(h.CreatedAt)).To(BeTrue())
		Expect(got.ExpiresAt).NotTo(BeNil())
		Expect(got.ExpiresAt.Equal(expires)).To(BeTrue())
		Expect(got.ReleasedAt).NotTo(BeNil())
		Expect(got.ReleasedAt.Equal(released)).To(BeTrue())
	})

	It("maps a nil HoldScope to the zero value", func() {
		Expect(holdScopeFromProto(nil)).To(Equal(retention.HoldScope{}))
	})

	It("leaves optional Hold timestamps nil when unset", func() {
		h := retention.Hold{ID: "h", Name: "n", CreatedAt: time.Unix(1, 0).UTC()}
		p := holdToProto(h)
		Expect(p.ExpiresAt).To(BeNil())
		Expect(p.ReleasedAt).To(BeNil())
	})
})
