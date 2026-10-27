//go:build !integration

package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/gateway/backend"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("Backend errors at the gateway boundary", func() {
	// secret stands in for driver detail a backend might return unsanitized:
	// it names a database role, which must never reach a client (CWE-209).
	const secret = "pq: password authentication failed for user secret_admin"
	traceID := trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}

	var logs *bytes.Buffer

	// call runs one gateway RPC whose backend dependency fails with err. Each
	// call builds its own service so a chained RPC (SubmitDocument) can fail
	// at a chosen step while the steps before it succeed.
	type call func(ctx context.Context, err error) error

	newService := func(opts ...gateway.ServiceOption) *gateway.Service {
		return newTestService(append(opts, gateway.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))...)
	}
	docRequest := connect.NewRequest(&pb.SubmitDocumentRequest{
		Source: &pb.SubmitDocumentRequest_Content{Content: []byte("test doc")},
	})

	// traceKind selects the span context ctxFor adds: none, one this server
	// started, or one the caller sent (whose trace ID the caller chose).
	type traceKind int
	const (
		noTrace traceKind = iota
		localTrace
		remoteTrace
	)

	// ctxFor adds the tenant the auth interceptor would set and a span
	// context of the given kind.
	ctxFor := func(base context.Context, kind traceKind) context.Context {
		ctx, err := tenant.WithTenant(base, "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		if kind == noTrace {
			return ctx
		}
		return trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: trace.FlagsSampled,
			Remote: kind == remoteTrace,
		}))
	}

	message := func(err error) string {
		var ce *connect.Error
		Expect(errors.As(err, &ce)).To(BeTrue(), "expected a *connect.Error, got %v", err)
		return ce.Message()
	}

	// withheldRef asserts err carries the opaque message and returns its
	// error reference, which the log must carry too.
	refPattern := regexp.MustCompile(`^internal gateway error; the server logged the cause under error reference ([0-9a-f]{32})\. Retry the request, and if it keeps failing, give that reference to an operator$`)
	withheldRef := func(err error) string {
		m := refPattern.FindStringSubmatch(message(err))
		Expect(m).To(HaveLen(2), "message %q must be the opaque one with an error reference", message(err))
		Expect(logs.String()).To(ContainSubstring("error_ref=" + m[1]))
		return m[1]
	}

	BeforeEach(func() { logs = &bytes.Buffer{} })

	DescribeTable("for every handler that forwards a backend error",
		func(method string, base func() context.Context, rpc call) {
			subject := gateway.ExportIdentityFromContext(base()).Subject

			By("withholding a plain error behind an error reference and logging it in full, once, with who and where")
			err := rpc(ctxFor(base(), localTrace), errors.New(secret))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			withheldRef(err)
			Expect(message(err)).NotTo(ContainSubstring("secret_admin"))
			Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1), "the full error must be logged exactly once")
			Expect(logs.String()).To(ContainSubstring("level=ERROR"))
			Expect(logs.String()).To(ContainSubstring("method=" + method))
			Expect(logs.String()).To(ContainSubstring("trace_id=" + traceID.String()))
			Expect(logs.String()).To(ContainSubstring("tenant=tenant-a"))
			Expect(logs.String()).To(ContainSubstring("subject=" + subject))

			By("still giving a reference when there is no trace")
			logs.Reset()
			err = rpc(ctxFor(base(), noTrace), errors.New(secret))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			withheldRef(err)
			Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1))

			By("not logging a remote, caller-chosen trace ID as this server's")
			logs.Reset()
			err = rpc(ctxFor(base(), remoteTrace), errors.New(secret))
			withheldRef(err)
			Expect(logs.String()).NotTo(ContainSubstring(traceID.String()))

			By("withholding Internal and Unknown Connect errors too, since neither is the caller's to fix")
			for _, code := range []connect.Code{connect.CodeInternal, connect.CodeUnknown} {
				logs.Reset()
				err = rpc(ctxFor(base(), localTrace), connect.NewError(code, fmt.Errorf("list jobs: %w", errors.New(secret))))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal), code.String())
				withheldRef(err)
				Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1), code.String())
			}

			By("withholding the message of a transient error but keeping its code, so client retries still work")
			logs.Reset()
			err = rpc(ctxFor(base(), localTrace), connect.NewError(connect.CodeUnavailable, errors.New("dial tcp 10.0.3.7:5432: "+secret)))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			withheldRef(err)
			Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1))

			By("passing an actionable Connect error through unchanged, without logging it")
			for _, code := range []connect.Code{connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodePermissionDenied} {
				logs.Reset()
				backendErr := connect.NewError(code, errors.New("job job-9 not found: list jobs to find a valid job_id"))
				err = rpc(ctxFor(base(), localTrace), backendErr)
				Expect(err).To(BeIdenticalTo(backendErr), code.String())
				Expect(logs.String()).To(BeEmpty(), code.String())
			}
		},
		Entry("Health", "Health", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithAdminBackend(&mockAdmin{
				healthCheckFn: func(context.Context, *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
					return nil, err
				},
			})).Health(ctx, connect.NewRequest(&pb.HealthRequest{}))
			return e
		})),
		Entry("ListCatalogs", "ListCatalogs", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithCatalogBackend(&mockCatalog{
				listCatalogsFn: func(context.Context, *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
					return nil, err
				},
			})).ListCatalogs(ctx, connect.NewRequest(&pb.ListCatalogsRequest{}))
			return e
		})),
		Entry("GetCatalog", "GetCatalog", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithCatalogBackend(&mockCatalog{
				getCatalogFn: func(context.Context, *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
					return nil, err
				},
			})).GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{CatalogId: "cat-1"}))
			return e
		})),
		Entry("SearchControls", "SearchControls", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithCatalogBackend(&mockCatalog{
				searchControlsFn: func(context.Context, *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error) {
					return nil, err
				},
			})).SearchControls(ctx, connect.NewRequest(&pb.SearchControlsRequest{Query: "access"}))
			return e
		})),
		Entry("GetControl", "GetControl", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithCatalogBackend(&mockCatalog{
				getControlFn: func(context.Context, *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error) {
					return nil, err
				},
			})).GetControl(ctx, connect.NewRequest(&pb.GetControlRequest{ControlId: "ac-1"}))
			return e
		})),
		Entry("GetJob", "GetJob", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithPipelineBackend(&mockPipeline{
				getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return nil, err
				},
			})).GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: "job-1"}))
			return e
		})),
		Entry("ListJobs", "ListJobs", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithPipelineBackend(&mockPipeline{
				listJobsFn: func(context.Context, *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
					return nil, err
				},
			})).ListJobs(ctx, connect.NewRequest(&pb.ListJobsRequest{}))
			return e
		})),
		// A non-admin's CancelJob first looks the job up to check ownership.
		Entry("CancelJob ownership lookup", "CancelJob", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithPipelineBackend(&mockPipeline{
				getJobFn: func(context.Context, *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
					return nil, err
				},
			})).CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			return e
		})),
		Entry("CancelJob", "CancelJob", adminCtx, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithPipelineBackend(&mockPipeline{
				cancelJobFn: func(context.Context, *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
					return nil, err
				},
			})).CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: "job-1"}))
			return e
		})),
		Entry("ListJobMappings", "ListJobMappings", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithMappingsBackend(&mockMappings{err: err})).
				ListJobMappings(ctx, connect.NewRequest(&pb.ListJobMappingsRequest{JobId: "job-1"}))
			return e
		})),
		Entry("GetControlMappings", "GetControlMappings", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithMappingsBackend(&mockMappings{err: err})).
				GetControlMappings(ctx, connect.NewRequest(&pb.GetControlMappingsRequest{ControlId: "ac-1"}))
			return e
		})),
		Entry("QueryGraph", "QueryGraph", adminCtx, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithGraphBackend(&mockGraph{
				queryFn: func(context.Context, *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
					return nil, err
				},
			})).QueryGraph(ctx, connect.NewRequest(&pb.QueryGraphRequest{Cypher: "MATCH (n) RETURN n"}))
			return e
		})),
		Entry("FindSimilar", "FindSimilar", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithGraphBackend(&mockGraph{
				similaritySearchFn: func(context.Context, *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
					return nil, err
				},
			})).FindSimilar(ctx, connect.NewRequest(&pb.FindSimilarRequest{ControlId: "ac-1"}))
			return e
		})),
		Entry("SubmitVote", "SubmitVote", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithFeedbackBackend(&mockFeedback{
				submitVoteFn: func(context.Context, *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
					return nil, err
				},
			})).SubmitVote(ctx, connect.NewRequest(&pb.SubmitVoteRequest{MappingId: "m-1"}))
			return e
		})),
		Entry("GetReviewQueue", "GetReviewQueue", adminCtx, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithFeedbackBackend(&mockFeedback{
				getReviewQueueFn: func(context.Context, *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
					return nil, err
				},
			})).GetReviewQueue(ctx, connect.NewRequest(&pb.GetReviewQueueRequest{}))
			return e
		})),
		Entry("SubmitDocument ingestion step", "SubmitDocument", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithIngestionBackend(&mockIngestion{
				convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
					return nil, err
				},
			})).SubmitDocument(ctx, docRequest)
			return e
		})),
		Entry("SubmitDocument catalog step", "SubmitDocument", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithCatalogBackend(&mockCatalog{
				parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
					return nil, err
				},
			})).SubmitDocument(ctx, docRequest)
			return e
		})),
		Entry("SubmitDocument pipeline step", "SubmitDocument", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			_, e := newService(gateway.WithPipelineBackend(&mockPipeline{
				createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
					return nil, err
				},
			})).SubmitDocument(ctx, docRequest)
			return e
		})),
		Entry("StreamDocument ingestion step", "StreamDocument", func() context.Context { return userCtx("user-a") }, call(func(ctx context.Context, err error) error {
			svc := newService(gateway.WithIngestionBackend(&mockIngestion{
				convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
					return nil, err
				},
			}))
			_, e := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), &pb.StreamDocumentMetadata{}, []byte("test doc"))
			return e
		})),
	)

	// Unimplemented states a feature the deployment lacks, which the caller
	// can act on by sending a supported request instead.
	It("passes the passthrough ingestion's source_uri refusal through to the client", func() {
		svc := newService(gateway.WithIngestionBackend(backend.NewPassthroughIngestion(nil)))
		_, err := svc.SubmitDocument(ctxFor(userCtx("user-a"), localTrace), connect.NewRequest(&pb.SubmitDocumentRequest{
			Source: &pb.SubmitDocumentRequest_SourceUri{SourceUri: "s3://bucket/catalog.json"},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
		Expect(message(err)).To(Equal("only inline content is supported; source_uri is not implemented"))
		Expect(logs.String()).To(BeEmpty())
	})

	// The policy is an allowlist: only codes a caller can act on keep their
	// message, so a code nobody classified fails closed.
	DescribeTable("maps every non-OK Connect code by the allowlist",
		func(code connect.Code, passThrough bool, wantCode connect.Code) {
			svc := newService()
			backendErr := connect.NewError(code, errors.New("dial tcp 10.0.3.7:5432: "+secret))
			err := gateway.ExportPublicError(svc, ctxFor(context.Background(), localTrace), "GetJob", backendErr)
			Expect(connect.CodeOf(err)).To(Equal(wantCode))
			if passThrough {
				Expect(err).To(BeIdenticalTo(backendErr))
				Expect(logs.String()).To(BeEmpty())
				return
			}
			withheldRef(err)
			Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1))
		},
		Entry("InvalidArgument passes through", connect.CodeInvalidArgument, true, connect.CodeInvalidArgument),
		Entry("NotFound passes through", connect.CodeNotFound, true, connect.CodeNotFound),
		Entry("AlreadyExists passes through", connect.CodeAlreadyExists, true, connect.CodeAlreadyExists),
		Entry("FailedPrecondition passes through", connect.CodeFailedPrecondition, true, connect.CodeFailedPrecondition),
		Entry("OutOfRange passes through", connect.CodeOutOfRange, true, connect.CodeOutOfRange),
		Entry("PermissionDenied passes through", connect.CodePermissionDenied, true, connect.CodePermissionDenied),
		Entry("Unauthenticated passes through", connect.CodeUnauthenticated, true, connect.CodeUnauthenticated),
		Entry("ResourceExhausted passes through", connect.CodeResourceExhausted, true, connect.CodeResourceExhausted),
		Entry("Canceled passes through", connect.CodeCanceled, true, connect.CodeCanceled),
		Entry("Unimplemented passes through", connect.CodeUnimplemented, true, connect.CodeUnimplemented),
		Entry("Unavailable is withheld but keeps its code", connect.CodeUnavailable, false, connect.CodeUnavailable),
		Entry("DeadlineExceeded is withheld but keeps its code", connect.CodeDeadlineExceeded, false, connect.CodeDeadlineExceeded),
		Entry("Aborted is withheld but keeps its code", connect.CodeAborted, false, connect.CodeAborted),
		Entry("Unknown is withheld as Internal", connect.CodeUnknown, false, connect.CodeInternal),
		Entry("Internal is withheld as Internal", connect.CodeInternal, false, connect.CodeInternal),
		Entry("DataLoss is withheld as Internal", connect.CodeDataLoss, false, connect.CodeInternal),
	)
})
