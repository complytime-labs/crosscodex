package authn_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/authn"
)

func FuzzGlobMatch(f *testing.F) {
	f.Add("*", "anything")
	f.Add("test-*", "test-value")
	f.Add("", "")
	f.Add("[a-z]*", "hello")
	f.Add("**", "deep/path")
	f.Add("exact", "exact")
	f.Add("exact", "different")
	f.Add("prefix*suffix", "prefixmiddlesuffix")

	f.Fuzz(func(t *testing.T, pattern, value string) {
		// Must not panic regardless of input
		_ = authn.GlobMatch(pattern, value)
	})
}
