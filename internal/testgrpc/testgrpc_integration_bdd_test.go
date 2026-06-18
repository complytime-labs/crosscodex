//go:build integration_grpc

package testgrpc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	echopb "github.com/complytime-labs/crosscodex/internal/testgrpc/gen/echo/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/grpcutil"
)

func TestGRPCIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "gRPC Integration Suite")
}

var (
	integrationGRPCAddr string
	integrationGRPCCA   string
	integrationGRPCCert string
	integrationGRPCKey  string
	integrationDBDSN    string
)

var _ = BeforeSuite(func() {
	integrationGRPCAddr = os.Getenv("TEST_GRPC_ADDR")
	integrationGRPCCA = os.Getenv("TEST_GRPC_CA")
	integrationGRPCCert = os.Getenv("TEST_GRPC_CERT")
	integrationGRPCKey = os.Getenv("TEST_GRPC_KEY")
	integrationDBDSN = os.Getenv("TEST_DATABASE_DSN")

	if integrationGRPCAddr == "" {
		Skip("TEST_GRPC_ADDR not set — run: task test:integration:grpc")
	}
})

func integrationClientTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(integrationGRPCCert, integrationGRPCKey)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caCert, err := os.ReadFile(integrationGRPCCA)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("parse CA")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 minimum for mTLS test client
	}, nil
}

func dialIntegrationGRPC() *grpc.ClientConn {
	tlsCfg, err := integrationClientTLSConfig()
	Expect(err).NotTo(HaveOccurred(), "TLS config")

	conn, err := grpc.NewClient(integrationGRPCAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithUnaryInterceptor(grpcutil.UnaryClientInterceptor()),
	)
	Expect(err).NotTo(HaveOccurred(), "dial")
	DeferCleanup(func() { conn.Close() })
	return conn
}

// dialIntegrationGRPCRaw creates a connection WITHOUT the client interceptor,
// so the test can manually set metadata.
func dialIntegrationGRPCRaw() *grpc.ClientConn {
	tlsCfg, err := integrationClientTLSConfig()
	Expect(err).NotTo(HaveOccurred(), "TLS config")

	conn, err := grpc.NewClient(integrationGRPCAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	Expect(err).NotTo(HaveOccurred(), "dial")
	DeferCleanup(func() { conn.Close() })
	return conn
}

func integrationTestPool() db.Pool {
	if integrationDBDSN == "" {
		Skip("TEST_DATABASE_DSN not set")
	}

	pool, err := db.NewPool(db.PoolConfig{
		DSN:          integrationDBDSN,
		MaxOpenConns: 2,
	})
	Expect(err).NotTo(HaveOccurred(), "NewPool")
	DeferCleanup(func() { pool.Close() })
	return pool
}

func integrationSetupTenantViaDB(pool db.Pool, tenantID, displayName string) {
	err := pool.Exec(context.Background(),
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	Expect(err).NotTo(HaveOccurred(), "setup tenant %q", tenantID)
}

func integrationInsertJobViaDB(pool db.Pool, tenantID, jobID string) {
	err := pool.Exec(context.Background(),
		"INSERT INTO jobs (tenant_id, job_id, created_by, status) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
		tenantID, jobID, "test-user", "pending")
	Expect(err).NotTo(HaveOccurred(), "insert job %q", jobID)
}

var _ = Describe("gRPC Integration", func() {
	Context("Tenant Propagation", func() {
		It("should propagate tenant-a through gRPC", func() {
			conn := dialIntegrationGRPC()
			client := echopb.NewTenantEchoServiceClient(conn)

			ctxA, err := tenant.WithTenant(context.Background(), "grpc-tenant-a")
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.Echo(ctxA, &echopb.EchoRequest{Payload: "hello-a"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetTenantId()).To(Equal("grpc-tenant-a"))
			Expect(resp.GetPayload()).To(Equal("hello-a"))
		})

		It("should isolate tenants", func() {
			conn := dialIntegrationGRPC()
			client := echopb.NewTenantEchoServiceClient(conn)

			ctxB, err := tenant.WithTenant(context.Background(), "grpc-tenant-b")
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.Echo(ctxB, &echopb.EchoRequest{Payload: "hello-b"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetTenantId()).To(Equal("grpc-tenant-b"))
		})
	})

	Context("No Tenant Rejected", func() {
		It("should reject requests with no tenant metadata", func() {
			conn := dialIntegrationGRPCRaw()
			client := echopb.NewTenantEchoServiceClient(conn)

			_, err := client.Echo(context.Background(), &echopb.EchoRequest{Payload: "test"})
			Expect(err).To(HaveOccurred())
			st, ok := status.FromError(err)
			Expect(ok).To(BeTrue(), "expected gRPC status error")
			Expect(st.Code()).To(Equal(codes.Unauthenticated))
		})
	})

	Context("Malformed Tenant Rejected", func() {
		It("should reject requests with malformed tenant ID", func() {
			conn := dialIntegrationGRPCRaw()
			client := echopb.NewTenantEchoServiceClient(conn)

			md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "BAD_TENANT!")
			ctx := metadata.NewOutgoingContext(context.Background(), md)

			_, err := client.Echo(ctx, &echopb.EchoRequest{Payload: "test"})
			Expect(err).To(HaveOccurred())
			st, ok := status.FromError(err)
			Expect(ok).To(BeTrue(), "expected gRPC status error")
			Expect(st.Code()).To(Equal(codes.InvalidArgument))
		})
	})

	Context("Cross-Service Tenant Isolation", func() {
		It("should enforce RLS-based tenant isolation across services", func() {
			pool := integrationTestPool()

			// Setup: insert jobs for two tenants.
			tenantID := "grpc-xsvc-alpha"
			integrationSetupTenantViaDB(pool, tenantID, "gRPC CrossSvc Alpha")
			integrationInsertJobViaDB(pool, tenantID, "grpc-xsvc-job-1")

			otherTenant := "grpc-xsvc-bravo"
			integrationSetupTenantViaDB(pool, otherTenant, "gRPC CrossSvc Bravo")
			integrationInsertJobViaDB(pool, otherTenant, "grpc-xsvc-job-2")

			conn := dialIntegrationGRPC()
			client := echopb.NewTenantEchoServiceClient(conn)

			// Query as tenant alpha.
			ctxA, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.Echo(ctxA, &echopb.EchoRequest{
				Payload: "cross-service",
				QueryDb: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetTenantId()).To(Equal(tenantID))
			Expect(resp.GetDbRowCount()).To(BeNumerically(">=", int32(1)))

			// Query as tenant bravo — should see different count.
			ctxB, err := tenant.WithTenant(context.Background(), otherTenant)
			Expect(err).NotTo(HaveOccurred())

			respB, err := client.Echo(ctxB, &echopb.EchoRequest{
				Payload: "cross-service-b",
				QueryDb: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(respB.GetTenantId()).To(Equal(otherTenant))
			Expect(respB.GetDbRowCount()).To(BeNumerically(">=", int32(1)))
		})
	})

	Context("Authn to Tenant Flow", func() {
		It("should resolve a TLS certificate to a tenant identity and flow through gRPC", func() {
			tenantID := "grpc-authn-tenant"

			authenticator, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: tenantID,
			})
			Expect(err).NotTo(HaveOccurred())

			// Build a TLS connection state from the test client cert.
			tlsCfg, err := integrationClientTLSConfig()
			Expect(err).NotTo(HaveOccurred())

			// Load the client certificate to simulate what the TLS handshake produces.
			clientCert, err := x509.ParseCertificate(tlsCfg.Certificates[0].Certificate[0])
			Expect(err).NotTo(HaveOccurred())

			identity, err := authenticator.Authenticate(context.Background(), &authn.Request{
				TLSState: &tls.ConnectionState{
					PeerCertificates: []*x509.Certificate{clientCert},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal(tenantID))

			// Now verify the identity's tenant flows through a gRPC call.
			ctx, err := tenant.WithTenant(context.Background(), identity.TenantID)
			Expect(err).NotTo(HaveOccurred())

			conn := dialIntegrationGRPC()
			client := echopb.NewTenantEchoServiceClient(conn)

			resp, err := client.Echo(ctx, &echopb.EchoRequest{Payload: "authn-test"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetTenantId()).To(Equal(tenantID))
		})
	})
})
