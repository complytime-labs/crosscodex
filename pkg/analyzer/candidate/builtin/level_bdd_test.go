package builtin_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
)

var _ = Describe("LevelGenerator", func() {
	var gen candidate.Generator

	BeforeEach(func() {
		gen = builtin.NewLevelGenerator()
	})

	Describe("Name", func() {
		It("returns level", func() {
			Expect(gen.Name()).To(Equal("level"))
		})
	})

	Describe("Generate", func() {
		Context("when operational sources require tactical and strategic targets", func() {
			It("pairs operational with tactical and strategic but not other operational", func() {
				sources := map[string]*candidate.ControlData{
					"OP-1": {
						ControlID: "OP-1",
						Text:      "Operational control 1",
						Level:     "Operational",
					},
					"OP-2": {
						ControlID: "OP-2",
						Text:      "Operational control 2",
						Level:     "Operational",
					},
				}

				targets := map[string]*candidate.ControlData{
					"TAC-1": {
						ControlID: "TAC-1",
						Text:      "Tactical control 1",
						Level:     "Tactical",
					},
					"STRAT-1": {
						ControlID: "STRAT-1",
						Text:      "Strategic control 1",
						Level:     "Strategic",
					},
					"OP-3": {
						ControlID: "OP-3",
						Text:      "Operational control 3",
						Level:     "Operational",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 0.7,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Operational sources (OP-1, OP-2) can require Tactical (TAC-1) and Strategic (STRAT-1)
				// Should NOT pair with other Operational (OP-3)
				// 2 sources x 2 higher-level targets = 4 candidates
				Expect(candidates).To(HaveLen(4))

				hasPair := func(sourceID, targetID string) bool {
					for _, c := range candidates {
						if c.SourceID == sourceID && c.TargetID == targetID {
							return true
						}
					}
					return false
				}

				// Operational can require Tactical and Strategic
				Expect(hasPair("OP-1", "TAC-1")).To(BeTrue())
				Expect(hasPair("OP-1", "STRAT-1")).To(BeTrue())
				Expect(hasPair("OP-2", "TAC-1")).To(BeTrue())
				Expect(hasPair("OP-2", "STRAT-1")).To(BeTrue())

				// Operational should NOT require other Operational
				Expect(hasPair("OP-1", "OP-3")).To(BeFalse())
				Expect(hasPair("OP-2", "OP-3")).To(BeFalse())

				// Verify scoring and metadata
				for _, c := range candidates {
					Expect(c.Score).To(Equal(1.0), "Level-based match should have score 1.0")
					Expect(c.Weight).To(Equal(0.7))
					Expect(c.GeneratorID).To(Equal("level"))
					Expect(c.Metadata).To(HaveKey("source_level"))
					Expect(c.Metadata).To(HaveKey("target_level"))
				}
			})
		})

		Context("when tactical sources require strategic targets", func() {
			It("pairs tactical with strategic only", func() {
				sources := map[string]*candidate.ControlData{
					"TAC-1": {
						ControlID: "TAC-1",
						Text:      "Tactical control",
						Level:     "Tactical",
					},
				}

				targets := map[string]*candidate.ControlData{
					"STRAT-1": {
						ControlID: "STRAT-1",
						Text:      "Strategic control",
						Level:     "Strategic",
					},
					"TAC-2": {
						ControlID: "TAC-2",
						Text:      "Another tactical control",
						Level:     "Tactical",
					},
					"OP-1": {
						ControlID: "OP-1",
						Text:      "Operational control",
						Level:     "Operational",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Tactical can only require Strategic
				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].SourceID).To(Equal("TAC-1"))
				Expect(candidates[0].TargetID).To(Equal("STRAT-1"))
			})
		})

		Context("when strategic is the source level", func() {
			It("returns no candidates because strategic is top-level", func() {
				sources := map[string]*candidate.ControlData{
					"STRAT-1": {
						ControlID: "STRAT-1",
						Text:      "Strategic control",
						Level:     "Strategic",
					},
				}

				targets := map[string]*candidate.ControlData{
					"STRAT-2": {
						ControlID: "STRAT-2",
						Text:      "Another strategic",
						Level:     "Strategic",
					},
					"TAC-1": {
						ControlID: "TAC-1",
						Text:      "Tactical control",
						Level:     "Tactical",
					},
					"OP-1": {
						ControlID: "OP-1",
						Text:      "Operational control",
						Level:     "Operational",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Strategic is top-level, cannot require anything higher
				Expect(candidates).To(BeEmpty())
			})
		})

		Context("when source has an unknown level", func() {
			It("skips the source and returns no candidates", func() {
				sources := map[string]*candidate.ControlData{
					"UNK-1": {
						ControlID: "UNK-1",
						Text:      "Unknown level control",
						Level:     "Unknown",
					},
				}

				targets := map[string]*candidate.ControlData{
					"STRAT-1": {
						ControlID: "STRAT-1",
						Text:      "Strategic control",
						Level:     "Strategic",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Unknown levels should be skipped
				Expect(candidates).To(BeEmpty())
			})
		})

		Context("when source has an empty level", func() {
			It("skips the source and returns no candidates", func() {
				sources := map[string]*candidate.ControlData{
					"EMPTY-1": {
						ControlID: "EMPTY-1",
						Text:      "Control with empty level",
						Level:     "",
					},
				}

				targets := map[string]*candidate.ControlData{
					"STRAT-1": {
						ControlID: "STRAT-1",
						Text:      "Strategic control",
						Level:     "Strategic",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Empty levels should be skipped
				Expect(candidates).To(BeEmpty())
			})
		})

		Context("when sources have mixed levels", func() {
			It("correctly pairs each level upward only", func() {
				sources := map[string]*candidate.ControlData{
					"OP-1":    {ControlID: "OP-1", Level: "Operational"},
					"TAC-1":   {ControlID: "TAC-1", Level: "Tactical"},
					"STRAT-1": {ControlID: "STRAT-1", Level: "Strategic"},
				}

				targets := map[string]*candidate.ControlData{
					"OP-2":    {ControlID: "OP-2", Level: "Operational"},
					"TAC-2":   {ControlID: "TAC-2", Level: "Tactical"},
					"STRAT-2": {ControlID: "STRAT-2", Level: "Strategic"},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Expected pairs:
				// OP-1 -> TAC-2, STRAT-2 (2 pairs)
				// TAC-1 -> STRAT-2 (1 pair)
				// STRAT-1 -> nothing (0 pairs)
				// Total: 3 pairs
				Expect(candidates).To(HaveLen(3))

				hasPair := func(sourceID, targetID string) bool {
					for _, c := range candidates {
						if c.SourceID == sourceID && c.TargetID == targetID {
							return true
						}
					}
					return false
				}

				Expect(hasPair("OP-1", "TAC-2")).To(BeTrue())
				Expect(hasPair("OP-1", "STRAT-2")).To(BeTrue())
				Expect(hasPair("TAC-1", "STRAT-2")).To(BeTrue())

				// Should NOT have same-level or downward pairs
				Expect(hasPair("OP-1", "OP-2")).To(BeFalse())
				Expect(hasPair("TAC-1", "TAC-2")).To(BeFalse())
				Expect(hasPair("TAC-1", "OP-2")).To(BeFalse())
			})
		})

		Context("when telemetry is configured", func() {
			It("generates candidates with a noop tracer", func() {
				tracer := noop.NewTracerProvider().Tracer("test")
				genWithTelemetry := builtin.NewLevelGenerator(builtin.WithLevelTelemetry(tracer))

				sources := map[string]*candidate.ControlData{
					"OP-1": {ControlID: "OP-1", Text: "Operational control", Level: "Operational"},
				}

				targets := map[string]*candidate.ControlData{
					"ST-1": {ControlID: "ST-1", Text: "Strategic control", Level: "Strategic"},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					SourceControls: sources,
					TargetControls: targets,
					Parameters:     map[string]interface{}{},
				}

				candidates, err := genWithTelemetry.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).NotTo(BeEmpty())
				Expect(candidates).To(HaveLen(1))
			})
		})
	})
})
