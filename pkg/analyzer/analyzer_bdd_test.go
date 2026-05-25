package analyzer_test

import (
	"errors"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

func TestAnalyzerBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Analyzer BDD Suite")
}

var _ = Describe("Analyzer Error Handling", func() {

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" - what business behaviors the error sentinels support
	// =================================================================

	Describe("Sentinel Error Behaviors", func() {
		Context("when reporting analyzer failures to callers", func() {
			It("provides distinct error identities for each failure mode", func() {
				sentinels := []error{
					analyzer.ErrNotFound,
					analyzer.ErrAlreadyRegistered,
					analyzer.ErrAnalysisFailed,
					analyzer.ErrInvalidArtifact,
				}

				By("ensuring every sentinel is non-nil")
				for _, s := range sentinels {
					Expect(s).NotTo(BeNil())
				}

				By("ensuring no two sentinels are mistakenly equal")
				for i := 0; i < len(sentinels); i++ {
					for j := i + 1; j < len(sentinels); j++ {
						Expect(errors.Is(sentinels[i], sentinels[j])).To(BeFalse(),
							"sentinel %d and %d should be distinct", i, j)
					}
				}
			})

			It("supports error wrapping for contextual diagnostics", func() {
				By("wrapping ErrNotFound and recovering it via errors.Is")
				wrapped := fmt.Errorf("plugin lookup failed: %w", analyzer.ErrNotFound)
				Expect(errors.Is(wrapped, analyzer.ErrNotFound)).To(BeTrue())

				By("wrapping ErrAlreadyRegistered and recovering it via errors.Is")
				wrapped = fmt.Errorf("registration rejected: %w", analyzer.ErrAlreadyRegistered)
				Expect(errors.Is(wrapped, analyzer.ErrAlreadyRegistered)).To(BeTrue())

				By("wrapping ErrAnalysisFailed and recovering it via errors.Is")
				wrapped = fmt.Errorf("pipeline step failed: %w", analyzer.ErrAnalysisFailed)
				Expect(errors.Is(wrapped, analyzer.ErrAnalysisFailed)).To(BeTrue())

				By("wrapping ErrInvalidArtifact and recovering it via errors.Is")
				wrapped = fmt.Errorf("input validation: %w", analyzer.ErrInvalidArtifact)
				Expect(errors.Is(wrapped, analyzer.ErrInvalidArtifact)).To(BeTrue())
			})
		})
	})

	// =================================================================
	// LEVEL 2: TECHNICAL EDGE CASES
	// These specs cover individual sentinel properties from the original test file
	// =================================================================

	Describe("Sentinel Error Properties", func() {
		Context("when verifying each sentinel is defined", func() {
			It("defines ErrNotFound", func() {
				Expect(analyzer.ErrNotFound).NotTo(BeNil())
				Expect(analyzer.ErrNotFound.Error()).To(ContainSubstring("not found"))
			})

			It("defines ErrAlreadyRegistered", func() {
				Expect(analyzer.ErrAlreadyRegistered).NotTo(BeNil())
				Expect(analyzer.ErrAlreadyRegistered.Error()).To(ContainSubstring("already registered"))
			})

			It("defines ErrAnalysisFailed", func() {
				Expect(analyzer.ErrAnalysisFailed).NotTo(BeNil())
				Expect(analyzer.ErrAnalysisFailed.Error()).To(ContainSubstring("failed"))
			})

			It("defines ErrInvalidArtifact", func() {
				Expect(analyzer.ErrInvalidArtifact).NotTo(BeNil())
				Expect(analyzer.ErrInvalidArtifact.Error()).To(ContainSubstring("invalid"))
			})
		})

		Context("when wrapping sentinels in nested error chains", func() {
			It("recovers ErrNotFound through multiple wrap layers", func() {
				inner := fmt.Errorf("db miss: %w", analyzer.ErrNotFound)
				outer := fmt.Errorf("service call: %w", inner)
				Expect(errors.Is(outer, analyzer.ErrNotFound)).To(BeTrue())
			})

			It("does not confuse wrapped errors with unrelated sentinels", func() {
				wrapped := fmt.Errorf("operation failed: %w", analyzer.ErrNotFound)
				Expect(errors.Is(wrapped, analyzer.ErrAlreadyRegistered)).To(BeFalse())
				Expect(errors.Is(wrapped, analyzer.ErrAnalysisFailed)).To(BeFalse())
				Expect(errors.Is(wrapped, analyzer.ErrInvalidArtifact)).To(BeFalse())
			})
		})
	})
})
