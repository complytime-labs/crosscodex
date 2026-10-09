//go:build !integration

package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"

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
