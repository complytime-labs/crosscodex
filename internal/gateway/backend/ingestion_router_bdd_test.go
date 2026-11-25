package backend

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	routerTenant    = "tenant-a"
	routerOutputKey = "documents/doc-1/document.json"
	// passthroughKey has the documents/<uuid>.json shape PassthroughIngestion
	// stores under.
	passthroughKey = "documents/0b7e6f3c-5d2a-4c1e-9f3b-2a6d8e4c1f70.json"
)

var (
	oscalCatalogJSON = []byte(`{"catalog":{"uuid":"11111111-1111-4111-8111-111111111111","metadata":{"title":"t"}}}`)
	pdfBytes         = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\n")
)

var _ gateway.IngestionBackend = (*IngestionRouter)(nil)

func routerTenantCtx() context.Context {
	ctx, err := tenant.WithTenant(context.Background(), routerTenant)
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

func routerContentReq(content []byte) *connect.Request[pb.ConvertDocumentRequest] {
	return connect.NewRequest(&pb.ConvertDocumentRequest{
		TenantContext: &pb.TenantContext{TenantId: routerTenant},
		Source:        &pb.ConvertDocumentRequest_Content{Content: content},
	})
}

func routerIngestionConfig() config.IngestionConfig {
	return config.IngestionConfig{PollInterval: 5 * time.Millisecond, ConvertTimeout: 5 * time.Second, MaxDocumentBytes: 1 << 20}
}

// completedAt makes the fake report a finished conversion whose output is uri.
func completedAt(uri string) func(int) *pb.Document {
	return func(int) *pb.Document {
		return &pb.Document{DocumentId: "doc-1", ConversionStatus: pb.JobStatus_JOB_STATUS_COMPLETED, StructuredJsonUri: uri}
	}
}

// expectPassthroughKey asserts key has the documents/<uuid>.json shape.
func expectPassthroughKey(key string) {
	Expect(key).To(MatchRegexp(`^documents/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json$`))
}

var _ = Describe("IngestionRouter", func() {
	var (
		ptStore *storagetest.Fake // the passthrough's storage
		clStore *storagetest.Fake // the client's storage
		fake    *fakeIngestionServer
		root    string
	)

	BeforeEach(func() {
		ptStore = &storagetest.Fake{}
		clStore = &storagetest.Fake{}
		fake = &fakeIngestionServer{}
		root = "/srv/crosscodex/objects"
	})

	configured := func() *IngestionRouter {
		rpc, _ := serveIngestion(fake)
		client, err := NewIngestionClient(rpc, clStore, StorageBackend{Kind: "local", Root: root}, routerIngestionConfig())
		Expect(err).NotTo(HaveOccurred())
		return NewIngestionRouter(NewPassthroughIngestion(ptStore), client)
	}
	unconfigured := func() *IngestionRouter {
		return NewIngestionRouter(NewPassthroughIngestion(ptStore), nil)
	}

	Describe("ConvertDocument", func() {
		It("stores OSCAL catalog JSON through the passthrough without calling ingestion", func() {
			resp, err := configured().ConvertDocument(routerTenantCtx(), routerContentReq(oscalCatalogJSON))
			Expect(err).NotTo(HaveOccurred())

			expectPassthroughKey(resp.Msg.GetDocumentId())
			Expect(ptStore.Objects()).To(HaveKeyWithValue(resp.Msg.GetDocumentId(), oscalCatalogJSON))
			converts, gets := fake.recorded()
			Expect(converts).To(BeEmpty())
			Expect(gets).To(BeEmpty())
			Expect(clStore.Objects()).To(BeEmpty())
		})

		It("sends a PDF to the ingestion service and returns its output key", func() {
			Expect(clStore.Put(routerTenantCtx(), routerOutputKey, strings.NewReader(`{"schema_version":"1.0"}`))).To(Succeed())
			fake.document = completedAt("file://" + root + "/" + routerTenant + "/" + routerOutputKey)

			resp, err := configured().ConvertDocument(routerTenantCtx(), routerContentReq(pdfBytes))
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.Msg.GetDocumentId()).To(Equal(routerOutputKey))
			converts, _ := fake.recorded()
			Expect(converts).To(HaveLen(1))
			Expect(converts[0].GetContent()).To(Equal(pdfBytes))
			Expect(ptStore.Objects()).To(BeEmpty())
		})

		DescribeTable("keeps every OSCAL model off the ingestion service, so ParseCatalog can explain a non-catalog model",
			func(content []byte) {
				resp, err := configured().ConvertDocument(routerTenantCtx(), routerContentReq(content))
				Expect(err).NotTo(HaveOccurred())

				expectPassthroughKey(resp.Msg.GetDocumentId())
				Expect(ptStore.Objects()).To(HaveKeyWithValue(resp.Msg.GetDocumentId(), content))
				converts, gets := fake.recorded()
				Expect(converts).To(BeEmpty())
				Expect(gets).To(BeEmpty())
			},
			Entry("a profile", []byte(`{"profile":{"uuid":"x","imports":[]}}`)),
			Entry("a component-definition", []byte(`{"component-definition":{"uuid":"x"}}`)),
			Entry("a mapping-collection", []byte(`{"mapping-collection":{"uuid":"x"}}`)),
		)

		It("sends a JSON object that is no OSCAL model to the ingestion service", func() {
			Expect(clStore.Put(routerTenantCtx(), routerOutputKey, strings.NewReader("{}"))).To(Succeed())
			fake.document = completedAt("file://" + root + "/" + routerTenant + "/" + routerOutputKey)
			content := []byte(`{"controls":[{"id":"x","text":"y"}]}`)

			resp, err := configured().ConvertDocument(routerTenantCtx(), routerContentReq(content))
			Expect(err).NotTo(HaveOccurred())

			Expect(resp.Msg.GetDocumentId()).To(Equal(routerOutputKey))
			converts, _ := fake.recorded()
			Expect(converts).To(HaveLen(1))
			Expect(ptStore.Objects()).To(BeEmpty())
		})

		DescribeTable("sends a request with a mapping to the ingestion service, OSCAL JSON included, since only it reads mappings",
			func(content []byte) {
				Expect(clStore.Put(routerTenantCtx(), routerOutputKey, strings.NewReader("{}"))).To(Succeed())
				fake.document = completedAt("file://" + root + "/" + routerTenant + "/" + routerOutputKey)
				req := routerContentReq(content)
				req.Msg.Mapping = "owasp-asvs-5.0.0-json"

				resp, err := configured().ConvertDocument(routerTenantCtx(), req)
				Expect(err).NotTo(HaveOccurred())

				Expect(resp.Msg.GetDocumentId()).To(Equal(routerOutputKey))
				converts, _ := fake.recorded()
				Expect(converts).To(HaveLen(1))
				Expect(converts[0].GetMapping()).To(Equal("owasp-asvs-5.0.0-json"))
				Expect(ptStore.Objects()).To(BeEmpty())
			},
			Entry("structured JSON", []byte(`{"chapters":[]}`)),
			Entry("OSCAL catalog JSON", oscalCatalogJSON),
		)

		It("refuses a mapping when no ingestion service is configured, storing nothing", func() {
			req := routerContentReq([]byte(`{"chapters":[]}`))
			req.Msg.Mapping = "owasp-asvs-5.0.0-json"

			_, err := unconfigured().ConvertDocument(routerTenantCtx(), req)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(err).To(MatchError(ContainSubstring(`mapping "owasp-asvs-5.0.0-json"`)))
			Expect(err).To(MatchError(ContainSubstring("ingestion.address")))
			Expect(err).To(MatchError(ContainSubstring("or import the file without a mapping")))
			Expect(ptStore.Objects()).To(BeEmpty())
		})

		DescribeTable("without an ingestion client, stores any inline content through the passthrough as before",
			func(content []byte) {
				resp, err := unconfigured().ConvertDocument(routerTenantCtx(), routerContentReq(content))
				Expect(err).NotTo(HaveOccurred())

				Expect(resp.Msg.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED))
				expectPassthroughKey(resp.Msg.GetDocumentId())
				Expect(ptStore.Objects()).To(HaveKeyWithValue(resp.Msg.GetDocumentId(), content))
			},
			Entry("a PDF", pdfBytes),
			Entry("OSCAL catalog JSON", oscalCatalogJSON),
			Entry("an OSCAL profile", []byte(`{"profile":{"imports":[]}}`)),
		)

		DescribeTable("leaves source_uri unimplemented, exactly as the passthrough reports it",
			func(build func() *IngestionRouter) {
				req := func() *connect.Request[pb.ConvertDocumentRequest] {
					return connect.NewRequest(&pb.ConvertDocumentRequest{
						TenantContext: &pb.TenantContext{TenantId: routerTenant},
						Source:        &pb.ConvertDocumentRequest_SourceUri{SourceUri: "https://example.org/catalog.pdf"},
					})
				}
				_, want := NewPassthroughIngestion(ptStore).ConvertDocument(routerTenantCtx(), req())
				Expect(want).To(HaveOccurred())

				_, err := build().ConvertDocument(routerTenantCtx(), req())
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
				Expect(err).To(MatchError(want.Error()))
				converts, _ := fake.recorded()
				Expect(converts).To(BeEmpty())
				Expect(ptStore.Objects()).To(BeEmpty())
			},
			Entry("with an ingestion client", func() *IngestionRouter { return configured() }),
			Entry("without an ingestion client", func() *IngestionRouter { return unconfigured() }),
		)
	})

	Describe("DiscardDocument", func() {
		const otherIngestionKey = "documents/doc-2/document.json"
		const otherPassthroughKey = "documents/5c1d2e3f-4a5b-4c6d-8e7f-901a2b3c4d5e.json"

		// seed puts the same keys in both stores, so a delete on the wrong
		// backend shows up as a missing object there.
		seed := func() {
			for _, st := range []*storagetest.Fake{ptStore, clStore} {
				for _, key := range []string{passthroughKey, otherPassthroughKey, routerOutputKey, otherIngestionKey} {
					Expect(st.Put(routerTenantCtx(), key, strings.NewReader("{}"))).To(Succeed())
				}
			}
		}
		keysOf := func(st *storagetest.Fake) []string {
			var keys []string
			for k := range st.Objects() {
				keys = append(keys, k)
			}
			return keys
		}
		all := []string{passthroughKey, otherPassthroughKey, routerOutputKey, otherIngestionKey}

		BeforeEach(seed)

		It("deletes a documents/<uuid>.json key through the passthrough only", func() {
			Expect(configured().DiscardDocument(routerTenantCtx(), passthroughKey)).To(Succeed())

			Expect(keysOf(ptStore)).To(ConsistOf(otherPassthroughKey, routerOutputKey, otherIngestionKey))
			Expect(keysOf(clStore)).To(ConsistOf(all))
		})

		It("deletes a documents/<id>/<file> key through the ingestion client only", func() {
			Expect(configured().DiscardDocument(routerTenantCtx(), routerOutputKey)).To(Succeed())

			Expect(keysOf(clStore)).To(ConsistOf(passthroughKey, otherPassthroughKey, otherIngestionKey))
			Expect(keysOf(ptStore)).To(ConsistOf(all))
		})

		It("deletes a passthrough key without an ingestion client", func() {
			Expect(unconfigured().DiscardDocument(routerTenantCtx(), passthroughKey)).To(Succeed())

			Expect(keysOf(ptStore)).To(ConsistOf(otherPassthroughKey, routerOutputKey, otherIngestionKey))
		})

		DescribeTable("refuses a key neither backend stores, deleting nothing",
			func(build func() *IngestionRouter, key string) {
				err := build().DiscardDocument(routerTenantCtx(), key)

				Expect(err).To(MatchError(ContainSubstring("cannot discard document")))
				Expect(err).To(MatchError(ContainSubstring("nothing was deleted")))
				Expect(keysOf(ptStore)).To(ConsistOf(all))
				Expect(keysOf(clStore)).To(ConsistOf(all))
			},
			Entry("an empty key", func() *IngestionRouter { return configured() }, ""),
			Entry("a key outside documents/", func() *IngestionRouter { return configured() }, "attestations/0b7e6f3c-5d2a-4c1e-9f3b-2a6d8e4c1f70.json"),
			Entry("a documents/ key without a uuid", func() *IngestionRouter { return configured() }, "documents/catalog.json"),
			Entry("a documents/ uuid key without .json", func() *IngestionRouter { return configured() }, "documents/0b7e6f3c-5d2a-4c1e-9f3b-2a6d8e4c1f70"),
			Entry("a non-canonical uuid", func() *IngestionRouter { return configured() }, "documents/{0b7e6f3c-5d2a-4c1e-9f3b-2a6d8e4c1f70}.json"),
			Entry("a nested ingestion path", func() *IngestionRouter { return configured() }, "documents/doc-1/sub/document.json"),
			Entry("an ingestion path with an empty segment", func() *IngestionRouter { return configured() }, "documents//document.json"),
			Entry("an ingestion path with a dot-dot segment", func() *IngestionRouter { return configured() }, "documents/../document.json"),
			Entry("an ingestion key without an ingestion client", func() *IngestionRouter { return unconfigured() }, routerOutputKey),
		)

		It("names the layouts it accepts in the refusal", func() {
			err := unconfigured().DiscardDocument(routerTenantCtx(), routerOutputKey)
			Expect(err).To(MatchError(ContainSubstring("documents/<uuid>.json")))
			Expect(err).To(MatchError(ContainSubstring("ingestion.address")))
		})
	})
})

// serveIngestionH2C starts fake over gRPC on unencrypted HTTP/2, what a
// client built with tls.mode off speaks, and returns its host:port.
func serveIngestionH2C(fake *fakeIngestionServer) string {
	_, handler := crosscodexv1connect.NewIngestionServiceHandler(fake)
	srv := httptest.NewUnstartedServer(handler)
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = p
	srv.Start()
	DeferCleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	Expect(err).NotTo(HaveOccurred())
	return u.Host
}

var _ = Describe("NewIngestion", func() {
	var (
		store *storagetest.Fake
		fake  *fakeIngestionServer
		ing   config.IngestionConfig
		off   config.TLSConfig
	)

	BeforeEach(func() {
		store = &storagetest.Fake{}
		fake = &fakeIngestionServer{}
		ing = routerIngestionConfig()
		off = config.TLSConfig{Mode: "off"}
	})

	It("routes everything through the passthrough when ingestion.address is empty, without resolving storage or TLS", func() {
		ing.Address = ""
		b, err := NewIngestion(context.Background(), ing, config.TLSConfig{Mode: "bogus"},
			config.ObjectStorageConfig{Backend: "local", BasePath: "/does/not/exist"}, store)
		Expect(err).NotTo(HaveOccurred())

		resp, err := b.ConvertDocument(routerTenantCtx(), routerContentReq(pdfBytes))
		Expect(err).NotTo(HaveOccurred())
		expectPassthroughKey(resp.Msg.GetDocumentId())
		Expect(store.Objects()).To(HaveKeyWithValue(resp.Msg.GetDocumentId(), pdfBytes))
	})

	It("converts through ingestion, accepting file:// URIs under the symlink-resolved local base_path", func() {
		real := GinkgoT().TempDir()
		link := filepath.Join(GinkgoT().TempDir(), "objects")
		Expect(os.Symlink(real, link)).To(Succeed())
		resolved, err := filepath.EvalSymlinks(real)
		Expect(err).NotTo(HaveOccurred())
		// The ingestion service writes under the resolved path
		// (crosscodex-ingestion storage.py resolves its base_path).
		fake.document = completedAt("file://" + resolved + "/" + routerTenant + "/" + routerOutputKey)
		Expect(store.Put(routerTenantCtx(), routerOutputKey, strings.NewReader("{}"))).To(Succeed())
		ing.Address = serveIngestionH2C(fake)

		b, err := NewIngestion(context.Background(), ing, off, config.ObjectStorageConfig{Backend: "local", BasePath: link}, store)
		Expect(err).NotTo(HaveOccurred())

		resp, err := b.ConvertDocument(routerTenantCtx(), routerContentReq(pdfBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal(routerOutputKey))
		converts, _ := fake.recorded()
		Expect(converts).To(HaveLen(1))
	})

	It("converts through ingestion, accepting s3:// URIs in the configured bucket", func() {
		fake.document = completedAt("s3://crosscodex-objects/" + routerTenant + "/" + routerOutputKey)
		Expect(store.Put(routerTenantCtx(), routerOutputKey, strings.NewReader("{}"))).To(Succeed())
		ing.Address = serveIngestionH2C(fake)

		b, err := NewIngestion(context.Background(), ing, off, config.ObjectStorageConfig{Backend: "s3", Bucket: "crosscodex-objects"}, store)
		Expect(err).NotTo(HaveOccurred())

		resp, err := b.ConvertDocument(routerTenantCtx(), routerContentReq(pdfBytes))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).To(Equal(routerOutputKey))
	})

	Describe("with TLS on", func() {
		var serverOnly config.TLSConfig
		var pki *testcerts.PKI

		BeforeEach(func() {
			var err error
			pki, err = testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			caPath := filepath.Join(GinkgoT().TempDir(), "ca.pem")
			Expect(os.WriteFile(caPath, pki.CACert, 0o600)).To(Succeed())
			// Server-only TLS on the ingestion target alone; the global mode stays off.
			serverOnly = config.TLSConfig{Mode: "off", Targets: map[string]config.TLSOverride{
				ingestionTLSTarget: {Mode: "server-only", CA: caPath},
			}}
			fake.document = completedAt("s3://crosscodex-objects/" + routerTenant + "/" + routerOutputKey)
		})

		It("converts over HTTP/2 with TLS, verifying the server against tls.targets.ingestion's CA", func() {
			cert, err := tls.X509KeyPair(pki.ServerCert, pki.ServerKey)
			Expect(err).NotTo(HaveOccurred())
			_, handler := crosscodexv1connect.NewIngestionServiceHandler(fake)
			srv := httptest.NewUnstartedServer(handler)
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			srv.EnableHTTP2 = true
			srv.StartTLS()
			DeferCleanup(srv.Close)
			u, err := url.Parse(srv.URL)
			Expect(err).NotTo(HaveOccurred())
			ing.Address = u.Host
			Expect(store.Put(routerTenantCtx(), routerOutputKey, strings.NewReader("{}"))).To(Succeed())

			b, err := NewIngestion(context.Background(), ing, serverOnly, config.ObjectStorageConfig{Backend: "s3", Bucket: "crosscodex-objects"}, store)
			Expect(err).NotTo(HaveOccurred())

			resp, err := b.ConvertDocument(routerTenantCtx(), routerContentReq(pdfBytes))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetDocumentId()).To(Equal(routerOutputKey))
			converts, _ := fake.recorded()
			Expect(converts).To(HaveLen(1))
		})

		It("fails against a plaintext server without sending it the document", func() {
			ing.Address = serveIngestionH2C(fake)

			b, err := NewIngestion(context.Background(), ing, serverOnly, config.ObjectStorageConfig{Backend: "s3", Bucket: "crosscodex-objects"}, store)
			Expect(err).NotTo(HaveOccurred())

			_, err = b.ConvertDocument(routerTenantCtx(), routerContentReq(pdfBytes))
			Expect(err).To(HaveOccurred())
			converts, gets := fake.recorded()
			Expect(converts).To(BeEmpty())
			Expect(gets).To(BeEmpty())
			Expect(store.Objects()).To(BeEmpty())
		})
	})

	It("still keeps OSCAL catalog JSON off the ingestion service", func() {
		ing.Address = serveIngestionH2C(fake)
		b, err := NewIngestion(context.Background(), ing, off, config.ObjectStorageConfig{Backend: "s3", Bucket: "crosscodex-objects"}, store)
		Expect(err).NotTo(HaveOccurred())

		resp, err := b.ConvertDocument(routerTenantCtx(), routerContentReq(oscalCatalogJSON))
		Expect(err).NotTo(HaveOccurred())
		expectPassthroughKey(resp.Msg.GetDocumentId())
		converts, _ := fake.recorded()
		Expect(converts).To(BeEmpty())
	})

	DescribeTable("refuses a configuration it cannot convert with, naming the setting to fix",
		func(mutate func(*config.IngestionConfig, *config.TLSConfig, *config.ObjectStorageConfig), wantSetting string) {
			ing.Address = "ingestion.internal:8090"
			objects := config.ObjectStorageConfig{Backend: "s3", Bucket: "crosscodex-objects"}
			tlsCfg := off
			mutate(&ing, &tlsCfg, &objects)

			b, err := NewIngestion(context.Background(), ing, tlsCfg, objects, store)
			Expect(b).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring(wantSetting)))
			Expect(store.Objects()).To(BeEmpty())
		},
		Entry("a local base_path that does not exist",
			func(_ *config.IngestionConfig, _ *config.TLSConfig, o *config.ObjectStorageConfig) {
				*o = config.ObjectStorageConfig{Backend: "local", BasePath: filepath.Join(GinkgoT().TempDir(), "missing")}
			}, "storage.objects.base_path"),
		Entry("a local backend without base_path",
			func(_ *config.IngestionConfig, _ *config.TLSConfig, o *config.ObjectStorageConfig) {
				*o = config.ObjectStorageConfig{Backend: "local"}
			}, "storage.objects.base_path"),
		Entry("an s3 backend without a bucket",
			func(_ *config.IngestionConfig, _ *config.TLSConfig, o *config.ObjectStorageConfig) {
				*o = config.ObjectStorageConfig{Backend: "s3"}
			}, "storage.objects.bucket"),
		Entry("an unsupported storage backend",
			func(_ *config.IngestionConfig, _ *config.TLSConfig, o *config.ObjectStorageConfig) {
				*o = config.ObjectStorageConfig{Backend: "ftp"}
			}, "storage.objects.backend"),
		Entry("an invalid ingestion block",
			func(i *config.IngestionConfig, _ *config.TLSConfig, _ *config.ObjectStorageConfig) {
				i.PollInterval = 0
			}, "ingestion.poll_interval"),
		Entry("an unresolvable tls.targets.ingestion",
			func(_ *config.IngestionConfig, t *config.TLSConfig, _ *config.ObjectStorageConfig) {
				*t = config.TLSConfig{Mode: "off", Targets: map[string]config.TLSOverride{"ingestion": {Mode: "bogus"}}}
			}, "tls.targets.ingestion"),
	)
})
