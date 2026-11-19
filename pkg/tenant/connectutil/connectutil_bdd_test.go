package connectutil_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
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

// peerContext returns a context whose TLS state carries a client certificate
// with the given CN and organization, as authn.TLSStateMiddleware would.
func peerContext(cn, org string) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn, Organization: []string{org}}}
	return authn.WithTLSState(context.Background(), &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}})
}

var _ = Describe("UnaryServerInterceptor", func() {
	gateway := []authn.X509Match{{CN: "client", Organization: "CrossCodex"}}
	serviceCtx := func() context.Context { return peerContext("client", "CrossCodex") }

	It("accepts tenant headers from a listed service identity and injects the tenant", func() {
		interceptor := connectutil.UnaryServerInterceptor(gateway)
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")

		var captured string
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			id, err := tenant.FromContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			captured = id
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(serviceCtx(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).To(Equal("acme-corp"))
	})

	It("injects the user when the header is present", func() {
		interceptor := connectutil.UnaryServerInterceptor(gateway)
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")
		req.Header().Set(connectutil.MetadataKeyUserID, "alice")

		var captured string
		next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			captured = tenant.UserFromContext(ctx)
			return connect.NewResponse(&pb.HealthCheckResponse{}), nil
		}

		_, err := interceptor.WrapUnary(next)(serviceCtx(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(captured).To(Equal("alice"))
	})

	DescribeTable("fails closed with Unauthenticated on a bad tenant header, without calling the handler",
		func(tenantHeader string) {
			interceptor := connectutil.UnaryServerInterceptor(gateway)
			req := connect.NewRequest(&pb.HealthRequest{})
			// "" means the header is absent (not set to an empty value), so the
			// missing-header and malformed-header paths are both exercised.
			if tenantHeader != "" {
				req.Header().Set(connectutil.MetadataKeyTenantID, tenantHeader)
			}

			called := false
			next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
				called = true
				return connect.NewResponse(&pb.HealthCheckResponse{}), nil
			}

			_, err := interceptor.WrapUnary(next)(serviceCtx(), req)
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
			Expect(called).To(BeFalse())
		},
		Entry("the tenant header is missing", ""),
		Entry("the tenant header is malformed", "Not A Valid Tenant!!"),
	)

	DescribeTable("denies tenant headers from a peer that is not a service identity, without calling the handler",
		func(ctx context.Context, identities []authn.X509Match, wantCode connect.Code, wantMsg string) {
			interceptor := connectutil.UnaryServerInterceptor(identities)
			req := connect.NewRequest(&pb.HealthRequest{})
			// A valid tenant header: the denial must come from the identity check.
			req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")

			called := false
			next := func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
				called = true
				return connect.NewResponse(&pb.HealthCheckResponse{}), nil
			}

			_, err := interceptor.WrapUnary(next)(ctx, req)
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(wantCode))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			Expect(called).To(BeFalse())
		},
		Entry("the certificate matches no listed identity",
			peerContext("mallory", "Elsewhere"), gateway, connect.CodePermissionDenied,
			"the client certificate CN=mallory is not listed in tls.service_identities"),
		Entry("the certificate matches only the CN of an identity that also requires an organization",
			peerContext("client", "Elsewhere"), gateway, connect.CodePermissionDenied,
			"CN=client is not listed in tls.service_identities"),
		Entry("no identities are configured",
			peerContext("client", "CrossCodex"), nil, connect.CodePermissionDenied,
			"tls.service_identities"),
		Entry("the request carries no TLS state",
			context.Background(), gateway, connect.CodeUnauthenticated,
			"TLS client certificate"),
		Entry("the TLS state has no peer certificate",
			authn.WithTLSState(context.Background(), &tls.ConnectionState{}), gateway, connect.CodeUnauthenticated,
			"TLS client certificate"),
	)

	It("rejects streaming RPCs without calling the handler", func() {
		called := false
		handler := connectutil.UnaryServerInterceptor(gateway).WrapStreamingHandler(
			func(context.Context, connect.StreamingHandlerConn) error {
				called = true
				return nil
			})

		err := handler(serviceCtx(), nil)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
		Expect(called).To(BeFalse())
	})
})
