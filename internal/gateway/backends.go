package gateway

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

type IngestionBackend interface {
	ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error)
	// DiscardDocument removes a document that ConvertDocument stored. The
	// gateway calls it when the catalog rejects the document, so a failed
	// import leaves no blob behind.
	DiscardDocument(ctx context.Context, documentID string) error
}

type CatalogBackend interface {
	ParseCatalog(ctx context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error)
	ListCatalogs(ctx context.Context, req *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error)
	GetCatalog(ctx context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error)
	SearchControls(ctx context.Context, req *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error)
	GetControl(ctx context.Context, req *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error)
	RetireCatalogVersion(ctx context.Context, req *connect.Request[pb.RetireCatalogVersionRequest]) (*connect.Response[pb.RetireCatalogVersionResponse], error)
}

type PipelineBackend interface {
	CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error)
	GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error)
	ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error)
	CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error)
	RetryJob(ctx context.Context, req *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error)
	// ListCatalogControls lists a catalog's controls as analysis receives
	// them. An unknown catalog and another tenant's are the same NotFound.
	ListCatalogControls(ctx context.Context, req *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error)
	// RenderPrompt renders an analyzer's prompt for one control pair without
	// calling an LLM. Another tenant's control or job, an unknown control
	// and an unknown job are the same NotFound.
	RenderPrompt(ctx context.Context, req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error)
}

type GraphBackend interface {
	Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error)
	Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error)
	SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error)
}

// MappingsBackend lists control mappings from the stored pipeline results. The
// gateway sets the request's tenant_context from the authenticated identity
// before calling it.
type MappingsBackend interface {
	ListMappings(ctx context.Context, req *connect.Request[pb.ListJobMappingsRequest]) (*connect.Response[pb.ListJobMappingsResponse], error)
}

// CandidatesBackend lists a job's relationship candidates from the stored
// pipeline results. The gateway sets the request's tenant_context from the
// authenticated identity before calling it.
type CandidatesBackend interface {
	ListCandidates(ctx context.Context, req *connect.Request[pb.ListJobCandidatesRequest]) (*connect.Response[pb.ListJobCandidatesResponse], error)
}

// CrosswalkBackend walks the graph for a crosswalk. The gateway has already
// validated the request, resolved the catalogs and selected the tenant's
// completed jobs (the request's jobs and catalogs fields), and set
// tenant_context from the authenticated identity; the backend lists only edges
// of those jobs.
type CrosswalkBackend interface {
	GetCrosswalk(ctx context.Context, req *connect.Request[pb.GetCrosswalkRequest]) (*connect.Response[pb.GetCrosswalkResponse], error)
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
	RecordDetermination(ctx context.Context, req *connect.Request[pb.RecordDeterminationRequest]) (*connect.Response[pb.RecordDeterminationResponse], error)
}

type AdminBackend interface {
	HealthCheck(ctx context.Context, req *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error)
}
