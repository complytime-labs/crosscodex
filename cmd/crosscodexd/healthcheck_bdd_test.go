package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

// unreachablePool is a db.Pool whose health check fails the way a
// stopped PostgreSQL does.
type unreachablePool struct{ db.Pool }

func (unreachablePool) Health(context.Context) (*db.HealthStatus, error) {
	return nil, errors.New("connection refused")
}

type activeTenants struct{}

func (activeTenants) TenantActive(context.Context, string) (bool, error) { return true, nil }

var _ = Describe("healthcheckURL", func() {
	DescribeTable("probes the address and scheme the role serves /healthz on",
		func(role, serverAddr, healthAddr, want string) {
			Expect(healthcheckURL(role, serverAddr, healthAddr)).To(Equal(want))
		},
		Entry("all: HTTPS on server.addr", config.RoleAll, ":50051", ":9091", "https://127.0.0.1:50051/healthz"),
		Entry("gateway: HTTPS on server.addr", config.RoleGateway, ":50051", ":9091", "https://127.0.0.1:50051/healthz"),
		Entry("graph: HTTP on health.addr", config.RoleGraph, ":50051", ":9091", "http://127.0.0.1:9091/healthz"),
		Entry("pipeline: HTTP on health.addr", config.RolePipeline, ":50051", ":9092", "http://127.0.0.1:9092/healthz"),
		Entry("worker: HTTP on health.addr", config.RoleWorker, ":50051", ":9093", "http://127.0.0.1:9093/healthz"),
		Entry("gateway: loopback in place of an explicit server.addr host", config.RoleGateway, "0.0.0.0:50051", ":9091", "https://127.0.0.1:50051/healthz"),
		Entry("worker: loopback in place of an explicit health.addr host", config.RoleWorker, ":50051", "0.0.0.0:9093", "http://127.0.0.1:9093/healthz"),
		Entry("worker: bracketed IPv6 wildcard health.addr", config.RoleWorker, ":50051", "[::]:9091", "http://127.0.0.1:9091/healthz"),
		Entry("worker: bracketed IPv6 loopback health.addr", config.RoleWorker, ":50051", "[::1]:9091", "http://127.0.0.1:9091/healthz"),
		Entry("gateway: bracketed IPv6 wildcard server.addr", config.RoleGateway, "[::]:50051", ":9091", "https://127.0.0.1:50051/healthz"),
		Entry("gateway: bracketed IPv6 loopback server.addr", config.RoleGateway, "[::1]:50051", ":9091", "https://127.0.0.1:50051/healthz"),
	)
})

var _ = Describe("probeHealthURL", func() {
	var server *httptest.Server
	var status int

	BeforeEach(func() {
		status = http.StatusOK
		server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
	})
	AfterEach(func() { server.Close() })

	It("returns nil on HTTP 200", func() {
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		Expect(probeHealthURL(context.Background(), server.URL, tlsCfg)).To(Succeed())
	})

	It("returns an error on HTTP 503", func() {
		status = http.StatusServiceUnavailable
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		Expect(probeHealthURL(context.Background(), server.URL, tlsCfg)).To(HaveOccurred())
	})

	It("returns an error when the target is unreachable", func() {
		// Stand up a throwaway TLS server, capture its URL, then close it so
		// the dial fails -- exercising the transport-error branch (client.Do
		// error), the common "daemon not up yet" case a healthcheck reports.
		closed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		url := closed.URL
		closed.Close()
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only
		Expect(probeHealthURL(context.Background(), url, tlsCfg)).To(HaveOccurred())
	})

	It("fails against a gateway that cannot reach its database", func() {
		srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
			Addr:         "127.0.0.1:0",
			Service:      gateway.NewService(gateway.WithAdminBackend(gateway.NewPoolAdminBackend(unreachablePool{}))),
			TenantStatus: activeTenants{},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		DeferCleanup(func() { _ = srv.Shutdown(context.Background()) })

		err = probeHealthURL(context.Background(), "http://"+srv.Addr()+"/healthz", nil)
		Expect(err).To(MatchError(ContainSubstring("unhealthy status 503")))
	})
})
