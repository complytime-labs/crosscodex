package oscal_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

func TestOscalBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OSCAL BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Foundation Types", func() {
	Describe("Class Constants", func() {
		It("defines ClassSection correctly", func() {
			Expect(oscal.ClassSection).To(Equal("compliance-section"))
		})

		It("defines ClassRequirement correctly", func() {
			Expect(oscal.ClassRequirement).To(Equal("compliance-requirement"))
		})

		It("defines ClassContext correctly", func() {
			Expect(oscal.ClassContext).To(Equal("compliance-context"))
		})
	})

	Describe("ControlItem", func() {
		It("can be constructed with all fields", func() {
			item := oscal.ControlItem{
				ID:       "ac-2",
				Title:    "Account Management",
				Text:     "The organization manages information system accounts.",
				Class:    oscal.ClassRequirement,
				ParentID: "ac",
				GroupID:  "access-control",
				Props: map[string]string{
					"parent-id":   "ac",
					"source-part": "statement",
				},
			}

			Expect(item.ID).To(Equal("ac-2"))
			Expect(item.Title).To(Equal("Account Management"))
			Expect(item.Text).To(Equal("The organization manages information system accounts."))
			Expect(item.Class).To(Equal(oscal.ClassRequirement))
			Expect(item.ParentID).To(Equal("ac"))
			Expect(item.GroupID).To(Equal("access-control"))
			Expect(item.Props).To(HaveKeyWithValue("parent-id", "ac"))
			Expect(item.Props).To(HaveKeyWithValue("source-part", "statement"))
		})
	})
})
