package resultcache_test

import (
	"bytes"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

// request is every input of the key encoding.
type request struct {
	name               string
	id                 resultcache.Identity
	observed, revision string
}

func drawRequest(t *rapid.T, label string) request {
	r := request{
		name: rapid.String().Draw(t, label+".name"),
		id: resultcache.Identity{
			TaskType:    rapid.String().Draw(t, label+".taskType"),
			ModelAlias:  rapid.String().Draw(t, label+".alias"),
			MaxTokens:   rapid.Int64().Draw(t, label+".maxTokens"),
			Sample:      rapid.Int64().Draw(t, label+".sample"),
			ContentHash: rapid.String().Draw(t, label+".contentHash"),
		},
		observed: rapid.String().Draw(t, label+".observed"),
		revision: rapid.String().Draw(t, label+".revision"),
	}
	if rapid.Bool().Draw(t, label+".hasTemperature") {
		v := rapid.Float64Range(0, 2).Draw(t, label+".temperature")
		r.id.Temperature = &v
	}
	return r
}

// mutateOne returns r with one drawn field redrawn.
func mutateOne(t *rapid.T, r request) request {
	switch rapid.IntRange(0, 9).Draw(t, "field") {
	case 0:
		r.name = rapid.String().Draw(t, "new")
	case 1:
		r.id.TaskType = rapid.String().Draw(t, "new")
	case 2:
		r.id.ModelAlias = rapid.String().Draw(t, "new")
	case 3:
		if r.id.Temperature == nil || rapid.Bool().Draw(t, "dropTemperature") {
			v := rapid.Float64Range(0, 2).Draw(t, "newTemperature")
			r.id.Temperature = &v
		} else {
			r.id.Temperature = nil
		}
	case 4:
		r.id.MaxTokens = rapid.Int64().Draw(t, "new")
	case 5:
		r.id.Sample = rapid.Int64().Draw(t, "new")
	case 6:
		r.id.ContentHash = rapid.String().Draw(t, "new")
	case 7:
		r.observed = rapid.String().Draw(t, "new")
	case 8:
		r.revision = rapid.String().Draw(t, "new")
	default:
		// Move one byte from the end of one field to the start of the next.
		if r.name != "" {
			r.id.TaskType = r.name[len(r.name)-1:] + r.id.TaskType
			r.name = r.name[:len(r.name)-1]
		}
	}
	return r
}

// same reports whether a and b are the same request: equal fields, and
// temperatures that are both absent or format identically.
func same(a, b request) bool {
	ta, tb := a.id.Temperature, b.id.Temperature
	sameTemperature := (ta == nil) == (tb == nil) &&
		(ta == nil || strconv.FormatFloat(*ta, 'g', -1, 64) == strconv.FormatFloat(*tb, 'g', -1, 64))
	a.id.Temperature, b.id.Temperature = nil, nil
	return sameTemperature && a == b
}

func encode(r request) []byte {
	return resultcache.ExportEncodeKey(r.name, r.id, r.observed, r.revision)
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("encodes two requests to the same bytes only when they are the same request", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			a := drawRequest(t, "a")
			b := mutateOne(t, a)
			if rapid.Bool().Draw(t, "independent") {
				b = drawRequest(t, "b")
			}
			if got, want := bytes.Equal(encode(a), encode(b)), same(a, b); got != want {
				t.Fatalf("encodings equal = %v, requests equal = %v:\n a = %+v\n b = %+v", got, want, a, b)
			}
		})
	})

	It("digests a manifest the same whatever the order of its entries", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			entries := rapid.SliceOf(rapid.Custom(func(t *rapid.T) resultcache.ManifestEntry {
				return resultcache.ManifestEntry{
					TaskID:        rapid.String().Draw(t, "task"),
					Role:          rapid.SampledFrom([]string{resultcache.RoleHit, resultcache.RoleWrite}).Draw(t, "role"),
					CacheKey:      rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "key"),
					ResultDigest:  "sha256:" + rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "digest"),
					ProducerJobID: rapid.String().Draw(t, "producer"),
				}
			})).Draw(t, "entries")
			shuffled := rapid.Permutation(entries).Draw(t, "shuffled")
			a := resultcache.Manifest{Entries: entries}.Digest()
			if b := (resultcache.Manifest{Entries: shuffled}).Digest(); a != b {
				t.Fatalf("digest changed with entry order: %s vs %s", a, b)
			}
		})
	})

	It("decodes every result it encodes back to an equal result", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			fields := rapid.MapOf(rapid.StringMatching(`[a-z_]{1,12}`), rapid.OneOf(
				rapid.Map(rapid.String(), func(s string) any { return s }),
				rapid.Map(rapid.Float64Range(-1e6, 1e6), func(f float64) any { return f }),
				rapid.Map(rapid.Bool(), func(b bool) any { return b }),
			)).Draw(t, "fields")
			fields["model"] = rapid.StringMatching(`[a-z0-9.:-]{1,20}`).Draw(t, "model")
			s, err := structpb.NewStruct(fields)
			if err != nil {
				t.Fatalf("NewStruct: %v", err)
			}
			data, err := resultcache.EncodeResult(s)
			if err != nil {
				t.Fatalf("EncodeResult: %v", err)
			}
			back, err := resultcache.DecodeResult(data)
			if err != nil {
				t.Fatalf("DecodeResult(%s): %v", data, err)
			}
			if !proto.Equal(back, s) {
				t.Fatalf("round trip changed the result: %v vs %v", back, s)
			}
		})
	})
})
