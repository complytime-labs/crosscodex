package pipeline_test

import (
	"context"
	"net/http"
	"path/filepath"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

var _ = Describe("pipeline.Server", func() {
	var (
		svc     *pipeline.Service
		certDir string
	)

	BeforeEach(func() {
		svc = pipeline.New(newFakeStore(), nil, nil, nil, pipeline.Attestors{}, nil, nil, nil,
			config.PipelineConfig{}, nil)
		certDir = GinkgoT().TempDir()
		pki, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		Expect(pki.WriteToDir(certDir)).To(Succeed())
	})

	// startMTLS serves svc over mutual TLS, trusting identities, and returns a
	// client presenting the generated client certificate (CN=client, O=CrossCodex Test).
	startMTLS := func(identities []authn.X509Match) crosscodexv1connect.PipelineServiceClient {
		srv, err := pipeline.NewServer(context.Background(), pipeline.ServerConfig{
			Addr: "127.0.0.1:0",
			TLS: config.TLSConfig{
				Mode: "mutual",
				CA:   filepath.Join(certDir, "ca.pem"),
				Cert: filepath.Join(certDir, "server.pem"),
				Key:  filepath.Join(certDir, "server-key.pem"),
			},
			Service:           svc,
			ServiceIdentities: identities,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		DeferCleanup(func() { _ = srv.Shutdown(context.Background()) })

		clientTLS, err := tlsconfig.BuildTLSConfig(context.Background(), config.TLSConfig{
			Mode: "mutual",
			CA:   filepath.Join(certDir, "ca.pem"),
			Cert: filepath.Join(certDir, "client.pem"),
			Key:  filepath.Join(certDir, "client-key.pem"),
		}, "")
		Expect(err).NotTo(HaveOccurred())
		httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS, ForceAttemptHTTP2: true}}
		return crosscodexv1connect.NewPipelineServiceClient(httpClient, "https://"+srv.Addr())
	}

	getJob := func(client crosscodexv1connect.PipelineServiceClient, withTenant bool) error {
		req := connect.NewRequest(&pb.GetJobRequest{JobId: "nonexistent"})
		if withTenant {
			req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")
		}
		_, err := client.GetJob(context.Background(), req)
		return err
	}

	listed := []authn.X509Match{{CN: "client", Organization: "CrossCodex Test"}}

	It("routes a request from a listed service identity to the service", func() {
		err := getJob(startMTLS(listed), true)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound),
			"request should reach Service.GetJob and fail on an unknown job, proving the tenant header round-tripped")
	})

	It("rejects a request with no tenant header before it reaches the service", func() {
		err := getJob(startMTLS(listed), false)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("denies a valid client certificate that is not a service identity, before the service", func() {
		err := getJob(startMTLS([]authn.X509Match{{CN: "someone-else"}}), true)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		Expect(err).To(MatchError(ContainSubstring("tls.service_identities")))
	})

	It("denies every request when no service identities are configured", func() {
		err := getJob(startMTLS(nil), true)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
	})
})
