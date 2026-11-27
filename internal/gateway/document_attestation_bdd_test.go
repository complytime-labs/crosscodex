//go:build !integration

package gateway_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	"go.opentelemetry.io/otel/codes"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// importDocument runs one catalog import RPC for content.
type importDocument func(svc *gateway.Service, ctx context.Context, content []byte) (*pb.SubmitDocumentResponse, error)

func submitDocument(svc *gateway.Service, ctx context.Context, content []byte) (*pb.SubmitDocumentResponse, error) {
	resp, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
		Source: &pb.SubmitDocumentRequest_Content{Content: content},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func streamDocument(svc *gateway.Service, ctx context.Context, content []byte) (*pb.SubmitDocumentResponse, error) {
	resp, err := gateway.ExportHandleStreamedDocument(svc, ctx, time.Now(), &pb.StreamDocumentMetadata{}, content)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// failingSigner is a Generator whose CreateLink fails; the gateway calls
// nothing else on it.
type failingSigner struct{ attestation.Generator }

func (failingSigner) CreateLink(context.Context, string, []attestation.Artifact, []attestation.Artifact, ...attestation.LinkOption) (*attestation.SignedLink, error) {
	return nil, errors.New("hsm at 10.0.0.7 refused to sign")
}

var _ = Describe("catalog import attestation", func() {
	// The ingestion backend's document ID is the document's storage key, as
	// PassthroughIngestion's is; the link is stored under it.
	const docID = "documents/doc-1.json"
	const ingestKey = "attestation/ingest/" + docID
	content := []byte(`{"catalog":{"uuid":"c"}}`)

	var (
		gen     attestation.Generator
		store   *storagetest.Fake
		tp      *telemetrytest.TestProvider
		ctx     context.Context
		logs    *bytes.Buffer
		forA    gateway.AttestorResolver
		service func(resolve gateway.AttestorResolver, extra ...gateway.ServiceOption) *gateway.Service
	)

	BeforeEach(func() {
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		store = &storagetest.Fake{}
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })

		tctx, err := tenant.WithTenant(userCtx("user-a"), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		ctx, _ = tp.TracerProvider().Tracer("test").Start(tctx, "client")

		logs = &bytes.Buffer{}
		forA = func(tenantID string) attestation.Generator {
			if tenantID == "tenant-a" {
				return gen
			}
			return nil
		}
		service = func(resolve gateway.AttestorResolver, extra ...gateway.ServiceOption) *gateway.Service {
			return newTestService(append([]gateway.ServiceOption{
				gateway.WithAttestor(resolve, store),
				gateway.WithIngestionBackend(&mockIngestion{convertFn: func(context.Context, *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
					return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: docID}), nil
				}}),
				gateway.WithTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")),
				gateway.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
			}, extra...)...)
		}
	})

	DescribeTable("stores a signed ingest link binding the uploaded bytes to the catalog",
		func(run importDocument, step string) {
			_, err := run(service(forA), ctx, content)
			Expect(err).NotTo(HaveOccurred())

			objects := store.Objects()
			Expect(objects).To(HaveLen(1))
			Expect(objects).To(HaveKey(ingestKey))
			link, err := gen.Verify(context.Background(), objects[ingestKey])
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Step).To(Equal(step))

			sum := sha256.Sum256(content)
			digest := hex.EncodeToString(sum[:])
			Expect(link.Materials).To(ConsistOf(attestation.Artifact{URI: "document://tenant-a/" + docID, Digest: digest}))
			Expect(link.Products).To(ConsistOf(HaveField("URI", "catalog://tenant-a/cat-1")))
			Expect(link.ByProducts).To(HaveKeyWithValue("content_hash", digest))
			Expect(link.ByProducts).To(HaveKeyWithValue("trace_id", telemetry.TraceIDFromContext(ctx)))
			Expect(telemetry.TraceIDFromContext(ctx)).NotTo(BeEmpty())
		},
		Entry("SubmitDocument", importDocument(submitDocument), "gateway.SubmitDocument"),
		Entry("StreamDocument", importDocument(streamDocument), "gateway.StreamDocument"),
	)

	DescribeTable("records the document slug, the stored label and the resolved version in the ingest link",
		func(requested string) {
			cat := &mockCatalog{parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return connect.NewResponse(&pb.ParseCatalogResponse{
					CatalogId:    "cat-1",
					Version:      "nist-800-53 @ rev5 (2026-01-02 · abc123)",
					VersionLabel: "rev5",
				}), nil
			}}
			svc := service(forA, gateway.WithCatalogBackend(cat))

			_, err := svc.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
				Source:       &pb.SubmitDocumentRequest_Content{Content: content},
				DocumentSlug: "nist-800-53",
				VersionLabel: requested,
			}))
			Expect(err).NotTo(HaveOccurred())

			link, err := gen.Verify(context.Background(), store.Objects()[ingestKey])
			Expect(err).NotTo(HaveOccurred())
			Expect(link.ByProducts).To(HaveKeyWithValue("document_slug", "nist-800-53"))
			Expect(link.ByProducts).To(HaveKeyWithValue("version_label", "rev5"))
			Expect(link.ByProducts).To(HaveKeyWithValue("version", "nist-800-53 @ rev5 (2026-01-02 · abc123)"))
		},
		Entry("an explicit label", "rev5"),
		Entry("no explicit label (OSCAL or fallback label stored)", ""),
		Entry("a requested label the catalog did not store", "other"),
	)

	DescribeTable("forwards the ingestion mapping to the catalog and records it, with its sha256, only when the import named one",
		func(mapping, sha string, wantForwarded string, want types.GomegaMatcher) {
			var parsed *pb.ParseCatalogRequest
			cat := &mockCatalog{parseFn: func(_ context.Context, req *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				parsed = req.Msg
				return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", IngestionMappingSha256: sha}), nil
			}}

			_, err := service(forA, gateway.WithCatalogBackend(cat)).SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
				Source:           &pb.SubmitDocumentRequest_Content{Content: content},
				IngestionMapping: mapping,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed.GetIngestionMapping()).To(Equal(wantForwarded))

			link, err := gen.Verify(context.Background(), store.Objects()[ingestKey])
			Expect(err).NotTo(HaveOccurred())
			Expect(link.ByProducts).To(want)
		},
		Entry("a mapped import records its mapping ID and the mapping file's sha256",
			"owasp-asvs-5.0.0-json", strings.Repeat("ab", 32), "owasp-asvs-5.0.0-json",
			And(HaveKeyWithValue("ingestion_mapping", "owasp-asvs-5.0.0-json"),
				HaveKeyWithValue("ingestion_mapping_sha256", strings.Repeat("ab", 32)))),
		Entry("an import without a mapping forwards none and records neither key", "", "", "",
			And(Not(HaveKey("ingestion_mapping")), Not(HaveKey("ingestion_mapping_sha256")))),
	)

	DescribeTable("records how ingestion structured the document, and returns it, only when ingestion reported a method",
		func(method string, want types.GomegaMatcher) {
			cat := &mockCatalog{parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return connect.NewResponse(&pb.ParseCatalogResponse{CatalogId: "cat-1", IngestionStructureMethod: method}), nil
			}}

			resp, err := service(forA, gateway.WithCatalogBackend(cat)).SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{Content: content},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetIngestionStructureMethod()).To(Equal(method))

			link, err := gen.Verify(context.Background(), store.Objects()[ingestKey])
			Expect(err).NotTo(HaveOccurred())
			Expect(link.ByProducts).To(want)
		},
		Entry("a guessed import records its method", "heading", HaveKeyWithValue("ingestion_structure_method", "heading")),
		Entry("a mapped import records mapping", "mapping", HaveKeyWithValue("ingestion_structure_method", "mapping")),
		Entry("OSCAL, which has no method, records no key", "", Not(HaveKey("ingestion_structure_method"))),
	)

	DescribeTable("records the HTML profile and its sha256 only when ingestion applied one",
		func(profile, sha string, want types.GomegaMatcher) {
			cat := &mockCatalog{parseFn: func(context.Context, *connect.Request[pb.ParseCatalogRequest]) (*connect.Response[pb.ParseCatalogResponse], error) {
				return connect.NewResponse(&pb.ParseCatalogResponse{
					CatalogId: "cat-1", IngestionHtmlProfile: profile, IngestionHtmlProfileSha256: sha,
				}), nil
			}}

			_, err := service(forA, gateway.WithCatalogBackend(cat)).SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{Content: content},
			}))
			Expect(err).NotTo(HaveOccurred())

			link, err := gen.Verify(context.Background(), store.Objects()[ingestKey])
			Expect(err).NotTo(HaveOccurred())
			Expect(link.ByProducts).To(want)
		},
		Entry("an HTML conversion records the profile id and file hash", "eurlex", strings.Repeat("cd", 32),
			And(HaveKeyWithValue("ingestion_html_profile", "eurlex"),
				HaveKeyWithValue("ingestion_html_profile_sha256", strings.Repeat("cd", 32)))),
		Entry("a source with no profile (OSCAL, PDF) records neither key", "", "",
			And(Not(HaveKey("ingestion_html_profile")), Not(HaveKey("ingestion_html_profile_sha256")))),
	)

	DescribeTable("fails the import, without leaking the cause, when the link cannot be stored",
		func(run importDocument) {
			store.PutFunc = func(context.Context, string, io.Reader) error {
				return errors.New("s3 bucket secret-bucket: AccessDenied for role arn:aws:iam::123:role/x")
			}

			_, err := run(service(forA), ctx, content)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).To(And(
				ContainSubstring("cat-1"), ContainSubstring("imported but not attested"), ContainSubstring("re-run the same import")))
			Expect(err.Error()).NotTo(Or(ContainSubstring("secret-bucket"), ContainSubstring("AccessDenied")))
			Expect(bytes.Count(logs.Bytes(), []byte("secret-bucket"))).To(Equal(1), "the cause is logged once for an operator")
			Expect(store.Objects()).To(BeEmpty())
		},
		Entry("SubmitDocument", importDocument(submitDocument)),
		Entry("StreamDocument", importDocument(streamDocument)),
	)

	It("marks the handler span as failed, carrying the cause for an operator, when the link cannot be stored", func() {
		store.PutFunc = func(context.Context, string, io.Reader) error {
			return errors.New("s3 bucket secret-bucket: AccessDenied")
		}

		_, err := submitDocument(service(forA), ctx, content)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))

		span := telemetrytest.FindSpan(tp.GetSpans(), "gateway.SubmitDocument")
		Expect(span).NotTo(BeNil())
		Expect(span.Status().Code).To(Equal(codes.Error))
		Expect(span.Events()).To(ContainElement(HaveField("Name", "exception")))
		Expect(span.Status().Description).NotTo(ContainSubstring("secret-bucket"), "the status text is not the cause")
	})

	DescribeTable("fails the import, without leaking the cause, when the link cannot be signed",
		func(run importDocument) {
			_, err := run(service(func(string) attestation.Generator { return failingSigner{} }), ctx, content)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).To(And(
				ContainSubstring("cat-1"), ContainSubstring("imported but not attested"), ContainSubstring("re-run the same import")))
			Expect(err.Error()).NotTo(ContainSubstring("10.0.0.7"))
			Expect(bytes.Count(logs.Bytes(), []byte("10.0.0.7"))).To(Equal(1), "the cause is logged once for an operator")
			Expect(store.Objects()).To(BeEmpty())
		},
		Entry("SubmitDocument", importDocument(submitDocument)),
		Entry("StreamDocument", importDocument(streamDocument)),
	)

	// Through the server, the auth interceptor puts the tenant on every
	// request's context, so the narrowest seam that reaches attestIngest
	// without one is the handler itself: an identity but no tenant.
	DescribeTable("fails the import closed, without leaking the cause, when the context carries no tenant",
		func(run importDocument) {
			noTenant, _ := tp.TracerProvider().Tracer("test").Start(userCtx("user-a"), "client")
			resolved := false

			_, err := run(service(func(string) attestation.Generator { resolved = true; return gen }), noTenant, content)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err.Error()).To(And(
				ContainSubstring("cat-1"), ContainSubstring("imported but not attested"),
				ContainSubstring("re-run the same import"), ContainSubstring("error reference")))
			Expect(err.Error()).NotTo(ContainSubstring(tenant.ErrNoTenant.Error()))
			Expect(bytes.Count(logs.Bytes(), []byte(tenant.ErrNoTenant.Error()))).To(Equal(1), "the cause is logged once for an operator")
			Expect(resolved).To(BeFalse(), "no generator is chosen without a tenant")
			Expect(store.Objects()).To(BeEmpty())
		},
		Entry("SubmitDocument", importDocument(submitDocument)),
		Entry("StreamDocument", importDocument(streamDocument)),
	)

	DescribeTable("stores no link for a tenant whose attestation is disabled",
		func(run importDocument) {
			resp, err := run(service(func(string) attestation.Generator { return nil }), ctx, content)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetCatalogId()).To(Equal("cat-1"))
			Expect(store.Objects()).To(BeEmpty())
		},
		Entry("SubmitDocument", importDocument(submitDocument)),
		Entry("StreamDocument", importDocument(streamDocument)),
	)

	It("stores nothing for StartAnalysis, which the pipeline attests", func() {
		svc := service(forA, gateway.WithPipelineBackend(&mockPipeline{
			createJobFn: func(context.Context, *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
				return connect.NewResponse(&pb.CreateJobResponse{JobId: "job-1"}), nil
			},
		}))

		_, err := svc.StartAnalysis(ctx, connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: "cat-1", TargetCatalogId: "cat-1"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Objects()).To(BeEmpty())
	})
})
