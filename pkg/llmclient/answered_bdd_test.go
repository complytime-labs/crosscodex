package llmclient_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("CheckAnswered", func() {
	req := &llmclient.CompletionRequest{Model: "default", MaxTokens: 20}

	response := func(completionTokens int, choices ...llmclient.CompletionChoice) *llmclient.CompletionResponse {
		return &llmclient.CompletionResponse{Choices: choices, Usage: llmclient.TokenUsage{CompletionTokens: completionTokens}}
	}
	choice := func(content, finish string) llmclient.CompletionChoice {
		return llmclient.CompletionChoice{Message: llmclient.ChatMessage{Role: "assistant", Content: content}, FinishReason: finish}
	}

	DescribeTable("accepts a response that is not an empty answer cut off at max_tokens",
		func(resp *llmclient.CompletionResponse) {
			Expect(llmclient.CheckAnswered(req, resp)).To(Succeed())
		},
		Entry("a complete answer", response(4, choice("REQUIREMENT", "stop"))),
		Entry("a partial answer cut off at max_tokens", response(20, choice("Technical|Tac", "length"))),
		Entry("an empty answer the model ended itself", response(1, choice("", "stop"))),
		Entry("no choices", response(0)),
		Entry("one answered choice among cut-off empty ones", response(40, choice("", "length"), choice("REQUIREMENT", "stop"))),
	)

	DescribeTable("refuses an empty answer cut off at max_tokens",
		func(resp *llmclient.CompletionResponse) {
			err := llmclient.CheckAnswered(req, resp)
			Expect(err).To(MatchError(llmclient.ErrOutputTruncated))
			Expect(err.Error()).To(ContainSubstring(`model "default"`))
			Expect(err.Error()).To(ContainSubstring("max_tokens 20"))
			Expect(err.Error()).To(ContainSubstring("20 completion tokens"))
			Expect(err.Error()).To(ContainSubstring("turn the model's reasoning off in the LLM gateway or raise max_tokens"))
		},
		Entry("no content", response(20, choice("", "length"))),
		Entry("whitespace only", response(20, choice(" \n\t", "length"))),
		Entry("every choice empty", response(20, choice("", "length"), choice("", "stop"))),
	)
})
