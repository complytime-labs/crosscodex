package authn_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/authn"
)

var _ = Describe("TLS state context", func() {
	It("round-trips a TLS connection state", func() {
		state := &tls.ConnectionState{ServerName: "gateway.test"}
		ctx := authn.WithTLSState(context.Background(), state)
		Expect(authn.TLSStateFromContext(ctx)).To(BeIdenticalTo(state))
	})

	It("returns nil when no TLS state was stored", func() {
		Expect(authn.TLSStateFromContext(context.Background())).To(BeNil())
	})

	Describe("TLSStateMiddleware", func() {
		serve := func(req *http.Request) *tls.ConnectionState {
			var seen *tls.ConnectionState
			handler := authn.TLSStateMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = authn.TLSStateFromContext(r.Context())
			}))
			handler.ServeHTTP(httptest.NewRecorder(), req)
			return seen
		}

		It("exposes the request's TLS state to the handler", func() {
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			req.TLS = &tls.ConnectionState{ServerName: "gateway.test"}
			Expect(serve(req)).To(BeIdenticalTo(req.TLS))
		})

		It("stores nothing for a plaintext request", func() {
			Expect(serve(httptest.NewRequest(http.MethodGet, "/", http.NoBody))).To(BeNil())
		})
	})
})
