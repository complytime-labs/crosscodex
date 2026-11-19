package pipeline

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	pbconnect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/rpcserver"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

// ServerConfig configures a standalone Server for a PipelineService.
type ServerConfig struct {
	Addr    string
	TLS     config.TLSConfig
	Service *Service
	Logger  *slog.Logger

	// ServiceIdentities lists the client certificates (in practice the
	// gateway's) allowed to assert a tenant via request headers; see
	// connectutil.UnaryServerInterceptor. Empty denies every request.
	ServiceIdentities []authn.X509Match
}

// Server exposes a Service over its own Connect RPC listener, independent
// of internal/gateway.Server. Used by the standalone "pipeline" crosscodexd
// role; RoleAll wires a Service directly into gateway.WithPipelineBackend
// instead, with no listener of its own.
type Server struct {
	rpc *rpcserver.Server
}

// NewServer mounts cfg.Service's PipelineServiceHandler on its own
// listener. Every RPC requires mTLS (RequireAndVerifyClientCert) --
// unlike gateway.Server, there is no unauthenticated health path to carve
// out of this listener, since health checks go through a separate
// dedicated listener (see cmd/crosscodexd's newHealthServer), not this
// one. Every request must also carry a tenant header (see
// pkg/tenant/connectutil), and that header is honored only when the mTLS
// client certificate matches cfg.ServiceIdentities: any certificate the CA
// signed could otherwise name any tenant. Pipeline does not re-run end-user
// authentication; the gateway role already did that once, at the perimeter.
func NewServer(ctx context.Context, cfg ServerConfig) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	interceptors := []connect.Interceptor{
		rpcserver.NewRecoveryInterceptor(cfg.Logger),
		connectutil.UnaryServerInterceptor(cfg.ServiceIdentities),
	}
	if otelInterceptor, err := otelconnect.NewInterceptor(); err == nil {
		interceptors = append([]connect.Interceptor{otelInterceptor}, interceptors...)
	}

	path, handler := pbconnect.NewPipelineServiceHandler(
		cfg.Service,
		connect.WithInterceptors(interceptors...),
	)

	mux := http.NewServeMux()
	mux.Handle(path, handler)

	lis, tlsCfg, err := rpcserver.Listen(ctx, cfg.Addr, cfg.TLS, "pipeline-server", tls.RequireAndVerifyClientCert)
	if err != nil {
		return nil, fmt.Errorf("create pipeline listener: %w", err)
	}

	return &Server{rpc: rpcserver.New(lis, tlsCfg, authn.TLSStateMiddleware(mux), cfg.Logger)}, nil
}

func (s *Server) Start() error                       { return s.rpc.Start() }
func (s *Server) Addr() string                       { return s.rpc.Addr() }
func (s *Server) Shutdown(ctx context.Context) error { return s.rpc.Shutdown(ctx) }
