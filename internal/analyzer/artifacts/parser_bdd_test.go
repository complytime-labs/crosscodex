package artifacts_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("ParseResponse", func() {
	// Python parity: test_valid_single_artifact
	It("extracts a single valid artifact", func() {
		raw := "ARTIFACT_NAME: Access Control Policy\n" +
			"ARTIFACT_TYPE: POLICY\n" +
			"FREQUENCY: Annual\n" +
			"OWNER_ROLE: CISO\n" +
			"DESCRIPTION: Defines access control requirements."
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseOK))
		Expect(arts).To(HaveLen(1))
		Expect(arts[0].Name).To(Equal("Access Control Policy"))
		Expect(arts[0].Type).To(Equal(artifacts.ArtifactPolicy))
		Expect(arts[0].Frequency).To(Equal("Annual"))
		Expect(arts[0].OwnerRole).To(Equal("CISO"))
		Expect(arts[0].Description).To(Equal("Defines access control requirements."))
	})

	// Python parity: test_multiple_artifacts_separated_by_delimiter
	It("extracts multiple artifacts separated by ---", func() {
		raw := "ARTIFACT_NAME: Policy Doc\n" +
			"ARTIFACT_TYPE: POLICY\n" +
			"DESCRIPTION: A policy\n" +
			"---\n" +
			"ARTIFACT_NAME: Config File\n" +
			"ARTIFACT_TYPE: CONFIGURATION\n" +
			"DESCRIPTION: A config"
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseOK))
		Expect(arts).To(HaveLen(2))
		Expect(arts[0].Type).To(Equal(artifacts.ArtifactPolicy))
		Expect(arts[1].Type).To(Equal(artifacts.ArtifactConfiguration))
	})

	// Python parity: test_none_sentinel_returns_empty
	It("returns ParseNone for ARTIFACTS: NONE", func() {
		arts, status := artifacts.ParseResponse("ARTIFACTS: NONE")
		Expect(status).To(Equal(artifacts.ParseNone))
		Expect(arts).To(BeEmpty())
	})

	// Python parity: test_none_sentinel_case_insensitive
	It("handles case-insensitive NONE sentinel", func() {
		arts, status := artifacts.ParseResponse("artifacts: none")
		Expect(status).To(Equal(artifacts.ParseNone))
		Expect(arts).To(BeEmpty())
	})

	// Python parity: test_empty_string_returns_empty
	It("returns ParseFail for empty string", func() {
		arts, status := artifacts.ParseResponse("")
		Expect(status).To(Equal(artifacts.ParseFail))
		Expect(arts).To(BeEmpty())
	})

	It("returns ParseFail for whitespace-only string", func() {
		arts, status := artifacts.ParseResponse("   ")
		Expect(status).To(Equal(artifacts.ParseFail))
		Expect(arts).To(BeEmpty())
	})

	// Python parity: test_invalid_type_rejected
	It("drops blocks with invalid artifact type", func() {
		raw := "ARTIFACT_NAME: Something\n" +
			"ARTIFACT_TYPE: INVALID_TYPE\n" +
			"DESCRIPTION: Bad type"
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseFail))
		Expect(arts).To(BeEmpty())
	})

	// Python parity: test_missing_name_rejected
	It("drops blocks missing name", func() {
		raw := "ARTIFACT_TYPE: POLICY\nDESCRIPTION: No name"
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseFail))
		Expect(arts).To(BeEmpty())
	})

	// Python parity: test_missing_type_rejected
	It("drops blocks missing type", func() {
		raw := "ARTIFACT_NAME: Something\nDESCRIPTION: No type"
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseFail))
		Expect(arts).To(BeEmpty())
	})

	// Python parity: test_frequency_none_normalized
	It("normalizes NONE to empty string for frequency and owner_role", func() {
		raw := "ARTIFACT_NAME: Report\n" +
			"ARTIFACT_TYPE: REPORT\n" +
			"FREQUENCY: NONE\n" +
			"OWNER_ROLE: NONE\n" +
			"DESCRIPTION: A report"
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseOK))
		Expect(arts[0].Frequency).To(BeEmpty())
		Expect(arts[0].OwnerRole).To(BeEmpty())
	})

	It("captures unknown fields into Properties", func() {
		raw := "ARTIFACT_NAME: Policy\n" +
			"ARTIFACT_TYPE: POLICY\n" +
			"CITATION: NIST 800-53\n" +
			"DESCRIPTION: A policy"
		arts, status := artifacts.ParseResponse(raw)
		Expect(status).To(Equal(artifacts.ParseOK))
		Expect(arts[0].Properties).To(HaveKeyWithValue("CITATION", "NIST 800-53"))
	})

	It("does not put known fields into Properties", func() {
		raw := "ARTIFACT_NAME: Policy\n" +
			"ARTIFACT_TYPE: POLICY\n" +
			"FREQUENCY: Annual\n" +
			"DESCRIPTION: A policy"
		arts, _ := artifacts.ParseResponse(raw)
		Expect(arts[0].Properties).To(BeEmpty())
	})
})
