package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("MessagesForPayload", func() {
	It("converts chat messages to structpb-compatible format", func() {
		messages := []llmclient.ChatMessage{
			{Role: llmclient.RoleSystem, Content: "You are a classifier."},
			{Role: llmclient.RoleUser, Content: "Classify this text."},
		}

		result := intanalyzer.MessagesForPayload(messages)

		Expect(result).To(HaveLen(2))
		msg0, ok := result[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(msg0["role"]).To(Equal("system"))
		Expect(msg0["content"]).To(Equal("You are a classifier."))

		msg1, ok := result[1].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(msg1["role"]).To(Equal("user"))
		Expect(msg1["content"]).To(Equal("Classify this text."))
	})

	It("returns empty slice for nil input", func() {
		result := intanalyzer.MessagesForPayload(nil)
		Expect(result).To(And(BeEmpty(), Not(BeNil())))
	})

	It("returns empty slice for empty input", func() {
		result := intanalyzer.MessagesForPayload([]llmclient.ChatMessage{})
		Expect(result).To(And(BeEmpty(), Not(BeNil())))
	})
})
