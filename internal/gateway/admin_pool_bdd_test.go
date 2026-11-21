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

	Describe("extra dependency checks", func() {
		connected := func() *fakeHealthPool { return &fakeHealthPool{status: &db.HealthStatus{Connected: true}} }
		pass := gateway.DependencyCheck{Name: "nats", Check: func(context.Context) error { return nil }}
		fail := gateway.DependencyCheck{Name: "nats", Check: func(context.Context) error { return errors.New("not connected") }}

		DescribeTable("reports each check as a named dependency next to the database",
			func(check gateway.DependencyCheck, wantOverall, wantNATS pb.HealthStatus, wantMessage string) {
				backend := gateway.NewPoolAdminBackend(connected(), check)
				resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Msg.Status).To(Equal(wantOverall))
				Expect(resp.Msg.Services["database"].GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
				Expect(resp.Msg.Services).To(HaveKey("nats"))
				dep := resp.Msg.Services["nats"]
				Expect(dep.ServiceName).To(Equal("nats"))
				Expect(dep.Status).To(Equal(wantNATS))
				Expect(dep.Message).To(Equal(wantMessage))
				Expect(dep.LastCheck).NotTo(BeNil())
			},
			Entry("passing check", pass, pb.HealthStatus_HEALTH_STATUS_HEALTHY, pb.HealthStatus_HEALTH_STATUS_HEALTHY, "nats reachable"),
			Entry("failing check", fail, pb.HealthStatus_HEALTH_STATUS_UNHEALTHY, pb.HealthStatus_HEALTH_STATUS_UNHEALTHY, "nats unreachable"),
		)

		It("keeps the overall status unhealthy when the database fails and the extra check passes", func() {
			backend := gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: false}}, pass)
			resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(resp.Msg.Services["nats"].GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
		})

		It("runs every check after a failing database and a failing extra check", func() {
			failing := gateway.DependencyCheck{Name: "graph_database", Check: fail.Check}
			backend := gateway.NewPoolAdminBackend(&fakeHealthPool{err: errors.New("connection refused")}, failing, pass)
			resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(resp.Msg.Services["database"].GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(resp.Msg.Services).To(HaveKey("graph_database"))
			Expect(resp.Msg.Services["graph_database"].GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(resp.Msg.Services).To(HaveKey("nats"))
			Expect(resp.Msg.Services["nats"].GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
		})

		It("withholds the check's error from the message (CWE-209)", func() {
			leaky := gateway.DependencyCheck{Name: "graph_database", Check: func(context.Context) error {
				return errors.New("dial tcp 10.0.0.9:5432: password authentication failed for user graph_user")
			}}
			backend := gateway.NewPoolAdminBackend(connected(), leaky)
			resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
			Expect(err).NotTo(HaveOccurred())
			msg := resp.Msg.Services["graph_database"].GetMessage()
			Expect(msg).To(Equal("graph_database unreachable"))
			for _, secret := range []string{"10.0.0.9", "5432", "graph_user", "password", "dial tcp"} {
				Expect(msg).NotTo(ContainSubstring(secret))
			}
		})
	})
})
