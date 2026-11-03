package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

func payloadTask(fields map[string]any) analyzer.Task {
	s, err := structpb.NewStruct(fields)
	Expect(err).NotTo(HaveOccurred())
	return analyzer.Task{TaskID: "relationship-a--b-m-s1", TaskType: "relationship", Payload: s}
}

func messagesOf(user string) []any {
	return intanalyzer.MessagesForPayload([]llmclient.ChatMessage{
		{Role: "system", Content: "sys"}, {Role: "user", Content: user},
	})
}

var _ = Describe("PayloadCacheRequest", func() {
	messages := messagesOf("question")
	hash := llmclient.ContentHash([]llmclient.ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "question"}})
	completion := func(extra map[string]any) analyzer.Task {
		f := map[string]any{"source_control_id": "a", "target_control_id": "b", "model": "m", "temperature": 0.3,
			"max_tokens": 300.0, "sample_index": 1.0, "messages": messages, "content_hash": hash, "job_id": "job-1"}
		for k, v := range extra {
			f[k] = v
		}
		return payloadTask(f)
	}
	keyOf := func(task analyzer.Task) string {
		req, ok := intanalyzer.PayloadCacheRequest(task, "source_control_id", "target_control_id")
		Expect(ok).To(BeTrue())
		return resultcache.Key("relationship", req.Identity, resultcache.ModelIdentity{Observed: "observed"})
	}

	It("reads a completion's alias, decoding parameters, sample and recomputed content hash, and its controls", func() {
		got, ok := intanalyzer.PayloadCacheRequest(completion(nil), "source_control_id", "target_control_id")
		Expect(ok).To(BeTrue())
		t := 0.3
		Expect(got).To(Equal(analyzer.CacheRequest{
			Identity:        resultcache.Identity{TaskType: "relationship", ModelAlias: "m", Temperature: &t, MaxTokens: 300, Sample: 1, ContentHash: hash},
			SourceControlID: "a", TargetControlID: "b",
		}))
	})

	It("reads an embedding, which sends no temperature or token limit", func() {
		got, ok := intanalyzer.PayloadCacheRequest(payloadTask(map[string]any{
			"control_id": "c", "model": "embed", "text": "t", "batch_size": 50.0, "content_hash": llmclient.ContentHash("t"),
		}), "control_id", "control_id")
		Expect(ok).To(BeTrue())
		Expect(got.Identity.Temperature).To(BeNil())
		Expect(got.Identity.MaxTokens).To(BeZero())
		Expect(got.Identity.ContentHash).To(Equal(llmclient.ContentHash("t")))
		Expect(got.SourceControlID).To(Equal("c"))
		Expect(got.TargetControlID).To(Equal("c"))
	})

	It("identifies a payload that carries no content_hash by the recomputed one, as the worker accepts it", func() {
		task := completion(nil)
		delete(task.Payload.(*structpb.Struct).Fields, "content_hash")
		got, ok := intanalyzer.PayloadCacheRequest(task, "source_control_id", "target_control_id")
		Expect(ok).To(BeTrue())
		Expect(got.Identity.ContentHash).To(Equal(hash))
	})

	It("ignores job-specific fields and the task ID", func() {
		first := completion(map[string]any{"job_id": "job-1"})
		second := completion(map[string]any{"job_id": "job-2"})
		second.TaskID = "another-task-id"
		a, okA := intanalyzer.PayloadCacheRequest(first, "source_control_id", "target_control_id")
		b, okB := intanalyzer.PayloadCacheRequest(second, "source_control_id", "target_control_id")
		Expect(okA).To(BeTrue())
		Expect(okB).To(BeTrue())
		Expect(a).To(Equal(b))
	})

	DescribeTable("changes the key when the request changes",
		func(change map[string]any) {
			Expect(keyOf(completion(change))).NotTo(Equal(keyOf(completion(nil))))
		},
		Entry("the model alias", map[string]any{"model": "other"}),
		Entry("the token limit", map[string]any{"max_tokens": 301.0}),
		Entry("the temperature", map[string]any{"temperature": 0.4}),
		Entry("the sample", map[string]any{"sample_index": 2.0}),
		Entry("the message text", map[string]any{"messages": messagesOf("another question"), "content_hash": ""}),
	)

	DescribeTable("reports a task it cannot identify as uncacheable",
		func(task analyzer.Task) {
			_, ok := intanalyzer.PayloadCacheRequest(task, "source_control_id", "target_control_id")
			Expect(ok).To(BeFalse())
		},
		Entry("a pre-built result", analyzer.Task{PreBuiltResult: true, Payload: &structpb.Struct{}}),
		Entry("a payload that is not a struct", analyzer.Task{Payload: &pb.AnalysisResult{}}),
		Entry("no payload", analyzer.Task{Payload: nil}),
		Entry("a stale content hash", completion(map[string]any{"messages": messagesOf("edited after hashing")})),
		Entry("neither messages nor text", completion(map[string]any{"messages": nil})),
		Entry("both messages and text", completion(map[string]any{"text": "t"})),
		Entry("messages that are not a list", completion(map[string]any{"messages": "oops"})),
		Entry("a message with an invalid role", completion(map[string]any{
			"messages": []any{map[string]any{"role": "root", "content": "x"}}, "content_hash": ""})),
		Entry("no control", completion(map[string]any{"source_control_id": ""})),
		Entry("a fractional token limit", completion(map[string]any{"max_tokens": 1.5})),
		Entry("a negative sample", completion(map[string]any{"sample_index": -1.0})),
		Entry("a token limit over int32", completion(map[string]any{"max_tokens": 4294967296.0})),
	)
})
