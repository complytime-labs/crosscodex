//go:build integration

package attestation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
	"go.opentelemetry.io/otel/trace"
)

// TestAttestationDemoPipeline proves the three-system bridge: OTel traces,
// NATS audit streams, and in-toto attestations, all correlated by the same
// trace ID. Uses embedded NATS and local storage — no containers needed.
func TestAttestationDemoPipeline(t *testing.T) {
	// 1. Generate ephemeral ECDSA P-256 keys via pki package.
	ca, err := pki.GenerateCA()
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "signing.pem")
	if err := os.WriteFile(privPath, ca.KeyPEM, 0600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(ca.Cert.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	pubPath := filepath.Join(tmpDir, "verification.pem")
	if err := os.WriteFile(pubPath, pubPEM, 0644); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	kp := &attestation.FileKeyProvider{
		PrivateKeyPath: privPath,
		PublicKeyPath:  pubPath,
	}

	// 2. Init in-memory telemetry provider.
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetry provider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("attestation-demo")
	meter := tp.MeterProvider().Meter("attestation-demo")

	// 3. Create embedded NATS client (no external server required).
	storeDir := filepath.Join(t.TempDir(), "nats-store")
	natsCfg := config.NATSConfig{
		URL: "", // empty = embedded mode
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: config.NATSStreamsConfig{
			AuditLLMRetention:    24 * time.Hour,
			AuditEventsRetention: 24 * time.Hour,
		},
	}
	natsClient, err := natsbus.New(natsCfg, natsbus.WithTelemetry(tracer, meter))
	if err != nil {
		t.Fatalf("create NATS client: %v", err)
	}
	t.Cleanup(func() { natsClient.Close() })

	// 4. Create local filesystem storage for attestation bundles.
	storageDir := filepath.Join(t.TempDir(), "storage")
	storageProvider, err := storage.NewLocal(storageDir, "demo-tenant")
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { storageProvider.Close() })

	// 5. Create attestation Generator with telemetry.
	gen, err := attestation.NewGenerator(kp, attestation.WithTelemetry(tracer, meter))
	if err != nil {
		t.Fatalf("create generator: %v", err)
	}

	// 6. Start a parent span — all downstream operations inherit its trace ID.
	ctx, parentSpan := tracer.Start(context.Background(), "attestation.demo.ingest")
	publisherTraceID := parentSpan.SpanContext().TraceID()

	// 7. Define materials (inputs) and products (outputs).
	inputData := []byte(`{"catalog": "nist-800-53", "version": "rev5"}`)
	outputData := []byte(`{"mapping": "ac-1", "confidence": 0.95}`)
	inputHash := sha256Hex(inputData)
	outputHash := sha256Hex(outputData)

	materials := []attestation.Artifact{
		{URI: "oscal/nist-800-53.json", Digest: inputHash},
	}
	products := []attestation.Artifact{
		{URI: "mappings/ac-1-mapping.json", Digest: outputHash},
	}

	// 8. Create a signed in-toto link attestation.
	signedLink, err := gen.CreateLink(ctx, "catalog.ingest", materials, products)
	if err != nil {
		t.Fatalf("create link: %v", err)
	}

	// 9. Verify trace ID correlation — the link must embed the parent span's trace ID.
	if signedLink.TraceID != publisherTraceID.String() {
		t.Errorf("link trace ID = %s, want %s", signedLink.TraceID, publisherTraceID)
	}

	// 10. Store attestation in content-addressed storage.
	storageKey := storage.ContentKey(signedLink.Raw)
	if err := storageProvider.Put(ctx, storageKey, bytes.NewReader(signedLink.Raw)); err != nil {
		t.Fatalf("store attestation: %v", err)
	}

	// 11. Publish audit event via NATS with tenant context.
	tenantCtx, err := tenant.WithTenant(ctx, "demo-tenant")
	if err != nil {
		t.Fatalf("tenant context: %v", err)
	}
	subject, err := natsbus.AuditSubject("demo-tenant", natsbus.AuditEvents, "link-created")
	if err != nil {
		t.Fatalf("audit subject: %v", err)
	}

	// Subscribe before publish to avoid race.
	resultCh := make(chan *natsbus.Message, 1)
	sub, err := natsClient.Subscribe(tenantCtx, subject, func(_ context.Context, msg *natsbus.Message) error {
		resultCh <- msg
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })
	// Allow subscription to propagate in embedded NATS.
	time.Sleep(100 * time.Millisecond)

	if err := natsClient.Publish(tenantCtx, subject, signedLink.Raw); err != nil {
		t.Fatalf("publish audit event: %v", err)
	}

	// 12. Wait for NATS subscriber — verify trace ID propagation through NATS.
	select {
	case msg := <-resultCh:
		if msg.Metadata.TraceID != publisherTraceID.String() {
			t.Errorf("NATS message trace ID = %s, want %s", msg.Metadata.TraceID, publisherTraceID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for NATS audit event")
	}

	// 13. Retrieve and verify attestation from storage (round-trip integrity).
	reader, err := storageProvider.Get(ctx, storageKey)
	if err != nil {
		t.Fatalf("get attestation: %v", err)
	}
	var retrieved bytes.Buffer
	if _, err := retrieved.ReadFrom(reader); err != nil {
		t.Fatalf("read attestation: %v", err)
	}
	reader.Close()

	verified, err := gen.Verify(ctx, retrieved.Bytes())
	if err != nil {
		t.Fatalf("verify attestation: %v", err)
	}

	// 14. Verify round-tripped fields.
	if verified.Step != "catalog.ingest" {
		t.Errorf("verified step = %s, want catalog.ingest", verified.Step)
	}

	if traceID, ok := verified.ByProducts["trace_id"].(string); !ok || traceID != publisherTraceID.String() {
		t.Errorf("verified trace_id = %v, want %s", verified.ByProducts["trace_id"], publisherTraceID)
	}

	matMap := make(map[string]string)
	for _, a := range verified.Materials {
		matMap[a.URI] = a.Digest
	}
	if matMap["oscal/nist-800-53.json"] != inputHash {
		t.Errorf("material hash mismatch: got %s, want %s", matMap["oscal/nist-800-53.json"], inputHash)
	}

	parentSpan.End()

	// 15. Verify span correlation — all spans share the same trace ID.
	spans := tp.GetSpans()
	spanNames := make(map[string]trace.TraceID)
	for _, s := range spans {
		spanNames[s.Name()] = s.SpanContext().TraceID()
	}

	expectedSpans := []string{
		"attestation.demo.ingest",
		"attestation.CreateLink",
		"natsbus.Publish",
	}
	for _, name := range expectedSpans {
		tid, found := spanNames[name]
		if !found {
			t.Errorf("missing span: %s", name)
			continue
		}
		if tid != publisherTraceID {
			t.Errorf("span %s trace ID = %s, want %s", name, tid, publisherTraceID)
		}
	}
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
