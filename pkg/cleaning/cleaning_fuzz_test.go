package cleaning_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
)

func FuzzParseStageRecord(f *testing.F) {
	f.Add(`{"prompt_name":"p","prompt_version":"1","models":{"a":"r1"}}`) // valid
	f.Add("")                                                             // unknown
	f.Add(`{"x":1}`)                                                      // unknown field
	f.Add(strings.Repeat(`{"models":`, 500))                              // deeply nested
	f.Add(`{"prompt_name":"` + strings.Repeat("x", 70*1024) + `"}`)       // 70 KiB string

	f.Fuzz(func(t *testing.T, s string) {
		r, err := cleaning.ParseStageRecord(s)
		if err != nil {
			if !errors.Is(err, cleaning.ErrInvalidStageRecord) {
				t.Fatalf("rejection must wrap ErrInvalidStageRecord, got %v", err)
			}
			return
		}
		if r == nil {
			return
		}
		out, err := r.Marshal()
		if err != nil {
			// Re-encoding can expand input (e.g. invalid UTF-8) past the size cap.
			if !errors.Is(err, cleaning.ErrInvalidStageRecord) {
				t.Fatalf("Marshal of accepted record: %v", err)
			}
			return
		}
		again, err := cleaning.ParseStageRecord(out)
		if err != nil || again == nil {
			t.Fatalf("re-parse of %q: %v", out, err)
		}
		out2, err := again.Marshal()
		if err != nil || out2 != out {
			t.Fatalf("not stable: %q vs %q (%v)", out, out2, err)
		}
	})
}

func FuzzParseVerdictKey(f *testing.F) {
	f.Add("job-1:844424930131969:844424930131970")                                                    // valid
	f.Add("")                                                                                         // empty
	f.Add(strings.Repeat("j", 256) + ":" + strings.Repeat("s", 256) + ":" + strings.Repeat("t", 256)) // boundary
	f.Add(`job\:x:src:tgt`)                                                                           // separator escape attempt
	f.Add("job:" + strings.Repeat("s", 4096) + ":tgt")                                                // oversized part

	f.Fuzz(func(t *testing.T, s string) {
		k, err := cleaning.ParseVerdictKey(s)
		if err != nil {
			if !errors.Is(err, cleaning.ErrInvalidVerdictKey) {
				t.Fatalf("rejection must wrap ErrInvalidVerdictKey, got %v", err)
			}
			return
		}
		if k.String() != s {
			t.Fatalf("accepted %q but String() gives %q", s, k.String())
		}
	})
}

func FuzzParseEvidence(f *testing.F) {
	f.Add([]byte(`{"verdicts":[{"key":{"job_id":"j","source_id":"s","target_id":"t"},"job_completed_at":"2026-01-01T00:00:00Z","relationship":"subset of","agreement":0.5,"edge_ids":["e1"]}],"detail":{"stages":"analyzer"}}`)) // valid
	f.Add([]byte(""))                                                                                                                                                                                                            // empty
	f.Add([]byte(`{"verdicts":[{"key":{"job_id":"` + strings.Repeat("j", 256) + `","source_id":"s","target_id":"t"}}]}`))                                                                                                        // boundary
	f.Add([]byte(`{"verdicts":[{"key":{"job_id":"a:b","source_id":"s","target_id":"t"}}],"verdicts":null}`))                                                                                                                     // separator and duplicate key
	f.Add([]byte(`{"detail":{"x":"` + strings.Repeat("x", 1<<20) + `"}}`))                                                                                                                                                       // oversized
	f.Add([]byte(`{"verdicts":null,"job":{"id":"j","completed_at":"2026-01-01T00:00:00Z","source_catalog":"SP 800-53 5.1.1","target_catalog":"c","verdicts_in_scope":2000}}`))                                                   // job-level
	f.Add([]byte(`{"verdicts":null,"job":{"id":"a:b"}}`))                                                                                                                                                                        // job ID with separator

	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := cleaning.ParseEvidence(b)
		if err != nil {
			if !errors.Is(err, cleaning.ErrInvalidEvidence) {
				t.Fatalf("rejection must wrap ErrInvalidEvidence, got %v", err)
			}
			return
		}
		out, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("Marshal of accepted evidence: %v", err)
		}
		if len(out) > 1<<20 {
			// Re-encoding can expand input (escaping, invalid UTF-8) past the size cap.
			return
		}
		again, err := cleaning.ParseEvidence(out)
		if err != nil {
			t.Fatalf("re-parse of %q: %v", out, err)
		}
		out2, err := json.Marshal(again)
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatalf("not stable: %q vs %q (%v)", out, out2, err)
		}
	})
}

func FuzzDecodeKeys(f *testing.F) {
	f.Add([]byte(`[{"job_id":"j","source_id":"s","target_id":"t"}]`))                                     // valid
	f.Add([]byte(`null`))                                                                                 // no targets
	f.Add([]byte(`[{"job_id":"` + strings.Repeat("j", 256) + `","source_id":"s","target_id":"t"}]`))      // boundary
	f.Add([]byte(`[{"job_id":"a:b","source_id":"s","target_id":"t","extra":1}] []`))                      // separator, unknown field, trailing data
	f.Add([]byte(`[` + strings.Repeat(`{"job_id":"j","source_id":"s","target_id":"t"},`, 1<<15) + `{}]`)) // oversized

	f.Fuzz(func(t *testing.T, b []byte) {
		keys, err := cleaning.DecodeKeys(b)
		if err != nil {
			if !errors.Is(err, cleaning.ErrInvalidVerdictKey) {
				t.Fatalf("rejection must wrap ErrInvalidVerdictKey, got %v", err)
			}
			return
		}
		for _, k := range keys {
			if _, err := cleaning.ParseVerdictKey(k.String()); err != nil {
				t.Fatalf("accepted key %q does not parse: %v", k.String(), err)
			}
		}
	})
}
