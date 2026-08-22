package rpcserver_test

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/rpcserver"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

func TestRpcServerBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RpcServer BDD Suite")
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var _ = Describe("NewRecoveryInterceptor", func() {
	It("converts a panic into a CodeInternal error instead of crashing", func() {
		interceptor := rpcserver.NewRecoveryInterceptor(discardLogger())
		panicking := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			panic("boom")
		}

		_, err := interceptor.WrapUnary(panicking)(context.Background(), connect.NewRequest(&struct{}{}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
	})

	It("passes through a non-panicking call unchanged", func() {
		interceptor := rpcserver.NewRecoveryInterceptor(discardLogger())
		ok := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return connect.NewResponse(&struct{}{}), nil
		}

		resp, err := interceptor.WrapUnary(ok)(context.Background(), connect.NewRequest(&struct{}{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp).NotTo(BeNil())
	})
})

var _ = Describe("Listen and Server", func() {
	It("serves plaintext HTTP when TLS is off", func() {
		lis, tlsCfg, err := rpcserver.Listen(context.Background(), "127.0.0.1:0", config.TLSConfig{}, "test-target", tls.NoClientCert)
		Expect(err).NotTo(HaveOccurred())
		Expect(tlsCfg).To(BeNil())

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		srv := rpcserver.New(lis, tlsCfg, handler, discardLogger())
		Expect(srv.Start()).To(Succeed())
		defer func() { _ = srv.Shutdown(context.Background()) }()

		resp, err := http.Get("http://" + srv.Addr())
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})

	It("returns an actionable error when the address is already in use", func() {
		lis, _, err := rpcserver.Listen(context.Background(), "127.0.0.1:0", config.TLSConfig{}, "test-target", tls.NoClientCert)
		Expect(err).NotTo(HaveOccurred())
		defer lis.Close()

		_, _, err = rpcserver.Listen(context.Background(), lis.Addr().String(), config.TLSConfig{}, "test-target", tls.NoClientCert)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("listen"))
	})

	It("reports an empty Addr before Start", func() {
		srv := rpcserver.New(nil, nil, http.NotFoundHandler(), discardLogger())
		Expect(srv.Addr()).To(Equal(""))
	})
})
