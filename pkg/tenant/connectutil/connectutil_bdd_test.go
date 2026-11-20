package connectutil_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
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

// tenantCtx returns a context carrying the tenant acme-corp.
func tenantCtx() context.Context {
	ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

// captureHeaders returns a client-side next that stores the outgoing request's
// headers in *dst; *dst stays nil when the request is never sent.
func captureHeaders(dst *http.Header) connect.UnaryFunc {
	return func(_ context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		*dst = r.Header().Clone()
		return connect.NewResponse(&pb.HealthCheckResponse{}), nil
	}
}

// recorder is a server-side handler that records what the interceptor put in
// its context.
type recorder struct {
	calls    int
	tenant   string
	user     string
	identity *authn.Identity
}

func (r *recorder) handle(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
	r.calls++
	r.tenant, _ = tenant.FromContext(ctx)
	r.user = tenant.UserFromContext(ctx)
	r.identity = authn.IdentityFromContext(ctx)
	return connect.NewResponse(&pb.HealthCheckResponse{}), nil
}

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

	It("sets the user and roles headers from the identity in context", func() {
		var captured http.Header
		ctx := tenantCtx()
		ctx = authn.WithIdentity(ctx, &authn.Identity{Subject: "alice", TenantID: "acme-corp", Roles: []string{"admin", "reviewer"}, Method: authn.AuthMethodMTLS})
		ctx = tenant.WithUser(ctx, "alice")

		_, err := connectutil.UnaryClientInterceptor().WrapUnary(captureHeaders(&captured))(ctx, connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(captured.Values(connectutil.MetadataKeyTenantID)).To(Equal([]string{"acme-corp"}))
		Expect(captured.Values(connectutil.MetadataKeyUserID)).To(Equal([]string{"alice"}))
		Expect(captured.Values(connectutil.MetadataKeyUserRoles)).To(Equal([]string{"admin,reviewer"}))
	})

	It("sends only the tenant header when the context has neither identity nor user", func() {
		var captured http.Header
		_, err := connectutil.UnaryClientInterceptor().WrapUnary(captureHeaders(&captured))(tenantCtx(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(captured.Values(connectutil.MetadataKeyTenantID)).To(Equal([]string{"acme-corp"}))
		Expect(captured.Values(connectutil.MetadataKeyUserID)).To(BeEmpty())
		Expect(captured.Values(connectutil.MetadataKeyUserRoles)).To(BeEmpty())
	})

	DescribeTable("fails closed with FailedPrecondition when the user and identity disagree, without sending the request",
		func(identity *authn.Identity, user string, wantMsg string) {
			ctx := tenantCtx()
			if identity != nil {
				ctx = authn.WithIdentity(ctx, identity)
			}
			ctx = tenant.WithUser(ctx, user)

			var captured http.Header
			_, err := connectutil.UnaryClientInterceptor().WrapUnary(captureHeaders(&captured))(ctx, connect.NewRequest(&pb.HealthRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			Expect(captured).To(BeNil())
		},
		Entry("a user with no identity", nil, "alice",
			"the context carries a user but no authn.Identity"),
		Entry("a user that differs from the identity's subject",
			&authn.Identity{Subject: "bob", TenantID: "acme-corp", Roles: []string{"admin"}}, "alice",
			"the context's user differs from its authn.Identity subject"),
		Entry("an identity with an empty subject",
			&authn.Identity{Subject: "", TenantID: "acme-corp", Roles: []string{"admin"}}, "",
			"the authn.Identity in the context has an empty subject"),
	)

	It("refuses an identity from another tenant with FailedPrecondition, without sending the request or naming either tenant", func() {
		ctx := authn.WithIdentity(tenantCtx(), &authn.Identity{Subject: "alice", TenantID: "globex-inc", Roles: []string{"admin"}, Method: authn.AuthMethodMTLS})
		ctx = tenant.WithUser(ctx, "alice")

		var captured http.Header
		_, err := connectutil.UnaryClientInterceptor().WrapUnary(captureHeaders(&captured))(ctx, connect.NewRequest(&pb.HealthRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("belongs to a different tenant than the request's context"))
		Expect(err.Error()).To(ContainSubstring("Re-scope the context"))
		Expect(err.Error()).NotTo(ContainSubstring("acme-corp"))
		Expect(err.Error()).NotTo(ContainSubstring("globex-inc"))
		Expect(captured).To(BeNil())
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

	It("asserts the end user and roles when both headers are present", func() {
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")
		req.Header().Set(connectutil.MetadataKeyUserID, "alice")
		req.Header().Set(connectutil.MetadataKeyUserRoles, "admin,reviewer")

		rec := &recorder{}
		_, err := connectutil.UnaryServerInterceptor(gateway).WrapUnary(rec.handle)(serviceCtx(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rec.calls).To(Equal(1))
		Expect(rec.tenant).To(Equal("acme-corp"))
		Expect(rec.user).To(Equal("alice"))
		Expect(rec.identity).To(Equal(&authn.Identity{
			Subject:  "alice",
			TenantID: "acme-corp",
			Roles:    []string{"admin", "reviewer"},
			Method:   authn.AuthMethodServiceAsserted,
		}))
	})

	It("injects only the tenant when neither user header is present", func() {
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")

		rec := &recorder{}
		_, err := connectutil.UnaryServerInterceptor(gateway).WrapUnary(rec.handle)(serviceCtx(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rec.calls).To(Equal(1))
		Expect(rec.tenant).To(Equal("acme-corp"))
		Expect(rec.identity).To(BeNil())
		Expect(rec.user).To(BeEmpty())
	})

	DescribeTable("refuses an incomplete or invalid user assertion with InvalidArgument, without calling the handler",
		func(headers [][2]string, wantMsg string) {
			req := connect.NewRequest(&pb.HealthRequest{})
			req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")
			for _, h := range headers {
				req.Header().Add(h[0], h[1])
			}

			rec := &recorder{}
			_, err := connectutil.UnaryServerInterceptor(gateway).WrapUnary(rec.handle)(serviceCtx(), req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			Expect(rec.calls).To(BeZero())
		},
		Entry("a user without roles",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}},
			"x-user-id is present without x-user-roles"),
		Entry("roles without a user",
			[][2]string{{connectutil.MetadataKeyUserRoles, "admin"}},
			"x-user-roles is present without x-user-id"),
		Entry("an empty roles header with a user",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, ""}},
			"x-user-roles is empty"),
		Entry("a trailing empty role",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, "admin,"}},
			"x-user-roles has an empty element"),
		Entry("a leading empty role",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, ",admin"}},
			"x-user-roles has an empty element"),
		Entry("an empty role between two roles",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, "admin,,reviewer"}},
			"x-user-roles has an empty element"),
		Entry("a role with a leading space",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, " reviewer"}},
			`x-user-roles names unknown role " reviewer"`),
		Entry("an unknown role, listing the known roles",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, "root"}},
			`x-user-roles names unknown role "root": use one of admin, reviewer`),
		Entry("an empty user with roles",
			[][2]string{{connectutil.MetadataKeyUserID, ""}, {connectutil.MetadataKeyUserRoles, "admin"}},
			"x-user-id is empty"),
		Entry("a repeated user header",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserID, "bob"}, {connectutil.MetadataKeyUserRoles, "admin"}},
			"x-user-id is sent 2 times"),
		Entry("a repeated roles header",
			[][2]string{{connectutil.MetadataKeyUserID, "alice"}, {connectutil.MetadataKeyUserRoles, "reviewer"}, {connectutil.MetadataKeyUserRoles, "admin"}},
			"x-user-roles is sent 2 times"),
	)

	It("denies a valid user assertion from a peer that is not a service identity, without calling the handler", func() {
		req := connect.NewRequest(&pb.HealthRequest{})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")
		req.Header().Set(connectutil.MetadataKeyUserID, "alice")
		req.Header().Set(connectutil.MetadataKeyUserRoles, "admin")

		rec := &recorder{}
		_, err := connectutil.UnaryServerInterceptor(gateway).WrapUnary(rec.handle)(peerContext("mallory", "Elsewhere"), req)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err.Error()).To(ContainSubstring("not listed in tls.service_identities"))
		Expect(err.Error()).To(ContainSubstring("the graph role calling the pipeline role"))
		Expect(err.Error()).NotTo(ContainSubstring("Add the gateway's client certificate subject"))
		Expect(rec.calls).To(BeZero())
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
