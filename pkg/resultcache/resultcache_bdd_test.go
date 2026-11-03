package resultcache_test

import (
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

func temp(v float64) *float64 { return &v }

// baseIdentity is the completion request every key spec varies from.
func baseIdentity() resultcache.Identity {
	return resultcache.Identity{TaskType: "classify", ModelAlias: "default", Temperature: temp(0.3),
		MaxTokens: 20, Sample: 0, ContentHash: strings.Repeat("a", 64)}
}

const attempt1 = "6f1c2b0e-8d4a-4c3e-9b7a-1d2e3f405162"

func validEntry() resultcache.Entry {
	return resultcache.Entry{JobID: "job-1", Analyzer: "classify", Attempt: attempt1, TaskID: "classify-ac-1",
		CacheKey: strings.Repeat("0", 64), Result: []byte(`{"model":"m","response":"x"}`),
		ResponseModel: "m", SourceCatalogID: "cat-1", TargetCatalogID: "cat-1"}
}

func validRecord() resultcache.StageRecord {
	return resultcache.StageRecord{JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
		Hits: []resultcache.HitEntry{{TaskID: "classify-ac-1", CacheKey: strings.Repeat("0", 64),
			ResultDigest: "sha256:" + strings.Repeat("1", 64), ProducerJobID: "job-1"}},
		Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 2, Hits: 1, Dispatched: 1}}
}

var _ = Describe("Key", func() {
	It("is the same 64-character lowercase hex SHA-256 for the same request", func() {
		a := resultcache.Key("classify", baseIdentity(), "qwen2.5:14b", "")
		Expect(a).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(resultcache.Key("classify", baseIdentity(), "qwen2.5:14b", "")).To(Equal(a))
	})

	DescribeTable("changes when any one component changes",
		func(mutate func(name *string, id *resultcache.Identity, observed, revision *string)) {
			name, id, observed, revision := "classify", baseIdentity(), "qwen2.5:14b", "r1"
			before := resultcache.Key(name, id, observed, revision)
			mutate(&name, &id, &observed, &revision)
			Expect(resultcache.Key(name, id, observed, revision)).NotTo(Equal(before))
		},
		Entry("analyzer name", func(n *string, _ *resultcache.Identity, _, _ *string) { *n = "artifacts" }),
		Entry("task type", func(_ *string, id *resultcache.Identity, _, _ *string) { id.TaskType = "artifacts" }),
		Entry("model alias", func(_ *string, id *resultcache.Identity, _, _ *string) { id.ModelAlias = "other" }),
		Entry("temperature", func(_ *string, id *resultcache.Identity, _, _ *string) { id.Temperature = temp(0.7) }),
		Entry("temperature absent", func(_ *string, id *resultcache.Identity, _, _ *string) { id.Temperature = nil }),
		Entry("max tokens", func(_ *string, id *resultcache.Identity, _, _ *string) { id.MaxTokens = 21 }),
		Entry("sample index", func(_ *string, id *resultcache.Identity, _, _ *string) { id.Sample = 1 }),
		Entry("content hash", func(_ *string, id *resultcache.Identity, _, _ *string) { id.ContentHash = strings.Repeat("b", 64) }),
		Entry("observed model", func(_ *string, _ *resultcache.Identity, o, _ *string) { *o = "qwen2.5:32b" }),
		Entry("declared revision", func(_ *string, _ *resultcache.Identity, _, r *string) { *r = "r2" }),
	)

	It("keeps two fields from trading bytes", func() {
		Expect(resultcache.Key("ab", resultcache.Identity{TaskType: "c"}, "", "")).
			NotTo(Equal(resultcache.Key("a", resultcache.Identity{TaskType: "bc"}, "", "")))
	})
})

// requestFields lists the field names of the llmclient request types the
// worker sends, and whether each is part of the cache identity. A new field
// fails the spec below until someone decides which side it belongs on.
var requestFields = map[string]map[string]string{
	"CompletionRequest": {
		"Model":         "identity: ModelAlias plus the observed model and revision in Key",
		"Messages":      "identity: covered by ContentHash (hash of the rendered messages)",
		"MaxTokens":     "identity: Identity.MaxTokens",
		"Temperature":   "identity: Identity.Temperature",
		"TopP":          "excluded: the worker does not set it today; add it to Identity before it does",
		"Stop":          "excluded: the worker does not set it today; add it to Identity before it does",
		"TenantID":      "excluded: routing and audit only, the cache is tenant-scoped by the store",
		"JobID":         "excluded: audit correlation only; job-specific by design",
		"PromptName":    "excluded: audit trail only; the rendered text is in ContentHash",
		"PromptVersion": "excluded: audit trail only; the rendered text is in ContentHash",
	},
	"EmbeddingRequest": {
		"Model":    "identity: ModelAlias plus the observed model and revision in Key",
		"Input":    "identity: covered by ContentHash (hash of the text)",
		"TenantID": "excluded: routing and audit only, the cache is tenant-scoped by the store",
		"JobID":    "excluded: audit correlation only; job-specific by design",
	},
}

var _ = Describe("Identity coverage of the LLM request types", func() {
	DescribeTable("accounts for every field of the request",
		func(typ reflect.Type) {
			var actual []string
			for f := range typ.Fields() {
				actual = append(actual, f.Name)
			}
			Expect(actual).To(ConsistOf(slices.Collect(maps.Keys(requestFields[typ.Name()]))),
				"llmclient.%s changed: decide whether each new field changes the model's output (then add it to resultcache.Identity and Key) or is audit-only, and record it in requestFields", typ.Name())
		},
		Entry("CompletionRequest", reflect.TypeFor[llmclient.CompletionRequest]()),
		Entry("EmbeddingRequest", reflect.TypeFor[llmclient.EmbeddingRequest]()),
	)
})

var _ = Describe("Entry.Validate", func() {
	It("accepts a complete entry", func() {
		Expect(validEntry().Validate()).To(Succeed())
	})

	DescribeTable("refuses an entry that breaks a table rule, naming the field",
		func(mutate func(*resultcache.Entry), field string) {
			e := validEntry()
			mutate(&e)
			err := e.Validate()
			Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
			Expect(err).To(MatchError(ContainSubstring(field)))
		},
		Entry("no job", func(e *resultcache.Entry) { e.JobID = "" }, "job_id"),
		Entry("no analyzer", func(e *resultcache.Entry) { e.Analyzer = "" }, "analyzer"),
		Entry("no attempt", func(e *resultcache.Entry) { e.Attempt = "" }, "attempt is empty"),
		Entry("an attempt over 64 bytes", func(e *resultcache.Entry) { e.Attempt = strings.Repeat("a", 65) }, "attempt is 65 bytes"),
		Entry("an attempt with invalid UTF-8", func(e *resultcache.Entry) { e.Attempt = "a\xff" }, "attempt is not UTF-8 text"),
		Entry("an attempt with a NUL byte", func(e *resultcache.Entry) { e.Attempt = "a\x00" }, "attempt is not UTF-8 text"),
		Entry("no task", func(e *resultcache.Entry) { e.TaskID = "" }, "task_id"),
		Entry("a short key", func(e *resultcache.Entry) { e.CacheKey = "abc" }, "cache_key"),
		Entry("an uppercase key", func(e *resultcache.Entry) { e.CacheKey = strings.Repeat("A", 64) }, "cache_key"),
		Entry("no result", func(e *resultcache.Entry) { e.Result = nil }, "result"),
		Entry("a result that is not JSON", func(e *resultcache.Entry) { e.Result = []byte(`{"model":`) }, "result is not valid JSON"),
		Entry("a result with invalid UTF-8", func(e *resultcache.Entry) { e.Result = []byte("{\"model\":\"\xff\"}") }, "result is not valid JSON"),
		Entry("a result with a raw NUL byte", func(e *resultcache.Entry) { e.Result = []byte("{\"model\":\"m\x00\"}") }, "result is not valid JSON"),
		Entry("no response model", func(e *resultcache.Entry) { e.ResponseModel = "" }, "response_model"),
		Entry("no source catalog", func(e *resultcache.Entry) { e.SourceCatalogID = "" }, "source_catalog_id"),
		Entry("no target catalog", func(e *resultcache.Entry) { e.TargetCatalogID = "" }, "target_catalog_id"),
	)
})

var _ = Describe("StageRecord.Validate", func() {
	It("accepts a complete record", func() {
		Expect(validRecord().Validate()).To(Succeed())
	})

	DescribeTable("refuses a record that breaks a table rule, naming it",
		func(mutate func(*resultcache.StageRecord), want string) {
			r := validRecord()
			mutate(&r)
			err := r.Validate()
			Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("no job", func(r *resultcache.StageRecord) { r.JobID = "" }, "job_id"),
		Entry("no attempt", func(r *resultcache.StageRecord) { r.Attempt = "" }, "attempt is empty"),
		Entry("an attempt over 64 bytes", func(r *resultcache.StageRecord) { r.Attempt = strings.Repeat("a", 65) }, "attempt is 65 bytes"),
		Entry("an unknown mode", func(r *resultcache.StageRecord) { r.Stats.Mode = "sometimes" }, `mode "sometimes"`),
		Entry("more hits and dispatches than tasks", func(r *resultcache.StageRecord) { r.Stats.Dispatched = 2 }, "exceed tasks"),
		Entry("a negative count", func(r *resultcache.StageRecord) { r.Stats.Tasks = -1 }, "negative"),
		Entry("tasks over the column's range", func(r *resultcache.StageRecord) { r.Stats.Tasks = math.MaxInt32 + 1 }, "exceed 2147483647"),
		Entry("hits over the column's range", func(r *resultcache.StageRecord) { r.Stats.Hits = math.MaxInt32 + 1 }, "exceed 2147483647"),
		Entry("dispatched over the column's range", func(r *resultcache.StageRecord) { r.Stats.Dispatched = math.MaxInt32 + 1 }, "exceed 2147483647"),
		Entry("hits under a mode that does not look up", func(r *resultcache.StageRecord) { r.Stats.Mode = resultcache.ModeBypassed }, "only mode enabled looks results up"),
		Entry("a duplicated hit entry", func(r *resultcache.StageRecord) {
			r.Hits = append(r.Hits, r.Hits[0])
			r.Stats.Hits, r.Stats.Tasks = 2, 3
		}, "duplicates"),
		Entry("hit entries that disagree with the hit count", func(r *resultcache.StageRecord) { r.Stats.Hits = 0 }, "stats.hits"),
		Entry("a hit without its producer", func(r *resultcache.StageRecord) { r.Hits[0].ProducerJobID = "" }, "producer_job_id"),
		Entry("a hit with a malformed digest", func(r *resultcache.StageRecord) { r.Hits[0].ResultDigest = "md5:1" }, "result_digest"),
	)
})

var _ = Describe("Snapshot.Verify", func() {
	const attempt2 = "0a9b8c7d-6e5f-4a3b-8c2d-1e0f9a8b7c6d"
	key := strings.Repeat("0", 64)
	result := []byte(`{"model":"m","response":"x"}`)
	ref1 := resultcache.AttemptRef{JobID: "job-1", Analyzer: "classify", Attempt: attempt1}
	ref2 := resultcache.AttemptRef{JobID: "job-1", Analyzer: "classify", Attempt: attempt2}
	write := func(digest string) resultcache.ManifestEntry {
		return resultcache.ManifestEntry{TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key, ResultDigest: digest}
	}
	// snapshot holds key's row, stored by job-1 in attempt, and the given
	// manifests.
	snapshot := func(stored []byte, attempt string, manifests ...resultcache.Manifest) resultcache.Snapshot {
		read := map[resultcache.AttemptRef]resultcache.Manifest{}
		for _, m := range manifests {
			read[resultcache.AttemptRef{JobID: m.JobID, Analyzer: m.Analyzer, Attempt: m.Attempt}] = m
		}
		return resultcache.NewSnapshot("classify", map[string]resultcache.Hit{key: {CacheKey: key, Result: stored,
			ResponseModel: "m", ProducerJobID: "job-1", ProducerAttempt: attempt}}, read)
	}
	manifest := func(ref resultcache.AttemptRef, entries ...resultcache.ManifestEntry) resultcache.Manifest {
		return resultcache.Manifest{TenantID: "tenant-a", JobID: ref.JobID, Analyzer: ref.Analyzer, Attempt: ref.Attempt, Entries: entries}
	}
	signedFor := func(ms ...resultcache.Manifest) map[resultcache.AttemptRef]string {
		out := map[resultcache.AttemptRef]string{}
		for _, m := range ms {
			out[resultcache.AttemptRef{JobID: m.JobID, Analyzer: m.Analyzer, Attempt: m.Attempt}] = m.Digest()
		}
		return out
	}

	It("returns the hit when the attempt that stored it is signed, matches its signature and vouches for its bytes", func() {
		m := manifest(ref1, write(resultcache.ResultDigest(result)))
		h, err := snapshot(result, attempt1, m).Verify(key, signedFor(m))
		Expect(err).NotTo(HaveOccurred())
		Expect(h.Result).To(Equal(result))
		Expect(h.ProducerJobID).To(Equal("job-1"))
		Expect(h.ProducerAttempt).To(Equal(attempt1))
	})

	It("does not let a hit entry vouch, even one naming the producer itself", func() {
		m := manifest(ref2, resultcache.ManifestEntry{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: key,
			ResultDigest: resultcache.ResultDigest(result), ProducerJobID: "job-1"})
		_, err := snapshot(result, attempt2, m).Verify(key, signedFor(m))
		Expect(err).To(MatchError(resultcache.ErrCorrupt))
	})

	DescribeTable("treats a result whose storing attempt is not signed as a miss",
		func(s resultcache.Snapshot, signed map[resultcache.AttemptRef]string, want string) {
			_, err := s.Verify(key, signed)
			Expect(err).To(MatchError(resultcache.ErrUntrusted))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("no cached row", resultcache.NewSnapshot("classify", nil, nil), map[resultcache.AttemptRef]string{}, "no cached result"),
		Entry("no checkpoint of the producer",
			snapshot(result, attempt1, manifest(ref1, write(resultcache.ResultDigest(result)))), map[resultcache.AttemptRef]string{},
			"attempt "+attempt1+" of job job-1, which stored the result, has no checkpoint-signed record"),
		Entry("only another job's checkpoint",
			snapshot(result, attempt1, manifest(ref1, write(resultcache.ResultDigest(result)))),
			map[resultcache.AttemptRef]string{{JobID: "job-9", Analyzer: "classify", Attempt: attempt1}: "sha256:" + strings.Repeat("9", 64)},
			"has no checkpoint-signed record"),
		// A1: the producer re-stored the key in a later attempt that was
		// never sealed (a crash, or a planted row): the result is not one a
		// signed record vouches for, but nothing signed was changed either.
		Entry("a later unsealed attempt of the signed producer re-stored the key with another result",
			snapshot([]byte(`{"model":"m","response":"y"}`), attempt2,
				manifest(ref1, write(resultcache.ResultDigest(result))),
				manifest(ref2, write(resultcache.ResultDigest([]byte(`{"model":"m","response":"y"}`))))),
			signedFor(manifest(ref1, write(resultcache.ResultDigest(result)))),
			"attempt "+attempt2+" of job job-1, which stored the result, has no checkpoint-signed record"),
	)

	DescribeTable("reports tampering as corrupt",
		func(s resultcache.Snapshot, signed map[resultcache.AttemptRef]string, want string) {
			_, err := s.Verify(key, signed)
			Expect(err).To(MatchError(resultcache.ErrCorrupt))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("an edited result",
			snapshot([]byte(`{"model":"m","response":"y"}`), attempt1, manifest(ref1, write(resultcache.ResultDigest(result)))),
			signedFor(manifest(ref1, write(resultcache.ResultDigest(result)))), "attempt "+attempt1+" of job job-1 does not vouch"),
		Entry("a row added to the signed attempt after it was signed",
			snapshot(result, attempt1, manifest(ref1, write(resultcache.ResultDigest(result)), write("sha256:"+strings.Repeat("e", 64)))),
			signedFor(manifest(ref1, write(resultcache.ResultDigest(result)))), "does not match its signed checkpoint"),
		Entry("a write only another attempt holds",
			snapshot(result, attempt1, manifest(ref1), manifest(ref2, write(resultcache.ResultDigest(result)))),
			signedFor(manifest(ref1)), "attempt "+attempt1+" of job job-1 does not vouch"),
		// M2: a hit naming another producer is that producer's result, not
		// one the manifest's job vouches for.
		Entry("a hit naming another producer",
			snapshot(result, attempt1, manifest(ref1, resultcache.ManifestEntry{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: key,
				ResultDigest: resultcache.ResultDigest(result), ProducerJobID: "job-9"})),
			signedFor(manifest(ref1, resultcache.ManifestEntry{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: key,
				ResultDigest: resultcache.ResultDigest(result), ProducerJobID: "job-9"})), "attempt "+attempt1+" of job job-1 does not vouch"),
	)

	It("returns only a copy of the result and lists the keys it holds", func() {
		m := manifest(ref1, write(resultcache.ResultDigest(result)))
		s := snapshot(result, attempt1, m)
		Expect(s.Keys()).To(Equal([]string{key}))
		h, err := s.Verify(key, signedFor(m))
		Expect(err).NotTo(HaveOccurred())
		h.Result[0] = 'X'
		again, err := s.Verify(key, signedFor(m))
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Result).To(Equal(result))
	})

	It("refuses a signed attempt of the producer for another analyzer", func() {
		m := manifest(ref1, write(resultcache.ResultDigest(result)))
		other := ref1
		other.Analyzer = "artifacts"
		_, err := snapshot(result, attempt1, m).Verify(key, map[resultcache.AttemptRef]string{other: m.Digest()})
		Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
		Expect(err).To(MatchError(ContainSubstring(`for analyzer "artifacts", not "classify"`)))
	})

	It("refuses a signed attempt the snapshot did not read, naming it", func() {
		m := manifest(ref1, write(resultcache.ResultDigest(result)))
		_, err := snapshot(result, attempt1).Verify(key, signedFor(m))
		Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
		Expect(err).To(MatchError(ContainSubstring("attempt " + attempt1 + " of job job-1 was not read")))
	})
})

var _ = Describe("EncodeResult and DecodeResult", func() {
	It("encodes equal results to equal bytes, keys sorted", func() {
		a, err := structpb.NewStruct(map[string]any{"response": "x", "model": "m", "tokens_used": 12.0})
		Expect(err).NotTo(HaveOccurred())
		data, err := resultcache.EncodeResult(a)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal(`{"model":"m","response":"x","tokens_used":12}`))
	})

	It("decodes what it encoded", func() {
		a, err := structpb.NewStruct(map[string]any{"model": "m", "embeddings": []any{[]any{0.125, -0.5}}})
		Expect(err).NotTo(HaveOccurred())
		data, err := resultcache.EncodeResult(a)
		Expect(err).NotTo(HaveOccurred())
		back, err := resultcache.DecodeResult(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(proto.Equal(back, a)).To(BeTrue())
	})

	It("refuses to encode no result", func() {
		_, err := resultcache.EncodeResult(nil)
		Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
	})

	DescribeTable("refuses to encode a result DecodeResult would reject as corrupt",
		func(fields map[string]any) {
			s, err := structpb.NewStruct(fields)
			Expect(err).NotTo(HaveOccurred())
			_, err = resultcache.EncodeResult(s)
			Expect(err).To(MatchError(resultcache.ErrInvalidEntry))
			Expect(err).To(MatchError(ContainSubstring("names no model")))
		},
		Entry("no model", map[string]any{"response": "x"}),
		Entry("an empty model", map[string]any{"model": "", "response": "x"}),
		Entry("a non-string model", map[string]any{"model": 7.0}),
	)

	DescribeTable("refuses stored bytes that are not a result, as corrupt",
		func(data, want string) {
			_, err := resultcache.DecodeResult([]byte(data))
			Expect(err).To(MatchError(resultcache.ErrCorrupt))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("not JSON", `{"model":`, "not a JSON object"),
		Entry("an array", `[]`, "not a JSON object"),
		Entry("JSON null", `null`, "not a JSON object"),
		Entry("trailing data", `{"model":"m"} {"model":"n"}`, "trailing data"),
		Entry("no model", `{"response":"x"}`, "names no model"),
		Entry("a non-string model", `{"model":7}`, "names no model"),
	)
})

var _ = Describe("Manifest.Digest", func() {
	entries := []resultcache.ManifestEntry{
		{TaskID: "t2", Role: resultcache.RoleWrite, CacheKey: strings.Repeat("2", 64), ResultDigest: "sha256:" + strings.Repeat("2", 64)},
		{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: strings.Repeat("1", 64), ResultDigest: "sha256:" + strings.Repeat("1", 64), ProducerJobID: "job-0"},
	}
	stats := []resultcache.Stats{{Mode: resultcache.ModeEnabled, Tasks: 2, Hits: 1, Dispatched: 1,
		Models: map[string]resultcache.ModelIdentity{"default": {Observed: "qwen2.5:14b"}}}}

	DescribeTable("binds the names of the attempt",
		func(mutate func(*resultcache.Manifest)) {
			a := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-1", Analyzer: "classify", Attempt: attempt1, Entries: entries, Stats: stats}
			b := a
			mutate(&b)
			Expect(a.Digest()).NotTo(Equal(b.Digest()))
		},
		Entry("the tenant", func(m *resultcache.Manifest) { m.TenantID = "tenant-b" }),
		Entry("the job", func(m *resultcache.Manifest) { m.JobID = "job-2" }),
		Entry("the analyzer", func(m *resultcache.Manifest) { m.Analyzer = "artifacts" }),
	)

	It("is a sha256 digest that ignores entry order", func() {
		d := resultcache.Manifest{Attempt: attempt1, Entries: entries, Stats: stats}.Digest()
		Expect(d).To(MatchRegexp(`^sha256:[0-9a-f]{64}$`))
		Expect(resultcache.Manifest{Attempt: attempt1, Entries: []resultcache.ManifestEntry{entries[1], entries[0]}, Stats: stats}.Digest()).To(Equal(d))
	})

	It("treats no models and an empty models map alike, as a database round trip yields either", func() {
		a := resultcache.Manifest{Stats: []resultcache.Stats{{Mode: resultcache.ModeDisabled}}}
		b := resultcache.Manifest{Stats: []resultcache.Stats{{Mode: resultcache.ModeDisabled, Models: map[string]resultcache.ModelIdentity{}}}}
		Expect(a.Digest()).To(Equal(b.Digest()))
	})

	DescribeTable("changes when any recorded fact changes",
		func(mutate func(*resultcache.Manifest)) {
			m := resultcache.Manifest{Attempt: attempt1, Entries: append([]resultcache.ManifestEntry(nil), entries...), Stats: append([]resultcache.Stats(nil), stats...)}
			before := m.Digest()
			mutate(&m)
			Expect(m.Digest()).NotTo(Equal(before))
		},
		Entry("an entry's digest", func(m *resultcache.Manifest) { m.Entries[0].ResultDigest = "sha256:" + strings.Repeat("9", 64) }),
		Entry("an entry's producer", func(m *resultcache.Manifest) { m.Entries[1].ProducerJobID = "job-9" }),
		Entry("a dropped entry", func(m *resultcache.Manifest) { m.Entries = m.Entries[:1] }),
		Entry("a stats count", func(m *resultcache.Manifest) { m.Stats[0].Hits = 2; m.Stats[0].Dispatched = 0 }),
		Entry("an observed model", func(m *resultcache.Manifest) {
			m.Stats[0].Models = map[string]resultcache.ModelIdentity{"default": {Observed: "qwen2.5:32b"}}
		}),
		Entry("a second stats row", func(m *resultcache.Manifest) { m.Stats = append(m.Stats, m.Stats[0]) }),
		Entry("the attempt", func(m *resultcache.Manifest) { m.Attempt = "0a9b8c7d-6e5f-4a3b-8c2d-1e0f9a8b7c6d" }),
	)
})

var _ = Describe("MatchRecord", func() {
	key := strings.Repeat("1", 64)
	recorded := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-1", Analyzer: "classify", Attempt: attempt1,
		Entries: []resultcache.ManifestEntry{{TaskID: "t1", Role: resultcache.RoleWrite, CacheKey: key, ResultDigest: "sha256:" + strings.Repeat("a", 64)}},
		Stats:   []resultcache.Stats{{Mode: resultcache.ModeEnabled, Tasks: 1, Dispatched: 1}}}

	It("accepts the same rows in any entry order, nil and empty models alike", func() {
		stored := recorded
		stored.Stats = []resultcache.Stats{{Mode: resultcache.ModeEnabled, Tasks: 1, Dispatched: 1, Models: map[string]resultcache.ModelIdentity{}}}
		Expect(resultcache.MatchRecord(recorded, stored)).To(Succeed())
	})

	It("names the unexpected and missing rows by identity, as corrupt", func() {
		stored := recorded
		stored.Entries = []resultcache.ManifestEntry{{TaskID: "t9", Role: resultcache.RoleWrite, CacheKey: key, ResultDigest: "sha256:" + strings.Repeat("f", 64)}}
		err := resultcache.MatchRecord(recorded, stored)
		Expect(err).To(MatchError(resultcache.ErrCorrupt))
		Expect(err).To(MatchError(And(
			ContainSubstring("attempt "+attempt1+" of job job-1 (analyzer classify)"),
			ContainSubstring(`unexpected rows: entry(task="t9" role=write key=`+key+" digest=sha256:"+strings.Repeat("f", 64)+` producer="")`),
			ContainSubstring(`missing rows: entry(task="t1" role=write`)),
		))
	})

	It("quotes row fields a writer chooses, so they cannot forge log lines", func() {
		stored := recorded
		stored.Entries = []resultcache.ManifestEntry{{TaskID: "x)\nINFO checkpoint ok", Role: resultcache.RoleHit, CacheKey: key,
			ResultDigest: "sha256:" + strings.Repeat("f", 64), ProducerJobID: "p\nINFO forged"}}
		err := resultcache.MatchRecord(recorded, stored)
		Expect(err).To(MatchError(resultcache.ErrCorrupt))
		Expect(err.Error()).NotTo(ContainSubstring("\n"))
		Expect(err).To(MatchError(And(
			ContainSubstring(`task="x)\nINFO checkpoint ok"`),
			ContainSubstring(`producer="p\nINFO forged"`),
		)))
	})

	It("reports an extra stats row", func() {
		stored := recorded
		stored.Stats = append(slices.Clone(recorded.Stats), resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 9, Dispatched: 9})
		err := resultcache.MatchRecord(recorded, stored)
		Expect(err).To(MatchError(resultcache.ErrCorrupt))
		Expect(err).To(MatchError(ContainSubstring("unexpected rows: stats#2(mode=enabled")))
		Expect(err).To(MatchError(ContainSubstring("missing rows: none")))
	})

	It("lists at most 20 rows per list", func() {
		stored := recorded
		for i := range 25 {
			stored.Entries = append(stored.Entries, resultcache.ManifestEntry{TaskID: fmt.Sprintf("x%02d", i), Role: resultcache.RoleWrite,
				CacheKey: key, ResultDigest: "sha256:" + strings.Repeat("b", 64)})
		}
		Expect(resultcache.MatchRecord(recorded, stored)).To(MatchError(ContainSubstring(", and 5 more; missing rows: none")))
	})
})

var _ = Describe("SummarizeSigned", func() {
	m := resultcache.Manifest{TenantID: "tenant-a", JobID: "job-2", Analyzer: "classify", Attempt: attempt1,
		Entries: []resultcache.ManifestEntry{
			{TaskID: "t1", Role: resultcache.RoleHit, CacheKey: strings.Repeat("1", 64), ResultDigest: "sha256:" + strings.Repeat("1", 64), ProducerJobID: "job-9"},
			{TaskID: "t2", Role: resultcache.RoleHit, CacheKey: strings.Repeat("2", 64), ResultDigest: "sha256:" + strings.Repeat("2", 64), ProducerJobID: "job-2"},
			{TaskID: "t3", Role: resultcache.RoleHit, CacheKey: strings.Repeat("3", 64), ResultDigest: "sha256:" + strings.Repeat("3", 64), ProducerJobID: "job-1"},
		},
		Stats: []resultcache.Stats{{Mode: resultcache.ModeEnabled, Tasks: 3, Hits: 0, Dispatched: 3}, {Mode: resultcache.ModeEnabled, Tasks: 3, Hits: 3}}}

	It("summarizes a matching attempt from its latest stats and its other producers", func() {
		sums := resultcache.SummarizeSigned(map[string]resultcache.Manifest{"classify": m},
			map[string]resultcache.SignedAttempt{"classify": {Attempt: attempt1, Digest: m.Digest()}})
		Expect(sums).To(Equal([]resultcache.Summary{{Analyzer: "classify", Attempt: attempt1, Attested: true,
			Stats: m.Stats[1], ProducerJobIDs: []string{"job-1", "job-9"}}}))
	})

	It("marks an attempt whose rows changed after signing unattested, showing nothing of it", func() {
		sums := resultcache.SummarizeSigned(map[string]resultcache.Manifest{"classify": m},
			map[string]resultcache.SignedAttempt{"classify": {Attempt: attempt1, Digest: "sha256:" + strings.Repeat("0", 64)}})
		Expect(sums).To(Equal([]resultcache.Summary{{Analyzer: "classify", Attempt: attempt1}}))
	})
})
