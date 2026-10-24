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

	DescribeTable("names the database as a dependency",
		func(pool *fakeHealthPool, want pb.HealthStatus) {
			backend := gateway.NewPoolAdminBackend(pool)
			resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Services).To(HaveKey("database"))
			dep := resp.Msg.Services["database"]
			Expect(dep.ServiceName).To(Equal("database"))
			Expect(dep.Status).To(Equal(want))
			Expect(dep.Message).NotTo(BeEmpty())
			Expect(dep.LastCheck).NotTo(BeNil())
		},
		Entry("connected", &fakeHealthPool{status: &db.HealthStatus{Connected: true}}, pb.HealthStatus_HEALTH_STATUS_HEALTHY),
		Entry("disconnected", &fakeHealthPool{status: &db.HealthStatus{Connected: false}}, pb.HealthStatus_HEALTH_STATUS_UNHEALTHY),
		Entry("health check error", &fakeHealthPool{err: errors.New("connection refused")}, pb.HealthStatus_HEALTH_STATUS_UNHEALTHY),
	)

	It("withholds connection details from the unhealthy message (CWE-209)", func() {
		driverErr := errors.New(`pool not ready: dial tcp 10.0.0.7:5432: password authentication failed for user "crosscodex_app"`)
		backend := gateway.NewPoolAdminBackend(&fakeHealthPool{err: driverErr})
		resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
		Expect(err).NotTo(HaveOccurred())
		msg := resp.Msg.Services["database"].GetMessage()
		Expect(msg).To(Equal("database unreachable"))
		for _, secret := range []string{"10.0.0.7", "5432", "crosscodex_app", "password", "dial tcp"} {
			Expect(msg).NotTo(ContainSubstring(secret))
		}
	})
})
