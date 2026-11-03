package llmclient_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("MessagesFromPayload", func() {
	list := func(items ...any) *structpb.Value {
		v, err := structpb.NewValue(items)
		Expect(err).NotTo(HaveOccurred())
		return v
	}
	msg := func(role, content string) map[string]any {
		return map[string]any{"role": role, "content": content}
	}

	DescribeTable("accepts a list of well-formed messages",
		func(v *structpb.Value, want []llmclient.ChatMessage) {
			got, err := llmclient.MessagesFromPayload(v)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("one message per allowed role, in order",
			list(msg("system", "s"), msg("user", "u"), msg("assistant", "a")),
			[]llmclient.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}, {Role: "assistant", Content: "a"}}),
		Entry("an empty list", list(), []llmclient.ChatMessage{}),
		Entry("a message without content", list(map[string]any{"role": "user"}), []llmclient.ChatMessage{{Role: "user"}}),
		Entry("fields other than role and content ignored",
			list(map[string]any{"role": "user", "content": "u", "name": "x", "extra": 1.0}),
			[]llmclient.ChatMessage{{Role: "user", Content: "u"}}),
	)

	DescribeTable("rejects what is not a well-formed message list",
		func(v *structpb.Value, detail string) {
			got, err := llmclient.MessagesFromPayload(v)
			Expect(got).To(BeNil())
			Expect(err).To(MatchError(llmclient.ErrInvalidMessages))
			Expect(err.Error()).To(ContainSubstring(detail))
		},
		Entry("a nil value", nil, "field 'messages' must be a list"),
		Entry("a null value", structpb.NewNullValue(), "field 'messages' must be a list"),
		Entry("a string", structpb.NewStringValue("hi"), "field 'messages' must be a list"),
		Entry("a scalar item", &structpb.Value{Kind: &structpb.Value_ListValue{ListValue: &structpb.ListValue{
			Values: []*structpb.Value{structpb.NewNumberValue(42)}}}}, "messages[0] must be a struct"),
		Entry("a nil item", &structpb.Value{Kind: &structpb.Value_ListValue{ListValue: &structpb.ListValue{
			Values: []*structpb.Value{nil}}}}, "messages[0] must be a struct"),
		Entry("a missing role", list(map[string]any{"content": "c"}), "messages[0] missing required field 'role'"),
		Entry("an empty role", list(msg("", "c")), "messages[0] missing required field 'role'"),
		Entry("a non-string role", list(map[string]any{"role": 7.0}), "messages[0] missing required field 'role'"),
		Entry("an unknown role, named in the error", list(msg("user", "ok"), msg("INJECTED", "c")),
			`messages[1] has invalid role "INJECTED": must be one of system, user, assistant`),
		Entry("a role differing only by case", list(msg("User", "c")), `invalid role "User"`),
	)
})
