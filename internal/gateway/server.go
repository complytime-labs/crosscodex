package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type Server struct {
	grpcServer *grpc.Server
	httpServer *http.Server
	grpcLis    net.Listener
	httpLis    net.Listener
	logger     *slog.Logger
}

type ServerConfig struct {
	GRPCAddr     string
	HTTPAddr     string
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

	var serverCreds grpc.ServerOption
	if cfg.TLS.Cert != "" {
		resolver := tlsconfig.Resolver{}
		tlsCfg, err := resolver.BuildTLSConfig(ctx, cfg.TLS, "grpc-server")
		if err != nil {
			return nil, fmt.Errorf("build TLS config: %w", err)
		}
		serverCreds = grpc.Creds(credentials.NewTLS(tlsCfg))
	} else {
		serverCreds = grpc.Creds(insecure.NewCredentials())
	}

	grpcSrv := grpc.NewServer(
		serverCreds,
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(
			cfg.Service.authInterceptor(),
			recoveryInterceptor(cfg.Logger),
		),
	)

	pb.RegisterGatewayServiceServer(grpcSrv, cfg.Service)

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("listen gRPC %s: %w", cfg.GRPCAddr, err)
	}

	gwMux := runtime.NewServeMux()

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if cfg.TLS.Cert != "" {
		resolver := tlsconfig.Resolver{}
		clientTLS, err := resolver.BuildTLSConfig(ctx, cfg.TLS, "grpc-client")
		if err != nil {
			grpcLis.Close()
			return nil, fmt.Errorf("build client TLS config: %w", err)
		}
		dialOpts = []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(clientTLS))}
	}

	err = pb.RegisterGatewayServiceHandlerFromEndpoint(ctx, gwMux, grpcLis.Addr().String(), dialOpts)
	if err != nil {
		grpcLis.Close()
		return nil, fmt.Errorf("register grpc-gateway handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", gwMux)
	mux.HandleFunc("GET /api/version", versionHandler)
	mux.HandleFunc("GET /healthz", healthzHandler(cfg.Service))

	httpLis, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		grpcLis.Close()
		return nil, fmt.Errorf("listen HTTP %s: %w", cfg.HTTPAddr, err)
	}

	httpSrv := &http.Server{
		Handler: mux,
	}

	return &Server{
		grpcServer: grpcSrv,
		httpServer: httpSrv,
		grpcLis:    grpcLis,
		httpLis:    httpLis,
		logger:     cfg.Logger,
	}, nil
}

func (s *Server) Start() error {
	go func() {
		s.logger.Info("gRPC server listening", "addr", s.grpcLis.Addr().String())
		if err := s.grpcServer.Serve(s.grpcLis); err != nil {
			s.logger.Error("gRPC server error", "error", err)
		}
	}()

	go func() {
		s.logger.Info("HTTP server listening", "addr", s.httpLis.Addr().String())
		if err := s.httpServer.Serve(s.httpLis); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", "error", err)
		}
	}()

	return nil
}

func (s *Server) GRPCAddr() string {
	if s.grpcLis == nil {
		return ""
	}
	return s.grpcLis.Addr().String()
}

func (s *Server) HTTPAddr() string {
	if s.httpLis == nil {
		return ""
	}
	return s.httpLis.Addr().String()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.logger.Warn("HTTP shutdown error", "error", err)
	}

	stopped := make(chan struct{})
	go func() {
		s.grpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		s.logger.Info("gRPC server stopped gracefully")
	case <-ctx.Done():
		s.logger.Warn("gRPC drain timeout exceeded, forcing stop")
		s.grpcServer.Stop()
	}

	return nil
}

func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in gRPC handler",
					"method", info.FullMethod,
					"panic", fmt.Sprintf("%v", r),
				)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}

func versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	info := version.GetInfo()
	_ = json.NewEncoder(w).Encode(info)
}

func healthzHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := svc.Health(r.Context(), &pb.HealthRequest{})
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"unhealthy","error":%q}`, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
