package relationship_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
)

func TestRelationshipBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Relationship Analyzer BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

var _ = Describe("ParseResponse", func() {
	DescribeTable("extracts fields from valid LLM responses",
		func(raw string, expectedRel relationship.RelationshipType, expectedContrib relationship.ContributionType, expectedConf relationship.ConfidenceLevel, expectedStatus relationship.ParseStatus) {
			vote := relationship.ParseResponse(raw)
			Expect(vote.Relationship).To(Equal(expectedRel), "relationship mismatch")
			Expect(vote.ContributionType).To(Equal(expectedContrib), "contribution_type mismatch")
			Expect(vote.Confidence).To(Equal(expectedConf), "confidence mismatch")
			Expect(vote.ParseStatus).To(Equal(expectedStatus), "parse_status mismatch")
			Expect(vote.RawResponse).To(Equal(raw), "raw_response should be preserved")
		},
		// Python parity: test_parse_response_valid
		Entry("valid SUPERSET_OF",
			"RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: SOX mandates audit oversight which includes IT controls.\nCONFIDENCE: HIGH",
			relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh, relationship.ParseOK,
		),
		// Python parity: test_parse_response_contribution_type_integral
		Entry("CONTRIBUTES_TO with INTEGRAL_TO",
			"RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nJUSTIFICATION: Monitoring requires audit records to exist.\nCONFIDENCE: HIGH",
			relationship.RelContributesTo, relationship.ContribIntegralTo, relationship.ConfidenceHigh, relationship.ParseOK,
		),
		// Python parity: test_parse_response_contribution_type_example
		Entry("CONTRIBUTES_TO with EXAMPLE_OF",
			"RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: EXAMPLE_OF\nJUSTIFICATION: Access controls are one way to support data integrity.\nCONFIDENCE: MEDIUM",
			relationship.RelContributesTo, relationship.ContribExampleOf, relationship.ConfidenceMedium, relationship.ParseOK,
		),
		// Python parity: test_parse_response_contribution_type_missing
		Entry("CONTRIBUTES_TO without CONTRIBUTION_TYPE",
			"RELATIONSHIP: CONTRIBUTES_TO\nJUSTIFICATION: Partial overlap.\nCONFIDENCE: MEDIUM",
			relationship.RelContributesTo, relationship.ContribNone, relationship.ConfidenceMedium, relationship.ParseOK,
		),
		// Python parity: test_parse_response_missing_confidence
		Entry("missing CONFIDENCE defaults to LOW",
			"RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: Same scope.",
			relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceLow, relationship.ParseOK,
		),
		// Python parity: test_parse_response_garbage
		Entry("garbage input",
			"I cannot determine this relationship.",
			relationship.RelNoRelationship, relationship.ContribNone, relationship.ConfidenceLow, relationship.ParseFail,
		),
		// Python parity: test_parse_response_invalid_relationship
		Entry("invalid relationship name",
			"RELATIONSHIP: MAYBE\nJUSTIFICATION: unclear\nCONFIDENCE: LOW",
			relationship.RelNoRelationship, relationship.ContribNone, relationship.ConfidenceLow, relationship.ParseFail,
		),
		// Python parity: test_parse_response_conflicts_with
		Entry("CONFLICTS_WITH",
			"RELATIONSHIP: CONFLICTS_WITH\nJUSTIFICATION: Source prohibits expiry while target mandates rotation.\nCONFIDENCE: HIGH",
			relationship.RelConflictsWith, relationship.ContribNone, relationship.ConfidenceHigh, relationship.ParseOK,
		),
		// Python parity: test_parse_response_complements
		Entry("COMPLEMENTS",
			"RELATIONSHIP: COMPLEMENTS\nJUSTIFICATION: Physical and logical access controls address the same risk through different mechanisms.\nCONFIDENCE: HIGH",
			relationship.RelComplements, relationship.ContribNone, relationship.ConfidenceHigh, relationship.ParseOK,
		),
		Entry("empty input",
			"",
			relationship.RelNoRelationship, relationship.ContribNone, relationship.ConfidenceLow, relationship.ParseFail,
		),
		// Additional: each of the 8 relationship types parsed individually
		Entry("EQUIVALENT", "RELATIONSHIP: EQUIVALENT\nCONFIDENCE: HIGH",
			relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh, relationship.ParseOK),
		Entry("SUBSET_OF", "RELATIONSHIP: SUBSET_OF\nCONFIDENCE: MEDIUM",
			relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceMedium, relationship.ParseOK),
		Entry("PARTIAL", "RELATIONSHIP: PARTIAL\nCONFIDENCE: LOW",
			relationship.RelPartial, relationship.ContribNone, relationship.ConfidenceLow, relationship.ParseOK),
		Entry("NO_RELATIONSHIP", "RELATIONSHIP: NO_RELATIONSHIP\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, relationship.ContribNone, relationship.ConfidenceHigh, relationship.ParseOK),
		// Case insensitivity
		Entry("case insensitive relationship",
			"relationship: superset_of\nconfidence: high",
			relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh, relationship.ParseOK),
	)

	It("extracts justification text", func() {
		raw := "RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: Both controls address the same access policy.\nCONFIDENCE: HIGH"
		vote := relationship.ParseResponse(raw)
		Expect(vote.Justification).To(Equal("Both controls address the same access policy."))
	})
})
