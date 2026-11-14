package requires_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

var _ = Describe("ParseResponse", func() {
	It("parses YES decisions", func() {
		raw := "REQUIRES: YES\nJUSTIFICATION: needs the target first\nSANITY_CHECK: PASS\nCONFIDENCE: HIGH"
		vote := requires.ParseResponse("voter-1", raw)
		Expect(vote.Decision).NotTo(BeNil())
		Expect(*vote.Decision).To(BeTrue())
		Expect(vote.Confidence).To(Equal("HIGH"))
		Expect(vote.Justification).To(Equal("needs the target first"))
	})

	It("parses NO decisions", func() {
		vote := requires.ParseResponse("voter-1", "REQUIRES: NO\nSANITY_CHECK: PASS\nCONFIDENCE: MEDIUM")
		Expect(vote.Decision).NotTo(BeNil())
		Expect(*vote.Decision).To(BeFalse())
	})

	It("returns a nil Decision for unparseable responses", func() {
		vote := requires.ParseResponse("voter-1", "the model rambled without the expected format")
		Expect(vote.Decision).To(BeNil())
	})

	It("defaults confidence to LOW when absent", func() {
		vote := requires.ParseResponse("voter-1", "REQUIRES: YES\nSANITY_CHECK: PASS")
		Expect(vote.Confidence).To(Equal("LOW"), "default when no CONFIDENCE line present")
	})
})

var _ = Describe("ParseResponse SANITY_CHECK", func() {
	DescribeTable("enforces the SANITY_CHECK line",
		func(raw string, expected *bool) {
			vote := requires.ParseResponse("voter-1", raw)
			if expected == nil {
				Expect(vote.Decision).To(BeNil())
				return
			}
			Expect(vote.Decision).NotTo(BeNil())
			Expect(*vote.Decision).To(Equal(*expected))
		},
		Entry("PASS keeps YES", "REQUIRES: YES\nSANITY_CHECK: PASS", ptr(true)),
		Entry("PASS keeps NO", "REQUIRES: NO\nSANITY_CHECK: PASS", ptr(false)),
		Entry("FAIL turns YES into NO", "REQUIRES: YES\nSANITY_CHECK: FAIL", ptr(false)),
		Entry("padded lowercase pass is PASS", "REQUIRES: YES\nSANITY_CHECK:  pass ", ptr(true)),
		Entry("missing line is invalid", "REQUIRES: YES", nil),
		Entry("MAYBE is invalid", "REQUIRES: YES\nSANITY_CHECK: MAYBE", nil),
		Entry("empty value is invalid", "REQUIRES: YES\nSANITY_CHECK:", nil),
	)

	It("keeps the justification of a FAIL vote", func() {
		vote := requires.ParseResponse("voter-1", "REQUIRES: YES\nJUSTIFICATION: Topic only.\nSANITY_CHECK: FAIL\nCONFIDENCE: LOW")
		Expect(vote.Justification).To(Equal("Topic only."))
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
		spec, err = reg.Resolve(context.Background(), "requires")
		Expect(err).NotTo(HaveOccurred())
		system, err = prompt.SubstitutePlaceholders(spec.Templates.System, map[string]string{
			"requires_guidance":      requires.ExportRequiresGuidance,
			"dependency_definitions": requires.ExportDependencyDefinitions,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("is version 1.4.0", func() {
		Expect(spec.Version).To(Equal("1.4.0"))
	})

	It("asks for the SANITY_CHECK line and a justification naming the shared obligation", func() {
		Expect(spec.Templates.User).To(ContainSubstring("SANITY_CHECK: PASS or FAIL"))
		Expect(spec.Templates.User).To(ContainSubstring("naming the shared obligation"))
	})

	DescribeTable("rendered system prompt carries the rule",
		func(sentence string) {
			Expect(system).To(ContainSubstring(sentence))
		},
		Entry("topics are not dependencies", "Sharing a topic, keywords or a control family is not a dependency."),
		Entry("name the obligation", "Name the specific obligation the source cannot meet without the target."),
		Entry("auditor check", "Would an auditor agree the source is impaired without the target?"),
		Entry("fall back", "If you cannot name the obligation or the check fails, answer NO."),
	)

	It("uses only the YES/NO output vocabulary in the rendered system prompt", func() {
		Expect(system).NotTo(ContainSubstring("NO_DEPENDENCY"))
		Expect(system).NotTo(ContainSubstring("BENEFITS_FROM"))
	})
})

func ptr(b bool) *bool { return &b }
