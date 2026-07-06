//go:build integration

package graphdb_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

func TestRequiresEdge(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Requires Edge Suite")
}

var _ = Describe("RequiresEdge", func() {
	Describe("CreateRequiresEdge", func() {
		It("creates a REQUIRES edge with full consensus metadata", func() {
			tenantID := testID("requires-edge")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create source control node.
			sourceNode := graphdb.Node{
				ID:             "NIST-800-53-AC-2",
				Label:          "Control",
				ValidFrom:      now,
				CreatedBy:      "import-job",
				CreationMethod: "oscal-import",
				Properties: map[string]any{
					"framework": "NIST-800-53",
					"type":      "Technical",
					"level":     "Operational",
				},
			}
			Expect(client.CreateNode(ctx, tenantID, sourceNode)).To(Succeed())

			// Create target control node (prerequisite).
			targetNode := graphdb.Node{
				ID:             "NIST-800-53-AC-1",
				Label:          "Control",
				ValidFrom:      now,
				CreatedBy:      "import-job",
				CreationMethod: "oscal-import",
				Properties: map[string]any{
					"framework": "NIST-800-53",
					"type":      "Administrative",
					"level":     "Strategic",
				},
			}
			Expect(client.CreateNode(ctx, tenantID, targetNode)).To(Succeed())

			// Create REQUIRES edge with consensus data.
			reqEdge := graphdb.RequiresEdge{
				SourceID:        "NIST-800-53-AC-2",
				TargetID:        "NIST-800-53-AC-1",
				Confidence:      0.85,
				Unanimous:       false,
				ValidVotes:      8,
				TotalVotes:      9,
				VoteWeight:      8.0,
				Models:          []string{"llama3.2:3b", "qwen2.5:7b", "mistral:7b"},
				SamplesPerModel: 3,
				PromptVersion:   "1.0.0",
				AnalyzedAt:      now,
				TenantID:        tenantID,
				JobID:           "job-abc123",
			}
			Expect(client.CreateRequiresEdge(ctx, tenantID, reqEdge)).To(Succeed())

			// Query back the REQUIRES edge and verify all properties.
			results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				SourceLabel: "Control",
				TargetLabel: "Control",
				EdgeLabel:   "REQUIRES",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))

			rel := results[0]

			// Verify edge label and node IDs.
			Expect(rel.Edge.Label).To(Equal("REQUIRES"))
			Expect(rel.Edge.Source).To(Equal("NIST-800-53-AC-2"))
			Expect(rel.Edge.Target).To(Equal("NIST-800-53-AC-1"))

			// Verify consensus metadata properties.
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("confidence", 0.85))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("unanimous", false))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("valid_votes", float64(8)))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("total_votes", float64(9)))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("vote_weight", 8.0))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("samples_per_model", float64(3)))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("prompt_version", "1.0.0"))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("job_id", "job-abc123"))
			Expect(rel.Edge.Properties).To(HaveKeyWithValue("tenant_id", tenantID))

			// Verify models array.
			modelsRaw, ok := rel.Edge.Properties["models"]
			Expect(ok).To(BeTrue(), "models property should exist")
			// Models come back as agtype array, which parses to []any
			modelsAny, ok := modelsRaw.([]any)
			Expect(ok).To(BeTrue(), "models should be an array")
			Expect(len(modelsAny)).To(Equal(3))

			// Verify source and target node properties.
			Expect(rel.Source.ID).To(Equal("NIST-800-53-AC-2"))
			Expect(rel.Source.Label).To(Equal("Control"))
			Expect(rel.Target.ID).To(Equal("NIST-800-53-AC-1"))
			Expect(rel.Target.Label).To(Equal("Control"))
		})

		It("validates required fields", func() {
			tenantID := testID("requires-edge-validation")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			baseEdge := graphdb.RequiresEdge{
				SourceID:        "NIST-800-53-AC-2",
				TargetID:        "NIST-800-53-AC-1",
				Confidence:      0.85,
				Unanimous:       false,
				ValidVotes:      8,
				TotalVotes:      9,
				VoteWeight:      8.0,
				Models:          []string{"llama3.2:3b"},
				SamplesPerModel: 3,
				PromptVersion:   "1.0.0",
				AnalyzedAt:      now,
				TenantID:        tenantID,
				JobID:           "job-abc123",
			}

			// Missing source_id.
			invalidEdge := baseEdge
			invalidEdge.SourceID = ""
			Expect(client.CreateRequiresEdge(ctx, tenantID, invalidEdge)).
				To(MatchError(ContainSubstring("source_id is required")))

			// Missing target_id.
			invalidEdge = baseEdge
			invalidEdge.TargetID = ""
			Expect(client.CreateRequiresEdge(ctx, tenantID, invalidEdge)).
				To(MatchError(ContainSubstring("target_id is required")))

			// Missing analyzed_at.
			invalidEdge = baseEdge
			invalidEdge.AnalyzedAt = time.Time{}
			Expect(client.CreateRequiresEdge(ctx, tenantID, invalidEdge)).
				To(MatchError(ContainSubstring("analyzed_at is required")))
		})

		It("handles optional fields correctly", func() {
			tenantID := testID("requires-edge-optional")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create nodes.
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:        "source",
				Label:     "Control",
				ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:        "target",
				Label:     "Control",
				ValidFrom: now,
			})).To(Succeed())

			// Create edge with minimal optional fields (no models, no prompt_version).
			minimalEdge := graphdb.RequiresEdge{
				SourceID:        "source",
				TargetID:        "target",
				Confidence:      0.75,
				Unanimous:       true,
				ValidVotes:      1,
				TotalVotes:      1,
				VoteWeight:      1.0,
				SamplesPerModel: 1,
				AnalyzedAt:      now,
				TenantID:        tenantID,
				JobID:           "job-minimal",
			}
			Expect(client.CreateRequiresEdge(ctx, tenantID, minimalEdge)).To(Succeed())

			// Verify edge was created without errors.
			results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "REQUIRES",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Edge.Properties).To(HaveKeyWithValue("confidence", 0.75))
		})

		It("supports multiple models in consensus", func() {
			tenantID := testID("requires-edge-multimodel")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create nodes.
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:        "src-multi",
				Label:     "Control",
				ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:        "tgt-multi",
				Label:     "Control",
				ValidFrom: now,
			})).To(Succeed())

			// Create edge with 5 models × 3 samples = 15 total votes.
			multiModelEdge := graphdb.RequiresEdge{
				SourceID:        "src-multi",
				TargetID:        "tgt-multi",
				Confidence:      0.9333,
				Unanimous:       false,
				ValidVotes:      14,
				TotalVotes:      15,
				VoteWeight:      14.0,
				Models:          []string{"model-1", "model-2", "model-3", "model-4", "model-5"},
				SamplesPerModel: 3,
				PromptVersion:   "2.0.0",
				AnalyzedAt:      now,
				TenantID:        tenantID,
				JobID:           "job-multi",
			}
			Expect(client.CreateRequiresEdge(ctx, tenantID, multiModelEdge)).To(Succeed())

			// Verify all models are stored.
			results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "REQUIRES",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))

			modelsRaw, ok := results[0].Edge.Properties["models"]
			Expect(ok).To(BeTrue())
			modelsAny := modelsRaw.([]any)
			Expect(len(modelsAny)).To(Equal(5))
		})

		It("isolates edges per tenant", func() {
			tenant1 := testID("requires-tenant-1")
			tenant2 := testID("requires-tenant-2")
			setupTenant(tenant1)
			setupTenant(tenant2)
			DeferCleanup(func() {
				cleanupTenant(tenant1)
				cleanupTenant(tenant2)
			})

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create nodes in both tenants.
			for _, tenantID := range []string{tenant1, tenant2} {
				Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
					ID:        "source",
					Label:     "Control",
					ValidFrom: now,
				})).To(Succeed())
				Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
					ID:        "target",
					Label:     "Control",
					ValidFrom: now,
				})).To(Succeed())
			}

			// Create REQUIRES edge in tenant1.
			edge1 := graphdb.RequiresEdge{
				SourceID:        "source",
				TargetID:        "target",
				Confidence:      0.8,
				Unanimous:       true,
				ValidVotes:      1,
				TotalVotes:      1,
				VoteWeight:      1.0,
				SamplesPerModel: 1,
				AnalyzedAt:      now,
				TenantID:        tenant1,
				JobID:           "job-1",
			}
			Expect(client.CreateRequiresEdge(ctx, tenant1, edge1)).To(Succeed())

			// Create REQUIRES edge in tenant2.
			edge2 := graphdb.RequiresEdge{
				SourceID:        "source",
				TargetID:        "target",
				Confidence:      0.6,
				Unanimous:       false,
				ValidVotes:      2,
				TotalVotes:      3,
				VoteWeight:      2.0,
				SamplesPerModel: 1,
				AnalyzedAt:      now,
				TenantID:        tenant2,
				JobID:           "job-2",
			}
			Expect(client.CreateRequiresEdge(ctx, tenant2, edge2)).To(Succeed())

			// Query tenant1 should return only tenant1's edge.
			results1, err := client.QueryRelationships(ctx, tenant1, graphdb.RelationshipQuery{
				EdgeLabel: "REQUIRES",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results1).To(HaveLen(1))
			Expect(results1[0].Edge.Properties).To(HaveKeyWithValue("confidence", 0.8))
			Expect(results1[0].Edge.Properties).To(HaveKeyWithValue("job_id", "job-1"))

			// Query tenant2 should return only tenant2's edge.
			results2, err := client.QueryRelationships(ctx, tenant2, graphdb.RelationshipQuery{
				EdgeLabel: "REQUIRES",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results2).To(HaveLen(1))
			Expect(results2[0].Edge.Properties).To(HaveKeyWithValue("confidence", 0.6))
			Expect(results2[0].Edge.Properties).To(HaveKeyWithValue("job_id", "job-2"))
		})

		It("preserves high-precision confidence values", func() {
			tenantID := testID("requires-precision")
			setupTenant(tenantID)
			DeferCleanup(func() { cleanupTenant(tenantID) })

			client, err := graphdb.New(testDB)
			Expect(err).NotTo(HaveOccurred())
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)

			// Create nodes.
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:        "src-prec",
				Label:     "Control",
				ValidFrom: now,
			})).To(Succeed())
			Expect(client.CreateNode(ctx, tenantID, graphdb.Node{
				ID:        "tgt-prec",
				Label:     "Control",
				ValidFrom: now,
			})).To(Succeed())

			// Create edge with high-precision confidence value.
			precisionEdge := graphdb.RequiresEdge{
				SourceID:        "src-prec",
				TargetID:        "tgt-prec",
				Confidence:      0.9333333333,
				Unanimous:       false,
				ValidVotes:      14,
				TotalVotes:      15,
				VoteWeight:      14.0,
				SamplesPerModel: 1,
				AnalyzedAt:      now,
				TenantID:        tenantID,
				JobID:           "job-precision",
			}
			Expect(client.CreateRequiresEdge(ctx, tenantID, precisionEdge)).To(Succeed())

			// Retrieve and verify precision is maintained (allowing for floating point rounding).
			results, err := client.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
				EdgeLabel: "REQUIRES",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))

			confidence := results[0].Edge.Properties["confidence"].(float64)
			Expect(confidence).To(BeNumericallyClose(0.9333333333, 0.00001))
		})
	})
})
