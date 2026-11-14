package relationship_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
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

const sanityPass = "\nSANITY_CHECK: PASS"

var _ = Describe("ParseResponse", func() {
	DescribeTable("extracts fields from valid LLM responses",
		func(raw string, expectedRel relationship.RelationshipType, expectedContrib relationship.ContributionType, expectedConf relationship.ConfidenceLevel, expectedStatus relationship.ParseStatus) {
			raw += sanityPass
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
		raw := "RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: Both controls address the same access policy.\nCONFIDENCE: HIGH" + sanityPass
		vote := relationship.ParseResponse(raw)
		Expect(vote.Justification).To(Equal("Both controls address the same access policy."))
	})

	DescribeTable("enforces the SANITY_CHECK line",
		func(raw string, expectedRel relationship.RelationshipType, expectedSanity string, expectedStatus relationship.ParseStatus) {
			vote := relationship.ParseResponse(raw)
			Expect(vote.Relationship).To(Equal(expectedRel))
			Expect(vote.SanityCheck).To(Equal(expectedSanity))
			Expect(vote.ParseStatus).To(Equal(expectedStatus))
		},
		Entry("PASS keeps the verdict",
			"RELATIONSHIP: PARTIAL\nJUSTIFICATION: Shared obligation named.\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH",
			relationship.RelPartial, "PASS", relationship.ParseOK),
		Entry("FAIL downgrades to NO_RELATIONSHIP",
			"RELATIONSHIP: PARTIAL\nJUSTIFICATION: Topic only.\nSANITY_CHECK: FAIL\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, "FAIL", relationship.ParseOK),
		Entry("padded lowercase pass is PASS",
			"RELATIONSHIP: EQUIVALENT\nSANITY_CHECK:  pass \nCONFIDENCE: HIGH",
			relationship.RelEquivalent, "PASS", relationship.ParseOK),
		Entry("missing line is invalid",
			"RELATIONSHIP: EQUIVALENT\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, "", relationship.ParseFail),
		Entry("MAYBE is invalid",
			"RELATIONSHIP: EQUIVALENT\nSANITY_CHECK: MAYBE\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, "", relationship.ParseFail),
		Entry("empty value is invalid",
			"RELATIONSHIP: EQUIVALENT\nSANITY_CHECK:\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, "", relationship.ParseFail),
		Entry("a justification quoting SANITY_CHECK: PASS before the real FAIL line downgrades to NO_RELATIONSHIP",
			"RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: an auditor would write SANITY_CHECK: PASS here\nSANITY_CHECK: FAIL\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, "FAIL", relationship.ParseOK),
		Entry("a SANITY_CHECK quoted only inside a justification is a missing line",
			"RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: it says SANITY_CHECK: PASS\nCONFIDENCE: HIGH",
			relationship.RelNoRelationship, "", relationship.ParseFail),
		Entry("an indented PASS line keeps the verdict",
			"RELATIONSHIP: EQUIVALENT\n  SANITY_CHECK: PASS\nCONFIDENCE: HIGH",
			relationship.RelEquivalent, "PASS", relationship.ParseOK),
	)

	It("discards the contribution type of a FAIL vote", func() {
		vote := relationship.ParseResponse("RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nSANITY_CHECK: FAIL\nCONFIDENCE: HIGH")
		Expect(vote.Relationship).To(Equal(relationship.RelNoRelationship))
		Expect(vote.ContributionType).To(Equal(relationship.ContribNone))
	})

	It("keeps the justification of a FAIL vote", func() {
		vote := relationship.ParseResponse("RELATIONSHIP: PARTIAL\nJUSTIFICATION: Topic only, no shared obligation.\nSANITY_CHECK: FAIL\nCONFIDENCE: LOW")
		Expect(vote.Justification).To(Equal("Topic only, no shared obligation."))
	})
})

var _ = Describe("sceptical default prompt", func() {
	var (
		spec   *prompt.PromptSpec
		system string
	)

	BeforeEach(func() {
		reg, err := prompt.NewRegistry(config.PromptConfig{Layers: config.PromptLayerConfig{Enabled: true}})
		Expect(err).NotTo(HaveOccurred())
		spec, err = reg.Resolve(context.Background(), "relationship")
		Expect(err).NotTo(HaveOccurred())
		system, err = prompt.SubstitutePlaceholders(spec.Templates.System, map[string]string{
			"classification_guidance":  relationship.ExportClassificationGuidance,
			"relationship_definitions": relationship.ExportRelationshipDefinitions,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("is version 1.3.0", func() {
		Expect(spec.Version).To(Equal("1.3.0"))
	})

	It("asks for the SANITY_CHECK line and a justification naming the shared obligation", func() {
		Expect(system).To(ContainSubstring("SANITY_CHECK: <PASS|FAIL>"))
		Expect(system).To(ContainSubstring("naming the shared obligation"))
	})

	DescribeTable("rendered system prompt carries the rule",
		func(sentence string) {
			Expect(system).To(ContainSubstring(sentence))
		},
		Entry("topics are not relationships", "Sharing a topic, keywords or a control family is not a relationship."),
		Entry("name the obligation", "Name the specific obligation both requirements share."),
		Entry("auditor check", "Would an auditor accept evidence of the target as evidence toward the source?"),
		Entry("fall back", "If you cannot name the obligation or the check fails, answer NO_RELATIONSHIP."),
	)
})
