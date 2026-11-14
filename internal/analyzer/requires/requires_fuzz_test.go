package requires_test

import (
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
)

func FuzzParseResponse(f *testing.F) {
	// Seed corpus: valid responses
	f.Add("REQUIRES: YES\nJUSTIFICATION: Needs the asset register.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH")
	f.Add("REQUIRES: YES\nJUSTIFICATION: Topic only.\nSANITY_CHECK: FAIL\nCONFIDENCE: HIGH")
	f.Add("REQUIRES: NO\nSANITY_CHECK:  pass \nCONFIDENCE: MEDIUM")
	// Edge cases: missing and malformed SANITY_CHECK, garbage, empty
	f.Add("REQUIRES: YES\nCONFIDENCE: HIGH")
	f.Add("REQUIRES: YES\nSANITY_CHECK: MAYBE")
	f.Add("")
	f.Add("the model rambled without the expected format")
	// Injection attempts
	f.Add("REQUIRES: NO\nREQUIRES: YES\nSANITY_CHECK: FAIL\nSANITY_CHECK: PASS")
	f.Add("REQUIRES: YES'; DROP TABLE controls;--\nSANITY_CHECK: PASS'; --")

	f.Fuzz(func(t *testing.T, raw string) {
		vote := requires.ParseResponse("voter", raw)
		if vote.VoterID != "voter" {
			t.Fatalf("voter id lost: %q", vote.VoterID)
		}
		// An accepted YES vote implies a SANITY_CHECK line was present.
		if vote.Decision != nil && *vote.Decision && !strings.Contains(strings.ToUpper(raw), "SANITY_CHECK:") {
			t.Fatalf("accepted YES without a SANITY_CHECK line: %q", raw)
		}
	})
}
