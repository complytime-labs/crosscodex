package testgrpc

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant/grpcutil"
)

// Harness wraps a gRPC server configured for tenant integration testing.
type Harness struct {
	server   *grpc.Server
	listener net.Listener
	opts     harnessOptions
}

type harnessOptions struct {
	addr    string
	tlsCA   string
	tlsCert string
	tlsKey  string
	dbPool  db.TenantConnection
	nats    natsbus.Client
}

// Option configures a Harness.
type Option func(*harnessOptions)

// WithAddress sets the bind address. Default: "localhost:0" (random port).
func WithAddress(addr string) Option {
	return func(o *harnessOptions) { o.addr = addr }
}

// WithTLS enables mutual TLS using the given PEM files.
func WithTLS(ca, cert, key string) Option {
	return func(o *harnessOptions) {
		o.tlsCA = ca
		o.tlsCert = cert
		o.tlsKey = key
	}
}

// WithDB injects a tenant-scoped database connection for handlers.
func WithDB(pool db.TenantConnection) Option {
	return func(o *harnessOptions) { o.dbPool = pool }
}

// WithNATS injects a NATS client for handlers.
func WithNATS(client natsbus.Client) Option {
	return func(o *harnessOptions) { o.nats = client }
}

// NewHarness creates a gRPC test harness with the tenant interceptor
// pre-installed. Call Start() to begin serving.
func NewHarness(opts ...Option) (*Harness, error) {
	o := harnessOptions{
		addr: "localhost:0",
	}
	for _, opt := range opts {
		opt(&o)
	}

	var serverOpts []grpc.ServerOption
	serverOpts = append(serverOpts, grpc.UnaryInterceptor(grpcutil.UnaryServerInterceptor()))

	if o.tlsCA != "" {
		tlsCfg, err := buildServerTLS(o.tlsCA, o.tlsCert, o.tlsKey)
		if err != nil {
			return nil, fmt.Errorf("TLS config: %w", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	srv := grpc.NewServer(serverOpts...)

	// Register the gRPC health service so container healthchecks work
	// in distroless images via grpc_health_probe.
	healthSrv := health.NewServer()
	healthgrpc.RegisterHealthServer(srv, healthSrv)

	return &Harness{
		server: srv,
		opts:   o,
	}, nil
}

// RegisterService registers a gRPC service implementation on the harness.
func (h *Harness) RegisterService(desc *grpc.ServiceDesc, impl any) {
	h.server.RegisterService(desc, impl)
}

// DB returns the injected TenantConnection, or nil if none was configured.
func (h *Harness) DB() db.TenantConnection {
	return h.opts.dbPool
}

// NATS returns the injected NATS client, or nil if none was configured.
func (h *Harness) NATS() natsbus.Client {
	return h.opts.nats
}

// Start begins serving on the configured address.
func (h *Harness) Start() error {
	lis, err := net.Listen("tcp", h.opts.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", h.opts.addr, err)
	}
	h.listener = lis

	go func() {
		_ = h.server.Serve(lis)
	}()

	return nil
}

// Stop gracefully stops the server and closes the listener.
func (h *Harness) Stop() {
	h.server.GracefulStop()
}

// Addr returns the listener address. Only valid after Start().
func (h *Harness) Addr() string {
	if h.listener == nil {
		return ""
	}
	return h.listener.Addr().String()
}

// ClientConn creates a gRPC client connection to the harness.
// If the harness has TLS configured, the client uses the same CA
// and presents the client certificate for mutual TLS.
func (h *Harness) ClientConn(extraOpts ...grpc.DialOption) (*grpc.ClientConn, error) {
	var dialOpts []grpc.DialOption
	dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(grpcutil.UnaryClientInterceptor()))

	if h.opts.tlsCA != "" {
		tlsCfg, err := buildClientTLS(h.opts.tlsCA, h.opts.tlsCert, h.opts.tlsKey)
		if err != nil {
			return nil, fmt.Errorf("client TLS config: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	dialOpts = append(dialOpts, extraOpts...)
	return grpc.NewClient(h.Addr(), dialOpts...)
}

func buildServerTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}

	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 minimum for mTLS test harness
	}, nil
}

func buildClientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12, // DevSkim: ignore DS440001,DS112852 - TLS 1.2 minimum for mTLS test harness
	}, nil
}
