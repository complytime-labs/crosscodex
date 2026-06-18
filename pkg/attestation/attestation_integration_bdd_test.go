//go:build integration

package attestation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestAttestationIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Attestation Integration Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// spanNameList extracts span names for diagnostic output.
func spanNameList(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}

var _ = Describe("Attestation Full Pipeline", func() {
	var (
		kp              attestation.KeyProvider
		tp              *telemetrytest.TestProvider
		tracer          trace.Tracer
		gen             attestation.Generator
		ctx             context.Context
		parentSpan      trace.Span
		parentTraceID   trace.TraceID
		storageProvider storage.Provider
		storageDir      string

		// Pipeline artifacts
		rawCatalog     []byte
		parsedControls []byte
		analysisResult []byte
		mappingOutput  []byte

		catalogDigest  string
		controlsDigest string
		analysisDigest string
		mappingDigest  string

		// Artifact sets
		ingestionMaterials []attestation.Artifact
		ingestionProducts  []attestation.Artifact
		analysisMaterials  []attestation.Artifact
		analysisProducts   []attestation.Artifact
		synthesisMaterials []attestation.Artifact
		synthesisProducts  []attestation.Artifact

		// Attestation objects
		layout        *attestation.SignedLayout
		linkIngestion *attestation.SignedLink
		linkAnalysis  *attestation.SignedLink
		linkSynthesis *attestation.SignedLink
		links         []*attestation.SignedLink

		// Storage keys
		jobID            string
		layoutJobKey     string
		layoutContentKey string
		linkJobKeys      []string
		linkContentKeys  []string
		manifestKey      string
	)

	BeforeEach(func() {
		var err error

		// Ephemeral key provider
		kp, err = attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())

		// In-memory telemetry
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { tp.Shutdown(context.Background()) })

		tracer = tp.TracerProvider().Tracer("attestation-pipeline-test")
		meter := tp.MeterProvider().Meter("attestation-pipeline-test")

		// Generator with telemetry + byproduct enrichment
		gen, err = attestation.NewGenerator(kp,
			attestation.WithTelemetry(tracer, meter),
			attestation.WithIncludeByProducts(true),
		)
		Expect(err).NotTo(HaveOccurred())

		// Parent span for trace correlation
		ctx, parentSpan = tracer.Start(context.Background(), "pipeline.full")
		parentTraceID = parentSpan.SpanContext().TraceID()

		// Define pipeline artifacts with chained digests
		rawCatalog = []byte(`{"catalog":"nist-800-53","version":"rev5"}`)
		parsedControls = []byte(`{"controls":["ac-1","ac-2","ac-3"]}`)
		analysisResult = []byte(`{"analysis":"gap-assessment","coverage":0.82}`)
		mappingOutput = []byte(`{"mapping":"ac-1->policy-doc","confidence":0.95}`)

		catalogDigest = sha256Hex(rawCatalog)
		controlsDigest = sha256Hex(parsedControls)
		analysisDigest = sha256Hex(analysisResult)
		mappingDigest = sha256Hex(mappingOutput)

		ingestionMaterials = []attestation.Artifact{
			{URI: "oscal/nist-800-53.json", Digest: catalogDigest},
		}
		ingestionProducts = []attestation.Artifact{
			{URI: "parsed/controls.json", Digest: controlsDigest},
		}
		analysisMaterials = []attestation.Artifact{
			{URI: "parsed/controls.json", Digest: controlsDigest},
		}
		analysisProducts = []attestation.Artifact{
			{URI: "analysis/gap-report.json", Digest: analysisDigest},
		}
		synthesisMaterials = []attestation.Artifact{
			{URI: "analysis/gap-report.json", Digest: analysisDigest},
		}
		synthesisProducts = []attestation.Artifact{
			{URI: "mappings/ac-1-mapping.json", Digest: mappingDigest},
		}

		// Local storage
		storageDir = filepath.Join(GinkgoT().TempDir(), "storage")
		storageProvider, err = storage.NewLocal(storageDir, "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { storageProvider.Close() })

		jobID = "pipeline-run-001"
	})

	Context("layout and link creation", func() {
		BeforeEach(func() {
			var err error

			// Create layout with 3 steps
			layout, err = gen.CreateLayout(ctx, attestation.LayoutOptions{
				Steps: []attestation.Step{
					{Name: "ingestion", ExpectedMaterials: []string{"oscal/nist-800-53.json"}, ExpectedProducts: []string{"parsed/controls.json"}},
					{Name: "analysis", ExpectedMaterials: []string{"parsed/controls.json"}, ExpectedProducts: []string{"analysis/gap-report.json"}},
					{Name: "synthesis", ExpectedMaterials: []string{"analysis/gap-report.json"}, ExpectedProducts: []string{"mappings/ac-1-mapping.json"}},
				},
				ExpiresIn: 24 * time.Hour,
			})
			Expect(err).NotTo(HaveOccurred())

			// Create 3 signed links
			linkIngestion, err = gen.CreateLink(ctx, "ingestion", ingestionMaterials, ingestionProducts)
			Expect(err).NotTo(HaveOccurred())
			linkAnalysis, err = gen.CreateLink(ctx, "analysis", analysisMaterials, analysisProducts)
			Expect(err).NotTo(HaveOccurred())
			linkSynthesis, err = gen.CreateLink(ctx, "synthesis", synthesisMaterials, synthesisProducts)
			Expect(err).NotTo(HaveOccurred())

			links = []*attestation.SignedLink{linkIngestion, linkAnalysis, linkSynthesis}
		})

		It("should carry the parent trace ID on all links", func() {
			for _, link := range links {
				Expect(link.TraceID).To(Equal(parentTraceID.String()),
					"link %s trace ID mismatch", link.Step)
			}
		})

		Context("storage operations", func() {
			BeforeEach(func() {
				// Store layout at both path types
				layoutJobKey = storage.JobAttestationKey(jobID, "layout.json")
				layoutContentKey = storage.ContentKey(layout.Raw)

				Expect(storageProvider.Put(ctx, layoutJobKey, bytes.NewReader(layout.Raw))).To(Succeed())
				Expect(storageProvider.Put(ctx, layoutContentKey, bytes.NewReader(layout.Raw))).To(Succeed())

				// Store all 3 links at both path types
				linkJobKeys = make([]string, len(links))
				linkContentKeys = make([]string, len(links))

				for i, link := range links {
					filename := link.Step + ".link.json"
					jk := storage.JobAttestationKey(jobID, filename)
					ck := storage.ContentKey(link.Raw)
					linkJobKeys[i] = jk
					linkContentKeys[i] = ck

					Expect(storageProvider.Put(ctx, jk, bytes.NewReader(link.Raw))).To(Succeed())
					Expect(storageProvider.Put(ctx, ck, bytes.NewReader(link.Raw))).To(Succeed())
				}

				// Generate and store input manifest
				allRaw := append(append(ingestionMaterials, ingestionProducts...), analysisMaterials...)
				allRaw = append(allRaw, analysisProducts...)
				allRaw = append(allRaw, synthesisMaterials...)
				allRaw = append(allRaw, synthesisProducts...)
				seen := make(map[string]bool, len(allRaw))
				var allArtifacts []attestation.Artifact
				for _, a := range allRaw {
					if !seen[a.URI] {
						seen[a.URI] = true
						allArtifacts = append(allArtifacts, a)
					}
				}

				manifest := attestation.GenerateManifest(allArtifacts)
				manifestKey = storage.JobAttestationKey(jobID, "input_manifest.sha256")
				Expect(storageProvider.Put(ctx, manifestKey, bytes.NewReader(manifest))).To(Succeed())
			})

			It("should verify the layout with 3 steps", func() {
				verifiedLayout, err := gen.VerifyLayout(ctx, layout.Raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(verifiedLayout.Steps).To(HaveLen(3))

				expectedStepNames := []string{"ingestion", "analysis", "synthesis"}
				for i, step := range verifiedLayout.Steps {
					Expect(step.Name).To(Equal(expectedStepNames[i]))
				}
			})

			It("should verify each link with trace ID and byproducts", func() {
				for _, link := range links {
					verified, err := gen.Verify(ctx, link.Raw)
					Expect(err).NotTo(HaveOccurred())
					Expect(verified.Step).To(Equal(link.Step))

					traceVal, ok := verified.ByProducts["trace_id"].(string)
					Expect(ok).To(BeTrue(), "link %s byproduct trace_id not a string", link.Step)
					Expect(traceVal).To(Equal(parentTraceID.String()))

					for _, key := range []string{"span_id", "timestamp", "hostname"} {
						Expect(verified.ByProducts).To(HaveKey(key),
							"link %s missing byproduct %q", link.Step, key)
					}
				}
			})

			It("should verify the full chain", func() {
				Expect(gen.VerifyChain(ctx, layout, links)).To(Succeed())
			})

			It("should round-trip layout from job-path storage", func() {
				layoutReader, err := storageProvider.Get(ctx, layoutJobKey)
				Expect(err).NotTo(HaveOccurred())

				var layoutBuf bytes.Buffer
				_, err = layoutBuf.ReadFrom(layoutReader)
				Expect(err).NotTo(HaveOccurred())
				layoutReader.Close()

				reVerifiedLayout, err := gen.VerifyLayout(ctx, layoutBuf.Bytes())
				Expect(err).NotTo(HaveOccurred())
				Expect(reVerifiedLayout.Steps).To(HaveLen(3))
			})

			It("should round-trip links from job-path storage", func() {
				for i, link := range links {
					reader, err := storageProvider.Get(ctx, linkJobKeys[i])
					Expect(err).NotTo(HaveOccurred())

					var buf bytes.Buffer
					_, err = buf.ReadFrom(reader)
					Expect(err).NotTo(HaveOccurred())
					reader.Close()

					verified, err := gen.Verify(ctx, buf.Bytes())
					Expect(err).NotTo(HaveOccurred())
					Expect(verified.Step).To(Equal(link.Step))
				}
			})

			It("should have a valid manifest with all unique artifact URIs", func() {
				manifestReader, err := storageProvider.Get(ctx, manifestKey)
				Expect(err).NotTo(HaveOccurred())

				var manifestBuf bytes.Buffer
				_, err = manifestBuf.ReadFrom(manifestReader)
				Expect(err).NotTo(HaveOccurred())
				manifestReader.Close()

				manifestContent := manifestBuf.String()
				expectedURIs := []string{
					"analysis/gap-report.json",
					"mappings/ac-1-mapping.json",
					"oscal/nist-800-53.json",
					"parsed/controls.json",
				}
				for _, uri := range expectedURIs {
					Expect(manifestContent).To(ContainSubstring(uri))
				}

				lines := strings.Split(strings.TrimSpace(manifestContent), "\n")
				Expect(lines).To(HaveLen(len(expectedURIs)))
			})

			It("should match content-addressed and job-path storage for links", func() {
				for i, link := range links {
					jobReader, err := storageProvider.Get(ctx, linkJobKeys[i])
					Expect(err).NotTo(HaveOccurred())
					var jobBuf bytes.Buffer
					_, err = jobBuf.ReadFrom(jobReader)
					Expect(err).NotTo(HaveOccurred())
					jobReader.Close()

					contentReader, err := storageProvider.Get(ctx, linkContentKeys[i])
					Expect(err).NotTo(HaveOccurred())
					var contentBuf bytes.Buffer
					_, err = contentBuf.ReadFrom(contentReader)
					Expect(err).NotTo(HaveOccurred())
					contentReader.Close()

					Expect(bytes.Equal(jobBuf.Bytes(), contentBuf.Bytes())).To(BeTrue(),
						"link %s: job-path content != content-addressed content", link.Step)
				}
			})

			It("should match content-addressed and job-path storage for layout", func() {
				jobReader, err := storageProvider.Get(ctx, layoutJobKey)
				Expect(err).NotTo(HaveOccurred())
				var jobBuf bytes.Buffer
				_, err = jobBuf.ReadFrom(jobReader)
				Expect(err).NotTo(HaveOccurred())
				jobReader.Close()

				contentReader, err := storageProvider.Get(ctx, layoutContentKey)
				Expect(err).NotTo(HaveOccurred())
				var contentBuf bytes.Buffer
				_, err = contentBuf.ReadFrom(contentReader)
				Expect(err).NotTo(HaveOccurred())
				contentReader.Close()

				Expect(bytes.Equal(jobBuf.Bytes(), contentBuf.Bytes())).To(BeTrue(),
					"layout: job-path content != content-addressed content")
			})

			It("should correlate all spans to the parent trace ID", func() {
				// Each It block gets a fresh TestProvider, so we must exercise
				// the verify operations within this block to capture their spans.
				_, err := gen.VerifyLayout(ctx, layout.Raw)
				Expect(err).NotTo(HaveOccurred())

				for _, link := range links {
					_, err = gen.Verify(ctx, link.Raw)
					Expect(err).NotTo(HaveOccurred())
				}

				Expect(gen.VerifyChain(ctx, layout, links)).To(Succeed())

				parentSpan.End()

				spans := tp.GetSpans()
				Expect(spans).NotTo(BeEmpty())

				spanNames := make(map[string]trace.TraceID)
				for _, s := range spans {
					spanNames[s.Name()] = s.SpanContext().TraceID()
				}

				expectedSpans := []string{
					"pipeline.full",
					"attestation.CreateLayout",
					"attestation.CreateLink",
					"attestation.VerifyLayout",
					"attestation.Verify",
					"attestation.VerifyChain",
				}
				for _, name := range expectedSpans {
					tid, found := spanNames[name]
					Expect(found).To(BeTrue(),
						"missing span: %s (have: %v)", name, spanNameList(spans))
					Expect(tid).To(Equal(parentTraceID),
						"span %s trace ID mismatch", name)
				}
			})
		})
	})
})
