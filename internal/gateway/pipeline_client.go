package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// connectPipelineBackend implements PipelineBackend by calling a
// standalone pipeline role over the network via Connect, instead of an
// in-process pipeline.Service. Used only when gateway and pipeline run as
// separate crosscodexd roles; RoleAll keeps the direct in-process wiring.
type connectPipelineBackend struct {
	client crosscodexv1connect.PipelineServiceClient
}

// NewConnectPipelineBackend builds a PipelineBackend that dials endpoint
// through NewPipelineClient.
func NewConnectPipelineBackend(ctx context.Context, endpoint string, tlsCfg config.TLSConfig) (PipelineBackend, error) {
	client, err := NewPipelineClient(ctx, endpoint, tlsCfg)
	if err != nil {
		return nil, err
	}
	return &connectPipelineBackend{client: client}, nil
}

// NewPipelineClient builds a PipelineService client that dials endpoint,
// resolving TLS via the "pipeline-client" named target (falls back to the
// base tlsCfg fields when that target isn't configured -- see
// pkg/tlsconfig.BuildTLSConfig). Every call carries the ambient tenant, the
// caller's identity when ctx has one (connectutil.UnaryClientInterceptor), and
// the active span's trace context.
//
// Fails closed if endpoint is empty, and fails closed unless the resolved TLS
// presents a client certificate and carries a CA (mtlsHTTPClient).
func NewPipelineClient(ctx context.Context, endpoint string, tlsCfg config.TLSConfig) (crosscodexv1connect.PipelineServiceClient, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("pipeline.endpoint must be set to reach a standalone pipeline role")
	}
	httpClient, err := mtlsHTTPClient(ctx, tlsCfg, "pipeline-client", "pipeline.endpoint")
	if err != nil {
		return nil, err
	}
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("build pipeline client trace interceptor: %w", err)
	}
	return crosscodexv1connect.NewPipelineServiceClient(
		httpClient,
		"https://"+endpoint,
		connect.WithInterceptors(otelInterceptor, connectutil.UnaryClientInterceptor()),
	), nil
}

// mtlsHTTPClient builds the HTTP client a gateway uses to reach a standalone
// role, resolving TLS via the named target (pipeline-client, graph-client).
// endpointKey is the config key holding that role's address
// (pipeline.endpoint, graph.endpoint); the role is its first segment.
//
// Fails closed unless the resolved TLS presents a client certificate and
// carries a CA to verify the role's server certificate. The standalone roles
// enforce mTLS (RequireAndVerifyClientCert) on every RPC and have no
// plaintext fallback, so a client that couldn't present a certificate would
// only fail at the first RPC. Rejecting it here mirrors the roles'
// server-side precondition: both halves of a split refuse to start without
// genuine mutual TLS rather than degrading to cleartext.
func mtlsHTTPClient(ctx context.Context, tlsCfg config.TLSConfig, target, endpointKey string) (*http.Client, error) {
	role, _, _ := strings.Cut(endpointKey, ".")
	resolvedTLS, err := tlsconfig.BuildTLSConfig(ctx, tlsCfg, target)
	if err != nil {
		return nil, fmt.Errorf("build %s client TLS config: %w", role, err)
	}
	if resolvedTLS == nil ||
		(len(resolvedTLS.Certificates) == 0 && resolvedTLS.GetClientCertificate == nil) ||
		resolvedTLS.RootCAs == nil {
		return nil, fmt.Errorf(`%s requires mutual TLS: set tls.mode (or tls.targets.%s.mode) to "mutual" with a client cert, key, and CA — the standalone %s role enforces mTLS on every RPC and has no plaintext fallback`,
			endpointKey, target, role)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: resolvedTLS}}, nil
}

func (b *connectPipelineBackend) CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
	return b.client.CreateJob(ctx, req)
}

func (b *connectPipelineBackend) GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
	return b.client.GetJob(ctx, req)
}

func (b *connectPipelineBackend) ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
	return b.client.ListJobs(ctx, req)
}

func (b *connectPipelineBackend) CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	return b.client.CancelJob(ctx, req)
}

func (b *connectPipelineBackend) RetryJob(ctx context.Context, req *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error) {
	return b.client.RetryJob(ctx, req)
}

var _ PipelineBackend = (*connectPipelineBackend)(nil)
