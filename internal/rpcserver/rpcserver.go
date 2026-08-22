// Package rpcserver provides the Connect-RPC server plumbing shared by
// every crosscodexd role that listens on its own HTTP port: a
// panic-recovery interceptor, TLS listener bring-up via pkg/tlsconfig, and
// a minimal Start/Addr/Shutdown wrapper. internal/gateway.Server and
// internal/pipeline.Server both build on this instead of duplicating it.
package rpcserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"connectrpc.com/connect"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// NewRecoveryInterceptor returns a connect.Interceptor that recovers from
// panics in unary and streaming handlers, logs them, and converts them into
// a connect.CodeInternal error instead of crashing the process.
func NewRecoveryInterceptor(logger *slog.Logger) connect.Interceptor {
	return &recoveryInterceptor{logger: logger}
}

type recoveryInterceptor struct {
	logger *slog.Logger
}

func (i *recoveryInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
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

func (i *recoveryInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *recoveryInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
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

// Listen opens a TCP listener on addr and resolves the TLS config for the
// named target (see pkg/tlsconfig.BuildTLSConfig -- an unconfigured target
// falls back to cfg's base fields). Returns a nil *tls.Config when the
// resolved TLS mode is "off" or empty, signaling the caller to serve
// plaintext HTTP/1.1+h2c instead. clientAuth is applied to the resolved TLS
// config when it is non-nil, letting callers require or merely accept
// client certificates on top of whatever pkg/tlsconfig produces.
func Listen(ctx context.Context, addr string, tlsCfg config.TLSConfig, target string, clientAuth tls.ClientAuthType) (net.Listener, *tls.Config, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	resolved, err := tlsconfig.BuildTLSConfig(ctx, tlsCfg, target)
	if err != nil {
		lis.Close()
		return nil, nil, fmt.Errorf("build TLS config for target %q: %w", target, err)
	}
	if resolved != nil {
		resolved.ClientAuth = clientAuth
	}

	return lis, resolved, nil
}

// Server is a minimal HTTP(S) server wrapper shared by every crosscodexd
// role that mounts a Connect handler on its own listener.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	logger     *slog.Logger
}

// New wraps listener and handler into a Server. A nil tlsConfig serves
// plaintext HTTP/1.1 and unencrypted h2c; a non-nil tlsConfig serves TLS.
func New(listener net.Listener, tlsConfig *tls.Config, handler http.Handler, logger *slog.Logger) *Server {
	httpSrv := &http.Server{Handler: handler}
	if tlsConfig != nil {
		httpSrv.TLSConfig = tlsConfig
	} else {
		p := new(http.Protocols)
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		httpSrv.Protocols = p
	}
	return &Server{httpServer: httpSrv, listener: listener, logger: logger}
}

// Start begins serving on a background goroutine and returns immediately --
// the listener is already bound by Listen, so Start only kicks off
// Serve/ServeTLS.
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

// Addr returns the listener's bound address (useful when addr was ":0"),
// or "" if the server has no listener.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Shutdown gracefully stops the server, waiting for in-flight requests to
// complete or ctx to be done, whichever comes first.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
