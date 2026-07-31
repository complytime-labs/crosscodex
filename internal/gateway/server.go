package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

type Server struct {
	httpServer *http.Server
	listener   net.Listener
	logger     *slog.Logger
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
		newConnectRecoveryInterceptor(cfg.Logger),
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

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	httpSrv := &http.Server{
		Handler: topHandler,
	}

	if cfg.TLS.Cert != "" {
		resolver := tlsconfig.Resolver{}
		tlsCfg, err := resolver.BuildTLSConfig(ctx, cfg.TLS, "gateway-server")
		if err != nil {
			lis.Close()
			return nil, fmt.Errorf("build TLS config: %w", err)
		}
		// Allow unauthenticated health probes; the Connect auth interceptor enforces mTLS on all other RPCs.
		tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven
		httpSrv.TLSConfig = tlsCfg
	} else {
		p := new(http.Protocols)
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		httpSrv.Protocols = p
	}

	return &Server{
		httpServer: httpSrv,
		listener:   lis,
		logger:     cfg.Logger,
	}, nil
}

func (s *Server) Start() error {
	go func() {
		s.logger.Info("server listening", "addr", s.listener.Addr().String())
		var err error
		if s.httpServer.TLSConfig != nil {
			err = s.httpServer.ServeTLS(s.listener, "", "")
		} else {
			err = s.httpServer.Serve(s.listener)
		}
		if err != nil && err != http.ErrServerClosed {
			s.logger.Error("server error", "error", err)
		}
	}()
	return nil
}

func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

type connectRecoveryInterceptor struct {
	logger *slog.Logger
}

func newConnectRecoveryInterceptor(logger *slog.Logger) connect.Interceptor {
	return &connectRecoveryInterceptor{logger: logger}
}

func (i *connectRecoveryInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				i.logger.ErrorContext(ctx, "panic in handler",
					"procedure", req.Spec().Procedure,
					"panic", fmt.Sprintf("%v", r),
				)
				err = connect.NewError(connect.CodeInternal, errors.New("internal server error"))
			}
		}()
		return next(ctx, req)
	}
}

func (i *connectRecoveryInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *connectRecoveryInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) (err error) {
		defer func() {
			if r := recover(); r != nil {
				i.logger.ErrorContext(ctx, "panic in handler",
					"procedure", conn.Spec().Procedure,
					"panic", fmt.Sprintf("%v", r),
				)
				err = connect.NewError(connect.CodeInternal, errors.New("internal server error"))
			}
		}()
		return next(ctx, conn)
	}
}

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
