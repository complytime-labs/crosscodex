package pipeline_test

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

var _ = Describe("pipeline.Server", func() {
	var svc *pipeline.Service

	BeforeEach(func() {
		svc = pipeline.New(newFakeStore(), nil, nil, nil, nil, nil, nil, nil,
			config.PipelineConfig{}, config.AttestationConfig{}, nil)
	})

	It("routes a request through the tenant interceptor to the service", func() {
		srv, err := pipeline.NewServer(context.Background(), pipeline.ServerConfig{
			Addr:    "127.0.0.1:0",
			Service: svc,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		defer func() { _ = srv.Shutdown(context.Background()) }()

		client := crosscodexv1connect.NewPipelineServiceClient(http.DefaultClient, "http://"+srv.Addr())

		req := connect.NewRequest(&pb.GetJobRequest{JobId: "nonexistent"})
		req.Header().Set(connectutil.MetadataKeyTenantID, "acme-corp")

		_, err = client.GetJob(context.Background(), req)
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound),
			"request should reach Service.GetJob and fail on an unknown job, proving the tenant header round-tripped")
	})

	It("rejects a request with no tenant header before it reaches the service", func() {
		srv, err := pipeline.NewServer(context.Background(), pipeline.ServerConfig{
			Addr:    "127.0.0.1:0",
			Service: svc,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		defer func() { _ = srv.Shutdown(context.Background()) }()

		client := crosscodexv1connect.NewPipelineServiceClient(http.DefaultClient, "http://"+srv.Addr())

		_, err = client.GetJob(context.Background(), connect.NewRequest(&pb.GetJobRequest{JobId: "nonexistent"}))
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})
})
