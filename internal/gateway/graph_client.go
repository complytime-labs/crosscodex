package gateway

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

// ConnectGraphBackend reaches a standalone graph role over mutual TLS. It is
// the gateway role's GraphBackend and CrosswalkBackend, and Cleaning returns
// the EdgeCleaningService handler that forwards to that role.
//
// Every method sends the request message in a new request rather than the
// one it was given: a gateway handler may pass the request it received, and
// forwarding that would pass the caller's HTTP headers (credentials, its
// Content-Encoding for a body this client re-encodes) to the graph role. The
// graph role gets only the headers this client sets.
type ConnectGraphBackend struct {
	graph     crosscodexv1connect.GraphServiceClient
	crosswalk crosscodexv1connect.CrosswalkServiceClient
	cleaning  graphRoleCleaning
}

var (
	_ GraphBackend     = (*ConnectGraphBackend)(nil)
	_ CrosswalkBackend = (*ConnectGraphBackend)(nil)
)

// NewConnectGraphBackend builds a ConnectGraphBackend that dials endpoint,
// resolving TLS via the "graph-client" named target (falls back to the base
// tlsCfg fields when that target isn't configured -- see
// pkg/tlsconfig.BuildTLSConfig). Every call carries the ambient tenant and
// the caller's authn.Identity (connectutil.UnaryClientInterceptor), which the
// graph role checks roles against, and the active span's trace context.
//
// Fails closed if endpoint is empty, and fails closed unless the resolved TLS
// presents a client certificate and carries a CA (mtlsHTTPClient).
func NewConnectGraphBackend(ctx context.Context, endpoint string, tlsCfg config.TLSConfig) (*ConnectGraphBackend, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("graph.endpoint must be set to reach a standalone graph role")
	}
	httpClient, err := mtlsHTTPClient(ctx, tlsCfg, "graph-client", "graph.endpoint")
	if err != nil {
		return nil, err
	}
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("build graph client trace interceptor: %w", err)
	}
	baseURL := "https://" + endpoint
	opts := connect.WithInterceptors(otelInterceptor, connectutil.UnaryClientInterceptor())
	return &ConnectGraphBackend{
		graph:     crosscodexv1connect.NewGraphServiceClient(httpClient, baseURL, opts),
		crosswalk: crosscodexv1connect.NewCrosswalkServiceClient(httpClient, baseURL, opts),
		cleaning:  graphRoleCleaning{client: crosscodexv1connect.NewEdgeCleaningServiceClient(httpClient, baseURL, opts)},
	}, nil
}

// forwardMessage calls call with req's message in a new request, so none of
// req's headers are sent.
func forwardMessage[Req, Res any](ctx context.Context, req *connect.Request[Req],
	call func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
) (*connect.Response[Res], error) {
	return call(ctx, connect.NewRequest(req.Msg))
}

func (b *ConnectGraphBackend) Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	return forwardMessage(ctx, req, b.graph.Traverse)
}

func (b *ConnectGraphBackend) Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	return forwardMessage(ctx, req, b.graph.Query)
}

func (b *ConnectGraphBackend) SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	return forwardMessage(ctx, req, b.graph.SimilaritySearch)
}

func (b *ConnectGraphBackend) GetCrosswalk(ctx context.Context, req *connect.Request[pb.GetCrosswalkRequest]) (*connect.Response[pb.GetCrosswalkResponse], error) {
	return forwardMessage(ctx, req, b.crosswalk.GetCrosswalk)
}

// Cleaning returns the gateway's EdgeCleaningService handler for a split
// deployment (ServerConfig.Cleaning): it forwards each RPC to the graph role,
// whose cleaning service checks the caller's roles and records the caller as
// a decision's reviewer. Its method set is the generated client's.
func (b *ConnectGraphBackend) Cleaning() crosscodexv1connect.EdgeCleaningServiceClient {
	return b.cleaning
}

// graphRoleCleaning forwards EdgeCleaningService RPCs to the graph role.
type graphRoleCleaning struct {
	client crosscodexv1connect.EdgeCleaningServiceClient
}

var _ crosscodexv1connect.EdgeCleaningServiceHandler = graphRoleCleaning{}

func (c graphRoleCleaning) RunCleaning(ctx context.Context, req *connect.Request[pb.RunCleaningRequest]) (*connect.Response[pb.RunCleaningResponse], error) {
	return forwardMessage(ctx, req, c.client.RunCleaning)
}

func (c graphRoleCleaning) ListFindings(ctx context.Context, req *connect.Request[pb.ListFindingsRequest]) (*connect.Response[pb.ListFindingsResponse], error) {
	return forwardMessage(ctx, req, c.client.ListFindings)
}

func (c graphRoleCleaning) GetFinding(ctx context.Context, req *connect.Request[pb.GetFindingRequest]) (*connect.Response[pb.GetFindingResponse], error) {
	return forwardMessage(ctx, req, c.client.GetFinding)
}

func (c graphRoleCleaning) DecideFinding(ctx context.Context, req *connect.Request[pb.DecideFindingRequest]) (*connect.Response[pb.DecideFindingResponse], error) {
	return forwardMessage(ctx, req, c.client.DecideFinding)
}

// needsGraphRole is the refusal for an RPC family that only a graph role
// serves, returned when the gateway has no graph role to reach (the embedded
// daemon). The copy is fixed: the CLI prints it as is.
func needsGraphRole(family string) error {
	return connect.NewError(connect.CodeUnavailable,
		fmt.Errorf("%s needs the crosscodexd daemon (role all, or gateway with a graph role)", family))
}

// noGraphRoleCleaning is the EdgeCleaningService NewServer mounts when
// ServerConfig.Cleaning is nil: every RPC refuses with needsGraphRole.
type noGraphRoleCleaning struct {
	crosscodexv1connect.UnimplementedEdgeCleaningServiceHandler
}

func (noGraphRoleCleaning) RunCleaning(context.Context, *connect.Request[pb.RunCleaningRequest]) (*connect.Response[pb.RunCleaningResponse], error) {
	return nil, needsGraphRole("edge cleaning")
}

func (noGraphRoleCleaning) ListFindings(context.Context, *connect.Request[pb.ListFindingsRequest]) (*connect.Response[pb.ListFindingsResponse], error) {
	return nil, needsGraphRole("edge cleaning")
}

func (noGraphRoleCleaning) GetFinding(context.Context, *connect.Request[pb.GetFindingRequest]) (*connect.Response[pb.GetFindingResponse], error) {
	return nil, needsGraphRole("edge cleaning")
}

func (noGraphRoleCleaning) DecideFinding(context.Context, *connect.Request[pb.DecideFindingRequest]) (*connect.Response[pb.DecideFindingResponse], error) {
	return nil, needsGraphRole("edge cleaning")
}
