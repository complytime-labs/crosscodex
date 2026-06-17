package natsbus_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

func FuzzValidateToken(f *testing.F) {
	// Valid tokens
	f.Add("job-001")
	f.Add("edge-abc-123")
	f.Add("simple")

	// Empty
	f.Add("")

	// Tokens containing NATS delimiters
	f.Add("job.001")
	f.Add("job*001")
	f.Add("job>001")
	f.Add(".*>")
	f.Add("...")

	// Binary
	f.Add(string([]byte{0x00, 0x01, 0x02}))

	f.Fuzz(func(t *testing.T, token string) {
		// Must not panic regardless of input
		_ = natsbus.ValidateToken(token, "fuzz")
	})
}

func FuzzReconstructSpanContext(f *testing.F) {
	// Valid 32-char trace ID + 16-char span ID
	f.Add("0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331") // DevSkim: ignore DS173237 — fuzz seed corpus, OTel trace/span IDs not secrets

	// All zeros (invalid per OTel spec)
	f.Add("00000000000000000000000000000000", "0000000000000000") // DevSkim: ignore DS173237 — fuzz seed corpus, invalid OTel IDs

	// Invalid hex
	f.Add("not-valid-hex", "also-not-hex")

	// Wrong lengths
	f.Add("abc", "def")
	f.Add("", "")

	// Oversized
	f.Add("0af7651916cd43dd8448eb211c80319cabcdef01", "b7ad6b7169203331aabb") // DevSkim: ignore DS173237 — fuzz seed corpus, oversized test IDs

	f.Fuzz(func(t *testing.T, traceID, spanID string) {
		headers := map[string][]string{
			natsbus.HeaderTraceID: {traceID},
			natsbus.HeaderSpanID:  {spanID},
		}
		// Must not panic regardless of input
		_, _ = natsbus.ReconstructSpanContext(headers)
	})
}
