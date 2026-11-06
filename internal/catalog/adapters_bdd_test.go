package catalog_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

var _ = Describe("LLMCompleter", func() {
	reply := func(content, finish string) func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
		return func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return &llmclient.CompletionResponse{Model: req.Model, Choices: []llmclient.CompletionChoice{
				{Message: llmclient.ChatMessage{Role: "assistant", Content: content}, FinishReason: finish},
			}}, nil
		}
	}
	messages := []oscal.Message{{Role: "user", Content: "structure this"}}

	It("returns the model's answer", func() {
		fake := &llmclienttest.Fake{CompleteFunc: reply(`{"controls": []}`, "stop")}
		out, err := catalog.NewLLMCompleter(fake, "default", "tenant-a").Complete(testspecs.SetupTenantContext("tenant-a"), messages)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(`{"controls": []}`))
	})

	It("refuses an empty answer cut off at the token limit instead of returning it", func() {
		fake := &llmclienttest.Fake{CompleteFunc: reply("", "length")}
		out, err := catalog.NewLLMCompleter(fake, "default", "tenant-a").Complete(testspecs.SetupTenantContext("tenant-a"), messages)
		Expect(err).To(MatchError(llmclient.ErrOutputTruncated))
		Expect(out).To(BeEmpty())
	})
})
