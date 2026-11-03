package analyzer_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

var _ = Describe("AsCacheable", func() {
	It("finds Cacheable on a registered analyzer that implements it", func() {
		stub := &analyzertest.CacheableStub[*emptypb.Empty]{}
		stub.AnalyzerName = "classify"
		stub.Models = []analyzer.ModelUse{{Alias: "default", Kind: analyzer.ModelCompletion}}
		want := analyzer.CacheRequest{
			Identity:        resultcache.Identity{TaskType: "classify", ContentHash: strings.Repeat("a", 64)},
			SourceControlID: "ac-1", TargetControlID: "ac-1",
		}
		stub.CacheIdentityFunc = func(analyzer.Task) (analyzer.CacheRequest, bool) { return want, true }
		reg := analyzer.NewRegistry()
		Expect(analyzer.Register[*emptypb.Empty](reg, stub)).To(Succeed())
		ra, err := reg.Get("classify")
		Expect(err).NotTo(HaveOccurred())

		c, ok := analyzer.AsCacheable(ra)
		Expect(ok).To(BeTrue())
		Expect(c.CacheModels()).To(Equal(stub.Models))
		got, ok := c.CacheIdentity(analyzer.Task{TaskID: "classify-ac-1"})
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal(want))
	})

	It("reports false for a registered analyzer that does not opt in", func() {
		reg := analyzer.NewRegistry()
		Expect(analyzer.Register[*emptypb.Empty](reg, &analyzertest.Stub[*emptypb.Empty]{AnalyzerName: "classify"})).To(Succeed())
		ra, err := reg.Get("classify")
		Expect(err).NotTo(HaveOccurred())
		_, ok := analyzer.AsCacheable(ra)
		Expect(ok).To(BeFalse())
	})
})
