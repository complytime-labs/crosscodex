package resultcachetest_test

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("after any sequence of puts, every key holds its last put, and that hit verifies against its attempt", func() {
		ctx := tenantCtx("tenant-a")
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			f := &resultcachetest.Fake{}
			jobs := []string{"job-1", "job-2", "job-3"}
			for _, j := range jobs {
				f.AddJob("tenant-a", j)
			}
			f.AddCatalog("tenant-a", "cat-1")
			last := map[string]resultcache.Entry{}
			n := rapid.IntRange(1, 20).Draw(t, "puts")
			for i := range n {
				// json.Marshal, not %q: Go escapes such as \x00 are not JSON.
				response, err := json.Marshal(rapid.String().Draw(t, "response"))
				if err != nil {
					t.Fatalf("marshal response: %v", err)
				}
				e := resultcache.Entry{
					JobID: rapid.SampledFrom(jobs).Draw(t, "job"), Analyzer: "classify", TaskID: fmt.Sprintf("t%d", i),
					Attempt:       rapid.SampledFrom([]string{"a1", "a2"}).Draw(t, "attempt"),
					CacheKey:      fmt.Sprintf("%064x", rapid.IntRange(0, 3).Draw(t, "key")),
					Result:        []byte(`{"model":"m","response":` + string(response) + `}`),
					ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-1",
				}
				if err := f.Put(ctx, e); err != nil {
					t.Fatalf("Put: %v", err)
				}
				last[e.CacheKey] = e
			}
			for key, e := range last {
				ref := resultcache.AttemptRef{JobID: e.JobID, Analyzer: "classify", Attempt: e.Attempt}
				m, err := f.Manifest(ctx, e.JobID, "classify", e.Attempt)
				if err != nil {
					t.Fatalf("Manifest: %v", err)
				}
				snap, err := f.LookupVerifiable(ctx, "classify", []string{key}, []resultcache.AttemptRef{ref})
				if err != nil {
					t.Fatalf("LookupVerifiable: %v", err)
				}
				h, err := snap.Verify(key, map[resultcache.AttemptRef]string{ref: m.Digest()})
				if err != nil {
					t.Fatalf("hit of key %s does not verify against its signed attempt: %v", key, err)
				}
				if string(h.Result) != string(e.Result) || h.ProducerJobID != e.JobID {
					t.Fatalf("key %s holds %s by %s, want %s by %s", key, h.Result, h.ProducerJobID, e.Result, e.JobID)
				}
			}
		})
	})
})
