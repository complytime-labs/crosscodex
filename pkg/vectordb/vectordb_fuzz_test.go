package vectordb_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func FuzzParseVectorString(f *testing.F) {
	f.Add("[1.0,2.0,3.0]")
	f.Add("[]")
	f.Add("")
	f.Add("not a vector")
	f.Add("[1.0,")
	f.Add("[,,,]")
	f.Add("[1e308,1e308]")
	f.Add("[NaN,Inf,-Inf]")

	f.Fuzz(func(t *testing.T, s string) {
		_, _ = vectordb.ParseVectorString(s)
	})
}
