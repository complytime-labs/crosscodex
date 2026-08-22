package gateway

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

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

// NewConnectPipelineBackend builds a PipelineBackend that dials endpoint,
// resolving TLS via the "pipeline-client" named target (falls back to the
// base tlsCfg fields when that target isn't configured -- see
// pkg/tlsconfig.BuildTLSConfig).
//
// Fails closed if endpoint is empty, and fails closed unless the resolved TLS
// presents a client certificate and carries a CA to verify the pipeline's
// server certificate. The standalone pipeline role enforces mTLS
// (RequireAndVerifyClientCert) on every RPC and has no plaintext fallback, so
// a client that couldn't present a certificate would only fail at the first
// RPC. Rejecting it here mirrors attachPipeline's server-side precondition:
// both halves of the gateway/pipeline split refuse to start without genuine
// mutual TLS rather than degrading to cleartext.
func NewConnectPipelineBackend(ctx context.Context, endpoint string, tlsCfg config.TLSConfig) (PipelineBackend, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("pipeline.endpoint must be set to reach a standalone pipeline role")
	}

	resolvedTLS, err := tlsconfig.BuildTLSConfig(ctx, tlsCfg, "pipeline-client")
	if err != nil {
		return nil, fmt.Errorf("build pipeline client TLS config: %w", err)
	}
	if resolvedTLS == nil ||
		(len(resolvedTLS.Certificates) == 0 && resolvedTLS.GetClientCertificate == nil) ||
		resolvedTLS.RootCAs == nil {
		return nil, fmt.Errorf(`pipeline.endpoint requires mutual TLS: set tls.mode (or tls.targets.pipeline-client.mode) to "mutual" with a client cert, key, and CA — the standalone pipeline role enforces mTLS on every RPC and has no plaintext fallback`)
	}

	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: resolvedTLS}}
	client := crosscodexv1connect.NewPipelineServiceClient(
		httpClient,
		"https://"+endpoint,
		connect.WithInterceptors(connectutil.UnaryClientInterceptor()),
	)
	return &connectPipelineBackend{client: client}, nil
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

var _ PipelineBackend = (*connectPipelineBackend)(nil)
