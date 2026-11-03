package natsbus_test

import (
	"errors"
	"strings"
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

// FuzzParseAbandonedSubject feeds arbitrary subjects to the parser every
// worker runs on control messages from the bus. An accepted subject must
// rebuild to itself, so a parsed collection never names anything the builder
// would refuse.
func FuzzParseAbandonedSubject(f *testing.F) {
	f.Add("crosscodex.control.acme-corp.relate.job-001.0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned")
	f.Add("crosscodex.control.BAD.relate.job-001.c-1.abandoned")
	f.Add("crosscodex.control.acme-corp..job-001.c-1.abandoned")
	f.Add("crosscodex.control.acme-corp.relate.*.>.abandoned")
	f.Add("crosscodex.work.acme-corp.relate.job-001")
	f.Add("")
	f.Add("crosscodex.control.acme-corp.relate.job-001.c-1.abandoned.extra")
	f.Add("crosscodex.control.acme-corp.summarize.job-001.0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned")
	f.Add("crosscodex.control.acme-corp.relate.job-001.0B7E7A9E-5F6C-4A51-9D7E-3C1F2B8A4D10.abandoned")
	f.Add("crosscodex.control.acme-corp.relate.job-001.{0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10}.abandoned")
	f.Add("crosscodex.control.acme-corp.relate.job-001.urn:uuid:0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned")
	f.Add("crosscodex.control.acme-corp.relate.job-001.0b7e7a9e5f6c4a519d7e3c1f2b8a4d10.abandoned")
	f.Add("crosscodex.control.acme-corp.relate." + strings.Repeat("j", natsbus.MaxJobIDLength) +
		".0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned")
	f.Add("crosscodex.control.acme-corp.relate." + strings.Repeat("j", natsbus.MaxJobIDLength+1) +
		".0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned")

	f.Fuzz(func(t *testing.T, subject string) {
		c, err := natsbus.ParseAbandonedSubject(subject)
		if err != nil {
			return
		}
		rebuilt, err := natsbus.AbandonedSubject(c)
		if err != nil {
			t.Fatalf("accepted subject %q parsed to %+v, which the builder rejects: %v", subject, c, err)
		}
		if rebuilt != subject {
			t.Fatalf("accepted subject %q rebuilt as %q", subject, rebuilt)
		}
	})
}

// FuzzWorkSubjectJobID feeds arbitrary job IDs to a job-ID subject builder.
// An accepted job ID must be a single subject token within MaxJobIDLength
// that ends the subject unchanged; a rejected one must say why with
// ErrInvalidJobID.
func FuzzWorkSubjectJobID(f *testing.F) {
	f.Add("9b2f8c1e-3d4a-4f5b-8c6d-7e8f9a0b1c2d")
	f.Add("artifact-adjudication-9b2f8c1e-3d4a-4f5b-8c6d-7e8f9a0b1c2d")
	f.Add(strings.Repeat("j", natsbus.MaxJobIDLength))
	f.Add(strings.Repeat("j", natsbus.MaxJobIDLength+1))
	f.Add(strings.Repeat("j", 4096))
	f.Add("")
	f.Add("job.001")
	f.Add("job>")
	f.Add(strings.Repeat("\u00e9", natsbus.MaxJobIDLength/2+1))

	f.Fuzz(func(t *testing.T, jobID string) {
		subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskRelate, jobID)
		if err != nil {
			if !errors.Is(err, natsbus.ErrInvalidJobID) || !errors.Is(err, natsbus.ErrInvalidSubject) {
				t.Fatalf("job ID %q rejected without ErrInvalidJobID and ErrInvalidSubject: %v", jobID, err)
			}
			return
		}
		if jobID == "" || len(jobID) > natsbus.MaxJobIDLength || strings.ContainsAny(jobID, ".*>") {
			t.Fatalf("accepted job ID %q (%d bytes) that is not a valid subject token within the cap", jobID, len(jobID))
		}
		if want := "crosscodex.work.acme-corp.relate." + jobID; subject != want {
			t.Fatalf("job ID %q built %q, want %q", jobID, subject, want)
		}
	})
}
