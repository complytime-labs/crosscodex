package connectutil_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

func TestConnectUtilBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ConnectUtil BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("UnaryClientInterceptor", func() {
	It("sets the tenant header from context", func() {
		interceptor := connectutil.UnaryClientInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})

		var captured string
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			captured = r.Header().Get(connectutil.MetadataKeyTenantID)
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())

		_, err = interceptor.WrapUnary(next)(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).To(Equal("acme-corp"))
	})

	It("sets the user header when present", func() {
		interceptor := connectutil.UnaryClientInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})

		var captured string
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			captured = r.Header().Get(connectutil.MetadataKeyUserID)
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())
		ctx = tenant.WithUser(ctx, "alice")

		_, err = interceptor.WrapUnary(next)(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).To(Equal("alice"))
	})

	It("fails closed when no tenant is present in context", func() {
		interceptor := connectutil.UnaryClientInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})

		called := false
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(called).To(BeFalse())
	})
})

var _ = Describe("UnaryServerInterceptor", func() {
	It("injects the tenant from the header into context", func() {
		interceptor := connectutil.UnaryServerInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")

		var captured string
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			id, err := tenant.FromContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			captured = id
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).To(Equal("acme-corp"))
	})

	It("injects the user when the header is present", func() {
		interceptor := connectutil.UnaryServerInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")
		req.Header().Set(connectutil.MetadataKeyUserID, "alice")

		var captured string
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			captured = tenant.UserFromContext(ctx)
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).To(Equal("alice"))
	})

	It("fails closed when the tenant header is missing", func() {
		interceptor := connectutil.UnaryServerInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})

		called := false
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		Expect(called).To(BeFalse())
	})

	It("fails closed when the tenant header is malformed", func() {
		interceptor := connectutil.UnaryServerInterceptor()
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "Not A Valid Tenant!!")

		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(context.Background(), req)
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})
