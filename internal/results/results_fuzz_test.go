package results_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/internal/results"
)

func FuzzDecodeCursor(f *testing.F) {
	f.Add("")                                                                             // first page
	f.Add(results.EncodeCursor(results.Cursor{JobID: "j", SourceID: "s", TargetID: "t"})) // valid
	f.Add("!!!")                                                                          // not base64
	f.Add("WyJhIiwiYiJd")                                                                 // ["a","b"] wrong arity
	f.Add("WyJhIiwiYiIsM10")                                                              // ["a","b",3] non-string element
	f.Add("eyJqb2JfaWQiOiInIE9SIDE9MSAtLSJ9")                                             // SQL-ish attack object
	f.Fuzz(func(t *testing.T, token string) {
		c, err := results.DecodeCursor(token)
		if err != nil || c == nil {
			return
		}
		again, err := results.DecodeCursor(results.EncodeCursor(*c))
		if err != nil || *again != *c {
			t.Fatalf("accepted token does not round-trip: %q -> %+v", token, c)
		}
	})
}
