package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeIngestionServer fakes the crosscodex-ingestion IngestionService. Like
// the real service it refuses a request without a tenant, and it records only
// the requests it accepts. document decides each GetDocument answer from the
// 1-based call number.
type fakeIngestionServer struct {
	crosscodexv1connect.UnimplementedIngestionServiceHandler

	convertErr error
	getErr     error
	document   func(call int) *pb.Document

	mu       sync.Mutex
	converts []*pb.ConvertDocumentRequest
	gets     []*pb.GetDocumentRequest
}

func (f *fakeIngestionServer) ConvertDocument(_ context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	if req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_id is required"))
	}
	f.mu.Lock()
	f.converts = append(f.converts, req.Msg)
	f.mu.Unlock()
	if f.convertErr != nil {
		return nil, f.convertErr
	}
	return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: "doc-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
}

func (f *fakeIngestionServer) GetDocument(_ context.Context, req *connect.Request[pb.GetDocumentRequest]) (*connect.Response[pb.GetDocumentResponse], error) {
	if req.Msg.GetTenantContext().GetTenantId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_id is required"))
	}
	f.mu.Lock()
	f.gets = append(f.gets, req.Msg)
	call := len(f.gets)
	f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return connect.NewResponse(&pb.GetDocumentResponse{Document: f.document(call)}), nil
}

func (f *fakeIngestionServer) recorded() (converts []*pb.ConvertDocumentRequest, gets []*pb.GetDocumentRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.ConvertDocumentRequest(nil), f.converts...), append([]*pb.GetDocumentRequest(nil), f.gets...)
}

// serveIngestion starts fake over gRPC on HTTP/2 with TLS, the protocol the
// real service speaks, and returns a client for it.
func serveIngestion(fake *fakeIngestionServer) (crosscodexv1connect.IngestionServiceClient, *httptest.Server) {
	_, handler := crosscodexv1connect.NewIngestionServiceHandler(fake)
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	DeferCleanup(srv.Close)
	return crosscodexv1connect.NewIngestionServiceClient(srv.Client(), srv.URL, connect.WithGRPC()), srv
}

var _ = Describe("IngestionClient", func() {
	const (
		callerTenant = "tenant-a"
		otherTenant  = "tenant-b-secret"
		root         = "/srv/crosscodex/objects"
		bucket       = "crosscodex-objects"
		outputKey    = "documents/doc-1/document.json"
		// ingestionSecret stands in for detail the ingestion service puts in
		// its errors (worker paths, library traces). It may be logged by the
		// gateway but never returned to the caller (CWE-209).
		ingestionSecret = "docling worker /opt/secret-model-path crashed"
	)
	local := StorageBackend{Kind: "local", Root: root}
	s3 := StorageBackend{Kind: "s3", Bucket: bucket}

	var (
		store *storagetest.Fake
		fake  *fakeIngestionServer
		cfg   config.IngestionConfig
		tp    *telemetrytest.TestProvider
	)

	tenantCtx := func() context.Context {
		ctx, err := tenant.WithTenant(context.Background(), callerTenant)
		Expect(err).NotTo(HaveOccurred())
		return ctx
	}
	contentReq := func(content []byte) *connect.Request[pb.ConvertDocumentRequest] {
		return connect.NewRequest(&pb.ConvertDocumentRequest{
			TenantContext: &pb.TenantContext{TenantId: callerTenant},
			Source:        &pb.ConvertDocumentRequest_Content{Content: content},
		})
	}
	completed := func(uri string) func(int) *pb.Document {
		return func(int) *pb.Document {
			return &pb.Document{DocumentId: "doc-1", ConversionStatus: pb.JobStatus_JOB_STATUS_COMPLETED, StructuredJsonUri: uri}
		}
	}
	newClient := func(rpc crosscodexv1connect.IngestionServiceClient, sb StorageBackend) *IngestionClient {
		c, err := NewIngestionClient(rpc, store, sb, cfg,
			WithIngestionClientTelemetry(tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test")))
		Expect(err).NotTo(HaveOccurred())
		return c
	}
	// outcomes returns the conversion counter's value per outcome label.
	outcomes := func() map[string]int64 {
		m := telemetrytest.FindMetric(tp.GetMetrics(), "crosscodex.ingestion.conversions.total")
		if m == nil {
			return nil
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		Expect(ok).To(BeTrue(), "counter data is %T", m.Data)
		got := map[string]int64{}
		for _, dp := range sum.DataPoints {
			v, _ := dp.Attributes.Value(attribute.Key("outcome"))
			got[v.AsString()] += dp.Value
		}
		return got
	}
	// expectWithheldCode asserts err is a Connect error whose code the gateway's
	// publicError withholds (logs, then replaces the message), never a
	// caller-actionable code that would pass ingestion's text through.
	expectWithheldCode := func(err error, want connect.Code) {
		var ce *connect.Error
		Expect(errors.As(err, &ce)).To(BeTrue(), "want a *connect.Error, got %T: %v", err, err)
		Expect(ce.Code()).To(Equal(want))
		Expect([]connect.Code{connect.CodeInternal, connect.CodeUnavailable, connect.CodeDeadlineExceeded}).To(ContainElement(ce.Code()))
	}

	BeforeEach(func() {
		store = &storagetest.Fake{}
		fake = &fakeIngestionServer{}
		cfg = config.IngestionConfig{PollInterval: 5 * time.Millisecond, ConvertTimeout: 5 * time.Second, MaxDocumentBytes: 16}
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
	})

	DescribeTable("returns the output's storage key once ingestion completes",
		func(sb StorageBackend, uri string) {
			Expect(store.Put(tenantCtx(), outputKey, strings.NewReader(`{"schema_version":"1.0"}`))).To(Succeed())
			fake.document = func(call int) *pb.Document {
				if call < 3 {
					return &pb.Document{DocumentId: "doc-1", ConversionStatus: pb.JobStatus_JOB_STATUS_RUNNING}
				}
				return completed(uri)(call)
			}
			rpc, _ := serveIngestion(fake)

			resp, err := newClient(rpc, sb).ConvertDocument(tenantCtx(), contentReq([]byte("%PDF-1.7")))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetDocumentId()).To(Equal(outputKey))
			Expect(resp.Msg.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED))

			rc, err := store.Get(tenantCtx(), resp.Msg.GetDocumentId())
			Expect(err).NotTo(HaveOccurred())
			body, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Close()).To(Succeed())
			Expect(string(body)).To(Equal(`{"schema_version":"1.0"}`))

			converts, gets := fake.recorded()
			Expect(converts).To(HaveLen(1))
			Expect(converts[0].GetTenantContext().GetTenantId()).To(Equal(callerTenant))
			Expect(converts[0].GetContent()).To(Equal([]byte("%PDF-1.7")))
			Expect(gets).To(HaveLen(3), "polls until COMPLETED")
			for _, g := range gets {
				Expect(g.GetDocumentId()).To(Equal("doc-1"))
				Expect(g.GetTenantContext().GetTenantId()).To(Equal(callerTenant))
			}

			span := telemetrytest.FindSpan(tp.GetSpans(), "IngestionClient.ConvertDocument")
			Expect(span).NotTo(BeNil())
			v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal(callerTenant))
			v, ok = telemetrytest.SpanAttribute(span, "operation")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal("convert_document"))
			hist := telemetrytest.FindMetric(tp.GetMetrics(), "crosscodex.ingestion.conversion.duration_ms")
			Expect(hist).NotTo(BeNil())
			Expect(telemetrytest.HistogramCount(hist)).To(BeNumerically("==", 1))
			Expect(outcomes()).To(Equal(map[string]int64{"completed": 1}))
		},
		Entry("local backend, file:// URI", local, "file://"+root+"/"+callerTenant+"/"+outputKey),
		Entry("s3 backend, s3:// URI", s3, "s3://"+bucket+"/"+callerTenant+"/"+outputKey),
	)

	Describe("max_document_bytes", func() {
		BeforeEach(func() {
			Expect(store.Put(tenantCtx(), outputKey, strings.NewReader("{}"))).To(Succeed())
			fake.document = completed("file://" + root + "/" + callerTenant + "/" + outputKey)
		})

		It("accepts a document of exactly max_document_bytes", func() {
			rpc, _ := serveIngestion(fake)
			_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte(strings.Repeat("x", 16))))
			Expect(err).NotTo(HaveOccurred())
			converts, _ := fake.recorded()
			Expect(converts).To(HaveLen(1))
		})

		It("rejects one byte more before sending anything to ingestion", func() {
			rpc, _ := serveIngestion(fake)
			before := store.Objects()
			_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte(strings.Repeat("x", 17))))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted))
			Expect(err.Error()).To(ContainSubstring("17 bytes"))
			Expect(err.Error()).To(ContainSubstring("ingestion.max_document_bytes (16)"))
			converts, gets := fake.recorded()
			Expect(converts).To(BeEmpty(), "the fake accepts this tenant-scoped request, so zero calls proves none was sent")
			Expect(gets).To(BeEmpty())
			Expect(store.Objects()).To(Equal(before))
			Expect(outcomes()).To(Equal(map[string]int64{"rejected": 1}))
		})
	})

	It("refuses source_uri without calling ingestion, since fetching URLs is not wired", func() {
		rpc, _ := serveIngestion(fake)
		_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), connect.NewRequest(&pb.ConvertDocumentRequest{
			TenantContext: &pb.TenantContext{TenantId: callerTenant},
			Source:        &pb.ConvertDocumentRequest_SourceUri{SourceUri: "https://example.org/catalog.pdf"},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
		Expect(err.Error()).To(ContainSubstring("upload the document's content instead"))
		converts, _ := fake.recorded()
		Expect(converts).To(BeEmpty())
	})

	It("refuses a request with no tenant on the context without calling ingestion", func() {
		rpc, _ := serveIngestion(fake)
		_, err := newClient(rpc, local).ConvertDocument(context.Background(), contentReq([]byte("x")))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		converts, _ := fake.recorded()
		Expect(converts).To(BeEmpty())
	})

	DescribeTable("withholds ingestion's text when conversion does not complete",
		func(status pb.JobStatus) {
			fake.document = func(int) *pb.Document {
				return &pb.Document{DocumentId: "doc-1", ConversionStatus: status,
					Error: &pb.Error{Code: pb.ErrorCode_ERROR_CODE_INTERNAL, Message: ingestionSecret}}
			}
			rpc, _ := serveIngestion(fake)
			_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
			expectWithheldCode(err, connect.CodeInternal)
			Expect(err.Error()).To(ContainSubstring(`ingestion could not convert document "doc-1"`))
			Expect(err.Error()).To(ContainSubstring(ingestionSecret), "the cause must reach publicError's log")
			Expect(store.Objects()).To(BeEmpty())
			Expect(outcomes()).To(Equal(map[string]int64{"failed": 1}))
		},
		Entry("FAILED", pb.JobStatus_JOB_STATUS_FAILED),
		Entry("CANCELLED", pb.JobStatus_JOB_STATUS_CANCELLED),
		Entry("UNSPECIFIED", pb.JobStatus_JOB_STATUS_UNSPECIFIED),
	)

	It("gives up after convert_timeout when ingestion never completes", func() {
		cfg.ConvertTimeout = 100 * time.Millisecond
		fake.document = func(int) *pb.Document {
			return &pb.Document{DocumentId: "doc-1", ConversionStatus: pb.JobStatus_JOB_STATUS_RUNNING}
		}
		rpc, _ := serveIngestion(fake)
		start := time.Now()
		_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
		Expect(time.Since(start)).To(BeNumerically("<", 3*time.Second), "bounded by convert_timeout, not by the caller")
		expectWithheldCode(err, connect.CodeDeadlineExceeded)
		Expect(err.Error()).To(ContainSubstring(`document "doc-1" did not finish converting within ingestion.convert_timeout (100ms)`))
		_, gets := fake.recorded()
		Expect(len(gets)).To(BeNumerically(">=", 1))
		Expect(store.Objects()).To(BeEmpty())
		Expect(outcomes()).To(Equal(map[string]int64{"timeout": 1}))
	})

	It("stops when the caller cancels and reports the caller's cancellation", func() {
		fake.document = func(int) *pb.Document {
			return &pb.Document{DocumentId: "doc-1", ConversionStatus: pb.JobStatus_JOB_STATUS_RUNNING}
		}
		rpc, _ := serveIngestion(fake)
		ctx, cancel := context.WithCancel(tenantCtx())
		done := make(chan error, 1)
		go func() {
			_, err := newClient(rpc, local).ConvertDocument(ctx, contentReq([]byte("x")))
			done <- err
		}()
		Eventually(func() int { _, gets := fake.recorded(); return len(gets) }).Should(BeNumerically(">=", 1))
		cancel()
		var err error
		Eventually(done).Should(Receive(&err))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeCanceled))
		Expect(err.Error()).NotTo(ContainSubstring(ingestionSecret))
		Expect(outcomes()).To(Equal(map[string]int64{"canceled": 1}))
	})

	It("reports an unreachable ingestion service as Unavailable", func() {
		rpc, srv := serveIngestion(fake)
		srv.Close()
		_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
		expectWithheldCode(err, connect.CodeUnavailable)
		Expect(err.Error()).To(ContainSubstring("ingestion ConvertDocument"))
		Expect(store.Objects()).To(BeEmpty())
		Expect(outcomes()).To(Equal(map[string]int64{"error": 1}))
	})

	DescribeTable("never passes an ingestion RPC error through as a caller-actionable code",
		func(onConvert bool, code connect.Code, want connect.Code) {
			rpcErr := connect.NewError(code, errors.New(ingestionSecret))
			if onConvert {
				fake.convertErr = rpcErr
			} else {
				fake.getErr = rpcErr
			}
			rpc, _ := serveIngestion(fake)
			_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
			expectWithheldCode(err, want)
			Expect(err.Error()).To(ContainSubstring(ingestionSecret), "the cause must reach publicError's log")
			Expect(store.Objects()).To(BeEmpty())
		},
		Entry("ConvertDocument InvalidArgument becomes Internal", true, connect.CodeInvalidArgument, connect.CodeInternal),
		Entry("ConvertDocument ResourceExhausted becomes Internal", true, connect.CodeResourceExhausted, connect.CodeInternal),
		Entry("ConvertDocument PermissionDenied becomes Internal", true, connect.CodePermissionDenied, connect.CodeInternal),
		Entry("ConvertDocument Unavailable stays Unavailable", true, connect.CodeUnavailable, connect.CodeUnavailable),
		Entry("GetDocument NotFound becomes Internal", false, connect.CodeNotFound, connect.CodeInternal),
		Entry("GetDocument FailedPrecondition becomes Internal", false, connect.CodeFailedPrecondition, connect.CodeInternal),
		Entry("GetDocument Internal stays Internal", false, connect.CodeInternal, connect.CodeInternal),
	)

	It("rejects an output URI in another tenant, naming neither it nor the tenant, and stores nothing", func() {
		uri := "file://" + root + "/" + otherTenant + "/" + outputKey
		fake.document = completed(uri)
		rpc, _ := serveIngestion(fake)
		_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
		expectWithheldCode(err, connect.CodeInternal)
		Expect(err).To(MatchError(ErrUnresolvableURI))
		Expect(err.Error()).NotTo(ContainSubstring(otherTenant))
		Expect(err.Error()).NotTo(ContainSubstring(uri))
		Expect(err.Error()).NotTo(ContainSubstring(root))
		Expect(store.Objects()).To(BeEmpty())
		Expect(outcomes()).To(Equal(map[string]int64{"error": 1}))
	})

	It("reports a relative local storage root as a server misconfiguration, not a bad URI", func() {
		fake.document = completed("file://" + root + "/" + callerTenant + "/" + outputKey)
		rpc, _ := serveIngestion(fake)
		_, err := newClient(rpc, StorageBackend{Kind: "local", Root: "relative/objects"}).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
		expectWithheldCode(err, connect.CodeInternal)
		Expect(err).NotTo(MatchError(ErrUnresolvableURI))
		Expect(err.Error()).To(ContainSubstring("absolute path"))
		Expect(store.Objects()).To(BeEmpty())
	})

	It("fails when ingestion reports completion but the output is not in the tenant's storage", func() {
		fake.document = completed("file://" + root + "/" + callerTenant + "/" + outputKey)
		rpc, _ := serveIngestion(fake)
		_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
		expectWithheldCode(err, connect.CodeInternal)
		Expect(err.Error()).To(ContainSubstring("share the same storage backend"))
		Expect(store.Objects()).To(BeEmpty())
	})

	It("does not turn a storage error while checking the output into a caller-actionable code", func() {
		store.ExistsFunc = func(context.Context, string) (bool, error) {
			return false, fmt.Errorf("stat /srv/crosscodex/objects/%s: %w", callerTenant, storage.ErrNotFound)
		}
		fake.document = completed("file://" + root + "/" + callerTenant + "/" + outputKey)
		rpc, _ := serveIngestion(fake)
		_, err := newClient(rpc, local).ConvertDocument(tenantCtx(), contentReq([]byte("x")))
		expectWithheldCode(err, connect.CodeInternal)
	})

	It("refuses an invalid ingestion configuration at construction", func() {
		rpc, _ := serveIngestion(fake)
		_, err := NewIngestionClient(rpc, store, local, config.IngestionConfig{PollInterval: 0, ConvertTimeout: time.Minute, MaxDocumentBytes: 1})
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("ingestion.poll_interval"))
	})

	Describe("DiscardDocument", func() {
		var client *IngestionClient
		BeforeEach(func() {
			rpc, _ := serveIngestion(fake)
			client = newClient(rpc, local)
		})

		It("deletes the ingestion output object", func() {
			Expect(store.Put(tenantCtx(), outputKey, strings.NewReader("{}"))).To(Succeed())
			Expect(store.Put(tenantCtx(), "documents/doc-2/document.json", strings.NewReader("{}"))).To(Succeed())
			Expect(client.DiscardDocument(tenantCtx(), outputKey)).To(Succeed())
			Expect(store.Objects()).To(HaveLen(1))
			Expect(store.Objects()).To(HaveKey("documents/doc-2/document.json"))
			Expect(telemetrytest.FindSpan(tp.GetSpans(), "IngestionClient.DiscardDocument")).NotTo(BeNil())
		})

		It("is idempotent when the object is already gone", func() {
			Expect(client.DiscardDocument(tenantCtx(), outputKey)).To(Succeed())
			store.DeleteFunc = func(context.Context, string) error { return storage.ErrNotFound }
			Expect(client.DiscardDocument(tenantCtx(), outputKey)).To(Succeed())
		})

		It("returns a storage failure with the document it was discarding, leaving the object in place", func() {
			Expect(store.Put(tenantCtx(), outputKey, strings.NewReader("{}"))).To(Succeed())
			boom := errors.New("disk is read-only")
			store.DeleteFunc = func(context.Context, string) error { return boom }
			err := client.DiscardDocument(tenantCtx(), outputKey)
			Expect(err).To(MatchError(boom))
			Expect(err.Error()).To(ContainSubstring("discard document " + outputKey))
			Expect(store.Objects()).To(HaveKey(outputKey))
		})
	})
})
