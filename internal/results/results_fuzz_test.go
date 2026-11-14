package results_test

import (
	"math"
	"strings"
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

func FuzzDecodeContributions(f *testing.F) {
	f.Add(`[{"GeneratorName":"g","Score":0.5,"Weight":0.5,"Metadata":{"rank":"1"}}]`) // valid
	f.Add(`[]`)                                                                       // empty
	f.Add(`null`)                                                                     // null
	f.Add(`{"GeneratorName":"g"}`)                                                    // wrong top-level type
	f.Add(`[{"GeneratorName":"g","Score":1e999,"Weight":0}]`)                         // overflow
	f.Add(`[{"GeneratorName":"g","Score":0.5`)                                        // truncated
	f.Add(`[{"GeneratorName":"'; DROP TABLE jobs;--","Score":0,"Weight":0}]`)         // SQL-ish attack
	f.Add(strings.Repeat("[", 20000))                                                 // deep nesting
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := results.DecodeContributions([]byte(raw))
		if err != nil {
			return
		}
		for _, c := range got {
			if c.Generator == "" || math.IsNaN(c.Score) || math.IsInf(c.Score, 0) || math.IsNaN(c.Weight) || math.IsInf(c.Weight, 0) {
				t.Fatalf("accepted invalid contribution %+v from %q", c, raw)
			}
		}
	})
}
