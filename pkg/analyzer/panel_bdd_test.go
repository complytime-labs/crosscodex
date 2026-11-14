package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
)

type panelStub struct {
	analyzertest.Stub[*emptypb.Empty]
	panel analyzer.Panel
}

func (s *panelStub) Panel() analyzer.Panel { return s.panel }

var _ = Describe("AsPanelled", func() {
	It("finds the panel of a registered analyzer that implements Panelled", func() {
		stub := &panelStub{panel: analyzer.Panel{Aliases: []string{"panel-1", "panel-2"}, AllowShared: true}}
		stub.AnalyzerName = "relationship"
		reg := analyzer.NewRegistry()
		Expect(analyzer.Register[*emptypb.Empty](reg, stub)).To(Succeed())
		ra, err := reg.Get("relationship")
		Expect(err).NotTo(HaveOccurred())

		p, ok := analyzer.AsPanelled(ra)
		Expect(ok).To(BeTrue())
		Expect(p.Panel()).To(Equal(stub.panel))
	})

	It("reports false for a registered analyzer that is not a panel", func() {
		reg := analyzer.NewRegistry()
		Expect(analyzer.Register[*emptypb.Empty](reg, &analyzertest.Stub[*emptypb.Empty]{AnalyzerName: "classify"})).To(Succeed())
		ra, err := reg.Get("classify")
		Expect(err).NotTo(HaveOccurred())
		_, ok := analyzer.AsPanelled(ra)
		Expect(ok).To(BeFalse())
	})
})
