package gateway

import (
	"context"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

type IngestionBackend interface {
	ConvertDocument(ctx context.Context, req *pb.ConvertDocumentRequest) (*pb.ConvertDocumentResponse, error)
}

type CatalogBackend interface {
	ParseCatalog(ctx context.Context, req *pb.ParseCatalogRequest) (*pb.ParseCatalogResponse, error)
	ListCatalogs(ctx context.Context, req *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error)
	GetCatalog(ctx context.Context, req *pb.GetCatalogRequest) (*pb.GetCatalogResponse, error)
	SearchControls(ctx context.Context, req *pb.SearchControlsRequest) (*pb.SearchControlsResponse, error)
	GetControl(ctx context.Context, req *pb.GetControlRequest) (*pb.GetControlResponse, error)
}

type PipelineBackend interface {
	CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.CreateJobResponse, error)
	GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error)
	ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error)
	CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error)
}

type GraphBackend interface {
	Traverse(ctx context.Context, req *pb.TraverseRequest) (*pb.TraverseResponse, error)
	Query(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error)
	SimilaritySearch(ctx context.Context, req *pb.SimilaritySearchRequest) (*pb.SimilaritySearchResponse, error)
}

type FeedbackBackend interface {
	SubmitVote(ctx context.Context, req *pb.SubmitVoteRequest) (*pb.SubmitVoteResponse, error)
	GetReviewQueue(ctx context.Context, req *pb.GetReviewQueueRequest) (*pb.GetReviewQueueResponse, error)
}

type AdminBackend interface {
	HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error)
}
