package candidate_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
)

var _ = Describe("Candidate Types", func() {

	Describe("Candidate structure", func() {
		It("stores and exposes all fields correctly", func() {
			c := candidate.Candidate{
				SourceID:    "AC-1",
				TargetID:    "AC-2",
				Score:       0.85,
				Weight:      1.0,
				GeneratorID: "semantic",
				Metadata: map[string]string{
					"similarity": "0.85",
				},
			}

			Expect(c.SourceID).To(Equal("AC-1"))
			Expect(c.TargetID).To(Equal("AC-2"))
			Expect(c.Score).To(Equal(0.85))
			Expect(c.Weight).To(Equal(1.0))
			Expect(c.GeneratorID).To(Equal("semantic"))
		})
	})

	Describe("ControlData structure", func() {
		It("stores and exposes all fields correctly", func() {
			cd := candidate.ControlData{
				ControlID: "AC-1",
				Text:      "Access control policy",
				Type:      "Procedural",
				Level:     "Strategic",
				Ancestor:  "Access Control",
			}

			Expect(cd.ControlID).To(Equal("AC-1"))
			Expect(cd.Type).To(Equal("Procedural"))
			Expect(cd.Level).To(Equal("Strategic"))
		})
	})

	Describe("GenerateRequest structure", func() {
		It("stores and exposes all fields correctly", func() {
			req := candidate.GenerateRequest{
				TenantID: "tenant-1",
				JobID:    "job-123",
				SourceControls: map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1"},
				},
				TargetControls: map[string]*candidate.ControlData{
					"AC-2": {ControlID: "AC-2"},
				},
				Parameters: map[string]interface{}{
					"top_k": 50,
				},
			}

			Expect(req.TenantID).To(Equal("tenant-1"))
			Expect(req.JobID).To(Equal("job-123"))
			Expect(req.SourceControls).To(HaveLen(1))
			Expect(req.TargetControls).To(HaveLen(1))
		})
	})
})
