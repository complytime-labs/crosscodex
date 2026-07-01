package telemetrytest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("OTLPFileExporter — roundtrip preservation", func() {
		It("preserves span name and attribute count through marshal/unmarshal", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				name := rapid.StringMatching(`^[a-zA-Z][a-zA-Z0-9._]{0,62}$`).Draw(t, "spanName")
				attrCount := rapid.IntRange(0, 10).Draw(t, "attrCount")
				attrs := make([]attribute.KeyValue, attrCount)
				for i := range attrCount {
					key := rapid.StringMatching(`^[a-z][a-z0-9._]{0,30}$`).Draw(t, "attrKey")
					val := rapid.String().Draw(t, "attrVal")
					attrs[i] = attribute.String(key, val)
				}

				dir, dirErr := os.MkdirTemp("", "otlp-prop-roundtrip-*")
				if dirErr != nil {
					t.Fatalf("MkdirTemp: %v", dirErr)
				}
				defer os.RemoveAll(dir)

				exp, err := telemetrytest.NewOTLPFileExporter(dir)
				if err != nil {
					t.Fatalf("NewOTLPFileExporter: %v", err)
				}

				tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
				tracer := tp.Tracer("prop-test")

				_, span := tracer.Start(context.Background(), name,
					trace.WithAttributes(attrs...))
				span.End()

				if err := tp.Shutdown(context.Background()); err != nil {
					t.Fatalf("Shutdown: %v", err)
				}
				if err := exp.Shutdown(context.Background()); err != nil {
					t.Fatalf("exp.Shutdown: %v", err)
				}

				files, globErr := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
				if globErr != nil {
					t.Fatalf("glob: %v", globErr)
				}
				if len(files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(files))
				}
				data, readErr := os.ReadFile(files[0])
				if readErr != nil {
					t.Fatalf("read: %v", readErr)
				}

				req := &collectortrace.ExportTraceServiceRequest{}
				// Convert hex-encoded IDs back to base64 for protojson.
				b64Data, hexErr := hexToBase64InJSON([]byte(strings.TrimSpace(string(data))))
				if hexErr != nil {
					t.Fatalf("hexToBase64InJSON: %v", hexErr)
				}
				if err := protojson.Unmarshal(b64Data, req); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}

				spans := req.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
				if len(spans) != 1 {
					t.Fatalf("expected 1 span, got %d", len(spans))
				}
				if spans[0].GetName() != name {
					t.Fatalf("name mismatch: got %q, want %q", spans[0].GetName(), name)
				}
				// Attribute deduplication may reduce count, but should not
				// exceed the input count.
				if len(spans[0].GetAttributes()) > attrCount {
					t.Fatalf("attribute count %d exceeds input %d",
						len(spans[0].GetAttributes()), attrCount)
				}
			})
		})
	})

	Context("OTLPFileExporter — determinism", func() {
		It("produces identical output for identical input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				name := rapid.StringMatching(`^[a-zA-Z][a-zA-Z0-9._]{0,30}$`).Draw(t, "spanName")

				var outputs [2]string
				for i := range 2 {
					dir, dirErr := os.MkdirTemp("", "otlp-prop-determ-*")
					if dirErr != nil {
						t.Fatalf("MkdirTemp[%d]: %v", i, dirErr)
					}
					defer os.RemoveAll(dir)

					exp, err := telemetrytest.NewOTLPFileExporter(dir)
					if err != nil {
						t.Fatalf("NewOTLPFileExporter[%d]: %v", i, err)
					}

					// Build a span stub directly to control all fields.
					stub := tracetest.SpanStub{Name: name}
					snapshot := stub.Snapshot()

					if err := exp.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{snapshot}); err != nil {
						t.Fatalf("ExportSpans[%d]: %v", i, err)
					}
					if err := exp.Shutdown(context.Background()); err != nil {
						t.Fatalf("Shutdown[%d]: %v", i, err)
					}

					files, globErr := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
					if globErr != nil {
						t.Fatalf("glob[%d]: %v", i, globErr)
					}
					data, readErr := os.ReadFile(files[0])
					if readErr != nil {
						t.Fatalf("read[%d]: %v", i, readErr)
					}
					// Normalize: parse and re-marshal to eliminate key ordering differences.
					var raw json.RawMessage
					if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &raw); err != nil {
						t.Fatalf("json parse[%d]: %v", i, err)
					}
					canonical, _ := json.Marshal(raw)
					outputs[i] = string(canonical)
				}
				if outputs[0] != outputs[1] {
					t.Fatalf("non-deterministic output:\n  run1: %s\n  run2: %s", outputs[0], outputs[1])
				}
			})
		})
	})
})
