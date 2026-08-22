package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/rpcserver"
	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

type Server struct {
	rpc *rpcserver.Server
}

type ServerConfig struct {
	Addr         string
	TLS          config.TLSConfig
	DrainTimeout time.Duration
	Service      *Service
	Logger       *slog.Logger
}

func NewServer(ctx context.Context, cfg ServerConfig) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = 15 * time.Second
	}

	interceptors := []connect.Interceptor{
		rpcserver.NewRecoveryInterceptor(cfg.Logger),
	}
	if cfg.Service.authn != nil {
		interceptors = append(interceptors, cfg.Service.connectAuthInterceptor())
	}

	otelInterceptor, err := otelconnect.NewInterceptor()
	if err == nil {
		interceptors = append([]connect.Interceptor{otelInterceptor}, interceptors...)
	}

	path, handler := crosscodexv1connect.NewGatewayServiceHandler(
		cfg.Service,
		connect.WithInterceptors(interceptors...),
	)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("GET /api/version", versionHandler)
	mux.HandleFunc("GET /healthz", healthzHandler(cfg.Service))

	var topHandler http.Handler = mux
	topHandler = tlsMiddleware(topHandler)

	// Health probes stay unauthenticated even under mTLS -- the Connect
	// auth interceptor enforces mTLS on every other RPC, so the listener
	// itself only needs to accept (not require) a client cert.
	lis, tlsCfg, err := rpcserver.Listen(ctx, cfg.Addr, cfg.TLS, "gateway-server", tls.VerifyClientCertIfGiven)
	if err != nil {
		return nil, fmt.Errorf("create gateway listener: %w", err)
	}

	return &Server{rpc: rpcserver.New(lis, tlsCfg, topHandler, cfg.Logger)}, nil
}

func (s *Server) Start() error                       { return s.rpc.Start() }
func (s *Server) Addr() string                       { return s.rpc.Addr() }
func (s *Server) Shutdown(ctx context.Context) error { return s.rpc.Shutdown(ctx) }

func versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	info := version.GetInfo()
	_ = json.NewEncoder(w).Encode(info)
}

func healthzHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := svc.Health(r.Context(), connect.NewRequest(&pb.HealthRequest{}))
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"unhealthy","error":%q}`, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp.Msg)
	}
}
