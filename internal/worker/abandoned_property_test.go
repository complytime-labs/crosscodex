package worker

import (
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// abandonedKeyGen draws keys from a small space so runs repeat keys.
func abandonedKeyGen() *rapid.Generator[natsbus.AbandonedCollection] {
	return rapid.Custom(func(t *rapid.T) natsbus.AbandonedCollection {
		return natsbus.AbandonedCollection{
			TenantID:     rapid.SampledFrom([]string{"tenant-a", "tenant-b"}).Draw(t, "tenant"),
			TaskType:     rapid.SampledFrom([]natsbus.TaskType{natsbus.TaskRelate, natsbus.TaskEmbed}).Draw(t, "task-type"),
			JobID:        rapid.SampledFrom([]string{"job-1", "job-2", "job-3"}).Draw(t, "job"),
			CollectionID: rapid.StringMatching(`c-[0-9]{1,2}`).Draw(t, "collection"),
		}
	})
}

var _ = Describe("Property Specifications", Ordered, func() {
	Context("abandonedSet — bounded memory", func() {
		It("never holds more than its limit and evicts the oldest collection first", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				limit := rapid.IntRange(1, 16).Draw(t, "limit")
				keys := rapid.SliceOfN(abandonedKeyGen(), 0, 200).Draw(t, "keys")
				now := time.Unix(0, 0)
				s := newAbandonedSet(time.Hour, limit, func() time.Time { return now })

				// model is the expected content, oldest first: a held key is
				// not moved by a repeat, a new key evicts the oldest at limit.
				var model []natsbus.AbandonedCollection
				for _, k := range keys {
					if !slices.Contains(model, k) {
						if len(model) == limit {
							model = model[1:]
						}
						model = append(model, k)
					}
					if got := s.add(k); got != len(model) {
						t.Fatalf("add(%+v) reports %d held, model holds %d (limit %d)", k, got, len(model), limit)
					}
					for _, m := range model {
						if !s.contains(m) {
							t.Fatalf("%+v should still be held (limit %d)", m, limit)
						}
					}
				}
				for _, k := range keys {
					if s.contains(k) != slices.Contains(model, k) {
						t.Fatalf("contains(%+v) = %v, model says %v", k, s.contains(k), slices.Contains(model, k))
					}
				}
			})
		})
	})

	Context("abandonedSet — expiry and the cap together", func() {
		It("matches a model of a FIFO whose entries expire after the TTL, and gives a re-added expired key a fresh TTL", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				limit := rapid.IntRange(1, 8).Draw(t, "limit")
				ttl := time.Duration(rapid.IntRange(1, 50).Draw(t, "ttl-ms")) * time.Millisecond
				now := time.Unix(0, 0)
				s := newAbandonedSet(ttl, limit, func() time.Time { return now })
				var resized []int
				s.onResize = func(n int) { resized = append(resized, n) }

				type entry struct {
					key     natsbus.AbandonedCollection
					expires time.Time
				}
				var model []entry // oldest first
				prune := func() {
					for len(model) > 0 && !now.Before(model[0].expires) {
						model = model[1:]
					}
				}
				held := func(k natsbus.AbandonedCollection) bool {
					return slices.ContainsFunc(model, func(e entry) bool { return e.key == k })
				}

				steps := rapid.IntRange(1, 100).Draw(t, "steps")
				for i := 0; i < steps; i++ {
					before := s.len()
					key := abandonedKeyGen().Draw(t, "key")
					switch rapid.IntRange(0, 2).Draw(t, "op") {
					case 0:
						prune()
						if !held(key) {
							if len(model) == limit {
								model = model[1:]
							}
							model = append(model, entry{key, now.Add(ttl)})
						}
						if got := s.add(key); got != len(model) {
							t.Fatalf("step %d: add reports %d held, model %d", i, got, len(model))
						}
					case 1:
						prune()
						if got := s.contains(key); got != held(key) {
							t.Fatalf("step %d: contains(%+v) = %v, model %v", i, key, got, held(key))
						}
					default:
						now = now.Add(time.Duration(rapid.IntRange(0, 60).Draw(t, "advance-ms")) * time.Millisecond)
					}
					if s.len() != len(model) {
						t.Fatalf("step %d: set holds %d, model %d", i, s.len(), len(model))
					}
					if s.len() != before && (len(resized) == 0 || resized[len(resized)-1] != s.len()) {
						t.Fatalf("step %d: size went %d -> %d without onResize reporting it (reports %v)", i, before, s.len(), resized)
					}
				}
			})
		})
	})

	Context("abandonedSet — expiry", func() {
		It("holds a collection for exactly its TTL and frees it on the next add", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				ttl := time.Duration(rapid.IntRange(1, 1000).Draw(t, "ttl-ms")) * time.Millisecond
				elapsed := time.Duration(rapid.IntRange(0, 2000).Draw(t, "elapsed-ms")) * time.Millisecond
				now := time.Unix(0, 0)
				s := newAbandonedSet(ttl, 16, func() time.Time { return now })
				old := natsbus.AbandonedCollection{TenantID: "tenant-a", TaskType: natsbus.TaskRelate, JobID: "job-1", CollectionID: "c-1"}
				s.add(old)

				now = now.Add(elapsed)
				if got, want := s.contains(old), elapsed < ttl; got != want {
					t.Fatalf("after %s with TTL %s: contains = %v, want %v", elapsed, ttl, got, want)
				}
				s.add(natsbus.AbandonedCollection{TenantID: "tenant-a", TaskType: natsbus.TaskRelate, JobID: "job-1", CollectionID: "c-2"})
				want := 2
				if elapsed >= ttl {
					want = 1
				}
				if s.len() != want {
					t.Fatalf("after %s with TTL %s: set holds %d, want %d", elapsed, ttl, s.len(), want)
				}
			})
		})
	})
})
