package connectutil_test

import (
	"net/http"
	"slices"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

// Suite bootstrap lives in connectutil_bdd_test.go (TestConnectUtilBDD).

var _ = Describe("Property Specifications", Ordered, func() {
	It("round-trips an asserted identity from the client interceptor to the server interceptor", func() {
		identities := []authn.X509Match{{CN: "client", Organization: "CrossCodex"}}
		serviceCtx := peerContext("client", "CrossCodex")
		client := connectutil.UnaryClientInterceptor()
		server := connectutil.UnaryServerInterceptor(identities)
		base := tenantCtx()

		rapid.Check(GinkgoT(), func(t *rapid.T) {
			// Non-empty printable ASCII, no control characters.
			subject := rapid.StringMatching(`[ -~]{1,32}`).Draw(t, "subject")
			roles := rapid.SliceOfNDistinct(rapid.SampledFrom(authn.KnownRoles), 1, len(authn.KnownRoles), rapid.ID[string]).Draw(t, "roles")
			sent := &authn.Identity{Subject: subject, TenantID: "acme-corp", Roles: roles, Method: authn.AuthMethodMTLS}

			ctx := tenant.WithUser(authn.WithIdentity(base, sent), subject)
			var headers http.Header
			if _, err := client.WrapUnary(captureHeaders(&headers))(ctx, connect.NewRequest(&pb.HealthRequest{})); err != nil {
				t.Fatalf("client refused identity %+v: %v", sent, err)
			}

			req := connect.NewRequest(&pb.HealthRequest{})
			for k, v := range headers {
				req.Header()[k] = v
			}
			rec := &recorder{}
			if _, err := server.WrapUnary(rec.handle)(serviceCtx, req); err != nil {
				t.Fatalf("server refused headers %v: %v", headers, err)
			}
			got := rec.identity
			if got == nil || got.Subject != subject || got.TenantID != "acme-corp" ||
				got.Method != authn.AuthMethodServiceAsserted || !slices.Equal(got.Roles, roles) {
				t.Fatalf("identity did not round-trip: sent %+v, got %+v", sent, got)
			}
			if rec.user != subject {
				t.Fatalf("user did not round-trip: sent %q, got %q", subject, rec.user)
			}
		})
	})
})
