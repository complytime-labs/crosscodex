//go:build !integration

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/authn"
)

// testAuthInterceptor injects a fixed identity into server-side contexts,
// bypassing mTLS-based authentication for unit tests.
type testAuthInterceptor struct {
	identity *authn.Identity
}

func (t *testAuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if t.identity != nil && !req.Spec().IsClient {
			ctx = authn.WithIdentity(ctx, t.identity)
		}
		return next(ctx, req)
	}
}

func (t *testAuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (t *testAuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if t.identity != nil {
			ctx = authn.WithIdentity(ctx, t.identity)
		}
		return next(ctx, conn)
	}
}

// streamTestEnv bundles an httptest server, a Connect client, and cleanup.
type streamTestEnv struct {
	server *httptest.Server
	client crosscodexv1connect.GatewayServiceClient
}

func (e *streamTestEnv) close() {
	e.server.Close()
}

// newStreamTestEnv spins up an httptest server backed by a gateway Service
// with the given options.  A test auth interceptor is installed server-side
// so calls are treated as authenticated without mTLS.
func newStreamTestEnv(t *testing.T, identity *authn.Identity, svcOpts ...ServiceOption) *streamTestEnv {
	t.Helper()

	svc := NewService(svcOpts...)

	authInterceptor := &testAuthInterceptor{identity: identity}
	path, handler := crosscodexv1connect.NewGatewayServiceHandler(
		svc,
		connect.WithInterceptors(authInterceptor),
	)

	mux := http.NewServeMux()
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)

	client := crosscodexv1connect.NewGatewayServiceClient(
		server.Client(),
		server.URL,
	)

	return &streamTestEnv{server: server, client: client}
}

// defaultIdentity returns a minimal authenticated identity for tests.
func defaultIdentity() *authn.Identity {
	return &authn.Identity{
		Subject:  "test-user",
		TenantID: "tenant-test",
		Roles:    []string{"user"},
		Method:   authn.AuthMethodMTLS,
	}
}

// stubBackends returns ServiceOptions that wire up minimal mock backends
// sufficient for the stream handler to proceed past protocol validation.
func stubBackends() []ServiceOption {
	return []ServiceOption{
		WithIngestionBackend(&stubIngestion{}),
		WithCatalogBackend(&stubCatalog{}),
		WithPipelineBackend(&stubPipeline{}),
		WithGraphBackend(&stubGraph{}),
		WithFeedbackBackend(&stubFeedback{}),
		WithAdminBackend(&stubAdmin{}),
	}
}

// ---------------------------------------------------------------------------
// Minimal stub backends — only the methods reachable from StreamDocument
// need real returns; the rest satisfy the interface.
// ---------------------------------------------------------------------------

type stubIngestion struct{}

func (s *stubIngestion) ConvertDocument(_ context.Context, _ *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: "doc-stub"}), nil
}

type stubCatalog struct{}

func (s *stubCatalog) ParseCatalog(_ context.Context, _ *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
	return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-stub", Status: pb.JobStatus_JOB_STATUS_COMPLETED}), nil
}

func (s *stubCatalog) ListCatalogs(_ context.Context, _ *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
	return connect.NewResponse(&pb.ListCatalogsResponse{}), nil
}

func (s *stubCatalog) GetCatalog(_ context.Context, _ *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
	return connect.NewResponse(&pb.GetCatalogResponse{}), nil
}

func (s *stubCatalog) SearchControls(_ context.Context, _ *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error) {
	return connect.NewResponse(&pb.SearchControlsResponse{}), nil
}

func (s *stubCatalog) GetControl(_ context.Context, _ *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error) {
	return connect.NewResponse(&pb.GetControlResponse{}), nil
}

type stubPipeline struct{}

func (s *stubPipeline) CreateJob(_ context.Context, _ *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
	return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-stub", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
}

func (s *stubPipeline) GetJob(_ context.Context, _ *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
	return connect.NewResponse(&pb.GetJobResponse{}), nil
}

func (s *stubPipeline) ListJobs(_ context.Context, _ *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
	return connect.NewResponse(&pb.ListJobsResponse{}), nil
}

func (s *stubPipeline) CancelJob(_ context.Context, _ *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	return connect.NewResponse(&pb.CancelJobResponse{}), nil
}

type stubGraph struct{}

func (s *stubGraph) Traverse(_ context.Context, _ *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	return connect.NewResponse(&pb.TraverseResponse{}), nil
}

func (s *stubGraph) Query(_ context.Context, _ *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	return connect.NewResponse(&pb.QueryResponse{}), nil
}

func (s *stubGraph) SimilaritySearch(_ context.Context, _ *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	return connect.NewResponse(&pb.SimilaritySearchResponse{}), nil
}

type stubFeedback struct{}

func (s *stubFeedback) SubmitVote(_ context.Context, _ *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
	return connect.NewResponse(&pb.SubmitVoteResponse{}), nil
}

func (s *stubFeedback) GetReviewQueue(_ context.Context, _ *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
	return connect.NewResponse(&pb.GetReviewQueueResponse{}), nil
}

type stubAdmin struct{}

func (s *stubAdmin) HealthCheck(_ context.Context, _ *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
	return connect.NewResponse(&pb.HealthCheckResponse{Status: pb.HealthStatus_HEALTH_STATUS_HEALTHY}), nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestStreamDocument_RejectsChunkBeforeMetadata(t *testing.T) {
	env := newStreamTestEnv(t, defaultIdentity(), stubBackends()...)
	defer env.close()

	stream := env.client.StreamDocument(context.Background())

	// Send a chunk without preceding metadata.
	if err := stream.Send(&pb.StreamDocumentChunk{
		Payload: &pb.StreamDocumentChunk_Chunk{Chunk: []byte("premature data")},
	}); err != nil {
		t.Fatalf("Send (chunk) failed at transport level: %v", err)
	}

	_, err := stream.CloseAndReceive()
	if err == nil {
		t.Fatal("expected error for chunk before metadata, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v: %v", got, err)
	}
}

func TestStreamDocument_RejectsDuplicateMetadata(t *testing.T) {
	env := newStreamTestEnv(t, defaultIdentity(), stubBackends()...)
	defer env.close()

	stream := env.client.StreamDocument(context.Background())

	meta := &pb.StreamDocumentChunk{
		Payload: &pb.StreamDocumentChunk_Metadata{
			Metadata: &pb.StreamDocumentMetadata{
				CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
				CatalogName:   "dup-test",
			},
		},
	}

	// First metadata — should succeed.
	if err := stream.Send(meta); err != nil {
		t.Fatalf("Send (first metadata) failed: %v", err)
	}

	// Second metadata — server should reject.
	if err := stream.Send(meta); err != nil {
		t.Fatalf("Send (second metadata) failed at transport level: %v", err)
	}

	_, err := stream.CloseAndReceive()
	if err == nil {
		t.Fatal("expected error for duplicate metadata, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v: %v", got, err)
	}
}

func TestStreamDocument_RejectsUploadExceedingSizeLimit(t *testing.T) {
	const limit = 1024 // 1 KB — small enough for fast test
	opts := append(stubBackends(), WithMaxUploadSize(limit))
	env := newStreamTestEnv(t, defaultIdentity(), opts...)
	defer env.close()

	stream := env.client.StreamDocument(context.Background())

	// Send metadata first.
	if err := stream.Send(&pb.StreamDocumentChunk{
		Payload: &pb.StreamDocumentChunk_Metadata{
			Metadata: &pb.StreamDocumentMetadata{
				CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
				CatalogName:   "oversize-test",
			},
		},
	}); err != nil {
		t.Fatalf("Send (metadata) failed: %v", err)
	}

	// Send a chunk that exceeds the limit.
	oversized := make([]byte, limit+1)
	if err := stream.Send(&pb.StreamDocumentChunk{
		Payload: &pb.StreamDocumentChunk_Chunk{Chunk: oversized},
	}); err != nil {
		// Transport-level failure is acceptable: the server may have
		// already closed the stream, causing a send error.
		t.Logf("Send (oversized chunk) returned transport error (expected): %v", err)
	}

	_, err := stream.CloseAndReceive()
	if err == nil {
		t.Fatal("expected error for oversized upload, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v: %v", got, err)
	}
}

func TestStreamDocument_RejectsEmptyStream(t *testing.T) {
	env := newStreamTestEnv(t, defaultIdentity(), stubBackends()...)
	defer env.close()

	stream := env.client.StreamDocument(context.Background())

	// Close immediately without sending any messages.
	_, err := stream.CloseAndReceive()
	if err == nil {
		t.Fatal("expected error for empty stream, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v: %v", got, err)
	}
}
