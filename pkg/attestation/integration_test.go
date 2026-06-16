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

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestAttestationFullPipeline exercises a 3-step compliance pipeline
// (ingestion -> analysis -> synthesis) with EphemeralKeyProvider, layout
// creation, link chaining via shared artifact URIs, dual storage paths
// (job-structured + content-addressed), manifest generation, VerifyLayout,
// VerifyChain, round-trip retrieval, and trace ID correlation across spans.
func TestAttestationFullPipeline(t *testing.T) {
	// --- 1. Ephemeral key provider ---
	kp, err := attestation.NewEphemeralKeyProvider()
	if err != nil {
		t.Fatalf("create ephemeral key provider: %v", err)
	}

	// --- 2. In-memory telemetry ---
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetry provider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("attestation-pipeline-test")
	meter := tp.MeterProvider().Meter("attestation-pipeline-test")

	// --- 3. Generator with telemetry + byproduct enrichment ---
	gen, err := attestation.NewGenerator(kp,
		attestation.WithTelemetry(tracer, meter),
		attestation.WithIncludeByProducts(true),
	)
	if err != nil {
		t.Fatalf("create generator: %v", err)
	}

	// --- 4. Parent span for trace correlation ---
	ctx, parentSpan := tracer.Start(context.Background(), "pipeline.full")
	parentTraceID := parentSpan.SpanContext().TraceID()

	// --- 5. Define pipeline artifacts with chained digests ---
	// Shared artifact URIs: ingestion products -> analysis materials,
	// analysis products -> synthesis materials.
	rawCatalog := []byte(`{"catalog":"nist-800-53","version":"rev5"}`)
	parsedControls := []byte(`{"controls":["ac-1","ac-2","ac-3"]}`)
	analysisResult := []byte(`{"analysis":"gap-assessment","coverage":0.82}`)
	mappingOutput := []byte(`{"mapping":"ac-1->policy-doc","confidence":0.95}`)

	catalogDigest := sha256Hex(rawCatalog)
	controlsDigest := sha256Hex(parsedControls)
	analysisDigest := sha256Hex(analysisResult)
	mappingDigest := sha256Hex(mappingOutput)

	// Step 1: ingestion
	ingestionMaterials := []attestation.Artifact{
		{URI: "oscal/nist-800-53.json", Digest: catalogDigest},
	}
	ingestionProducts := []attestation.Artifact{
		{URI: "parsed/controls.json", Digest: controlsDigest},
	}

	// Step 2: analysis (materials include ingestion products)
	analysisMaterials := []attestation.Artifact{
		{URI: "parsed/controls.json", Digest: controlsDigest}, // chain link
	}
	analysisProducts := []attestation.Artifact{
		{URI: "analysis/gap-report.json", Digest: analysisDigest},
	}

	// Step 3: synthesis (materials include analysis products)
	synthesisMaterials := []attestation.Artifact{
		{URI: "analysis/gap-report.json", Digest: analysisDigest}, // chain link
	}
	synthesisProducts := []attestation.Artifact{
		{URI: "mappings/ac-1-mapping.json", Digest: mappingDigest},
	}

	// --- 6. Create layout with 3 steps ---
	layout, err := gen.CreateLayout(ctx, attestation.LayoutOptions{
		Steps: []attestation.Step{
			{Name: "ingestion", ExpectedMaterials: []string{"oscal/nist-800-53.json"}, ExpectedProducts: []string{"parsed/controls.json"}},
			{Name: "analysis", ExpectedMaterials: []string{"parsed/controls.json"}, ExpectedProducts: []string{"analysis/gap-report.json"}},
			{Name: "synthesis", ExpectedMaterials: []string{"analysis/gap-report.json"}, ExpectedProducts: []string{"mappings/ac-1-mapping.json"}},
		},
		ExpiresIn: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create layout: %v", err)
	}

	// --- 7. Create 3 signed links ---
	linkIngestion, err := gen.CreateLink(ctx, "ingestion", ingestionMaterials, ingestionProducts)
	if err != nil {
		t.Fatalf("create ingestion link: %v", err)
	}
	linkAnalysis, err := gen.CreateLink(ctx, "analysis", analysisMaterials, analysisProducts)
	if err != nil {
		t.Fatalf("create analysis link: %v", err)
	}
	linkSynthesis, err := gen.CreateLink(ctx, "synthesis", synthesisMaterials, synthesisProducts)
	if err != nil {
		t.Fatalf("create synthesis link: %v", err)
	}

	// Verify all links carry the parent trace ID.
	for _, link := range []*attestation.SignedLink{linkIngestion, linkAnalysis, linkSynthesis} {
		if link.TraceID != parentTraceID.String() {
			t.Errorf("link %s trace ID = %s, want %s", link.Step, link.TraceID, parentTraceID)
		}
	}

	// --- 8. Set up local storage ---
	storageDir := filepath.Join(t.TempDir(), "storage")
	storageProvider, err := storage.NewLocal(storageDir, "test-tenant")
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { storageProvider.Close() })

	jobID := "pipeline-run-001"

	// --- 9. Store layout at both path types ---
	layoutJobKey := storage.JobAttestationKey(jobID, "layout.json")
	layoutContentKey := storage.ContentKey(layout.Raw)

	if err := storageProvider.Put(ctx, layoutJobKey, bytes.NewReader(layout.Raw)); err != nil {
		t.Fatalf("store layout (job path): %v", err)
	}
	if err := storageProvider.Put(ctx, layoutContentKey, bytes.NewReader(layout.Raw)); err != nil {
		t.Fatalf("store layout (content path): %v", err)
	}

	// --- 10. Store all 3 links at both path types ---
	links := []*attestation.SignedLink{linkIngestion, linkAnalysis, linkSynthesis}
	linkJobKeys := make([]string, len(links))
	linkContentKeys := make([]string, len(links))

	for i, link := range links {
		filename := link.Step + ".link.json"
		jobKey := storage.JobAttestationKey(jobID, filename)
		contentKey := storage.ContentKey(link.Raw)

		linkJobKeys[i] = jobKey
		linkContentKeys[i] = contentKey

		if err := storageProvider.Put(ctx, jobKey, bytes.NewReader(link.Raw)); err != nil {
			t.Fatalf("store link %s (job path): %v", link.Step, err)
		}
		if err := storageProvider.Put(ctx, contentKey, bytes.NewReader(link.Raw)); err != nil {
			t.Fatalf("store link %s (content path): %v", link.Step, err)
		}
	}

	// --- 11. Generate and store input manifest ---
	// Collect unique artifacts by URI (chain links appear in both products and materials).
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
	manifestKey := storage.JobAttestationKey(jobID, "input_manifest.sha256")
	if err := storageProvider.Put(ctx, manifestKey, bytes.NewReader(manifest)); err != nil {
		t.Fatalf("store manifest: %v", err)
	}

	// --- 12. Verify layout ---
	verifiedLayout, err := gen.VerifyLayout(ctx, layout.Raw)
	if err != nil {
		t.Fatalf("verify layout: %v", err)
	}
	if len(verifiedLayout.Steps) != 3 {
		t.Errorf("verified layout steps = %d, want 3", len(verifiedLayout.Steps))
	}
	expectedStepNames := []string{"ingestion", "analysis", "synthesis"}
	for i, step := range verifiedLayout.Steps {
		if step.Name != expectedStepNames[i] {
			t.Errorf("layout step[%d] = %s, want %s", i, step.Name, expectedStepNames[i])
		}
	}

	// --- 13. Verify each link ---
	for _, link := range links {
		verified, verifyErr := gen.Verify(ctx, link.Raw)
		if verifyErr != nil {
			t.Fatalf("verify link %s: %v", link.Step, verifyErr)
		}
		if verified.Step != link.Step {
			t.Errorf("verified step = %s, want %s", verified.Step, link.Step)
		}
		traceVal, ok := verified.ByProducts["trace_id"].(string)
		if !ok || traceVal != parentTraceID.String() {
			t.Errorf("link %s byproduct trace_id = %v, want %s", link.Step, verified.ByProducts["trace_id"], parentTraceID)
		}
		// WithIncludeByProducts adds span_id, timestamp, hostname.
		for _, key := range []string{"span_id", "timestamp", "hostname"} {
			if _, exists := verified.ByProducts[key]; !exists {
				t.Errorf("link %s missing byproduct %q", link.Step, key)
			}
		}
	}

	// --- 14. Verify chain ---
	if err := gen.VerifyChain(ctx, layout, links); err != nil {
		t.Fatalf("verify chain: %v", err)
	}

	// --- 15. Round-trip: retrieve from job paths and verify ---
	// Layout round-trip.
	layoutReader, err := storageProvider.Get(ctx, layoutJobKey)
	if err != nil {
		t.Fatalf("get layout from job path: %v", err)
	}
	var layoutBuf bytes.Buffer
	if _, err := layoutBuf.ReadFrom(layoutReader); err != nil {
		t.Fatalf("read layout: %v", err)
	}
	layoutReader.Close()

	reVerifiedLayout, err := gen.VerifyLayout(ctx, layoutBuf.Bytes())
	if err != nil {
		t.Fatalf("re-verify layout from storage: %v", err)
	}
	if len(reVerifiedLayout.Steps) != 3 {
		t.Errorf("re-verified layout steps = %d, want 3", len(reVerifiedLayout.Steps))
	}

	// Link round-trips.
	for i, link := range links {
		reader, getErr := storageProvider.Get(ctx, linkJobKeys[i])
		if getErr != nil {
			t.Fatalf("get link %s from job path: %v", link.Step, getErr)
		}
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(reader); readErr != nil {
			t.Fatalf("read link %s: %v", link.Step, readErr)
		}
		reader.Close()

		verified, verifyErr := gen.Verify(ctx, buf.Bytes())
		if verifyErr != nil {
			t.Fatalf("re-verify link %s from storage: %v", link.Step, verifyErr)
		}
		if verified.Step != link.Step {
			t.Errorf("round-trip link step = %s, want %s", verified.Step, link.Step)
		}
	}

	// --- 16. Verify manifest content ---
	manifestReader, err := storageProvider.Get(ctx, manifestKey)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	var manifestBuf bytes.Buffer
	if _, err := manifestBuf.ReadFrom(manifestReader); err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifestReader.Close()

	manifestContent := manifestBuf.String()
	// Manifest lines are sorted by URI in "<digest>  <uri>" format.
	expectedURIs := []string{
		"analysis/gap-report.json",
		"mappings/ac-1-mapping.json",
		"oscal/nist-800-53.json",
		"parsed/controls.json",
	}
	for _, uri := range expectedURIs {
		if !strings.Contains(manifestContent, uri) {
			t.Errorf("manifest missing URI %q", uri)
		}
	}
	// Verify manifest line count (unique artifacts, sorted).
	lines := strings.Split(strings.TrimSpace(manifestContent), "\n")
	if len(lines) != len(expectedURIs) {
		t.Errorf("manifest line count = %d, want %d", len(lines), len(expectedURIs))
	}

	// --- 17. Content-addressed retrieval matches job-path retrieval ---
	for i, link := range links {
		jobReader, getErr := storageProvider.Get(ctx, linkJobKeys[i])
		if getErr != nil {
			t.Fatalf("get link %s job path: %v", link.Step, getErr)
		}
		var jobBuf bytes.Buffer
		if _, readErr := jobBuf.ReadFrom(jobReader); readErr != nil {
			t.Fatalf("read link %s job path: %v", link.Step, readErr)
		}
		jobReader.Close()

		contentReader, getErr := storageProvider.Get(ctx, linkContentKeys[i])
		if getErr != nil {
			t.Fatalf("get link %s content path: %v", link.Step, getErr)
		}
		var contentBuf bytes.Buffer
		if _, readErr := contentBuf.ReadFrom(contentReader); readErr != nil {
			t.Fatalf("read link %s content path: %v", link.Step, readErr)
		}
		contentReader.Close()

		if !bytes.Equal(jobBuf.Bytes(), contentBuf.Bytes()) {
			t.Errorf("link %s: job-path content != content-addressed content", link.Step)
		}
	}

	// Layout content-addressed vs job-path.
	layoutContentReader, err := storageProvider.Get(ctx, layoutContentKey)
	if err != nil {
		t.Fatalf("get layout content path: %v", err)
	}
	var layoutContentBuf bytes.Buffer
	if _, err := layoutContentBuf.ReadFrom(layoutContentReader); err != nil {
		t.Fatalf("read layout content path: %v", err)
	}
	layoutContentReader.Close()

	if !bytes.Equal(layoutBuf.Bytes(), layoutContentBuf.Bytes()) {
		t.Errorf("layout: job-path content != content-addressed content")
	}

	// --- 18. End parent span and verify trace correlation ---
	parentSpan.End()

	spans := tp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans emitted")
	}

	// All spans must share the parent trace ID.
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
		if !found {
			t.Errorf("missing span: %s (have: %v)", name, spanNameList(spans))
			continue
		}
		if tid != parentTraceID {
			t.Errorf("span %s trace ID = %s, want %s", name, tid, parentTraceID)
		}
	}
}

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
