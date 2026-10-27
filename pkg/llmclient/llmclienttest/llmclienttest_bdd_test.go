package llmclienttest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
)

func TestLLMClientTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "LLMClientTest Suite")
}

var errScripted = errors.New("scripted")

// completion returns a request the real client accepts.
func completion(model string) *llmclient.CompletionRequest {
	return &llmclient.CompletionRequest{
		Model: model, TenantID: "tenant-a",
		Messages: []llmclient.ChatMessage{{Role: llmclient.RoleUser, Content: "hi"}},
	}
}

func embedding() *llmclient.EmbeddingRequest {
	return &llmclient.EmbeddingRequest{Model: "m", TenantID: "tenant-a", Input: []string{"x"}}
}

var _ = Describe("Fake", func() {
	var (
		ctx  context.Context
		fake *llmclienttest.Fake
	)

	BeforeEach(func() {
		ctx = context.Background()
		fake = &llmclienttest.Fake{}
	})

	DescribeTable("refuses an unscripted call, naming the field to set",
		func(call func(*llmclienttest.Fake) error, field string) {
			err := call(fake)
			Expect(err).To(MatchError(llmclienttest.ErrUnscripted))
			Expect(err.Error()).To(ContainSubstring(field))
		},
		Entry("Complete", func(f *llmclienttest.Fake) error {
			resp, err := f.Complete(context.Background(), completion("m"))
			Expect(resp).To(BeNil())
			return err
		}, "Fake.CompleteFunc"),
		Entry("Embed", func(f *llmclienttest.Fake) error {
			resp, err := f.Embed(context.Background(), embedding())
			Expect(resp).To(BeNil())
			return err
		}, "Fake.EmbedFunc"),
	)

	DescribeTable("rejects what the real client rejects, before the Func and without recording",
		func(complete *llmclient.CompletionRequest, embed *llmclient.EmbeddingRequest, wantText string) {
			reached := false
			fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				reached = true
				return &llmclient.CompletionResponse{}, nil
			}
			fake.EmbedFunc = func(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
				reached = true
				return &llmclient.EmbeddingResponse{}, nil
			}
			var err error
			if embed != nil {
				_, err = fake.Embed(ctx, embed)
			} else {
				_, err = fake.Complete(ctx, complete)
			}
			Expect(errors.Is(err, llmclient.ErrInvalidRequest)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring(wantText))
			Expect(reached).To(BeFalse())
			Expect(fake.CompleteRequests()).To(BeEmpty())
		},
		Entry("nil completion request", nil, nil, "completion request must not be nil"),
		Entry("completion without tenant", func() *llmclient.CompletionRequest { r := completion("m"); r.TenantID = ""; return r }(), nil, "tenant ID is required"),
		Entry("completion with invalid tenant", func() *llmclient.CompletionRequest { r := completion("m"); r.TenantID = "bad tenant!"; return r }(), nil, "invalid tenant ID"),
		Entry("completion without messages", func() *llmclient.CompletionRequest { r := completion("m"); r.Messages = nil; return r }(), nil, "at least one message is required"),
		Entry("embedding without tenant", nil, func() *llmclient.EmbeddingRequest { r := embedding(); r.TenantID = ""; return r }(), "tenant ID is required"),
		Entry("embedding with invalid tenant", nil, func() *llmclient.EmbeddingRequest { r := embedding(); r.TenantID = "bad tenant!"; return r }(), "invalid tenant ID"),
		Entry("embedding without input", nil, func() *llmclient.EmbeddingRequest { r := embedding(); r.Input = nil; return r }(), "at least one input text is required"),
	)

	It("rejects a nil embedding request", func() {
		_, err := fake.Embed(ctx, nil)
		Expect(err).To(MatchError(llmclient.ErrInvalidRequest))
		Expect(err.Error()).To(ContainSubstring("embedding request must not be nil"))
	})

	It("does not fill in the default model", func() {
		fake.CompleteFunc = func(_ context.Context, r *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			Expect(r.Model).To(BeEmpty())
			return &llmclient.CompletionResponse{}, nil
		}
		_, err := fake.Complete(ctx, completion(""))
		Expect(err).NotTo(HaveOccurred())
	})

	It("deep-copies reference fields, so later mutation of the request or a result leaves the record intact", func() {
		fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return &llmclient.CompletionResponse{}, nil
		}
		temp := 0.5
		topP := 0.8
		req := completion("m")
		req.Temperature = &temp
		req.TopP = &topP
		req.Stop = []string{"END"}
		_, err := fake.Complete(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		temp, topP = 0.9, 0.9
		req.Messages[0].Content = "mutated"
		req.Stop[0] = "mutated"
		got := fake.CompleteRequests()
		*got[0].Temperature = 0.1
		*got[0].TopP = 0.1
		got[0].Messages[0].Content = "mutated-result"
		got[0].Stop[0] = "mutated-result"

		again := fake.CompleteRequests()[0]
		Expect(*again.Temperature).To(Equal(0.5))
		Expect(*again.TopP).To(Equal(0.8))
		Expect(again.Stop).To(Equal([]string{"END"}))
		Expect(again.Messages[0].Content).To(Equal("hi"))
	})

	It("records an unscripted Complete even though it fails", func() {
		_, _ = fake.Complete(ctx, completion("m"))
		Expect(fake.CompleteRequests()).To(HaveLen(1))
	})

	It("lets a scripted Func decide the result and still records Complete", func() {
		want := &llmclient.CompletionResponse{ID: "r1"}
		fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return want, errScripted
		}
		fake.EmbedFunc = func(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
			return nil, errScripted
		}

		resp, err := fake.Complete(ctx, completion("m"))
		Expect(err).To(MatchError(errScripted))
		Expect(resp).To(BeIdenticalTo(want))
		_, err = fake.Embed(ctx, embedding())
		Expect(err).To(MatchError(errScripted))
		Expect(err).NotTo(MatchError(llmclienttest.ErrUnscripted))
		Expect(fake.CompleteRequests()).To(HaveLen(1))
	})

	It("passes the context and request to the Func", func() {
		type key struct{}
		ctx = context.WithValue(ctx, key{}, "v")
		req := completion("m")
		fake.CompleteFunc = func(c context.Context, r *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			Expect(c.Value(key{})).To(Equal("v"))
			Expect(r).To(BeIdenticalTo(req))
			return &llmclient.CompletionResponse{}, nil
		}
		_, err := fake.Complete(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	})

	It("records Complete requests in call order", func() {
		fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return &llmclient.CompletionResponse{}, nil
		}
		for _, m := range []string{"a", "b", "c"} {
			_, err := fake.Complete(ctx, completion(m))
			Expect(err).NotTo(HaveOccurred())
		}
		var got []string
		for _, r := range fake.CompleteRequests() {
			got = append(got, r.Model)
		}
		Expect(got).To(Equal([]string{"a", "b", "c"}))
	})

	It("reports no requests before any call", func() {
		Expect(fake.CompleteRequests()).To(BeEmpty())
	})

	It("returns copies, so mutating a result or the original request leaves the record intact", func() {
		fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return &llmclient.CompletionResponse{}, nil
		}
		req := completion("orig")
		_, err := fake.Complete(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		req.Model = "mutated-after-call"
		got := fake.CompleteRequests()
		got[0].Model = "mutated-result"
		_ = append(got, nil)

		again := fake.CompleteRequests()
		Expect(again).To(HaveLen(1))
		Expect(again[0].Model).To(Equal("orig"))
	})

	It("returns HealthErr from Health, nil by default, and nil from Close", func() {
		Expect(fake.Health(ctx)).To(Succeed())
		fake.HealthErr = errScripted
		Expect(fake.Health(ctx)).To(MatchError(errScripted))
		Expect(fake.Close()).To(Succeed())
	})

	It("is safe for concurrent use and records every call", func() {
		fake.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return &llmclient.CompletionResponse{}, nil
		}
		const n = 50
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				_, err := fake.Complete(ctx, completion(""))
				Expect(err).NotTo(HaveOccurred())
				_ = fake.CompleteRequests()
			}()
		}
		wg.Wait()
		Expect(fake.CompleteRequests()).To(HaveLen(n))
	})
})
