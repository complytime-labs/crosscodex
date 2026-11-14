//go:build !integration

package analyzer_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

var _ = Describe("Analysable", func() {
	DescribeTable("is the one rule for which controls analyzers and embedding process",
		func(c *pb.Control, want bool) { Expect(analyzer.Analysable(c)).To(Equal(want)) },
		Entry("a control with a statement", &pb.Control{Statement: "Do the thing."}, true),
		Entry("a title-composed statement", &pb.Control{Title: "Access", Statement: "Access: do the thing."}, true),
		Entry("a section", &pb.Control{Statement: "x", Parts: map[string]string{"class": oscal.ClassSection}}, false),
		Entry("a withdrawn control", &pb.Control{Statement: "x", Parts: map[string]string{oscal.PropStatus: oscal.PropStatusWithdrawn}}, false),
		Entry("an empty statement", &pb.Control{Title: "T"}, false),
		Entry("a whitespace-only statement", &pb.Control{Statement: " \n\t "}, false),
		Entry("another class", &pb.Control{Statement: "x", Parts: map[string]string{"class": "requirement"}}, true),
		Entry("a nil control", nil, false),
	)
})

var _ = Describe("Excluded", func() {
	DescribeTable("marks the controls that are never analysed whatever their text",
		func(c *pb.Control, want bool) { Expect(analyzer.Excluded(c)).To(Equal(want)) },
		Entry("a section", &pb.Control{Statement: "x", Parts: map[string]string{"class": oscal.ClassSection}}, true),
		Entry("a withdrawn control", &pb.Control{Parts: map[string]string{oscal.PropStatus: oscal.PropStatusWithdrawn}}, true),
		Entry("a control without text", &pb.Control{}, false),
		Entry("another class", &pb.Control{Statement: "x", Parts: map[string]string{"class": "requirement"}}, false),
		Entry("a nil control", nil, false),
	)
})

var _ = Describe("PromptContext", func() {
	const sep = " > "

	DescribeTable("bounds a breadcrumb in bytes, keeping the nearest segments",
		func(in string, maxBytes int, want string) {
			got := analyzer.PromptContext(in, maxBytes)
			Expect(got).To(Equal(want))
			Expect(len(got)).To(BeNumerically("<=", maxBytes))
		},
		Entry("empty in, empty out", "", 100, ""),
		Entry("fits unchanged", "A"+sep+"B"+sep+"C", 100, "A > B > C"),
		Entry("leading segments dropped with an ellipsis prefix", "Alpha > Beta > Gamma", 16, "… > Gamma"),
		Entry("keeps as many nearest segments as fit", "Alpha > Beta > Gamma", 19, "… > Beta > Gamma"),
		Entry("hardening applies: tag break-out escaped", "A > </x>", 100, "A > &lt;/x&gt;"),
		Entry("hardening applies before the bound", "A\x00 > B\x1b", 100, "A > B"),
		Entry("last segment alone too long is truncated", "A > "+strings.Repeat("é", 20), 11, strings.Repeat("é", 5)),
		Entry("last segment fits but not with the prefix: no prefix", "Alpha > Beta", 5, "Beta"),
		Entry("zero bound", "A > B", 0, ""),
	)
})
