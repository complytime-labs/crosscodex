package main

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

var _ = Describe("run status result cache lines", func() {
	qwen := []*pb.AnalyzerCacheModel{{Alias: "default", ObservedModel: "qwen2.5:14b"}}
	attested := pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED

	DescribeTable("describes one analyzer's cache use",
		func(s *pb.AnalyzerCacheSummary, want string) {
			Expect(cacheLine(s)).To(Equal(want))
		},
		Entry("reuse from jobs the caller created",
			&pb.AnalyzerCacheSummary{Analyzer: "classify", Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED,
				Tasks: 1200, Hits: 1190, Dispatched: 10, Models: qwen, ProducerJobIds: []string{"job-a", "job-b"}},
			"1,190 of 1,200 reused (from 2 earlier jobs: job-a, job-b), 10 sent to model default → qwen2.5:14b"),
		Entry("reuse from the caller's job and other users' jobs",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED, Tasks: 3, Hits: 3,
				Models: qwen, ProducerJobIds: []string{"job-a"}, OtherProducerJobs: 2},
			"3 of 3 reused (from 3 earlier jobs: job-a and 2 by other users), 0 sent to model default → qwen2.5:14b"),
		Entry("reuse only from other users' jobs",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED, Tasks: 2, Hits: 1, Dispatched: 1,
				Models: qwen, OtherProducerJobs: 1},
			"1 of 2 reused (from 1 earlier job by other users), 1 sent to model default → qwen2.5:14b"),
		Entry("nothing reused yet, with a declared revision and a partly uncached panel",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED, Tasks: 4, Dispatched: 4,
				Reason: `the model gateway returned no model name for alias "b"`,
				Models: []*pb.AnalyzerCacheModel{{Alias: "a", ObservedModel: "m1", Revision: "r1"}, {Alias: "b"}}},
			`0 of 4 reused, 4 sent to models a → m1 (revision r1), b → unknown; the model gateway returned no model name for alias "b"`),
		Entry("--no-cache",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_BYPASSED, Tasks: 5, Dispatched: 5, Models: qwen},
			"cache bypassed (--no-cache): 5 of 5 sent to model default → qwen2.5:14b; results stored for later jobs"),
		Entry("disabled",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_DISABLED,
				Reason: "analysis.result_cache.enabled is false for this tenant"},
			"cache disabled: analysis.result_cache.enabled is false for this tenant"),
		Entry("skipped",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_SKIPPED, Reason: "the analyzer does not opt in"},
			"not cached: the analyzer does not opt in"),
		Entry("a stage signed as unrecorded",
			&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_UNRECORDED,
				Reason: "the stage completed in an earlier run that recorded no result-cache attempt"},
			"cache use not recorded for this stage (signed as unrecorded): the stage completed in an earlier run that recorded no result-cache attempt"),
		Entry("no signed record",
			&pb.AnalyzerCacheSummary{Attestation: pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED},
			"cache use not attested: no signed checkpoint vouches for this stage's records"),
		Entry("not completed yet",
			&pb.AnalyzerCacheSummary{Attestation: pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS},
			"in progress"),
	)

	It("trusts no counts the server did not say were attested", func() {
		Expect(cacheLine(&pb.AnalyzerCacheSummary{Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED, Tasks: 2, Hits: 2})).
			To(Equal("cache use not attested: no signed checkpoint vouches for this stage's records"))
	})

	It("removes control and format characters from server text", func() {
		line := cacheLine(&pb.AnalyzerCacheSummary{Attestation: attested, Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_SKIPPED, Reason: "a\x1b[31mb\u202ec"})
		Expect(line).NotTo(ContainSubstring("\x1b"))
		Expect(line).NotTo(ContainSubstring("\u202e"))
	})

	It("renders the summaries as snake_case JSON with the mode and attestation as words, and no mode as empty", func() {
		data, err := json.Marshal(cacheToJSON([]*pb.AnalyzerCacheSummary{{Analyzer: "classify", Attestation: attested,
			Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED, Tasks: 2, Hits: 1, Dispatched: 1, Models: qwen,
			ProducerJobIds: []string{"job-a"}, OtherProducerJobs: 1}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[{"analyzer":"classify","attestation":"attested","mode":"enabled","reason":"","tasks":2,"hits":1,"dispatched":1,
			"models":[{"alias":"default","observed_model":"qwen2.5:14b","revision":""}],"producer_job_ids":["job-a"],"other_producer_jobs":1}]`))
		data, err = json.Marshal(cacheToJSON([]*pb.AnalyzerCacheSummary{{Analyzer: "requires",
			Attestation: pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[{"analyzer":"requires","attestation":"in_progress","mode":"","reason":"","tasks":0,"hits":0,
			"dispatched":0,"models":[],"producer_job_ids":[],"other_producer_jobs":0}]`))
		data, err = json.Marshal(cacheToJSON([]*pb.AnalyzerCacheSummary{{Analyzer: "embedding", Attestation: attested,
			Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_UNRECORDED}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[{"analyzer":"embedding","attestation":"attested","mode":"unrecorded","reason":"","tasks":0,"hits":0,
			"dispatched":0,"models":[],"producer_job_ids":[],"other_producer_jobs":0}]`))
		Expect(json.Marshal(cacheToJSON(nil))).To(MatchJSON(`[]`))
	})
})
