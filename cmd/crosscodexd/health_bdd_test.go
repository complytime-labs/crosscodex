package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
)

// serveHealthz runs one GET /healthz through healthCheckHandler(checks...).
func serveHealthz(checks ...gateway.DependencyCheck) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	healthCheckHandler(checks...).ServeHTTP(rec, req)
	return rec
}

func check(name string, err error) gateway.DependencyCheck {
	return gateway.DependencyCheck{Name: name, Check: func(context.Context) error { return err }}
}

var _ = Describe("healthCheckHandler", func() {
	It("returns 200 with zero checks (pure liveness)", func() {
		rec := serveHealthz()
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Body.String()).To(ContainSubstring(`"status":"healthy"`))
	})

	It("returns 200 when every check passes", func() {
		rec := serveHealthz(check("database", nil), check("nats", nil))
		Expect(rec.Code).To(Equal(http.StatusOK))
	})

	It("returns 503 naming the failing dependency", func() {
		rec := serveHealthz(check("database", nil), check("nats", errors.New("not connected")))
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
		Expect(rec.Body.String()).To(MatchJSON(`{"status":"unhealthy","error":"nats unreachable"}`))
	})

	It("withholds the check's error from the unauthenticated body (CWE-209)", func() {
		rec := serveHealthz(check("database", errors.New(`dial tcp 10.0.0.7:5432: password authentication failed for user "app_user"`)))
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
		for _, leak := range []string{"10.0.0.7", "5432", "password", "app_user", "dial tcp"} {
			Expect(rec.Body.String()).NotTo(ContainSubstring(leak))
		}
	})

	It("stops at the first failing check", func() {
		called := false
		neverCalled := gateway.DependencyCheck{Name: "nats", Check: func(context.Context) error { called = true; return nil }}
		rec := serveHealthz(check("database", errors.New("first check failed")), neverCalled)
		Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
		Expect(called).To(BeFalse())
	})
})

// healthPool is a db.Pool whose Health reports connected unless down.
type healthPool struct {
	db.Pool
	down bool
}

func (p healthPool) Health(context.Context) (*db.HealthStatus, error) {
	if p.down {
		return nil, errors.New("dial tcp 10.0.0.7:5432: connection refused")
	}
	return &db.HealthStatus{Connected: true}, nil
}

type pingFunc func(context.Context) error

func (f pingFunc) PingContext(ctx context.Context) error { return f(ctx) }

// down names the dependency a spec takes down; "" leaves every one up.
func roleChecks(role, down string) []gateway.DependencyCheck {
	bus := &natsbustest.Fake{Disconnected: down == "nats"}
	graphDB := pingFunc(func(context.Context) error {
		if down == "graph_database" {
			return errors.New("dial tcp 10.0.0.9:5432: connection refused")
		}
		return nil
	})
	return roleHealthChecks(role, healthPool{down: down == "database"}, bus, graphDB)
}

var _ = Describe("roleHealthChecks", func() {
	DescribeTable("checks only the dependencies each role uses",
		func(role string, want []string) {
			var names []string
			for _, c := range roleChecks(role, "") {
				names = append(names, c.Name)
			}
			Expect(names).To(Equal(want))
		},
		Entry("worker: NATS", config.RoleWorker, []string{"nats"}),
		Entry("pipeline: database and NATS", config.RolePipeline, []string{"database", "nats"}),
		Entry("graph: database, NATS and graph database", config.RoleGraph, []string{"database", "nats", "graph_database"}),
		// PoolAdminBackend always reports the database for gateway and all.
		Entry("gateway: NATS beside the gateway's database check", config.RoleGateway, []string{"nats"}),
		Entry("all: NATS and graph database beside the gateway's database check", config.RoleAll, []string{"nats", "graph_database"}),
	)

	It("fails closed for an unknown role with a check that always errors", func() {
		checks := roleChecks("bogus", "")
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].Name).To(Equal("role"))
		err := checks[0].Check(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`"bogus"`))
		Expect(err.Error()).To(ContainSubstring("must be one of all, gateway, pipeline, worker, graph"))
		Expect(serveHealthz(checks...).Code).To(Equal(http.StatusServiceUnavailable))
	})

	DescribeTable("a split role's /healthz returns 503 naming the dependency that is down",
		func(role, down string) {
			rec := serveHealthz(roleChecks(role, down)...)
			Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
			var body map[string]string
			Expect(json.Unmarshal(rec.Body.Bytes(), &body)).To(Succeed())
			Expect(body).To(HaveKeyWithValue("error", down+" unreachable"))
			Expect(rec.Body.String()).NotTo(ContainSubstring("10.0.0."))
		},
		Entry("worker, NATS down", config.RoleWorker, "nats"),
		Entry("pipeline, database down", config.RolePipeline, "database"),
		Entry("pipeline, NATS down", config.RolePipeline, "nats"),
		Entry("graph, database down", config.RoleGraph, "database"),
		Entry("graph, NATS down", config.RoleGraph, "nats"),
		Entry("graph, graph database down", config.RoleGraph, "graph_database"),
	)

	DescribeTable("a split role's /healthz returns 200 when its dependencies are up",
		func(role string) {
			Expect(serveHealthz(roleChecks(role, "")...).Code).To(Equal(http.StatusOK))
		},
		Entry("worker", config.RoleWorker),
		Entry("pipeline", config.RolePipeline),
		Entry("graph", config.RoleGraph),
	)

	DescribeTable("gateway and all report the dependency that is down through PoolAdminBackend",
		func(role, down string) {
			backend := gateway.NewPoolAdminBackend(healthPool{down: down == "database"}, roleChecks(role, down)...)
			resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(resp.Msg.Services[down].GetStatus()).To(Equal(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(resp.Msg.Services[down].GetMessage()).To(Equal(down + " unreachable"))
		},
		Entry("gateway, database down", config.RoleGateway, "database"),
		Entry("gateway, NATS down", config.RoleGateway, "nats"),
		Entry("all, database down", config.RoleAll, "database"),
		Entry("all, NATS down", config.RoleAll, "nats"),
		Entry("all, graph database down", config.RoleAll, "graph_database"),
	)

	DescribeTable("gateway and all report healthy through PoolAdminBackend when their dependencies are up",
		func(role string) {
			backend := gateway.NewPoolAdminBackend(healthPool{}, roleChecks(role, "")...)
			resp, err := backend.HealthCheck(context.Background(), connect.NewRequest(&pb.HealthCheckRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
		},
		Entry("gateway", config.RoleGateway),
		Entry("all", config.RoleAll),
	)
})

var _ = Describe("runtime.start health listener", func() {
	It("fails start with an actionable error when health.addr is already taken", func() {
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(taken.Close)
		addr := taken.Addr().String()
		rt := &runtime{healthServer: newHealthServer(addr)}

		err = rt.start(context.Background())

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("health.addr " + addr))
		Expect(err.Error()).To(ContainSubstring("give each role on one host a distinct health.addr"))
		var opErr *net.OpError
		Expect(errors.As(err, &opErr)).To(BeTrue(), "the bind error is wrapped")
	})

	It("serves /healthz once start returns", func() {
		free, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr := free.Addr().String()
		Expect(free.Close()).To(Succeed())
		rt := &runtime{healthServer: newHealthServer(addr)}

		Expect(rt.start(context.Background())).To(Succeed())
		DeferCleanup(func() { _ = rt.stop(context.Background()) })

		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})
})
