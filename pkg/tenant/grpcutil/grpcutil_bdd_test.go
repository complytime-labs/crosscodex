package grpcutil_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/grpcutil"
)

func TestGrpcUtilBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GrpcUtil BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// stubHandler is a gRPC handler that captures the context for inspection.
func stubHandler(ctx context.Context, req any) (any, error) {
	return ctx, nil
}

var _ = Describe("UnaryServerInterceptor", func() {
	var interceptor grpc.UnaryServerInterceptor

	BeforeEach(func() {
		interceptor = grpcutil.UnaryServerInterceptor()
	})

	It("extracts a valid tenant from metadata", func() {
		md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "acme-corp")
		ctx := metadata.NewIncomingContext(context.Background(), md)

		resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).NotTo(HaveOccurred())

		resultCtx := resp.(context.Context)
		id, err := tenant.FromContext(resultCtx)
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal("acme-corp"))
	})

	It("extracts user ID from metadata", func() {
		md := metadata.Pairs(
			grpcutil.MetadataKeyTenantID, "acme-corp",
			grpcutil.MetadataKeyUserID, "alice",
		)
		ctx := metadata.NewIncomingContext(context.Background(), md)

		resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).NotTo(HaveOccurred())

		resultCtx := resp.(context.Context)
		Expect(tenant.UserFromContext(resultCtx)).To(Equal("alice"))
	})

	It("rejects requests with no metadata", func() {
		_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).To(HaveOccurred())

		st, ok := status.FromError(err)
		Expect(ok).To(BeTrue())
		Expect(st.Code()).To(Equal(codes.Unauthenticated))
	})

	It("rejects requests with empty tenant metadata", func() {
		md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "")
		ctx := metadata.NewIncomingContext(context.Background(), md)

		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).To(HaveOccurred())

		st, _ := status.FromError(err)
		Expect(st.Code()).To(Equal(codes.Unauthenticated))
	})

	It("rejects requests with malformed tenant ID", func() {
		md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "BAD!TENANT")
		ctx := metadata.NewIncomingContext(context.Background(), md)

		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).To(HaveOccurred())

		st, _ := status.FromError(err)
		Expect(st.Code()).To(Equal(codes.InvalidArgument))
	})

	It("rejects requests when metadata exists but tenant key is absent", func() {
		md := metadata.Pairs("x-other-key", "value")
		ctx := metadata.NewIncomingContext(context.Background(), md)

		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).To(HaveOccurred())

		st, _ := status.FromError(err)
		Expect(st.Code()).To(Equal(codes.Unauthenticated))
	})
})

var _ = Describe("UnaryClientInterceptor", func() {
	var interceptor grpc.UnaryClientInterceptor

	BeforeEach(func() {
		interceptor = grpcutil.UnaryClientInterceptor()
	})

	It("injects tenant ID into outgoing metadata", func() {
		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())

		var capturedCtx context.Context
		fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			capturedCtx = ctx
			return nil
		}

		err = interceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
		Expect(err).NotTo(HaveOccurred())

		md, ok := metadata.FromOutgoingContext(capturedCtx)
		Expect(ok).To(BeTrue())
		vals := md.Get(grpcutil.MetadataKeyTenantID)
		Expect(vals).To(HaveLen(1))
		Expect(vals[0]).To(Equal("acme-corp"))
	})

	It("injects user ID into outgoing metadata", func() {
		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())
		ctx = tenant.WithUser(ctx, "bob")

		var capturedCtx context.Context
		fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			capturedCtx = ctx
			return nil
		}

		err = interceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
		Expect(err).NotTo(HaveOccurred())

		md, _ := metadata.FromOutgoingContext(capturedCtx)
		vals := md.Get(grpcutil.MetadataKeyUserID)
		Expect(vals).To(HaveLen(1))
		Expect(vals[0]).To(Equal("bob"))
	})

	It("returns FailedPrecondition when no tenant in context", func() {
		fakeInvoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			Fail("invoker should not be called")
			return nil
		}

		err := interceptor(context.Background(), "/test.Service/Method", nil, nil, nil, fakeInvoker)
		Expect(err).To(HaveOccurred())

		st, _ := status.FromError(err)
		Expect(st.Code()).To(Equal(codes.FailedPrecondition))
	})

	It("preserves existing outgoing metadata", func() {
		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())

		md := metadata.Pairs("x-request-id", "abc123")
		ctx = metadata.NewOutgoingContext(ctx, md)

		var capturedCtx context.Context
		fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			capturedCtx = ctx
			return nil
		}

		err = interceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
		Expect(err).NotTo(HaveOccurred())

		outMD, _ := metadata.FromOutgoingContext(capturedCtx)
		Expect(outMD.Get(grpcutil.MetadataKeyTenantID)).To(ConsistOf("acme-corp"))
		Expect(outMD.Get("x-request-id")).To(ConsistOf("abc123"))
	})
})

var _ = Describe("Round-trip", func() {
	It("propagates tenant and user from client to server", func() {
		clientInterceptor := grpcutil.UnaryClientInterceptor()
		serverInterceptor := grpcutil.UnaryServerInterceptor()

		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())
		ctx = tenant.WithUser(ctx, "alice")

		var wireCtx context.Context
		fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			outMD, _ := metadata.FromOutgoingContext(ctx)
			wireCtx = metadata.NewIncomingContext(context.Background(), outMD)
			return nil
		}

		err = clientInterceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
		Expect(err).NotTo(HaveOccurred())

		resp, err := serverInterceptor(wireCtx, nil, &grpc.UnaryServerInfo{}, stubHandler)
		Expect(err).NotTo(HaveOccurred())

		resultCtx := resp.(context.Context)
		id, err := tenant.FromContext(resultCtx)
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal("acme-corp"))
		Expect(tenant.UserFromContext(resultCtx)).To(Equal("alice"))
	})
})
