package telemetrytest

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// OTLPFileExporter writes spans as native OTLP JSON (one
// ExportTraceServiceRequest per line) to a JSONL file. The output is
// directly ingestible by any OTLP-compatible backend (Jaeger, Tempo,
// otel-desktop-viewer).
//
// Byte ID fields (traceId, spanId, parentSpanId) are hex-encoded per
// the OTLP JSON spec, not base64 as protojson would produce.
type OTLPFileExporter struct {
	mu   sync.Mutex
	f    *os.File
	w    io.Writer
	done bool
}

var (
	_ sdktrace.SpanExporter = (*OTLPFileExporter)(nil)
	_ io.Closer             = (*OTLPFileExporter)(nil)
)

// idFieldRe matches JSON fields that protojson base64-encodes but OTLP
// JSON requires as lowercase hex: traceId, spanId, parentSpanId.
var idFieldRe = regexp.MustCompile(`"(traceId|spanId|parentSpanId)"\s*:\s*"([A-Za-z0-9+/=]+)"`)

// base64ToHex converts a protojson-encoded base64 byte field value to
// the lowercase hex encoding required by the OTLP JSON specification.
func base64ToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Try RawStdEncoding (no padding).
		raw, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("decode base64 %q: %w", b64, err)
		}
	}
	return hex.EncodeToString(raw), nil
}

// fixIDEncoding replaces base64-encoded traceId/spanId/parentSpanId
// values in protojson output with hex encoding per the OTLP JSON spec.
func fixIDEncoding(data []byte) ([]byte, error) {
	var convErr error
	result := idFieldRe.ReplaceAllFunc(data, func(match []byte) []byte {
		if convErr != nil {
			return match
		}
		sub := idFieldRe.FindSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		hexVal, err := base64ToHex(string(sub[2]))
		if err != nil {
			convErr = err
			return match
		}
		return []byte(fmt.Sprintf(`"%s":"%s"`, sub[1], hexVal))
	})
	return result, convErr
}

// NewOTLPFileExporter creates an exporter that writes OTLP JSON to a
// file in dir. The directory is created if it does not exist.
func NewOTLPFileExporter(dir string) (*OTLPFileExporter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create trace dir %q: %w", dir, err)
	}
	path := fmt.Sprintf("%s/otlp-traces-%d-%d.jsonl", dir, os.Getpid(), time.Now().UnixMilli())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("create trace file %q: %w", path, err)
	}
	return &OTLPFileExporter{f: f, w: f}, nil
}

// ExportSpans implements sdktrace.SpanExporter. It groups spans by
// resource and instrumentation scope, builds an ExportTraceServiceRequest,
// marshals it to JSON, and appends it as a single line.
func (e *OTLPFileExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.done {
		return nil
	}

	req := buildExportRequest(spans)

	data, err := protojson.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal OTLP trace request: %w", err)
	}
	// protojson base64-encodes bytes fields, but OTLP JSON requires
	// traceId/spanId/parentSpanId as lowercase hex.
	data, err = fixIDEncoding(data)
	if err != nil {
		return fmt.Errorf("fix ID encoding in OTLP JSON: %w", err)
	}
	data = append(data, '\n')
	_, err = e.w.Write(data)
	return err
}

// Shutdown flushes and closes the file.
func (e *OTLPFileExporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.done {
		return nil
	}
	e.done = true
	return e.f.Close()
}

// Close implements io.Closer. It delegates to Shutdown.
func (e *OTLPFileExporter) Close() error {
	return e.Shutdown(context.Background())
}

// buildExportRequest groups ReadOnlySpans by Resource and
// InstrumentationScope, producing an ExportTraceServiceRequest.
func buildExportRequest(spans []sdktrace.ReadOnlySpan) *collectorpb.ExportTraceServiceRequest {
	// Group by resource pointer identity, then by scope.
	type scopeKey struct {
		name    string
		version string
	}
	type resourceGroup struct {
		resource   *resourcepb.Resource
		scopes     map[scopeKey]*tracepb.ScopeSpans
		scopeOrder []scopeKey
	}

	// Use resource string key for grouping (pointer identity is not
	// guaranteed across batches).
	groups := make(map[string]*resourceGroup)
	var groupOrder []string

	for _, s := range spans {
		resKey := s.Resource().String()
		rg, ok := groups[resKey]
		if !ok {
			rg = &resourceGroup{
				resource: convertResource(s),
				scopes:   make(map[scopeKey]*tracepb.ScopeSpans),
			}
			groups[resKey] = rg
			groupOrder = append(groupOrder, resKey)
		}

		sk := scopeKey{
			name:    s.InstrumentationScope().Name,
			version: s.InstrumentationScope().Version,
		}
		ss, ok := rg.scopes[sk]
		if !ok {
			ss = &tracepb.ScopeSpans{
				Scope: &commonpb.InstrumentationScope{
					Name:    sk.name,
					Version: sk.version,
				},
			}
			rg.scopes[sk] = ss
			rg.scopeOrder = append(rg.scopeOrder, sk)
		}
		ss.Spans = append(ss.Spans, convertSpan(s))
	}

	var resourceSpans []*tracepb.ResourceSpans
	for _, key := range groupOrder {
		rg := groups[key]
		rs := &tracepb.ResourceSpans{
			Resource: rg.resource,
		}
		for _, sk := range rg.scopeOrder {
			rs.ScopeSpans = append(rs.ScopeSpans, rg.scopes[sk])
		}
		resourceSpans = append(resourceSpans, rs)
	}

	return &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: resourceSpans,
	}
}

// convertSpan maps an SDK ReadOnlySpan to an OTLP Span proto.
func convertSpan(s sdktrace.ReadOnlySpan) *tracepb.Span {
	sc := s.SpanContext()
	tid := sc.TraceID()
	sid := sc.SpanID()

	span := &tracepb.Span{
		TraceId:                tid[:],
		SpanId:                 sid[:],
		Name:                   s.Name(),
		Kind:                   convertSpanKind(s.SpanKind()),
		StartTimeUnixNano:      uint64(s.StartTime().UnixNano()),
		EndTimeUnixNano:        uint64(s.EndTime().UnixNano()),
		Attributes:             convertAttributes(s.Attributes()),
		DroppedAttributesCount: uint32(s.DroppedAttributes()),
		Events:                 convertEvents(s.Events()),
		DroppedEventsCount:     uint32(s.DroppedEvents()),
		Links:                  convertLinks(s.Links()),
		DroppedLinksCount:      uint32(s.DroppedLinks()),
		Status:                 convertStatus(s.Status()),
		TraceState:             sc.TraceState().String(),
	}

	if s.Parent().IsValid() {
		psid := s.Parent().SpanID()
		span.ParentSpanId = psid[:]
	}

	return span
}

// convertSpanKind maps SDK SpanKind to OTLP Span_SpanKind.
// Numeric values happen to match: Unspecified=0, Internal=1, Server=2,
// Client=3, Producer=4, Consumer=5.
func convertSpanKind(k trace.SpanKind) tracepb.Span_SpanKind {
	return tracepb.Span_SpanKind(k)
}

// convertStatus maps SDK Status to OTLP Status.
// CRITICAL: SDK and OTLP swap Ok and Error enum values.
//
//	SDK:  Unset=0, Error=1, Ok=2
//	OTLP: UNSET=0, OK=1,    ERROR=2
func convertStatus(st sdktrace.Status) *tracepb.Status {
	var code tracepb.Status_StatusCode
	switch st.Code {
	case codes.Ok:
		code = tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		code = tracepb.Status_STATUS_CODE_ERROR
	default:
		code = tracepb.Status_STATUS_CODE_UNSET
	}
	return &tracepb.Status{
		Code:    code,
		Message: st.Description,
	}
}

// convertAttributes maps SDK KeyValue attributes to OTLP KeyValue protos.
func convertAttributes(attrs []attribute.KeyValue) []*commonpb.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, len(attrs))
	for i, kv := range attrs {
		out[i] = &commonpb.KeyValue{
			Key:   string(kv.Key),
			Value: convertValue(kv.Value),
		}
	}
	return out
}

// convertValue maps an SDK attribute.Value to an OTLP AnyValue.
func convertValue(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.STRING:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.AsString()}}
	case attribute.BOOL:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.BOOLSLICE:
		return sliceToArrayValue(v.AsBoolSlice(), func(b bool) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
		})
	case attribute.INT64SLICE:
		return sliceToArrayValue(v.AsInt64Slice(), func(n int64) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
		})
	case attribute.FLOAT64SLICE:
		return sliceToArrayValue(v.AsFloat64Slice(), func(f float64) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
		})
	case attribute.STRINGSLICE:
		return sliceToArrayValue(v.AsStringSlice(), func(s string) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
		})
	default:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.String()}}
	}
}

// sliceToArrayValue converts a typed slice to an OTLP ArrayValue.
func sliceToArrayValue[T any](slice []T, conv func(T) *commonpb.AnyValue) *commonpb.AnyValue {
	values := make([]*commonpb.AnyValue, len(slice))
	for i, v := range slice {
		values[i] = conv(v)
	}
	return &commonpb.AnyValue{
		Value: &commonpb.AnyValue_ArrayValue{
			ArrayValue: &commonpb.ArrayValue{Values: values},
		},
	}
}

// convertEvents maps SDK Events to OTLP Span_Events.
func convertEvents(events []sdktrace.Event) []*tracepb.Span_Event {
	if len(events) == 0 {
		return nil
	}
	out := make([]*tracepb.Span_Event, len(events))
	for i, ev := range events {
		out[i] = &tracepb.Span_Event{
			TimeUnixNano:           uint64(ev.Time.UnixNano()),
			Name:                   ev.Name,
			Attributes:             convertAttributes(ev.Attributes),
			DroppedAttributesCount: uint32(ev.DroppedAttributeCount),
		}
	}
	return out
}

// convertLinks maps SDK Links to OTLP Span_Links.
func convertLinks(links []sdktrace.Link) []*tracepb.Span_Link {
	if len(links) == 0 {
		return nil
	}
	out := make([]*tracepb.Span_Link, len(links))
	for i, ln := range links {
		tid := ln.SpanContext.TraceID()
		sid := ln.SpanContext.SpanID()
		out[i] = &tracepb.Span_Link{
			TraceId:                tid[:],
			SpanId:                 sid[:],
			TraceState:             ln.SpanContext.TraceState().String(),
			Attributes:             convertAttributes(ln.Attributes),
			DroppedAttributesCount: uint32(ln.DroppedAttributeCount),
		}
	}
	return out
}

// convertResource maps an SDK ReadOnlySpan's Resource to an OTLP Resource.
func convertResource(s sdktrace.ReadOnlySpan) *resourcepb.Resource {
	r := s.Resource()
	if r.Len() == 0 {
		return nil
	}
	attrs := make([]*commonpb.KeyValue, 0, r.Len())
	iter := r.Iter()
	for iter.Next() {
		kv := iter.Attribute()
		attrs = append(attrs, &commonpb.KeyValue{
			Key:   string(kv.Key),
			Value: convertValue(kv.Value),
		})
	}
	return &resourcepb.Resource{Attributes: attrs}
}
