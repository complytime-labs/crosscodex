//go:build !integration

package gateway_test

import (
	"context"
	"testing"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pgregory.net/rapid"
)

// TestPropertyTenantContextSpoofing verifies that regardless of the
// TenantContext a client supplies in the request, the backend always
// receives the auth-derived tenant from the identity in the context.
func TestPropertyTenantContextSpoofing(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		realTenant := rapid.StringMatching(`^[a-z][a-z0-9-]{2,15}$`).Draw(rt, "realTenant")
		spoofedTenant := rapid.StringMatching(`^[a-z][a-z0-9-]{2,15}$`).Draw(rt, "spoofedTenant")
		subject := rapid.StringMatching(`^user-[a-z0-9]{1,8}$`).Draw(rt, "subject")

		var captured *pb.TenantContext
		cat := &mockCatalog{
			listCatalogsFn: func(_ context.Context, req *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error) {
				captured = req.GetTenantContext()
				return &pb.ListCatalogsResponse{}, nil
			},
		}

		svc := gateway.NewService(
			gateway.WithCatalogBackend(cat),
			gateway.WithIngestionBackend(&mockIngestion{}),
			gateway.WithPipelineBackend(&mockPipeline{}),
			gateway.WithGraphBackend(&mockGraph{}),
			gateway.WithFeedbackBackend(&mockFeedback{}),
			gateway.WithAdminBackend(&mockAdmin{}),
		)

		ctx := gateway.ExportContextWithIdentity(context.Background(), &authn.Identity{
			Subject:  subject,
			TenantID: realTenant,
			Roles:    []string{"user"},
			Method:   authn.AuthMethodMTLS,
		})

		req := &pb.ListCatalogsRequest{
			TenantContext: &pb.TenantContext{TenantId: spoofedTenant},
		}

		_, err := svc.ListCatalogs(ctx, req)
		if err != nil {
			rt.Fatalf("ListCatalogs returned unexpected error: %v", err)
		}
		if captured == nil {
			rt.Fatal("backend did not receive a TenantContext")
		}
		if captured.GetTenantId() != realTenant {
			rt.Fatalf("expected tenant %q from auth, got %q (spoofed was %q)",
				realTenant, captured.GetTenantId(), spoofedTenant)
		}
	})
}

// TestPropertyJobOwnership verifies that a non-admin user never sees
// jobs created by another user in the ListJobs response.
func TestPropertyJobOwnership(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		callerSubject := rapid.StringMatching(`^user-[a-z]{1,8}$`).Draw(rt, "callerSubject")
		nJobs := rapid.IntRange(0, 20).Draw(rt, "nJobs")

		allJobs := make([]*pb.PipelineJob, nJobs)
		expectedCount := 0
		for i := 0; i < nJobs; i++ {
			owner := rapid.StringMatching(`^user-[a-z]{1,8}$`).Draw(rt, "owner")
			allJobs[i] = &pb.PipelineJob{
				JobId: rapid.StringMatching(`^job-[0-9]{1,5}$`).Draw(rt, "jobId"),
				Audit: &pb.AuditMetadata{CreatedBy: owner},
			}
			if owner == callerSubject {
				expectedCount++
			}
		}

		pipeline := &mockPipeline{
			listJobsFn: func(_ context.Context, _ *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
				return &pb.ListJobsResponse{Jobs: allJobs}, nil
			},
		}

		svc := gateway.NewService(
			gateway.WithPipelineBackend(pipeline),
			gateway.WithIngestionBackend(&mockIngestion{}),
			gateway.WithCatalogBackend(&mockCatalog{}),
			gateway.WithGraphBackend(&mockGraph{}),
			gateway.WithFeedbackBackend(&mockFeedback{}),
			gateway.WithAdminBackend(&mockAdmin{}),
		)

		ctx := gateway.ExportContextWithIdentity(context.Background(), &authn.Identity{
			Subject:  callerSubject,
			TenantID: "tenant-a",
			Roles:    []string{"user"},
			Method:   authn.AuthMethodMTLS,
		})

		resp, err := svc.ListJobs(ctx, &pb.ListJobsRequest{})
		if err != nil {
			rt.Fatalf("ListJobs returned unexpected error: %v", err)
		}

		if len(resp.GetJobs()) != expectedCount {
			rt.Fatalf("expected %d jobs for %q, got %d", expectedCount, callerSubject, len(resp.GetJobs()))
		}

		for _, job := range resp.GetJobs() {
			if job.GetAudit().GetCreatedBy() != callerSubject {
				rt.Fatalf("non-admin %q received job owned by %q", callerSubject, job.GetAudit().GetCreatedBy())
			}
		}
	})
}

// TestPropertyFailClosedAuth verifies that a request without an
// identity in the context always receives an Unauthenticated error,
// regardless of what RPC is invoked.
func TestPropertyFailClosedAuth(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		svc := gateway.NewService(
			gateway.WithIngestionBackend(&mockIngestion{}),
			gateway.WithCatalogBackend(&mockCatalog{}),
			gateway.WithPipelineBackend(&mockPipeline{}),
			gateway.WithGraphBackend(&mockGraph{}),
			gateway.WithFeedbackBackend(&mockFeedback{}),
			gateway.WithAdminBackend(&mockAdmin{}),
		)

		// No identity injected - bare context.
		ctx := context.Background()

		// Pick a random RPC to exercise. Health is excluded because it
		// does not require authentication.
		type rpcCall struct {
			name string
			fn   func() error
		}

		rpcs := []rpcCall{
			{"ListCatalogs", func() error {
				_, err := svc.ListCatalogs(ctx, &pb.ListCatalogsRequest{})
				return err
			}},
			{"GetCatalog", func() error {
				_, err := svc.GetCatalog(ctx, &pb.GetCatalogRequest{CatalogId: "cat-1"})
				return err
			}},
			{"GetControl", func() error {
				_, err := svc.GetControl(ctx, &pb.GetControlRequest{ControlId: "ctrl-1"})
				return err
			}},
			{"SearchControls", func() error {
				_, err := svc.SearchControls(ctx, &pb.SearchControlsRequest{Query: "test"})
				return err
			}},
			{"GetJob", func() error {
				_, err := svc.GetJob(ctx, &pb.GetJobRequest{JobId: "job-1"})
				return err
			}},
			{"ListJobs", func() error {
				_, err := svc.ListJobs(ctx, &pb.ListJobsRequest{})
				return err
			}},
			{"CancelJob", func() error {
				_, err := svc.CancelJob(ctx, &pb.CancelJobRequest{JobId: "job-1"})
				return err
			}},
			{"SubmitDocument", func() error {
				_, err := svc.SubmitDocument(ctx, &pb.SubmitDocumentRequest{
					Source: &pb.SubmitDocumentRequest_Content{Content: []byte("x")},
				})
				return err
			}},
			{"QueryGraph", func() error {
				_, err := svc.QueryGraph(ctx, &pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"})
				return err
			}},
			{"FindSimilar", func() error {
				_, err := svc.FindSimilar(ctx, &pb.FindSimilarRequest{ControlId: "ctrl-1"})
				return err
			}},
			{"GetControlMappings", func() error {
				_, err := svc.GetControlMappings(ctx, &pb.GetControlMappingsRequest{ControlId: "ctrl-1"})
				return err
			}},
			{"SubmitVote", func() error {
				_, err := svc.SubmitVote(ctx, &pb.SubmitVoteRequest{MappingId: "map-1"})
				return err
			}},
			{"GetReviewQueue", func() error {
				_, err := svc.GetReviewQueue(ctx, &pb.GetReviewQueueRequest{})
				return err
			}},
		}

		idx := rapid.IntRange(0, len(rpcs)-1).Draw(rt, "rpcIndex")
		chosen := rpcs[idx]

		err := chosen.fn()
		if err == nil {
			rt.Fatalf("%s did not return error for unauthenticated context", chosen.name)
		}
		if status.Code(err) != codes.Unauthenticated {
			rt.Fatalf("%s returned %v, expected Unauthenticated", chosen.name, status.Code(err))
		}
	})
}
