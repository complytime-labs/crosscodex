package gateway_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/gateway"
)

var acceptedMappingID = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)

// FuzzValidateIngestionMapping checks that no input panics, that every
// accepted non-empty ID is at most 128 bytes of the mapping-ID charset, and
// that a refusal stays one bounded line whatever the input.
func FuzzValidateIngestionMapping(f *testing.F) {
	for _, seed := range []string{
		"owasp-asvs-5.0.0-json",  // valid
		"",                       // valid: no mapping
		strings.Repeat("a", 128), // boundary: longest
		strings.Repeat("a", 129), // boundary: one too long
		"OWASP-ASVS",             // invalid: uppercase
		"asvs..json",             // invalid: doubled separator
		"../etc/passwd",          // attack: path
		"asvs\x1b[31m\nlog-line", // attack: terminal and log injection
		"\xff\xfea\u202e",        // attack: invalid UTF-8, bidi override
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, mapping string) {
		err := gateway.ValidateIngestionMapping(mapping)
		if err != nil {
			msg := err.Error()
			if strings.ContainsAny(msg, "\n\r\x1b") {
				t.Fatalf("refusal of %q is not one escaped line: %q", mapping, msg)
			}
			if len(msg) > 1024 {
				t.Fatalf("refusal of a %d-byte mapping is %d bytes", len(mapping), len(msg))
			}
			return
		}
		if mapping == "" {
			return
		}
		if len(mapping) > 128 || !acceptedMappingID.MatchString(mapping) {
			t.Fatalf("accepted malformed mapping %q", mapping)
		}
	})
}
