//go:build !integration

package gateway_test

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("Server", func() {
	It("starts, serves Health over plain HTTP, and shuts down cleanly", func() {
		svc := gateway.NewService(
			gateway.WithAdminBackend(gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: true}})),
		)
		srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
			Addr:    "127.0.0.1:0",
			Service: svc,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		defer func() { _ = srv.Shutdown(context.Background()) }()

		client := crosscodexv1connect.NewGatewayServiceClient(http.DefaultClient, "http://"+srv.Addr())
		resp, err := client.Health(context.Background(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
	})
})
