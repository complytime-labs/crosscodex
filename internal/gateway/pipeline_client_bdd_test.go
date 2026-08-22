//go:build !integration

package gateway_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// stubPipelineHandler implements crosscodexv1connect.PipelineServiceHandler,
// capturing the tenant header each call carried so tests can assert the
// client-side connectutil interceptor actually ran.
type stubPipelineHandler struct {
	crosscodexv1connect.UnimplementedPipelineServiceHandler
	capturedTenantHeader string
}

func (h *stubPipelineHandler) CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
	h.capturedTenantHeader = req.Header().Get(connectutil.MetadataKeyTenantID)
	return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1"}), nil
}

var _ = Describe("NewConnectPipelineBackend", func() {
	It("fails closed when the endpoint is empty", func() {
		_, err := gateway.NewConnectPipelineBackend(context.Background(), "", config.TLSConfig{})
		Expect(err).To(HaveOccurred())
	})

	It("fails closed when TLS is not configured for mutual auth", func() {
		// The standalone pipeline listener requires mTLS and has no plaintext
		// fallback, so a client with TLS off (the default) would only fail at
		// the first RPC. It must fail fast at construction instead.
		_, err := gateway.NewConnectPipelineBackend(context.Background(), "pipeline.internal:8443", config.TLSConfig{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutual TLS"))
	})

	It("propagates the ambient tenant to the standalone pipeline service over mutual TLS", func() {
		// Genuine end-to-end mTLS: NewConnectPipelineBackend now refuses any
		// non-mutual config, so this wires real CA-backed server and client
		// identities the same way a split deployment does.
		certDir := GinkgoT().TempDir()
		pki, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		Expect(pki.WriteToDir(certDir)).To(Succeed())

		serverTLS, err := tlsconfig.BuildTLSConfig(context.Background(), config.TLSConfig{
			Mode: "mutual",
			CA:   filepath.Join(certDir, "ca.pem"),
			Cert: filepath.Join(certDir, "server.pem"),
			Key:  filepath.Join(certDir, "server-key.pem"),
		}, "")
		Expect(err).NotTo(HaveOccurred())

		stub := &stubPipelineHandler{}
		_, handler := crosscodexv1connect.NewPipelineServiceHandler(stub)
		srv := httptest.NewUnstartedServer(handler)
		srv.TLS = serverTLS
		srv.StartTLS()
		defer srv.Close()

		clientTLS := config.TLSConfig{
			Mode: "mutual",
			CA:   filepath.Join(certDir, "ca.pem"),
			Cert: filepath.Join(certDir, "client.pem"),
			Key:  filepath.Join(certDir, "client-key.pem"),
		}
		backend, err := gateway.NewConnectPipelineBackend(context.Background(), srv.Listener.Addr().String(), clientTLS)
		Expect(err).NotTo(HaveOccurred())

		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())

		resp, err := backend.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.JobId).To(Equal("job-1"))
		Expect(stub.capturedTenantHeader).To(Equal("acme-corp"))
	})
})
