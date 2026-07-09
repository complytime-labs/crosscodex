package candidate_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
)

// mockGenerator is a test generator.
type mockGenerator struct {
	name       string
	candidates []candidate.Candidate
	err        error
}

func (m *mockGenerator) Name() string {
	return m.name
}

func (m *mockGenerator) Generate(_ context.Context, _ candidate.GenerateRequest) ([]candidate.Candidate, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.candidates, nil
}

var _ = Describe("Registry", func() {

	Describe("Register and Get", func() {
		It("registers a generator and retrieves it by name", func() {
			registry := candidate.NewRegistry()

			gen := &mockGenerator{name: "test-gen"}
			err := registry.Register(gen)
			Expect(err).NotTo(HaveOccurred())

			retrieved, err := registry.Get("test-gen")
			Expect(err).NotTo(HaveOccurred())
			Expect(retrieved.Name()).To(Equal("test-gen"))
		})
	})

	Describe("Register duplicate", func() {
		It("returns an error when registering a generator with an existing name", func() {
			registry := candidate.NewRegistry()

			gen1 := &mockGenerator{name: "test-gen"}
			gen2 := &mockGenerator{name: "test-gen"}

			err := registry.Register(gen1)
			Expect(err).NotTo(HaveOccurred())

			err = registry.Register(gen2)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already registered"))
		})
	})

	Describe("Get not found", func() {
		It("returns an error when getting a nonexistent generator", func() {
			registry := candidate.NewRegistry()

			_, err := registry.Get("nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("All", func() {
		It("returns all registered generators", func() {
			registry := candidate.NewRegistry()

			gen1 := &mockGenerator{name: "gen1"}
			gen2 := &mockGenerator{name: "gen2"}

			Expect(registry.Register(gen1)).NotTo(HaveOccurred())
			Expect(registry.Register(gen2)).NotTo(HaveOccurred())

			all := registry.All()
			Expect(all).To(HaveLen(2))

			names := make(map[string]bool)
			for _, g := range all {
				names[g.Name()] = true
			}
			Expect(names["gen1"]).To(BeTrue())
			Expect(names["gen2"]).To(BeTrue())
		})
	})

	Describe("Generate", func() {

		Context("when using union strategy", func() {
			It("returns all unique source-target pairs across generators", func() {
				registry := candidate.NewRegistry()

				gen1 := &mockGenerator{
					name: "gen1",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
						{SourceID: "S1", TargetID: "T2", Score: 0.7, Weight: 1.0, GeneratorID: "gen1"},
					},
				}
				gen2 := &mockGenerator{
					name: "gen2",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T2", Score: 0.9, Weight: 0.5, GeneratorID: "gen2"}, // Duplicate pair
						{SourceID: "S1", TargetID: "T3", Score: 0.6, Weight: 0.5, GeneratorID: "gen2"},
					},
				}

				Expect(registry.Register(gen1)).NotTo(HaveOccurred())
				Expect(registry.Register(gen2)).NotTo(HaveOccurred())

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				results, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
				Expect(err).NotTo(HaveOccurred())

				// Union should contain all unique pairs: (S1,T1), (S1,T2), (S1,T3)
				Expect(results).To(HaveLen(3))

				// Verify deduplication - (S1,T2) should appear only once
				pairCount := make(map[string]int)
				for _, c := range results {
					key := c.SourceID + ":" + c.TargetID
					pairCount[key]++
				}
				Expect(pairCount["S1:T1"]).To(Equal(1))
				Expect(pairCount["S1:T2"]).To(Equal(1))
				Expect(pairCount["S1:T3"]).To(Equal(1))
			})
		})

		Context("when using weighted union strategy", func() {
			It("filters candidates below the minimum score threshold", func() {
				registry := candidate.NewRegistry()

				// Generator 1: weight 1.0
				gen1 := &mockGenerator{
					name: "gen1",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
						{SourceID: "S1", TargetID: "T2", Score: 0.5, Weight: 1.0, GeneratorID: "gen1"}, // Low score
					},
				}
				// Generator 2: weight 0.5
				gen2 := &mockGenerator{
					name: "gen2",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.9, Weight: 0.5, GeneratorID: "gen2"}, // Duplicate, high score
						{SourceID: "S1", TargetID: "T3", Score: 0.3, Weight: 0.5, GeneratorID: "gen2"}, // Very low score
					},
				}

				Expect(registry.Register(gen1)).NotTo(HaveOccurred())
				Expect(registry.Register(gen2)).NotTo(HaveOccurred())

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				// Use threshold 0.6 for weighted union
				// For (S1,T1): gen1(0.8*1.0) + gen2(0.9*0.5) = 0.8 + 0.45 = 1.25, normalized = 1.25/1.5 = 0.833 > 0.6
				// For (S1,T2): gen1(0.5*1.0) = 0.5, normalized = 0.5/1.0 = 0.5 < 0.6
				// For (S1,T3): gen2(0.3*0.5) = 0.15, normalized = 0.15/0.5 = 0.3 < 0.6
				results, err := registry.Generate(context.Background(), req, candidate.StrategyWeightedUnion, candidate.WithMinScore(0.6))
				Expect(err).NotTo(HaveOccurred())

				Expect(results).To(HaveLen(1))
				Expect(results[0].TargetID).To(Equal("T1"))
			})

			It("computes weighted average score from multiple generator contributions", func() {
				registry := candidate.NewRegistry()

				gen1 := &mockGenerator{
					name: "gen1",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.6, Weight: 0.8, GeneratorID: "gen1"},
					},
				}
				gen2 := &mockGenerator{
					name: "gen2",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 0.2, GeneratorID: "gen2"},
					},
				}

				Expect(registry.Register(gen1)).NotTo(HaveOccurred())
				Expect(registry.Register(gen2)).NotTo(HaveOccurred())

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				// Weighted average: (0.6*0.8 + 0.8*0.2) / (0.8 + 0.2) = (0.48 + 0.16) / 1.0 = 0.64
				results, err := registry.Generate(context.Background(), req, candidate.StrategyWeightedUnion, candidate.WithMinScore(0.6))
				Expect(err).NotTo(HaveOccurred())

				Expect(results).To(HaveLen(1))
				Expect(results[0].TargetID).To(Equal("T1"))
				Expect(results[0].Score).To(BeNumerically("~", 0.64, 0.01))
			})
		})

		Context("when deduplicating candidates", func() {
			It("keeps only one entry per source-target pair from the same generator", func() {
				registry := candidate.NewRegistry()

				gen := &mockGenerator{
					name: "gen1",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
						{SourceID: "S1", TargetID: "T1", Score: 0.9, Weight: 1.0, GeneratorID: "gen1"}, // Duplicate
					},
				}

				Expect(registry.Register(gen)).NotTo(HaveOccurred())

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				results, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
				Expect(err).NotTo(HaveOccurred())

				// Should deduplicate
				Expect(results).To(HaveLen(1))
			})
		})

		Context("when the registry is empty", func() {
			It("returns an empty slice without error", func() {
				registry := candidate.NewRegistry()

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				results, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
				Expect(err).NotTo(HaveOccurred())

				Expect(results).To(BeEmpty())
			})
		})

		Context("when a generator returns an error", func() {
			It("propagates the error with the generator name", func() {
				registry := candidate.NewRegistry()

				gen := &mockGenerator{
					name: "failing-gen",
					err:  errors.New("mock error"),
				}

				Expect(registry.Register(gen)).NotTo(HaveOccurred())

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				_, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failing-gen"))
			})
		})

		Context("when using an unknown strategy", func() {
			It("returns an error indicating the unknown strategy", func() {
				registry := candidate.NewRegistry()

				gen := &mockGenerator{
					name: "gen1",
					candidates: []candidate.Candidate{
						{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
					},
				}

				Expect(registry.Register(gen)).NotTo(HaveOccurred())

				req := candidate.GenerateRequest{
					TenantID: "tenant1",
					JobID:    "job1",
				}

				_, err := registry.Generate(context.Background(), req, candidate.AggregationStrategy("invalid"))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("unknown aggregation strategy"))
			})
		})
	})
})
