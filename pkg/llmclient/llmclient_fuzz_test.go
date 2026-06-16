package llmclient_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

func FuzzResolveCredential(f *testing.F) {
	// Valid scheme prefixes
	f.Add("env:MY_KEY")
	f.Add("file:/tmp/key")
	f.Add("vault:secret/data/key")

	// Empty and schemeless
	f.Add("")
	f.Add("no-scheme-at-all")

	// Unknown schemes
	f.Add("s3:bucket/key")
	f.Add("http://example.com")

	// Null bytes and binary
	f.Add(string([]byte{0x00}))
	f.Add("env:\x00BADVAR")
	f.Add("file:\x00/etc/passwd")

	// Edge cases
	f.Add("env:")
	f.Add("file:")
	f.Add("vault:")
	f.Add(":")
	f.Add(":value")

	f.Fuzz(func(t *testing.T, ref string) {
		// Must not panic regardless of input.
		_, _ = llmclient.ResolveCredential(ref)
	})
}

func FuzzParseRetryAfter(f *testing.F) {
	// Valid integer seconds
	f.Add("0")
	f.Add("1")
	f.Add("30")
	f.Add("120")

	// Negative
	f.Add("-1")
	f.Add("-999")

	// Non-numeric
	f.Add("")
	f.Add("abc")
	f.Add("not-a-number")

	// Very large
	f.Add("999999999999")
	f.Add("9223372036854775807") // max int64

	// Whitespace
	f.Add("  5  ")
	f.Add("\t10\n")

	// HTTP-date format
	f.Add("Fri, 31 Dec 1999 23:59:59 GMT")

	// Binary
	f.Add(string([]byte{0x00, 0x01, 0x02}))

	f.Fuzz(func(t *testing.T, header string) {
		// Must not panic regardless of input.
		d := llmclient.ParseRetryAfter(header)
		if d < 0 {
			t.Errorf("parseRetryAfter(%q) returned negative duration: %v", header, d)
		}
	})
}
