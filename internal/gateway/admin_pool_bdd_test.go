//go:build !integration

package gateway_test

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

type fakeHealthPool struct {
	db.Pool
	status *db.HealthStatus
	err    error
}

func (f *fakeHealthPool) Health(context.Context) (*db.HealthStatus, error) {
	return f.status, f.err
}

var _ = Describe("PoolAdminBackend", func() {
	It("reports healthy when the pool is connected", func() {
		backend := gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: true}})
		resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
	})

	It("reports unhealthy when the pool reports disconnected", func() {
		backend := gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: false}})
		resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
	})

	It("reports unhealthy when the pool health check errors", func() {
		backend := gateway.NewPoolAdminBackend(&fakeHealthPool{err: errors.New("connection refused")})
		resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
	})
})
