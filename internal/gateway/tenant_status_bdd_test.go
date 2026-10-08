//go:build !integration

package gateway_test

import (
	"context"
	"crypto/tls"
	"errors"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// fakeTenantStatus is a TenantStatusChecker with a fixed answer.
type fakeTenantStatus struct {
	active bool
	err    error
	calls  int
	got    string
}

func (f *fakeTenantStatus) TenantActive(_ context.Context, tenantID string) (bool, error) {
	f.calls++
	f.got = tenantID
	return f.active, f.err
}

// fixedIdentity authenticates every mTLS request as one identity.
type fixedIdentity struct{ id *authn.Identity }

func (a fixedIdentity) Authenticate(context.Context, *authn.Request) (*authn.Identity, error) {
	return a.id, nil
}
func (fixedIdentity) SupportedMethods() []authn.AuthMethod {
	return []authn.AuthMethod{authn.AuthMethodMTLS}
}

// fakeStreamConn is a non-health streaming handler connection; only Spec is used.
type fakeStreamConn struct{ connect.StreamingHandlerConn }

func (fakeStreamConn) Spec() connect.Spec { return connect.Spec{Procedure: "/test.v1.Test/Stream"} }

var _ = Describe("auth interceptor tenant status check", func() {
	var (
		tp      *telemetrytest.TestProvider
		svc     *gateway.Service
		checker *fakeTenantStatus
		reached bool
	)

	authFailureReasons := func() []string {
		m := telemetrytest.FindMetric(tp.GetMetrics(), "crosscodex.gateway.auth.failures.total")
		if m == nil {
			return nil
		}
		var reasons []string
		for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
			v, _ := dp.Attributes.Value("reason")
			reasons = append(reasons, v.AsString())
		}
		return reasons
	}

	BeforeEach(func() {
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		reg, err := authn.NewRegistry(nil, []authn.Authenticator{
			fixedIdentity{id: &authn.Identity{Subject: "alice", TenantID: "acme", Method: authn.AuthMethodMTLS}},
		})
		Expect(err).NotTo(HaveOccurred())
		svc = gateway.NewService(
			gateway.WithAuthn(reg),
			gateway.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")),
		)
		checker = &fakeTenantStatus{}
		reached = false
	})

	tlsCtx := func() context.Context {
		return gateway.ExportContextWithTLSState(context.Background(), &tls.ConnectionState{})
	}

	// callUnary runs a non-health unary request through the interceptor.
	// connect.NewRequest has an empty Spec, so its procedure is never Health.
	callUnary := func() error {
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			reached = true
			return nil, nil
		})
		_, err := gateway.ExportAuthInterceptor(svc, checker).WrapUnary(next)(tlsCtx(), connect.NewRequest(&pb.HealthRequest{}))
		return err
	}
	callStream := func() error {
		next := connect.StreamingHandlerFunc(func(context.Context, connect.StreamingHandlerConn) error {
			reached = true
			return nil
		})
		return gateway.ExportAuthInterceptor(svc, checker).WrapStreamingHandler(next)(tlsCtx(), fakeStreamConn{})
	}

	DescribeTable("lets an active tenant through",
		func(call func() error) {
			checker.active = true
			Expect(call()).To(Succeed())
			Expect(reached).To(BeTrue())
			Expect(checker.got).To(Equal("acme"))
			Expect(authFailureReasons()).To(BeEmpty())
		},
		Entry("unary", func() error { return callUnary() }),
		Entry("streaming", func() error { return callStream() }),
	)

	DescribeTable("refuses a suspended or unknown tenant with the operator's fix",
		func(call func() error) {
			checker.active = false
			err := call()
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
			Expect(err.Error()).To(ContainSubstring(`tenant "acme" is suspended or not provisioned`))
			Expect(err.Error()).To(ContainSubstring("crosscodexd admin tenant resume --tenant acme"))
			Expect(reached).To(BeFalse())
			Expect(authFailureReasons()).To(Equal([]string{"tenant_inactive"}))
		},
		Entry("unary", func() error { return callUnary() }),
		Entry("streaming", func() error { return callStream() }),
	)

	DescribeTable("fails closed without leaking detail when the check errors",
		func(call func() error) {
			checker.err = errors.New("dial tcp db-internal.example:5432: connection refused")
			err := call()
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).NotTo(ContainSubstring("db-internal"))
			Expect(reached).To(BeFalse())
			Expect(authFailureReasons()).To(Equal([]string{"tenant_status_error"}))
		},
		Entry("unary", func() error { return callUnary() }),
		Entry("streaming", func() error { return callStream() }),
	)
})

// scanRow is a db.Row that scans a fixed bool or fails.
type scanRow struct {
	active bool
	err    error
}

func (r scanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*bool) = r.active
	return nil
}

// statusConn is a db.Connection whose QueryRow answers tenant_is_active.
type statusConn struct {
	db.Connection
	row   scanRow
	query string
	args  []any
}

func (c *statusConn) QueryRow(_ context.Context, query string, args ...any) db.Row {
	c.query, c.args = query, args
	return c.row
}

var _ = Describe("NewPoolTenantStatus", func() {
	It("asks tenant_is_active for the tenant", func() {
		conn := &statusConn{row: scanRow{active: true}}
		active, err := gateway.NewPoolTenantStatus(conn).TenantActive(context.Background(), "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(BeTrue())
		Expect(conn.query).To(ContainSubstring("tenant_is_active"))
		Expect(conn.args).To(Equal([]any{"acme"}))
	})

	It("returns the query error", func() {
		conn := &statusConn{row: scanRow{err: errors.New("boom")}}
		_, err := gateway.NewPoolTenantStatus(conn).TenantActive(context.Background(), "acme")
		Expect(err).To(MatchError(ContainSubstring("boom")))
	})
})
