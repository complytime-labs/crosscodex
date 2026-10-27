//go:build !integration

package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

var _ = Describe("Server", func() {
	It("starts, serves Health over plain HTTP, and shuts down cleanly", func() {
		svc := gateway.NewService(
			gateway.WithAdminBackend(gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: true}})),
		)
		srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
			Addr:         "127.0.0.1:0",
			Service:      svc,
			TenantStatus: &fakeTenantStatus{active: true},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		defer func() { _ = srv.Shutdown(context.Background()) }()

		client := crosscodexv1connect.NewGatewayServiceClient(http.DefaultClient, "http://"+srv.Addr())
		resp, err := client.Health(context.Background(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
	})

	It("refuses to build without a tenant status checker (fail closed)", func() {
		svc := gateway.NewService(
			gateway.WithAdminBackend(gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: true}})),
		)
		_, err := gateway.NewServer(context.Background(), gateway.ServerConfig{Addr: "127.0.0.1:0", Service: svc})
		Expect(err).To(MatchError(ContainSubstring("TenantStatus is required")))
	})

	It("serves Health under authn without consulting the tenant status checker", func() {
		reg, err := authn.NewRegistry(nil, nil)
		Expect(err).NotTo(HaveOccurred())
		svc := gateway.NewService(
			gateway.WithAuthn(reg),
			gateway.WithAdminBackend(gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: true}})),
		)
		checker := &fakeTenantStatus{err: errors.New("must not be called")}
		srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
			Addr:         "127.0.0.1:0",
			Service:      svc,
			TenantStatus: checker,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		defer func() { _ = srv.Shutdown(context.Background()) }()

		client := crosscodexv1connect.NewGatewayServiceClient(http.DefaultClient, "http://"+srv.Addr())
		resp, err := client.Health(context.Background(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.Status).To(Equal(pb.HealthStatus_HEALTH_STATUS_HEALTHY))
		Expect(checker.calls).To(BeZero())
	})

	Describe("GET /healthz", func() {
		// /healthz is the container readiness probe (crosscodexd healthcheck
		// checks only the status code), so anything short of HEALTHY must be
		// a non-200 or the orchestrator reports a daemon without its
		// database as healthy.
		getHealthz := func(admin gateway.AdminBackend) (int, string) {
			srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
				Addr:         "127.0.0.1:0",
				Service:      gateway.NewService(gateway.WithAdminBackend(admin)),
				TenantStatus: &fakeTenantStatus{active: true},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(srv.Start()).To(Succeed())
			DeferCleanup(func() { _ = srv.Shutdown(context.Background()) })

			resp, err := http.Get("http://" + srv.Addr() + "/healthz")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			return resp.StatusCode, string(body)
		}

		withStatus := func(status pb.HealthStatus) gateway.AdminBackend {
			return &mockAdmin{healthCheckFn: func(context.Context, *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
				return connect.NewResponse(&pb.HealthCheckResponse{
					Status: status,
					Services: map[string]*pb.ServiceHealth{
						"database": {ServiceName: "database", Status: status, Message: "dependency message"},
					},
				}), nil
			}}
		}

		DescribeTable("maps the overall health status to the HTTP status code",
			func(admin gateway.AdminBackend, wantCode int) {
				code, _ := getHealthz(admin)
				Expect(code).To(Equal(wantCode))
			},
			Entry("HEALTHY", withStatus(pb.HealthStatus_HEALTH_STATUS_HEALTHY), http.StatusOK),
			Entry("DEGRADED", withStatus(pb.HealthStatus_HEALTH_STATUS_DEGRADED), http.StatusServiceUnavailable),
			Entry("UNHEALTHY", withStatus(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY), http.StatusServiceUnavailable),
			Entry("UNSPECIFIED", withStatus(pb.HealthStatus_HEALTH_STATUS_UNSPECIFIED), http.StatusServiceUnavailable),
			Entry("backend error", &mockAdmin{healthCheckFn: func(context.Context, *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("backend down"))
			}}, http.StatusServiceUnavailable),
		)

		It("keeps the per-dependency JSON body on a 503", func() {
			code, body := getHealthz(withStatus(pb.HealthStatus_HEALTH_STATUS_UNHEALTHY))
			Expect(code).To(Equal(http.StatusServiceUnavailable))
			Expect(body).To(ContainSubstring(`"service_name":"database"`))
			Expect(body).To(ContainSubstring(`"message":"dependency message"`))
		})

		It("returns 503 through PoolAdminBackend when the database is unreachable, without leaking the driver error", func() {
			code, body := getHealthz(gateway.NewPoolAdminBackend(&fakeHealthPool{err: errors.New("dial tcp 10.0.0.5:5432: connection refused")}))
			Expect(code).To(Equal(http.StatusServiceUnavailable))
			Expect(body).To(ContainSubstring("database unreachable"))
			Expect(body).NotTo(ContainSubstring("10.0.0.5"))
		})

		It("withholds a backend error from the unauthenticated body (CWE-209)", func() {
			code, body := getHealthz(&mockAdmin{healthCheckFn: func(context.Context, *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
				return nil, errors.New("dial tcp 10.0.0.5:5432: password authentication failed for user app_user")
			}})
			Expect(code).To(Equal(http.StatusServiceUnavailable))
			Expect(json.Valid([]byte(body))).To(BeTrue(), "body is not valid JSON: %s", body)
			Expect(body).To(MatchJSON(`{"status":"unhealthy"}`))
			for _, leak := range []string{"10.0.0.5", "5432", "password", "app_user", "dial tcp"} {
				Expect(body).NotTo(ContainSubstring(leak))
			}
		})

		It("returns 503 through PoolAdminBackend when the pool reports not connected", func() {
			code, body := getHealthz(gateway.NewPoolAdminBackend(&fakeHealthPool{status: &db.HealthStatus{Connected: false}}))
			Expect(code).To(Equal(http.StatusServiceUnavailable))
			Expect(body).To(ContainSubstring("database unreachable"))
		})
	})

	Describe("tenant status wiring over mTLS", func() {
		// The auth interceptor only runs on TLS requests, so these specs
		// serve real mTLS; fixedIdentity maps every client to tenant acme.
		var (
			checker *fakeTenantStatus
			client  crosscodexv1connect.GatewayServiceClient
		)

		BeforeEach(func() {
			certDir := GinkgoT().TempDir()
			pki, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			Expect(pki.WriteToDir(certDir)).To(Succeed())

			reg, err := authn.NewRegistry(nil, []authn.Authenticator{
				fixedIdentity{id: &authn.Identity{Subject: "alice", TenantID: "acme", Method: authn.AuthMethodMTLS}},
			})
			Expect(err).NotTo(HaveOccurred())
			checker = &fakeTenantStatus{}
			srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
				Addr: "127.0.0.1:0",
				TLS: config.TLSConfig{
					Mode: "mutual",
					CA:   filepath.Join(certDir, "ca.pem"),
					Cert: filepath.Join(certDir, "server.pem"),
					Key:  filepath.Join(certDir, "server-key.pem"),
				},
				Service:      gateway.NewService(gateway.WithAuthn(reg)),
				TenantStatus: checker,
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
			client = crosscodexv1connect.NewGatewayServiceClient(httpClient, "https://"+srv.Addr())
		})

		listCatalogs := func() error {
			_, err := client.ListCatalogs(context.Background(), connect.NewRequest(&pb.ListCatalogsRequest{}))
			return err
		}
		streamDocument := func() error {
			_, err := client.StreamDocument(context.Background()).CloseAndReceive()
			return err
		}

		DescribeTable("refuses an inactive tenant on a non-health RPC",
			func(call func() error) {
				checker.active = false
				err := call()
				Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
				Expect(err.Error()).To(ContainSubstring(`tenant "acme" is suspended or not provisioned`))
				Expect(checker.calls).To(Equal(1))
				Expect(checker.got).To(Equal("acme"))
			},
			Entry("unary", listCatalogs),
			Entry("client streaming", streamDocument),
		)

		DescribeTable("lets an active tenant reach the handler",
			func(call func() error, wantCode connect.Code, wantMsg string) {
				checker.active = true
				err := call()
				Expect(connect.CodeOf(err)).To(Equal(wantCode))
				Expect(err.Error()).To(ContainSubstring(wantMsg))
				Expect(checker.calls).To(Equal(1))
				Expect(checker.got).To(Equal("acme"))
			},
			Entry("unary", listCatalogs, connect.CodeUnavailable, "catalog backend not configured"),
			Entry("client streaming", streamDocument, connect.CodeInvalidArgument, "metadata"),
		)
	})
})

// fakeAdminService answers ListHolds with a fixed error; every other
// AdminService RPC is the generated Unimplemented stub.
type fakeAdminService struct {
	crosscodexv1connect.UnimplementedAdminServiceHandler
	err error
}

func (f *fakeAdminService) ListHolds(context.Context, *connect.Request[pb.ListHoldsRequest]) (*connect.Response[pb.ListHoldsResponse], error) {
	return nil, f.err
}

var _ = Describe("Server error boundary", func() {
	const secret = "pq: password authentication failed for user secret_admin"

	var logs *bytes.Buffer

	// start serves svc and admin over plain HTTP and returns the base URL.
	start := func(svc *gateway.Service, admin crosscodexv1connect.AdminServiceHandler) string {
		srv, err := gateway.NewServer(context.Background(), gateway.ServerConfig{
			Addr:         "127.0.0.1:0",
			Service:      svc,
			Admin:        admin,
			TenantStatus: &fakeTenantStatus{active: true},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(srv.Start()).To(Succeed())
		DeferCleanup(func() { _ = srv.Shutdown(context.Background()) })
		return "http://" + srv.Addr()
	}

	BeforeEach(func() { logs = &bytes.Buffer{} })

	// AdminService is served by internal/admin, which does not sanitize its
	// errors, so the mount applies the gateway's boundary policy itself.
	DescribeTable("withholds an AdminService error the caller cannot act on",
		func(adminErr error) {
			url := start(gateway.NewService(gateway.WithLogger(slog.New(slog.NewTextHandler(logs, nil)))), &fakeAdminService{err: adminErr})
			client := crosscodexv1connect.NewAdminServiceClient(http.DefaultClient, url)
			_, err := client.ListHolds(context.Background(), connect.NewRequest(&pb.ListHoldsRequest{}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(err.Error()).NotTo(ContainSubstring("secret_admin"))
			Expect(err.Error()).To(MatchRegexp(`the server logged the cause under error reference [0-9a-f]{32}\. Retry the request, and if it keeps failing, give that reference to an operator`))
			Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1), "the full error must be logged exactly once")
			Expect(logs.String()).To(ContainSubstring("method=" + crosscodexv1connect.AdminServiceListHoldsProcedure))
		},
		Entry("plain error", errors.New("list holds: "+secret)),
		Entry("Internal wrapping a store error", connect.NewError(connect.CodeInternal, fmt.Errorf("list holds: %w", errors.New(secret)))),
	)

	It("passes an AdminService error the caller can act on through unchanged", func() {
		url := start(gateway.NewService(gateway.WithLogger(slog.New(slog.NewTextHandler(logs, nil)))),
			&fakeAdminService{err: connect.NewError(connect.CodeInvalidArgument, errors.New("hold_id is required"))})
		client := crosscodexv1connect.NewAdminServiceClient(http.DefaultClient, url)
		_, err := client.ListHolds(context.Background(), connect.NewRequest(&pb.ListHoldsRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("hold_id is required"))
		Expect(logs.String()).To(BeEmpty())
	})

	It("logs a GatewayService handler's withheld error only once", func() {
		svc := gateway.NewService(
			gateway.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
			gateway.WithAdminBackend(&mockAdmin{healthCheckFn: func(context.Context, *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
				return nil, errors.New(secret)
			}}),
		)
		url := start(svc, &fakeAdminService{})
		client := crosscodexv1connect.NewGatewayServiceClient(http.DefaultClient, url)
		_, err := client.Health(context.Background(), connect.NewRequest(&pb.HealthRequest{}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
		Expect(err.Error()).NotTo(ContainSubstring("secret_admin"))
		Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1))
	})
})
