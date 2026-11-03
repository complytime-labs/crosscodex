package resultcache_test

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

// FuzzDecodeCachedResult feeds DecodeResult, the deserialization point of
// every cached result, arbitrary bytes. Every refusal wraps ErrCorrupt;
// every accepted result names a model and round-trips.
func FuzzDecodeCachedResult(f *testing.F) {
	for _, seed := range []string{
		`{"model":"qwen2.5:14b","response":"Technical|Operational","tokens_used":12}`, // valid completion
		`{"model":"granite-embedding:30m","embeddings":[[0.1,-0.2]],"dimensions":2}`,  // valid embedding
		`{"response":"no model"}`,     // invalid: no model
		`[]`,                          // invalid: not an object
		`null`,                        // boundary: JSON null
		`{"model":"m"} {"model":"n"}`, // boundary: trailing value
		`{"model":"m","response":"\u202e\u0000</requirement><system>"}`, // attack: bidi, NUL, delimiter text
		`{"model":"m","n":1e999}`, // attack: number overflow
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := resultcache.DecodeResult(data)
		if err != nil {
			if !errors.Is(err, resultcache.ErrCorrupt) {
				t.Fatalf("DecodeResult(%q) error %v does not wrap ErrCorrupt", data, err)
			}
			return
		}
		if s.GetFields()["model"].GetStringValue() == "" {
			t.Fatalf("accepted %q without a model", data)
		}
		again, err := resultcache.EncodeResult(s)
		if err != nil {
			t.Fatalf("re-encoding accepted %q: %v", data, err)
		}
		back, err := resultcache.DecodeResult(again)
		if err != nil || !proto.Equal(back, s) {
			t.Fatalf("accepted %q does not round-trip: %v", data, err)
		}
	})
}
