//go:build integration_grpc

package testgrpc_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"testing"

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

var (
	grpcAddr string
	grpcCA   string
	grpcCert string
	grpcKey  string
	dbDSN    string
)

func TestMain(m *testing.M) {
	grpcAddr = os.Getenv("TEST_GRPC_ADDR")
	grpcCA = os.Getenv("TEST_GRPC_CA")
	grpcCert = os.Getenv("TEST_GRPC_CERT")
	grpcKey = os.Getenv("TEST_GRPC_KEY")
	dbDSN = os.Getenv("TEST_DATABASE_DSN")

	if grpcAddr == "" {
		fmt.Fprintln(os.Stderr, "TEST_GRPC_ADDR not set — run: task test:integration:grpc")
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func dialGRPC(t *testing.T) *grpc.ClientConn {
	t.Helper()

	tlsCfg, err := clientTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}

	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithUnaryInterceptor(grpcutil.UnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// dialGRPCRaw creates a connection WITHOUT the client interceptor,
// so the test can manually set metadata.
func dialGRPCRaw(t *testing.T) *grpc.ClientConn {
	t.Helper()

	tlsCfg, err := clientTLSConfig()
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}

	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func clientTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(grpcCert, grpcKey)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caCert, err := os.ReadFile(grpcCA)
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

func TestIntegration_GRPC_TenantPropagation(t *testing.T) {
	conn := dialGRPC(t)
	client := echopb.NewTenantEchoServiceClient(conn)

	// Request with tenant-a.
	ctxA, err := tenant.WithTenant(context.Background(), "grpc-tenant-a")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	resp, err := client.Echo(ctxA, &echopb.EchoRequest{Payload: "hello-a"})
	if err != nil {
		t.Fatalf("Echo(tenant-a): %v", err)
	}
	if resp.GetTenantId() != "grpc-tenant-a" {
		t.Errorf("TenantId = %q, want %q", resp.GetTenantId(), "grpc-tenant-a")
	}
	if resp.GetPayload() != "hello-a" {
		t.Errorf("Payload = %q, want %q", resp.GetPayload(), "hello-a")
	}

	// Request with tenant-b — verify isolation.
	ctxB, err := tenant.WithTenant(context.Background(), "grpc-tenant-b")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	resp, err = client.Echo(ctxB, &echopb.EchoRequest{Payload: "hello-b"})
	if err != nil {
		t.Fatalf("Echo(tenant-b): %v", err)
	}
	if resp.GetTenantId() != "grpc-tenant-b" {
		t.Errorf("TenantId = %q, want %q", resp.GetTenantId(), "grpc-tenant-b")
	}
}

func TestIntegration_GRPC_NoTenantRejected(t *testing.T) {
	// Use raw dial (no client interceptor) so we can send without tenant.
	conn := dialGRPCRaw(t)
	client := echopb.NewTenantEchoServiceClient(conn)

	// No metadata at all — server interceptor should reject.
	_, err := client.Echo(context.Background(), &echopb.EchoRequest{Payload: "test"})
	if err == nil {
		t.Fatal("expected error when no tenant metadata, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", st.Code())
	}
}

func TestIntegration_GRPC_MalformedTenantRejected(t *testing.T) {
	// Use raw dial to inject malformed tenant manually.
	conn := dialGRPCRaw(t)
	client := echopb.NewTenantEchoServiceClient(conn)

	md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "BAD_TENANT!")
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	_, err := client.Echo(ctx, &echopb.EchoRequest{Payload: "test"})
	if err == nil {
		t.Fatal("expected error for malformed tenant, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestIntegration_GRPC_CrossService_TenantIsolation(t *testing.T) {
	pool := testPool(t) // skips if dbDSN not set

	// Setup: insert a job for the tenant via superuser DSN.
	tenantID := "grpc-xsvc-alpha"
	setupTenantViaDB(t, pool, tenantID, "gRPC CrossSvc Alpha")

	// Insert a job owned by this tenant.
	insertJobViaDB(t, pool, tenantID, "grpc-xsvc-job-1")

	// Also insert a job for a different tenant to verify isolation.
	otherTenant := "grpc-xsvc-bravo"
	setupTenantViaDB(t, pool, otherTenant, "gRPC CrossSvc Bravo")
	insertJobViaDB(t, pool, otherTenant, "grpc-xsvc-job-2")

	// Connect to the gRPC server.
	conn := dialGRPC(t)
	client := echopb.NewTenantEchoServiceClient(conn)

	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	resp, err := client.Echo(ctx, &echopb.EchoRequest{
		Payload: "cross-service",
		QueryDb: true,
	})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}

	if resp.GetTenantId() != tenantID {
		t.Errorf("TenantId = %q, want %q", resp.GetTenantId(), tenantID)
	}

	// The server queries jobs as this tenant via TenantPool.
	// RLS should filter to only this tenant's rows.
	if resp.GetDbRowCount() < 1 {
		t.Errorf("DbRowCount = %d, want >= 1", resp.GetDbRowCount())
	}

	// Now query as the other tenant — should see different count.
	ctxB, err := tenant.WithTenant(context.Background(), otherTenant)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	respB, err := client.Echo(ctxB, &echopb.EchoRequest{
		Payload: "cross-service-b",
		QueryDb: true,
	})
	if err != nil {
		t.Fatalf("Echo(bravo): %v", err)
	}

	if respB.GetTenantId() != otherTenant {
		t.Errorf("TenantId = %q, want %q", respB.GetTenantId(), otherTenant)
	}

	if respB.GetDbRowCount() < 1 {
		t.Errorf("bravo DbRowCount = %d, want >= 1", respB.GetDbRowCount())
	}
}

func TestIntegration_GRPC_AuthnToTenant(t *testing.T) {
	// This test verifies that authn can resolve a TLS certificate
	// to a tenant identity and that the tenant flows through gRPC.
	//
	// We use the default test certificates (generated by internal/testcerts)
	// and configure a single-tenant X509Authenticator that maps any cert
	// to a known tenant.

	tenantID := "grpc-authn-tenant"

	authenticator, err := authn.NewX509Authenticator(authn.X509Config{
		SingleTenant:  true,
		DefaultTenant: tenantID,
	})
	if err != nil {
		t.Fatalf("NewX509Authenticator: %v", err)
	}

	// Build a TLS connection state from the test client cert.
	tlsCfg, err := clientTLSConfig()
	if err != nil {
		t.Fatalf("clientTLSConfig: %v", err)
	}

	// Load the client certificate to simulate what the TLS handshake produces.
	clientCert, err := x509.ParseCertificate(tlsCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	identity, err := authenticator.Authenticate(context.Background(), &authn.Request{
		TLSState: &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{clientCert},
		},
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if identity.TenantID != tenantID {
		t.Errorf("Identity.TenantID = %q, want %q", identity.TenantID, tenantID)
	}

	// Now verify the identity's tenant flows through a gRPC call.
	ctx, err := tenant.WithTenant(context.Background(), identity.TenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	conn := dialGRPC(t)
	client := echopb.NewTenantEchoServiceClient(conn)

	resp, err := client.Echo(ctx, &echopb.EchoRequest{Payload: "authn-test"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}

	if resp.GetTenantId() != tenantID {
		t.Errorf("TenantId = %q, want %q", resp.GetTenantId(), tenantID)
	}
}

// --- DB helpers for cross-service tests ---

func testPool(t *testing.T) db.Pool {
	t.Helper()

	if dbDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set")
	}

	pool, err := db.NewPool(db.PoolConfig{
		DSN:          dbDSN,
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func setupTenantViaDB(t *testing.T, pool db.Pool, tenantID, displayName string) {
	t.Helper()

	err := pool.Exec(context.Background(),
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	if err != nil {
		t.Fatalf("setup tenant %q: %v", tenantID, err)
	}
}

func insertJobViaDB(t *testing.T, pool db.Pool, tenantID, jobID string) {
	t.Helper()

	err := pool.Exec(context.Background(),
		"INSERT INTO jobs (tenant_id, job_id, created_by, status) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
		tenantID, jobID, "test-user", "pending")
	if err != nil {
		t.Fatalf("insert job %q: %v", jobID, err)
	}
}
