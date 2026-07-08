//go:build !integration

package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer"
)

var _ = Describe("NormalizeArtifactName", func() {
	// Python parity: test_strips_articles
	It("strips leading articles", func() {
		Expect(analyzer.NormalizeArtifactName("The Access Policy")).To(Equal("access policy"))
		Expect(analyzer.NormalizeArtifactName("A configuration")).To(Equal("configuration"))
		Expect(analyzer.NormalizeArtifactName("An audit report")).To(Equal("audit report"))
	})

	// Python parity: test_strips_trailing_period
	It("strips trailing periods", func() {
		Expect(analyzer.NormalizeArtifactName("Access Policy.")).To(Equal("access policy"))
	})

	// Python parity: test_collapses_whitespace
	It("collapses whitespace", func() {
		Expect(analyzer.NormalizeArtifactName("access   control   policy")).To(Equal("access control policy"))
	})

	// Python parity: test_case_insensitive
	It("lowercases", func() {
		Expect(analyzer.NormalizeArtifactName("ACCESS POLICY")).To(Equal("access policy"))
	})

	It("handles empty string", func() {
		Expect(analyzer.NormalizeArtifactName("")).To(Equal(""))
	})

	It("handles string that is only an article", func() {
		Expect(analyzer.NormalizeArtifactName("the")).To(Equal(""))
	})
})

var _ = Describe("ArtifactNamesMatch", func() {
	// Python parity: test_identical_names_match
	It("matches identical names", func() {
		Expect(analyzer.ArtifactNamesMatch("access policy", "access policy", analyzer.DefaultFuzzyThreshold)).To(BeTrue())
	})

	// Python parity: test_overlapping_names_match
	It("matches overlapping names above threshold", func() {
		// 2/3 tokens overlap = 66% >= 60%
		Expect(analyzer.ArtifactNamesMatch("access control policy", "access control plan", analyzer.DefaultFuzzyThreshold)).To(BeTrue())
	})

	// Python parity: test_disjoint_names_no_match
	It("rejects disjoint names", func() {
		Expect(analyzer.ArtifactNamesMatch("access control", "incident response", analyzer.DefaultFuzzyThreshold)).To(BeFalse())
	})

	// Python parity: test_empty_names_no_match
	It("rejects empty names", func() {
		Expect(analyzer.ArtifactNamesMatch("", "something", analyzer.DefaultFuzzyThreshold)).To(BeFalse())
		Expect(analyzer.ArtifactNamesMatch("something", "", analyzer.DefaultFuzzyThreshold)).To(BeFalse())
	})

	It("respects custom threshold", func() {
		// 1/2 tokens overlap = 50% — below 0.6 but above 0.4
		Expect(analyzer.ArtifactNamesMatch("access control", "access review", 0.4)).To(BeTrue())
		Expect(analyzer.ArtifactNamesMatch("access control", "access review", 0.6)).To(BeFalse())
	})
})
