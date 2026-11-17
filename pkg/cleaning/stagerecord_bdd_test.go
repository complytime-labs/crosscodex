//go:build !integration

package cleaning_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
)

var _ = Describe("StageRecord staleness", func() {
	current := func(version string, models map[string]string) cleaning.StageRecord {
		return cleaning.StageRecord{PromptName: "p", PromptVersion: version, Models: models}
	}
	recorded := func(version string, models map[string]string) *cleaning.StageRecord {
		r := current(version, models)
		return &r
	}

	DescribeTable("PromptStale",
		func(rec *cleaning.StageRecord, cur cleaning.StageRecord, want bool) {
			Expect(cleaning.PromptStale(rec, cur)).To(Equal(want))
		},
		Entry("nil recorded is never stale", nil, current("2", nil), false),
		Entry("same version is fresh", recorded("1", nil), current("1", nil), false),
		Entry("different version is stale", recorded("1", nil), current("2", nil), true),
		Entry("empty recorded version is unknown, not stale", recorded("", nil), current("3", nil), false),
		Entry("empty current version is unknown, not stale", recorded("3", nil), current("", nil), false),
	)

	DescribeTable("ModelStale",
		func(rec *cleaning.StageRecord, cur cleaning.StageRecord, want bool) {
			Expect(cleaning.ModelStale(rec, cur)).To(Equal(want))
		},
		Entry("nil recorded is never stale", nil, current("1", map[string]string{"a": "r1"}), false),
		Entry("identical models are fresh",
			recorded("1", map[string]string{"a": "r1"}), current("1", map[string]string{"a": "r1"}), false),
		Entry("alias added to the panel is stale",
			recorded("1", map[string]string{"a": ""}), current("1", map[string]string{"a": "", "b": ""}), true),
		Entry("alias removed from the panel is stale",
			recorded("1", map[string]string{"a": "", "b": ""}), current("1", map[string]string{"a": ""}), true),
		Entry("changed revision is stale",
			recorded("1", map[string]string{"a": "r1"}), current("1", map[string]string{"a": "r2"}), true),
		Entry("revision newly set is not stale",
			recorded("1", map[string]string{"a": ""}), current("1", map[string]string{"a": "r2"}), false),
		Entry("revision newly unset is not stale",
			recorded("1", map[string]string{"a": "r1"}), current("1", map[string]string{"a": ""}), false),
		Entry("prompt version change alone is not model-stale",
			recorded("1", map[string]string{"a": "r1"}), current("2", map[string]string{"a": "r1"}), false),
	)
})

var _ = Describe("StageRecord encoding", func() {
	It("round-trips through Marshal and ParseStageRecord", func() {
		in := cleaning.StageRecord{PromptName: "p", PromptVersion: "1", Models: map[string]string{"b": "r2", "a": ""}}
		s, err := in.Marshal()
		Expect(err).NotTo(HaveOccurred())
		out, err := cleaning.ParseStageRecord(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(*out).To(Equal(in))
	})

	It("marshals with sorted keys", func() {
		in := cleaning.StageRecord{PromptName: "p", PromptVersion: "1", Models: map[string]string{"b": "r2", "a": "r1"}}
		s, err := in.Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(s).To(Equal(`{"prompt_name":"p","prompt_version":"1","models":{"a":"r1","b":"r2"}}`))
	})

	It("treats the empty string as unknown", func() {
		out, err := cleaning.ParseStageRecord("")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(BeNil())
	})

	long := strings.Repeat("x", 257)
	DescribeTable("rejects invalid input",
		func(in string) {
			out, err := cleaning.ParseStageRecord(in)
			Expect(err).To(MatchError(cleaning.ErrInvalidStageRecord))
			Expect(out).To(BeNil())
		},
		Entry("unknown field", `{"x":1}`),
		Entry("truncated JSON", `{"prompt_name":"p"`),
		Entry("trailing data", `{"prompt_name":"p"} {}`),
		Entry("not an object", `[1]`),
		Entry("over 64 KiB", `{"prompt_name":"`+strings.Repeat("x", 64*1024)+`"}`),
		Entry("alias over 256 bytes", `{"models":{"`+long+`":"r"}}`),
		Entry("revision over 256 bytes", `{"models":{"a":"`+long+`"}}`),
	)

	It("accepts alias and revision of exactly 256 bytes", func() {
		edge := strings.Repeat("x", 256)
		out, err := cleaning.ParseStageRecord(`{"models":{"` + edge + `":"` + edge + `"}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Models).To(HaveKeyWithValue(edge, edge))
	})

	It("refuses to marshal an oversized alias so stored records stay parseable", func() {
		in := cleaning.StageRecord{Models: map[string]string{strings.Repeat("x", 257): ""}}
		_, err := in.Marshal()
		Expect(err).To(MatchError(cleaning.ErrInvalidStageRecord))
	})
})
