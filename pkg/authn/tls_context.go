package authn

import (
	"context"
	"crypto/tls"
	"net/http"
)

type tlsStateKey struct{}

// WithTLSState returns ctx carrying the client connection's TLS state, which
// X509Authenticator reads peer certificates from via Request.TLSState.
func WithTLSState(ctx context.Context, s *tls.ConnectionState) context.Context {
	return context.WithValue(ctx, tlsStateKey{}, s)
}

// TLSStateFromContext returns the TLS state stored by WithTLSState, or nil
// when the request did not arrive over TLS. Callers must treat nil as
// unauthenticated.
func TLSStateFromContext(ctx context.Context) *tls.ConnectionState {
	s, _ := ctx.Value(tlsStateKey{}).(*tls.ConnectionState)
	return s
}

// TLSStateMiddleware stores each request's r.TLS in its context. Plaintext
// requests store nothing, so TLSStateFromContext returns nil for them.
func TLSStateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			r = r.WithContext(WithTLSState(r.Context(), r.TLS))
		}
		next.ServeHTTP(w, r)
	})
}
