package cleaning_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	cleaningsvc "github.com/complytime-labs/crosscodex/internal/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// mappedSentinels is the binding engine-error mapping.
var mappedSentinels = []struct {
	err  error
	code connect.Code
}{
	{cleaning.ErrProjectionIncomplete, connect.CodeFailedPrecondition},
	{cleaning.ErrTenantMismatch, connect.CodePermissionDenied},
	{cleaning.ErrFindingNotFound, connect.CodeNotFound},
	{cleaning.ErrInvalidScope, connect.CodeInvalidArgument},
	{cleaning.ErrInvalidDecision, connect.CodeInvalidArgument},
	{cleaning.ErrVerdictNotInFinding, connect.CodeInvalidArgument},
	{cleaning.ErrRetireNeedsVerdicts, connect.CodeInvalidArgument},
	{cleaning.ErrTokenMismatch, connect.CodeFailedPrecondition},
	{cleaning.ErrStateChanged, connect.CodeFailedPrecondition},
	{cleaning.ErrUnprojected, connect.CodeFailedPrecondition},
	{cleaning.ErrSigningDisabled, connect.CodeFailedPrecondition},
	{cleaning.ErrInvalidTransition, connect.CodeFailedPrecondition},
	{cleaning.ErrEdgeNotLive, connect.CodeFailedPrecondition},
	{cleaning.ErrRetireCapExceeded, connect.CodeFailedPrecondition},
	{cleaning.ErrReviewerNotRegistered, connect.CodeFailedPrecondition},
	{cleaning.ErrReviewerDeleted, connect.CodeFailedPrecondition},
}

// propCtx is callerCtx without Gomega, for use inside rapid.Check.
func propCtx(t *rapid.T, tenantID string, roles ...string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("scope tenant %q: %v", tenantID, err)
	}
	return authn.WithIdentity(ctx, &authn.Identity{Subject: reviewer, TenantID: tenantID, Roles: roles})
}

func propMessage(t *rapid.T, err error) string {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a Connect error, got %v", err)
	}
	return ce.Message()
}

var _ = Describe("Property Specifications", Ordered, func() {
	allRPCs := []rpc{runPreview, listFindings, getUnknown, decideUnknown}
	both := []string{authn.RoleAdmin, authn.RoleReviewer}

	It("never passes driver text through a mapped sentinel's message", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			m := rapid.SampledFrom(mappedSentinels).Draw(t, "sentinel")
			driver := "driver-detail-" + rapid.StringMatching(`[ -~]{0,40}`).Draw(t, "driver")
			prefix := rapid.StringMatching(`[a-z ]{0,20}`).Draw(t, "prefix")
			stub := &stubEngine{err: fmt.Errorf("%s: %w: %w", prefix, m.err, errors.New(driver))}
			svc := cleaningsvc.NewService(stub)
			call := rapid.SampledFrom(allRPCs).Draw(t, "rpc")

			err := call(propCtx(t, tenantA, both...), svc, tenantA)
			if got := connect.CodeOf(err); got != m.code {
				t.Fatalf("code %v, want %v", got, m.code)
			}
			if msg := propMessage(t, err); msg != m.err.Error() || strings.Contains(msg, "driver-detail-") {
				t.Fatalf("message %q, want the sentinel's own text %q", msg, m.err.Error())
			}
		})
	})

	It("passes a mapped sentinel's message through when the engine wrote all of it", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			m := rapid.SampledFrom(mappedSentinels).Draw(t, "sentinel")
			err := m.err
			for i, n := 0, rapid.IntRange(1, 3).Draw(t, "depth"); i < n; i++ {
				err = fmt.Errorf("%s: %w", rapid.StringMatching(`[a-z ]{1,20}`).Draw(t, "context"), err)
			}
			stub := &stubEngine{err: err}
			svc := cleaningsvc.NewService(stub)
			call := rapid.SampledFrom(allRPCs).Draw(t, "rpc")

			got := call(propCtx(t, tenantA, both...), svc, tenantA)
			if code := connect.CodeOf(got); code != m.code {
				t.Fatalf("code %v, want %v", code, m.code)
			}
			if msg := propMessage(t, got); msg != err.Error() {
				t.Fatalf("message %q, want %q", msg, err.Error())
			}
		})
	})

	It("refuses any tenant_context other than the caller's before the engine, naming neither tenant", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			caller := rapid.StringMatching(`[a-z][a-z0-9-]{1,20}[a-z0-9]`).Draw(t, "caller")
			other := rapid.StringMatching(`[a-z][a-z0-9-]{1,20}[a-z0-9]`).Filter(func(s string) bool { return s != caller }).Draw(t, "other")
			stub := &stubEngine{}
			svc := cleaningsvc.NewService(stub)
			call := rapid.SampledFrom(allRPCs).Draw(t, "rpc")

			err := call(propCtx(t, caller, both...), svc, other)
			if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
				t.Fatalf("code %v, want PermissionDenied", code)
			}
			if msg := propMessage(t, err); strings.Contains(msg, caller) || strings.Contains(msg, other) {
				t.Fatalf("message %q names a tenant", msg)
			}
			if len(stub.tenants) != 0 {
				t.Fatalf("engine reached for tenants %v", stub.tenants)
			}
		})
	})

	It("passes well-formed retire_verdicts to the engine unchanged and refuses malformed ones before it", func() {
		part := rapid.StringMatching(`[A-Za-z0-9._()-]{1,30}`)
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			n := rapid.IntRange(1, 4).Draw(t, "n")
			refs := make([]*pb.VerdictRef, n)
			want := make([]cleaning.VerdictKey, n)
			for i := range refs {
				k := cleaning.VerdictKey{JobID: part.Draw(t, "job"), SourceID: part.Draw(t, "source"), TargetID: part.Draw(t, "target")}
				want[i] = k
				refs[i] = &pb.VerdictRef{JobId: k.JobID, SourceNodeId: k.SourceID, TargetNodeId: k.TargetID}
			}
			bad := rapid.IntRange(-1, n-1).Draw(t, "bad")
			if bad >= 0 {
				broken := rapid.SampledFrom([]string{"", "x:y"}).Draw(t, "broken")
				switch rapid.IntRange(0, 2).Draw(t, "field") {
				case 0:
					refs[bad].JobId = broken
				case 1:
					refs[bad].SourceNodeId = broken
				default:
					refs[bad].TargetNodeId = broken
				}
			}
			stub := &stubEngine{}
			svc := cleaningsvc.NewService(stub)

			_, err := svc.DecideFinding(propCtx(t, tenantA, authn.RoleReviewer), connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantA}, FindingId: "f1", Verdict: cleaning.VerdictRetire,
				Rationale: "r", ExpectStateToken: "s", RetireVerdicts: refs,
			}))
			if bad >= 0 {
				if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
					t.Fatalf("code %v, want InvalidArgument", code)
				}
				if msg := propMessage(t, err); !strings.Contains(msg, fmt.Sprintf("retire_verdicts[%d]", bad)) {
					t.Fatalf("message %q does not name entry %d", msg, bad)
				}
				if len(stub.decided) != 0 {
					t.Fatalf("engine reached with %v", stub.decided)
				}
				return
			}
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if len(stub.decided) != 1 || !slices.Equal(stub.decided[0].RetireVerdicts, want) {
				t.Fatalf("engine got %v, want %v", stub.decided, want)
			}
		})
	})
})
