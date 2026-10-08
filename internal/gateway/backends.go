package gateway

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

type IngestionBackend interface {
	ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error)
}

type CatalogBackend interface {
	ParseCatalog(ctx context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error)
	ListCatalogs(ctx context.Context, req *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error)
	GetCatalog(ctx context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error)
	SearchControls(ctx context.Context, req *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error)
	GetControl(ctx context.Context, req *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error)
}

type PipelineBackend interface {
	CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error)
	GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error)
	ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error)
	CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error)
}

type GraphBackend interface {
	Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error)
	Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error)
	SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error)
}

// TenantStatusChecker reports whether a tenant may make requests. The auth
// interceptor refuses every non-health request from a tenant it reports
// inactive (suspended or not provisioned).
type TenantStatusChecker interface {
	TenantActive(ctx context.Context, tenantID string) (bool, error)
}

type FeedbackBackend interface {
	SubmitVote(ctx context.Context, req *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error)
	GetReviewQueue(ctx context.Context, req *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error)
}

type AdminBackend interface {
	HealthCheck(ctx context.Context, req *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error)
}
