package artifacts_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("ArtifactType", func() {
	DescribeTable("String returns canonical SCREAMING_SNAKE_CASE",
		func(at artifacts.ArtifactType, expected string) {
			Expect(at.String()).To(Equal(expected))
		},
		Entry("POLICY", artifacts.ArtifactPolicy, "POLICY"),
		Entry("PROCEDURE", artifacts.ArtifactProcedure, "PROCEDURE"),
		Entry("PLAN", artifacts.ArtifactPlan, "PLAN"),
		Entry("REPORT", artifacts.ArtifactReport, "REPORT"),
		Entry("RECORD", artifacts.ArtifactRecord, "RECORD"),
		Entry("CONFIGURATION", artifacts.ArtifactConfiguration, "CONFIGURATION"),
		Entry("MECHANISM", artifacts.ArtifactMechanism, "MECHANISM"),
		Entry("ROLE", artifacts.ArtifactRole, "ROLE"),
		Entry("PROCESS", artifacts.ArtifactProcess, "PROCESS"),
	)

	It("Valid returns true for all defined types", func() {
		for _, at := range artifacts.AllArtifactTypes() {
			Expect(at.Valid()).To(BeTrue(), "expected %s to be valid", at)
		}
	})

	It("Valid returns false for out-of-range", func() {
		Expect(artifacts.ArtifactType(-1).Valid()).To(BeFalse())
		Expect(artifacts.ArtifactType(99).Valid()).To(BeFalse())
	})

	DescribeTable("ParseArtifactType round-trips",
		func(name string, expected artifacts.ArtifactType) {
			at, ok := artifacts.ParseArtifactType(name)
			Expect(ok).To(BeTrue())
			Expect(at).To(Equal(expected))
		},
		Entry("POLICY", "POLICY", artifacts.ArtifactPolicy),
		Entry("PROCEDURE", "PROCEDURE", artifacts.ArtifactProcedure),
		Entry("CONFIGURATION", "CONFIGURATION", artifacts.ArtifactConfiguration),
		Entry("ROLE", "ROLE", artifacts.ArtifactRole),
		Entry("PROCESS", "PROCESS", artifacts.ArtifactProcess),
	)

	It("ParseArtifactType returns false for invalid", func() {
		_, ok := artifacts.ParseArtifactType("INVALID")
		Expect(ok).To(BeFalse())
	})

	It("AllArtifactTypes returns exactly 9 types", func() {
		Expect(artifacts.AllArtifactTypes()).To(HaveLen(9))
	})
})

var _ = Describe("ParseStatus", func() {
	It("String returns canonical names", func() {
		Expect(artifacts.ParseOK.String()).To(Equal("OK"))
		Expect(artifacts.ParseFail.String()).To(Equal("PARSE_FAIL"))
		Expect(artifacts.ParseNone.String()).To(Equal("ARTIFACTS_NONE"))
	})
})
