package natsbus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Provenance header keys.
const (
	HeaderTraceID       = "X-Trace-Id"
	HeaderSpanID        = "X-Span-Id"
	HeaderTenantID      = "X-Tenant-Id"
	HeaderTimestamp     = "X-Timestamp"
	HeaderContentSHA256 = "X-Content-SHA256"
)

// injectProvenance builds the mandatory provenance headers for a message.
// Trace and span IDs are extracted from the context's active span.
func injectProvenance(ctx context.Context, data []byte, tenantID string) map[string][]string {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()

	return map[string][]string{
		HeaderTraceID:       {sc.TraceID().String()},
		HeaderSpanID:        {sc.SpanID().String()},
		HeaderTenantID:      {tenantID},
		HeaderTimestamp:     {time.Now().UTC().Format(time.RFC3339Nano)},
		HeaderContentSHA256: {contentHash(data)},
	}
}

// extractProvenance parses provenance fields from message headers
// into MessageMetadata fields. Returns ErrMissingProvenance if any
// mandatory header is absent or empty, listing the missing fields.
func extractProvenance(headers map[string][]string) (MessageMetadata, error) {
	meta := MessageMetadata{
		TraceID:     firstValue(headers, HeaderTraceID),
		SpanID:      firstValue(headers, HeaderSpanID),
		TenantID:    firstValue(headers, HeaderTenantID),
		ContentHash: firstValue(headers, HeaderContentSHA256),
	}

	var missing []string
	if meta.TenantID == "" {
		missing = append(missing, HeaderTenantID)
	}
	if meta.ContentHash == "" {
		missing = append(missing, HeaderContentSHA256)
	}
	if firstValue(headers, HeaderTimestamp) == "" {
		missing = append(missing, HeaderTimestamp)
	}
	if meta.TraceID == "" {
		missing = append(missing, HeaderTraceID)
	}
	if meta.SpanID == "" {
		missing = append(missing, HeaderSpanID)
	}

	if len(missing) > 0 {
		return meta, fmt.Errorf("%w: %s", ErrMissingProvenance, strings.Join(missing, ", "))
	}

	return meta, nil
}

// mergeHeaders merges user-provided headers with provenance headers.
// Provenance headers take precedence on key conflict.
func mergeHeaders(user, provenance map[string][]string) map[string][]string {
	merged := make(map[string][]string, len(user)+len(provenance))
	for k, v := range user {
		merged[k] = v
	}
	for k, v := range provenance {
		merged[k] = v
	}
	return merged
}

// contentHash computes the SHA-256 hex digest of data.
func contentHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// firstValue returns the first value for a header key, or empty string.
func firstValue(headers map[string][]string, key string) string {
	if vals, ok := headers[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}
