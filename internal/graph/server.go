package graph

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	pbconnect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/rpcserver"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

// errReadOnlyListener is the fixed refusal for every GraphService RPC the
// listener does not forward.
var errReadOnlyListener = errors.New("graph writes go through materialization; this listener serves the gateway's read RPCs only")

// ServerConfig configures the standalone graph role's Server.
type ServerConfig struct {
	Addr    string
	TLS     config.TLSConfig
	Service *Service

	// Cleaning serves EdgeCleaningService on the same listener. Nil mounts
	// no cleaning routes, so those RPCs get Connect's 404 (Unimplemented).
	Cleaning pbconnect.EdgeCleaningServiceHandler

	// Feedback serves FeedbackService (votes, the review queue and
	// determinations) on the same listener, for the gateway role to forward
	// to. Nil mounts no feedback routes, so those RPCs get Connect's 404
	// (Unimplemented).
	Feedback pbconnect.FeedbackServiceHandler

	// ServiceIdentities lists the client certificates (in practice the
	// gateway's) allowed to assert a tenant and end user via request
	// headers; see connectutil.UnaryServerInterceptor. Empty denies every
	// request.
	ServiceIdentities []authn.X509Match

	Logger *slog.Logger
}

// Server exposes the graph role's read RPCs to the gateway over its own
// Connect RPC listener. Used by the standalone "graph" crosscodexd role;
// RoleAll wires a Service directly into the gateway instead, with no
// listener of its own.
type Server struct {
	rpc *rpcserver.Server
}

// NewServer mounts a read-only GraphService (Traverse, Query and
// SimilaritySearch), CrosswalkService and, when set, cfg.Cleaning's
// EdgeCleaningService and cfg.Feedback's FeedbackService on one listener. Graph writes stay with the NATS
// materializers, so every other GraphService RPC returns CodeUnimplemented.
//
// Every RPC requires mTLS (RequireAndVerifyClientCert); health checks use
// cmd/crosscodexd's separate health listener. The tenant and end-user
// headers are honored only when the client certificate matches
// cfg.ServiceIdentities, since any certificate the CA signed could
// otherwise name any tenant or user. The graph role does not re-run end-user
// authentication; the gateway already did that at the perimeter, and the
// Service's own role checks apply to the user it asserts.
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
	opts := connect.WithInterceptors(interceptors...)

	mux := http.NewServeMux()
	mux.Handle(pbconnect.NewGraphServiceHandler(readOnlyGraphHandler{svc: cfg.Service}, opts))
	mux.Handle(pbconnect.NewCrosswalkServiceHandler(cfg.Service, opts))
	if cfg.Cleaning != nil {
		mux.Handle(pbconnect.NewEdgeCleaningServiceHandler(cfg.Cleaning, opts))
	}
	if cfg.Feedback != nil {
		mux.Handle(pbconnect.NewFeedbackServiceHandler(cfg.Feedback, opts))
	}

	lis, tlsCfg, err := rpcserver.Listen(ctx, cfg.Addr, cfg.TLS, "graph-server", tls.RequireAndVerifyClientCert)
	if err != nil {
		return nil, fmt.Errorf("create graph listener: %w", err)
	}

	return &Server{rpc: rpcserver.New(lis, tlsCfg, authn.TLSStateMiddleware(mux), cfg.Logger)}, nil
}

func (s *Server) Start() error                       { return s.rpc.Start() }
func (s *Server) Addr() string                       { return s.rpc.Addr() }
func (s *Server) Shutdown(ctx context.Context) error { return s.rpc.Shutdown(ctx) }

// readOnlyGraphHandler forwards the gateway's GraphService reads to svc and
// refuses the rest. Embedding the Unimplemented handler keeps an RPC added
// to GraphService later refused until it is deliberately forwarded here.
type readOnlyGraphHandler struct {
	pbconnect.UnimplementedGraphServiceHandler
	svc *Service
}

func (h readOnlyGraphHandler) Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	return h.svc.Traverse(ctx, req)
}

func (h readOnlyGraphHandler) Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	return h.svc.Query(ctx, req)
}

func (h readOnlyGraphHandler) SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	return h.svc.SimilaritySearch(ctx, req)
}

func (readOnlyGraphHandler) CreateNode(context.Context, *connect.Request[pb.CreateNodeRequest]) (*connect.Response[pb.CreateNodeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}

func (readOnlyGraphHandler) CreateEdge(context.Context, *connect.Request[pb.CreateEdgeRequest]) (*connect.Response[pb.CreateEdgeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}

func (readOnlyGraphHandler) BulkCreateEdges(context.Context, *connect.Request[pb.BulkCreateEdgesRequest]) (*connect.Response[pb.BulkCreateEdgesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}

func (readOnlyGraphHandler) SupersedeFact(context.Context, *connect.Request[pb.SupersedeFactRequest]) (*connect.Response[pb.SupersedeFactResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}

func (readOnlyGraphHandler) GetNode(context.Context, *connect.Request[pb.GetNodeRequest]) (*connect.Response[pb.GetNodeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}

func (readOnlyGraphHandler) GetEdge(context.Context, *connect.Request[pb.GetEdgeRequest]) (*connect.Response[pb.GetEdgeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}

func (readOnlyGraphHandler) TemporalQuery(context.Context, *connect.Request[pb.TemporalQueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errReadOnlyListener)
}
