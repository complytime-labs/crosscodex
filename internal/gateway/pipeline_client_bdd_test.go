//go:build !integration

package gateway_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
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
	capturedTraceparent  string
}

func (h *stubPipelineHandler) CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
	h.capturedTenantHeader = req.Header().Get(connectutil.MetadataKeyTenantID)
	h.capturedTraceparent = req.Header().Get("traceparent")
	return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1"}), nil
}

// startMTLSPipelineStub serves stub over genuine mutual TLS, with CA-backed
// server and client identities wired the way a split deployment does, and
// returns its address and the client TLS config that reaches it.
func startMTLSPipelineStub(stub *stubPipelineHandler) (string, config.TLSConfig) {
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

	_, handler := crosscodexv1connect.NewPipelineServiceHandler(stub)
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = serverTLS
	srv.StartTLS()
	DeferCleanup(srv.Close)

	return srv.Listener.Addr().String(), config.TLSConfig{
		Mode: "mutual",
		CA:   filepath.Join(certDir, "ca.pem"),
		Cert: filepath.Join(certDir, "client.pem"),
		Key:  filepath.Join(certDir, "client-key.pem"),
	}
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

	DescribeTable("NewPipelineClient fails closed with a message naming the setting to fix",
		func(endpoint, wantKey string) {
			client, err := gateway.NewPipelineClient(context.Background(), endpoint, config.TLSConfig{})
			Expect(err).To(MatchError(ContainSubstring(wantKey)))
			Expect(client).To(BeNil())
		},
		Entry("an empty endpoint names pipeline.endpoint", "", "pipeline.endpoint"),
		Entry("TLS off names the pipeline-client target", "pipeline.internal:8443", "tls.targets.pipeline-client.mode"),
	)

	It("propagates the ambient tenant to the standalone pipeline service over mutual TLS", func() {
		// Genuine end-to-end mTLS: NewConnectPipelineBackend refuses any
		// non-mutual config.
		stub := &stubPipelineHandler{}
		addr, clientTLS := startMTLSPipelineStub(stub)
		backend, err := gateway.NewConnectPipelineBackend(context.Background(), addr, clientTLS)
		Expect(err).NotTo(HaveOccurred())

		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())

		resp, err := backend.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.JobId).To(Equal("job-1"))
		Expect(stub.capturedTenantHeader).To(Equal("acme-corp"))
	})

	It("carries the caller's trace context to the standalone pipeline service", func() {
		DeferCleanup(otel.SetTextMapPropagator, otel.GetTextMapPropagator())
		otel.SetTextMapPropagator(propagation.TraceContext{})
		provider, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(provider.Shutdown, context.Background())

		stub := &stubPipelineHandler{}
		addr, clientTLS := startMTLSPipelineStub(stub)
		client, err := gateway.NewPipelineClient(context.Background(), addr, clientTLS)
		Expect(err).NotTo(HaveOccurred())

		ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
		Expect(err).NotTo(HaveOccurred())
		ctx, span := provider.TracerProvider().Tracer("pipeline-client-spec").Start(ctx, "caller")
		defer span.End()

		_, err = client.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stub.capturedTraceparent).To(ContainSubstring(span.SpanContext().TraceID().String()))
	})
})
