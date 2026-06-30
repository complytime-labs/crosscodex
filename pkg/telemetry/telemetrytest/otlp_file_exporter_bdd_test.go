package telemetrytest_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// hexIDFieldRe matches hex-encoded traceId/spanId/parentSpanId fields
// produced by the OTLP-compliant exporter so they can be converted
// back to base64 for protojson.Unmarshal (which expects base64 bytes).
var hexIDFieldRe = regexp.MustCompile(`"(traceId|spanId|parentSpanId)"\s*:\s*"([0-9a-f]+)"`)

// hexToBase64InJSON converts hex-encoded ID fields back to base64 so
// protojson.Unmarshal can decode them into the proto bytes fields.
func hexToBase64InJSON(data []byte) ([]byte, error) {
	var convErr error
	result := hexIDFieldRe.ReplaceAllFunc(data, func(match []byte) []byte {
		if convErr != nil {
			return match
		}
		sub := hexIDFieldRe.FindSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		raw, err := hex.DecodeString(string(sub[2]))
		if err != nil {
			convErr = err
			return match
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		return []byte(fmt.Sprintf(`"%s":"%s"`, sub[1], b64))
	})
	return result, convErr
}

var _ = Describe("OTLPFileExporter", Ordered, func() {
	var (
		dir string
		exp *telemetrytest.OTLPFileExporter
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		var err error
		exp, err = telemetrytest.NewOTLPFileExporter(dir)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(exp.Shutdown(context.Background())).To(Succeed())
	})

	// Helper: create a TracerProvider that exports through the exporter,
	// produce spans, flush, and return the file content parsed as OTLP.
	// Uses WithBatcher so all spans from a single createSpans call are
	// grouped into one ExportSpans batch on ForceFlush (WithSyncer would
	// export each span individually on End()).
	exportAndParse := func(createSpans func(tracer trace.Tracer)) []*collectortrace.ExportTraceServiceRequest {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)),
		)
		tracer := tp.Tracer("test-scope", trace.WithInstrumentationVersion("v1.0.0"))
		createSpans(tracer)
		Expect(tp.ForceFlush(context.Background())).To(Succeed())
		Expect(tp.Shutdown(context.Background())).To(Succeed())

		files, err := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).To(HaveLen(1))

		data, err := os.ReadFile(files[0])
		Expect(err).NotTo(HaveOccurred())

		var reqs []*collectortrace.ExportTraceServiceRequest
		dec := json.NewDecoder(strings.NewReader(string(data)))
		for dec.More() {
			var raw json.RawMessage
			Expect(dec.Decode(&raw)).To(Succeed())
			// The exporter writes hex-encoded IDs per the OTLP JSON
			// spec, but protojson.Unmarshal expects base64 for bytes
			// fields. Convert hex back to base64 for deserialization.
			b64JSON, err := hexToBase64InJSON(raw)
			Expect(err).NotTo(HaveOccurred())
			req := &collectortrace.ExportTraceServiceRequest{}
			Expect(protojson.Unmarshal(b64JSON, req)).To(Succeed())
			reqs = append(reqs, req)
		}
		return reqs
	}

	Describe("Roundtrip field preservation", func() {
		It("preserves TraceID, SpanID, ParentSpanID, Name, and Kind", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				ctx, parent := tracer.Start(context.Background(), "parent-span",
					trace.WithSpanKind(trace.SpanKindServer))
				_, child := tracer.Start(ctx, "child-span",
					trace.WithSpanKind(trace.SpanKindClient))
				child.End()
				parent.End()
			})
			Expect(reqs).To(HaveLen(1))

			rs := reqs[0].GetResourceSpans()
			Expect(rs).To(HaveLen(1))
			ss := rs[0].GetScopeSpans()
			Expect(ss).To(HaveLen(1))
			spans := ss[0].GetSpans()
			Expect(spans).To(HaveLen(2))

			// Find spans by name — batcher ordering is not guaranteed.
			var childSpan, parentSpan *otlptrace.Span
			for _, s := range spans {
				switch s.GetName() {
				case "child-span":
					childSpan = s
				case "parent-span":
					parentSpan = s
				}
			}
			Expect(childSpan).NotTo(BeNil())
			Expect(parentSpan).NotTo(BeNil())

			// Parent relationship: child's ParentSpanId == parent's SpanId.
			Expect(childSpan.GetParentSpanId()).To(Equal(parentSpan.GetSpanId()))
			// Same trace.
			Expect(childSpan.GetTraceId()).To(Equal(parentSpan.GetTraceId()))
			// IDs are non-empty 16/8 bytes.
			Expect(childSpan.GetTraceId()).To(HaveLen(16))
			Expect(childSpan.GetSpanId()).To(HaveLen(8))

			// SpanKind mapping.
			Expect(childSpan.GetKind().String()).To(Equal("SPAN_KIND_CLIENT"))
			Expect(parentSpan.GetKind().String()).To(Equal("SPAN_KIND_SERVER"))
		})

		It("writes hex-encoded IDs per the OTLP JSON spec", func() {
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithSyncer(exp),
			)
			tracer := tp.Tracer("hex-test")
			_, span := tracer.Start(context.Background(), "hex-span")
			span.End()
			Expect(tp.ForceFlush(context.Background())).To(Succeed())
			Expect(tp.Shutdown(context.Background())).To(Succeed())

			files, err := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
			Expect(err).NotTo(HaveOccurred())
			Expect(files).To(HaveLen(1))

			data, err := os.ReadFile(files[0])
			Expect(err).NotTo(HaveOccurred())

			// Parse as generic JSON to inspect raw field values.
			var generic map[string]interface{}
			Expect(json.Unmarshal([]byte(strings.TrimSpace(string(data))), &generic)).To(Succeed())

			// Walk to the first span.
			rs := generic["resourceSpans"].([]interface{})
			Expect(rs).To(HaveLen(1))
			ss := rs[0].(map[string]interface{})["scopeSpans"].([]interface{})
			spans := ss[0].(map[string]interface{})["spans"].([]interface{})
			s := spans[0].(map[string]interface{})

			traceID := s["traceId"].(string)
			spanID := s["spanId"].(string)

			// OTLP JSON: traceId is 32 hex chars (16 bytes), spanId is 16 hex chars (8 bytes).
			Expect(traceID).To(HaveLen(32), "traceId should be 32 hex chars")
			Expect(spanID).To(HaveLen(16), "spanId should be 16 hex chars")
			Expect(traceID).To(MatchRegexp(`^[0-9a-f]{32}$`), "traceId should be lowercase hex")
			Expect(spanID).To(MatchRegexp(`^[0-9a-f]{16}$`), "spanId should be lowercase hex")
		})

		It("preserves timestamps", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "timed-span")
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			s := spans[0]
			Expect(s.GetStartTimeUnixNano()).To(BeNumerically(">", 0))
			Expect(s.GetEndTimeUnixNano()).To(BeNumerically(">=", s.GetStartTimeUnixNano()))
		})

		// Exhaustive SpanKind mapping — guards against SDK↔OTLP enum drift.
		DescribeTable("maps SpanKind correctly",
			func(sdkKind trace.SpanKind, expectedOTLP string) {
				reqs := exportAndParse(func(tracer trace.Tracer) {
					_, span := tracer.Start(context.Background(), "kind-span",
						trace.WithSpanKind(sdkKind))
					span.End()
				})
				spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
				Expect(spans).To(HaveLen(1))
				Expect(spans[0].GetKind().String()).To(Equal(expectedOTLP))
			},
			// SDK promotes SpanKind(0) to Internal at span creation time,
			// so the exporter never sees UNSPECIFIED from real spans.
			Entry("Unspecified (promoted to Internal by SDK)", trace.SpanKind(0), "SPAN_KIND_INTERNAL"),
			Entry("Internal", trace.SpanKindInternal, "SPAN_KIND_INTERNAL"),
			Entry("Server", trace.SpanKindServer, "SPAN_KIND_SERVER"),
			Entry("Client", trace.SpanKindClient, "SPAN_KIND_CLIENT"),
			Entry("Producer", trace.SpanKindProducer, "SPAN_KIND_PRODUCER"),
			Entry("Consumer", trace.SpanKindConsumer, "SPAN_KIND_CONSUMER"),
		)
	})

	Describe("Attribute types", func() {
		It("maps all attribute value types correctly", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "attr-span",
					trace.WithAttributes(
						attribute.String("str.key", "hello"),
						attribute.Int64("int.key", 42),
						attribute.Float64("float.key", 3.14),
						attribute.Bool("bool.key", true),
						attribute.StringSlice("strslice.key", []string{"a", "b"}),
						attribute.Int64Slice("intslice.key", []int64{1, 2, 3}),
						attribute.Float64Slice("floatslice.key", []float64{1.1, 2.2}),
						attribute.BoolSlice("boolslice.key", []bool{true, false}),
					))
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			attrs := spans[0].GetAttributes()
			Expect(attrs).To(HaveLen(8))

			byKey := make(map[string]*commonpb.AnyValue)
			for _, kv := range attrs {
				byKey[kv.GetKey()] = kv.GetValue()
			}

			// Scalar types.
			Expect(byKey["str.key"].GetStringValue()).To(Equal("hello"))
			Expect(byKey["int.key"].GetIntValue()).To(Equal(int64(42)))
			Expect(byKey["float.key"].GetDoubleValue()).To(BeNumerically("~", 3.14, 0.001))
			Expect(byKey["bool.key"].GetBoolValue()).To(BeTrue())

			// Slice types — verify element count and values.
			strArr := byKey["strslice.key"].GetArrayValue().GetValues()
			Expect(strArr).To(HaveLen(2))
			Expect(strArr[0].GetStringValue()).To(Equal("a"))
			Expect(strArr[1].GetStringValue()).To(Equal("b"))

			intArr := byKey["intslice.key"].GetArrayValue().GetValues()
			Expect(intArr).To(HaveLen(3))
			Expect(intArr[0].GetIntValue()).To(Equal(int64(1)))
			Expect(intArr[1].GetIntValue()).To(Equal(int64(2)))
			Expect(intArr[2].GetIntValue()).To(Equal(int64(3)))

			floatArr := byKey["floatslice.key"].GetArrayValue().GetValues()
			Expect(floatArr).To(HaveLen(2))
			Expect(floatArr[0].GetDoubleValue()).To(BeNumerically("~", 1.1, 0.001))
			Expect(floatArr[1].GetDoubleValue()).To(BeNumerically("~", 2.2, 0.001))

			boolArr := byKey["boolslice.key"].GetArrayValue().GetValues()
			Expect(boolArr).To(HaveLen(2))
			Expect(boolArr[0].GetBoolValue()).To(BeTrue())
			Expect(boolArr[1].GetBoolValue()).To(BeFalse())
		})
	})

	Describe("Events", func() {
		It("preserves span events with attributes", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "event-span")
				span.AddEvent("cache.miss",
					trace.WithAttributes(attribute.String("cache.key", "user:123")))
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			events := spans[0].GetEvents()
			Expect(events).To(HaveLen(1))
			Expect(events[0].GetName()).To(Equal("cache.miss"))
			Expect(events[0].GetTimeUnixNano()).To(BeNumerically(">", 0))
			Expect(events[0].GetAttributes()).To(HaveLen(1))
			Expect(events[0].GetAttributes()[0].GetKey()).To(Equal("cache.key"))
		})
	})

	Describe("Links", func() {
		It("preserves span links", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				// Create a span to link to.
				ctx, linked := tracer.Start(context.Background(), "linked-span")
				linked.End()
				linkedSC := trace.SpanFromContext(ctx).SpanContext()

				_, span := tracer.Start(context.Background(), "linking-span",
					trace.WithLinks(trace.Link{
						SpanContext: linkedSC,
						Attributes: []attribute.KeyValue{
							attribute.String("link.reason", "causal"),
						},
					}))
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			// Find the linking span.
			var linkingSpan *otlptrace.Span
			for _, s := range spans {
				if s.GetName() == "linking-span" {
					linkingSpan = s
				}
			}
			Expect(linkingSpan).NotTo(BeNil())
			links := linkingSpan.GetLinks()
			Expect(links).To(HaveLen(1))
			Expect(links[0].GetTraceId()).To(HaveLen(16))
			Expect(links[0].GetSpanId()).To(HaveLen(8))
			Expect(links[0].GetAttributes()).To(HaveLen(1))
		})
	})

	Describe("Status", func() {
		It("maps Ok status", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "ok-span")
				span.SetStatus(codes.Ok, "all good")
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			// SDK codes.Ok=2 → OTLP STATUS_CODE_OK=1
			Expect(spans[0].GetStatus().GetCode().String()).To(Equal("STATUS_CODE_OK"))
		})

		It("maps Error status with description", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "err-span")
				span.SetStatus(codes.Error, "something broke")
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			// SDK codes.Error=1 → OTLP STATUS_CODE_ERROR=2
			Expect(spans[0].GetStatus().GetCode().String()).To(Equal("STATUS_CODE_ERROR"))
			Expect(spans[0].GetStatus().GetMessage()).To(Equal("something broke"))
		})

		It("maps Unset status", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "unset-span")
				span.End()
			})
			spans := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()
			Expect(spans[0].GetStatus().GetCode().String()).To(Equal("STATUS_CODE_UNSET"))
		})
	})

	Describe("InstrumentationScope", func() {
		It("preserves scope name and version", func() {
			reqs := exportAndParse(func(tracer trace.Tracer) {
				_, span := tracer.Start(context.Background(), "scoped-span")
				span.End()
			})
			scope := reqs[0].GetResourceSpans()[0].GetScopeSpans()[0].GetScope()
			Expect(scope.GetName()).To(Equal("test-scope"))
			Expect(scope.GetVersion()).To(Equal("v1.0.0"))
		})
	})

	Describe("JSONL format", func() {
		It("writes one JSON object per ExportSpans call", func() {
			// Export two batches.
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			tracer := tp.Tracer("test")

			_, s1 := tracer.Start(context.Background(), "batch1")
			s1.End()
			Expect(tp.ForceFlush(context.Background())).To(Succeed())

			_, s2 := tracer.Start(context.Background(), "batch2")
			s2.End()
			Expect(tp.ForceFlush(context.Background())).To(Succeed())

			Expect(tp.Shutdown(context.Background())).To(Succeed())

			files, err := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
			Expect(err).NotTo(HaveOccurred())
			Expect(files).To(HaveLen(1))

			data, err := os.ReadFile(files[0])
			Expect(err).NotTo(HaveOccurred())

			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			Expect(lines).To(HaveLen(2))

			// Each line is valid OTLP JSON.
			for _, line := range lines {
				b64Line, err := hexToBase64InJSON([]byte(line))
				Expect(err).NotTo(HaveOccurred())
				req := &collectortrace.ExportTraceServiceRequest{}
				Expect(protojson.Unmarshal(b64Line, req)).To(Succeed())
			}
		})
	})

	Describe("File lifecycle", func() {
		It("creates the output directory if it does not exist", func() {
			nested := filepath.Join(dir, "deep", "nested")
			nestedExp, err := telemetrytest.NewOTLPFileExporter(nested)
			Expect(err).NotTo(HaveOccurred())
			Expect(nestedExp.Shutdown(context.Background())).To(Succeed())
			Expect(nested).To(BeADirectory())
		})

		It("handles empty span slice without writing", func() {
			Expect(exp.ExportSpans(context.Background(), nil)).To(Succeed())
			Expect(exp.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{})).To(Succeed())
			// No file content should be written.
			files, err := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
			Expect(err).NotTo(HaveOccurred())
			Expect(files).To(HaveLen(1))
			data, err := os.ReadFile(files[0])
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(string(data))).To(BeEmpty())
		})

		It("uses the expected file naming pattern", func() {
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			tracer := tp.Tracer("test")
			_, s := tracer.Start(context.Background(), "naming-test")
			s.End()
			Expect(tp.Shutdown(context.Background())).To(Succeed())

			files, err := filepath.Glob(filepath.Join(dir, "otlp-traces-*.jsonl"))
			Expect(err).NotTo(HaveOccurred())
			Expect(files).To(HaveLen(1))
		})
	})
})
