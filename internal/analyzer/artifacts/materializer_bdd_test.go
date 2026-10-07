package artifacts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// failingGetNode wraps a memdriver graph and fails GetNode for one node ID.
type failingGetNode struct {
	graphdb.GraphDB
	nodeID string
	err    error
}

func (f failingGetNode) GetNode(ctx context.Context, tenantID, nodeID string) (*graphdb.Node, error) {
	if nodeID == f.nodeID {
		return nil, f.err
	}
	return f.GraphDB.GetNode(ctx, tenantID, nodeID)
}

// hideNodeOnce wraps a memdriver graph and reports one stored node as missing
// on the first GetNode for it, simulating a concurrent writer that creates the
// node between Materialize's pre-write check and its write.
type hideNodeOnce struct {
	graphdb.GraphDB
	nodeID string
	hidden bool
}

func (h *hideNodeOnce) GetNode(ctx context.Context, tenantID, nodeID string) (*graphdb.Node, error) {
	if nodeID == h.nodeID && !h.hidden {
		h.hidden = true
		return nil, fmt.Errorf("get node %s: %w", nodeID, graphdb.ErrNodeNotFound)
	}
	return h.GraphDB.GetNode(ctx, tenantID, nodeID)
}

// countingGetNode wraps a graph and counts GetNode calls per node ID.
type countingGetNode struct {
	graphdb.GraphDB
	calls map[string]int
}

func (c *countingGetNode) GetNode(ctx context.Context, tenantID, nodeID string) (*graphdb.Node, error) {
	c.calls[nodeID]++
	return c.GraphDB.GetNode(ctx, tenantID, nodeID)
}

// failingWrite wraps a memdriver graph and fails CreateNode for nodes labeled
// nodeLabel and CreateEdge for edges labeled edgeLabel. An empty label disables
// that override; no real write has an empty label, since CheckNode and
// CreateEdge reject one.
type failingWrite struct {
	graphdb.GraphDB
	nodeLabel string
	edgeLabel string
	err       error
}

func (f failingWrite) CreateNode(ctx context.Context, tenantID string, node graphdb.Node) error {
	if f.nodeLabel != "" && node.Label == f.nodeLabel {
		return f.err
	}
	return f.GraphDB.CreateNode(ctx, tenantID, node)
}

func (f failingWrite) CreateEdge(ctx context.Context, tenantID, sourceID, targetID string, edge graphdb.Edge) error {
	if f.edgeLabel != "" && edge.Label == f.edgeLabel {
		return f.err
	}
	return f.GraphDB.CreateEdge(ctx, tenantID, sourceID, targetID, edge)
}

// fakeStorage serves pre-loaded ControlResult JSON files.
type fakeStorage struct {
	storage.Provider
	files map[string][]byte
}

// List returns matching keys sorted, as the local and S3 providers do, so
// specs that depend on file order are deterministic.
func (f *fakeStorage) List(_ context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	var result []storage.ObjectMetadata
	for key := range f.files {
		if strings.HasPrefix(key, prefix) {
			result = append(result, storage.ObjectMetadata{Key: key})
		}
	}
	slices.SortFunc(result, func(a, b storage.ObjectMetadata) int { return strings.Compare(a.Key, b.Key) })
	return result, nil
}

// counterByAttr sums the data points of the int64 counter name whose
// attribute key equals value.
func counterByAttr(tp *telemetrytest.TestProvider, name, key, value string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), name)
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	Expect(ok).To(BeTrue(), "metric %s data is %T", name, m.Data)
	var total int64
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
			total += dp.Value
		}
	}
	return total
}

func (f *fakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.files[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

var _ = Describe("GraphMaterializer", func() {
	const tenantID = "test-tenant"

	var (
		ctx   context.Context
		graph graphdb.GraphDB
		store *fakeStorage
		cfg   config.ArtifactsConfig
		m     *artifacts.GraphMaterializer
	)

	// newGraph returns a memdriver graph holding the control the fixture's
	// artifacts hang off, as catalog ingest would have created it.
	newGraph := func() graphdb.GraphDB {
		g := memdriver.New()
		Expect(g.CreateGraph(ctx, tenantID)).To(Succeed())
		Expect(g.CreateNode(ctx, tenantID, graphdb.Node{ID: "ac-1", Label: "Control", ValidFrom: time.Now()})).To(Succeed())
		return g
	}

	demands := func(g graphdb.GraphDB) []graphdb.Relationship {
		rels, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "DEMANDS"})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}

	// artifactCount counts distinct Artifact nodes reachable by an edge.
	artifactCount := func() int {
		rels, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{TargetLabel: "Artifact"})
		Expect(err).NotTo(HaveOccurred())
		ids := map[string]bool{}
		for _, r := range rels {
			ids[r.Target.ID] = true
		}
		return len(ids)
	}

	// expectNothingWritten asserts that a rejected job left no trace: none of
	// artIDs, no ArtifactType node and no DEMANDS or IS_TYPE edge.
	expectNothingWritten := func(artIDs ...string) {
		for _, id := range artIDs {
			_, err := graph.GetNode(ctx, tenantID, id)
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound), "Artifact %s", id)
		}
		for _, at := range artifacts.AllArtifactTypes() {
			_, err := graph.GetNode(ctx, tenantID, strings.ToLower(at.String()))
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound), "ArtifactType %s", at)
		}
		Expect(demands(graph)).To(BeEmpty())
		isType, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "IS_TYPE"})
		Expect(err).NotTo(HaveOccurred())
		Expect(isType).To(BeEmpty())
	}

	BeforeEach(func() {
		ctx = context.Background()
		graph = newGraph()
		cfg = config.ArtifactsConfig{FuzzyThreshold: 0.6}

		cr := artifacts.ControlResult{
			ControlID: "ac-1",
			Artifacts: []artifacts.ConsensusArtifact{
				{
					Name:       "access control policy",
					Type:       artifacts.ArtifactPolicy,
					Confidence: 1.0,
					VoterKeys:  []string{"m1", "m2"},
					VoteCount:  2,
					Unanimous:  true,
				},
			},
		}
		crJSON, err := json.Marshal(cr)
		Expect(err).NotTo(HaveOccurred())

		store = &fakeStorage{
			files: map[string][]byte{
				"test-tenant/analysis/artifacts/job-1/ac-1.json": crJSON,
			},
		}

		m = artifacts.NewGraphMaterializer(graph, store, cfg)
	})

	It("creates an ArtifactType node for every artifact type", func() {
		Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

		for _, at := range artifacts.AllArtifactTypes() {
			node, err := graph.GetNode(ctx, tenantID, strings.ToLower(at.String()))
			Expect(err).NotTo(HaveOccurred(), "ArtifactType %s", at)
			Expect(node.Label).To(Equal("ArtifactType"))
		}
	})

	It("creates an Artifact node without a dedup_generation property", func() {
		Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

		rels := demands(graph)
		Expect(rels).To(HaveLen(1))
		artifactNode := rels[0].Target
		Expect(artifactNode.Label).To(Equal("Artifact"))
		Expect(artifactNode.Properties).NotTo(HaveKey("dedup_generation"))
		Expect(artifactNode.Properties).To(HaveKeyWithValue("name", "access control policy"))
		Expect(artifactNode.Properties).To(HaveKeyWithValue("confidence", 1.0))
	})

	It("creates DEMANDS and IS_TYPE edges", func() {
		Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

		rels := demands(graph)
		Expect(rels).To(HaveLen(1))
		Expect(rels[0].Source.ID).To(Equal("ac-1"))

		isType, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "IS_TYPE"})
		Expect(err).NotTo(HaveOccurred())
		Expect(isType).To(HaveLen(1))
		Expect(isType[0].Target.ID).To(Equal("policy"))
	})

	It("generates stable artifact IDs from content hash", func() {
		Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

		graph2 := newGraph()
		m2 := artifacts.NewGraphMaterializer(graph2, store, cfg)
		Expect(m2.Materialize(ctx, tenantID, "job-1")).To(Succeed())

		first, second := demands(graph), demands(graph2)
		Expect(first).To(HaveLen(1))
		Expect(second).To(HaveLen(1))
		Expect(second[0].Target.ID).To(Equal(first[0].Target.ID))
	})

	Context("re-materializing into the same graph", func() {
		// policyArtifact is the fixture's single artifact, as it is stored.
		policyArtifact := artifacts.ConsensusArtifact{Name: "access control policy", Type: artifacts.ArtifactPolicy, Confidence: 1.0}
		artID := artifacts.ExportArtifactID("ac-1", policyArtifact)

		edgeCount := func(label string) int {
			rels, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: label})
			Expect(err).NotTo(HaveOccurred())
			return len(rels)
		}
		It("succeeds for the same job and adds no nodes or edges", func() {
			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(artifactCount()).To(Equal(1))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))

			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(artifactCount()).To(Equal(1))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))

			rels := demands(graph)
			Expect(rels[0].Edge.ID).To(Equal(graphdb.DerivedID("demands", "ac-1", artID)))
		})

		// storedPolicyNode is the node job-1 writes for policyArtifact.
		storedPolicyNode := func() graphdb.Node {
			return graphdb.Node{
				ID:    artID,
				Label: "Artifact",
				Properties: map[string]any{
					"name":        policyArtifact.Name,
					"frequency":   "",
					"owner_role":  "",
					"description": "",
					"confidence":  1.0,
				},
				ValidFrom:      time.Now(),
				CreatedBy:      "job-1",
				CreationMethod: "llm_panel",
			}
		}

		It("resumes a partial write by adding the missing edges", func() {
			Expect(graph.CreateNode(ctx, tenantID, storedPolicyNode())).To(Succeed())

			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

			Expect(artifactCount()).To(Equal(1))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			isType, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "IS_TYPE"})
			Expect(err).NotTo(HaveOccurred())
			Expect(isType).To(HaveLen(1))
			Expect(isType[0].Source.ID).To(Equal(artID))
			Expect(isType[0].Edge.ID).To(Equal(graphdb.DerivedID("is-type", artID, "policy")))
		})

		It("rejects a different job's conflicting artifact and leaves the stored node unchanged", func() {
			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			before, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())

			changed := policyArtifact
			changed.Description = "revised wording"
			crJSON, err := json.Marshal(artifacts.ControlResult{ControlID: "ac-1", Artifacts: []artifacts.ConsensusArtifact{changed}})
			Expect(err).NotTo(HaveOccurred())
			store.files["test-tenant/analysis/artifacts/job-2/ac-1.json"] = crJSON

			err = m.Materialize(ctx, tenantID, "job-2")
			Expect(err).To(MatchError(graphdb.ErrNodeExists))
			Expect(err.Error()).To(ContainSubstring(artID))
			Expect(err.Error()).To(ContainSubstring(`job "job-1" already materialized it with different description.`))
			Expect(err.Error()).To(ContainSubstring("this materializer never replaces a stored Artifact, so the stored node is kept"))
			Expect(err.Error()).To(ContainSubstring("the graph has no per-node delete"))
			Expect(err.Error()).To(ContainSubstring(`materialize job "job-2" before job "job-1" into a rebuilt tenant graph`))
			Expect(err.Error()).To(ContainSubstring("re-import the catalogs so the Control nodes exist"))
			Expect(err.Error()).To(ContainSubstring("docs/dev/design-principles.md"))
			Expect(err.Error()).NotTo(ContainSubstring("per-node replace"))
			Expect(err.Error()).NotTo(ContainSubstring("empty tenant graph"))
			Expect(err.Error()).NotTo(ContainSubstring("rebuild the tenant graph from the authoritative stores"))
			Expect(err.Error()).NotTo(ContainSubstring("created_by"))

			after, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))
		})
		It("accepts another job rediscovering an identical artifact and keeps the stored node", func() {
			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			before, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			store.files["test-tenant/analysis/artifacts/job-2/ac-1.json"] = store.files["test-tenant/analysis/artifacts/job-1/ac-1.json"]

			Expect(m.Materialize(ctx, tenantID, "job-2")).To(Succeed())

			after, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			Expect(after.CreatedBy).To(Equal("job-1"))
			Expect(artifactCount()).To(Equal(1))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))
		})

		It("rejects drifted results for the same job as a bug, leaving the stored node unchanged", func() {
			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			before, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())

			drifted := policyArtifact
			drifted.Description = "revised wording"
			crJSON, err := json.Marshal(artifacts.ControlResult{ControlID: "ac-1", Artifacts: []artifacts.ConsensusArtifact{drifted}})
			Expect(err).NotTo(HaveOccurred())
			store.files["test-tenant/analysis/artifacts/job-1/ac-1.json"] = crJSON

			err = m.Materialize(ctx, tenantID, "job-1")
			Expect(err).To(MatchError(graphdb.ErrNodeExists))
			Expect(err.Error()).To(ContainSubstring(artID))
			Expect(err.Error()).To(ContainSubstring("for this job with different description, so this job's stored results changed"))
			Expect(err.Error()).To(ContainSubstring("bug to investigate, not something to retry"))

			after, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))
		})

		It("writes nothing when a later artifact in the job conflicts", func() {
			Expect(graph.CreateNode(ctx, tenantID, graphdb.Node{
				ID:             artID,
				Label:          "Artifact",
				Properties:     map[string]any{"name": policyArtifact.Name},
				ValidFrom:      time.Now(),
				CreatedBy:      "job-other",
				CreationMethod: "llm_panel",
			})).To(Succeed())

			fresh := artifacts.ConsensusArtifact{Name: "audit log", Type: artifacts.ArtifactRecord, Confidence: 1.0}
			crJSON, err := json.Marshal(artifacts.ControlResult{
				ControlID: "ac-1",
				Artifacts: []artifacts.ConsensusArtifact{fresh, policyArtifact},
			})
			Expect(err).NotTo(HaveOccurred())
			store.files["test-tenant/analysis/artifacts/job-2/ac-1.json"] = crJSON

			err = m.Materialize(ctx, tenantID, "job-2")
			Expect(err).To(MatchError(graphdb.ErrNodeExists))
			Expect(err.Error()).To(ContainSubstring(`job "job-other" already materialized it`))

			expectNothingWritten(artifacts.ExportArtifactID("ac-1", fresh))
		})

		It("rejects another job's artifact whose properties differ, naming the property", func() {
			cited := policyArtifact
			cited.Properties = map[string]string{"CITATION": "a"}
			crJSON, err := json.Marshal(artifacts.ControlResult{ControlID: "ac-1", Artifacts: []artifacts.ConsensusArtifact{cited}})
			Expect(err).NotTo(HaveOccurred())
			store.files["test-tenant/analysis/artifacts/job-1/ac-1.json"] = crJSON
			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			before, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())

			cited.Properties = map[string]string{"CITATION": "b"}
			crJSON, err = json.Marshal(artifacts.ControlResult{ControlID: "ac-1", Artifacts: []artifacts.ConsensusArtifact{cited}})
			Expect(err).NotTo(HaveOccurred())
			store.files["test-tenant/analysis/artifacts/job-2/ac-1.json"] = crJSON

			err = m.Materialize(ctx, tenantID, "job-2")
			Expect(err).To(MatchError(graphdb.ErrNodeExists))
			Expect(err.Error()).To(ContainSubstring(`job "job-1" already materialized it with different prop_CITATION.`))

			after, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))
		})

		DescribeTable("rejects a stored node whose content differs from the run, leaving it unchanged",
			func(field string, alter func(*graphdb.Node)) {
				stored := storedPolicyNode()
				alter(&stored)
				Expect(graph.CreateNode(ctx, tenantID, stored)).To(Succeed())
				before, err := graph.GetNode(ctx, tenantID, artID)
				Expect(err).NotTo(HaveOccurred())

				err = m.Materialize(ctx, tenantID, "job-1")
				Expect(err).To(MatchError(graphdb.ErrNodeExists))
				Expect(err.Error()).To(ContainSubstring("for this job with different " + field + ","))
				Expect(err.Error()).To(ContainSubstring("bug to investigate, not something to retry"))

				after, err := graph.GetNode(ctx, tenantID, artID)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				Expect(edgeCount("DEMANDS")).To(BeZero())
				Expect(edgeCount("IS_TYPE")).To(BeZero())
			},
			Entry("a prop_ property the run lacks", "prop_EXTRA", func(n *graphdb.Node) {
				n.Properties["prop_EXTRA"] = "left over"
			}),
			Entry("another creation method", "creation_method", func(n *graphdb.Node) {
				n.CreationMethod = "static"
			}),
		)

		DescribeTable("accepts a stored node with an extra non-prop_ property and keeps it",
			func(key string, value any) {
				stored := storedPolicyNode()
				stored.Properties[key] = value
				Expect(graph.CreateNode(ctx, tenantID, stored)).To(Succeed())
				before, err := graph.GetNode(ctx, tenantID, artID)
				Expect(err).NotTo(HaveOccurred())

				Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

				after, err := graph.GetNode(ctx, tenantID, artID)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))
				Expect(edgeCount("DEMANDS")).To(Equal(1))
				Expect(edgeCount("IS_TYPE")).To(Equal(1))
			},
			Entry("merged_into", "merged_into", "ac-1__art_other"),
			// Nodes written before dedup_generation was retired still carry it.
			Entry("the retired dedup_generation", "dedup_generation", 0),
		)

		It("rejects a conflicting node created between the check and the write, adding no edges", func() {
			conflicting := storedPolicyNode()
			conflicting.CreatedBy = "job-other"
			conflicting.Properties["description"] = "written concurrently"
			Expect(graph.CreateNode(ctx, tenantID, conflicting)).To(Succeed())
			before, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			racing := &hideNodeOnce{GraphDB: graph, nodeID: artID}

			err = artifacts.NewGraphMaterializer(racing, store, cfg).Materialize(ctx, tenantID, "job-1")
			Expect(racing.hidden).To(BeTrue(), "the pre-write check must have looked up the artifact")
			Expect(err).To(MatchError(graphdb.ErrNodeExists))
			Expect(err.Error()).To(ContainSubstring(`job "job-other" already materialized it with different description.`))

			after, err := graph.GetNode(ctx, tenantID, artID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
			Expect(edgeCount("DEMANDS")).To(BeZero())
			Expect(edgeCount("IS_TYPE")).To(BeZero())
		})

		It("looks up an artifact the pre-write check already verified only once on a re-run", func() {
			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			counting := &countingGetNode{GraphDB: graph, calls: map[string]int{}}

			Expect(artifacts.NewGraphMaterializer(counting, store, cfg).Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(counting.calls[artID]).To(Equal(1),
				"the pre-write check found and verified the stored artifact, so the write must not look it up again")
			Expect(artifactCount()).To(Equal(1))
			Expect(edgeCount("DEMANDS")).To(Equal(1))
		})

		It("still compares an identical artifact written between the check and the write, and accepts it", func() {
			Expect(graph.CreateNode(ctx, tenantID, storedPolicyNode())).To(Succeed())
			counting := &countingGetNode{GraphDB: &hideNodeOnce{GraphDB: graph, nodeID: artID}, calls: map[string]int{}}

			Expect(artifacts.NewGraphMaterializer(counting, store, cfg).Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(counting.calls[artID]).To(Equal(2),
				"the pre-write check saw the artifact absent, so the write must compare the node it collided with")
			Expect(edgeCount("DEMANDS")).To(Equal(1))
			Expect(edgeCount("IS_TYPE")).To(Equal(1))
		})

		DescribeTable("fails with actionable retry guidance and writes nothing when a node cannot be read",
			func(failingID, wantGuidance string) {
				injected := errors.New("graph connection reset")
				failing := failingGetNode{GraphDB: graph, nodeID: failingID, err: injected}

				err := artifacts.NewGraphMaterializer(failing, store, cfg).Materialize(ctx, tenantID, "job-1")
				Expect(err).To(MatchError(injected))
				Expect(err.Error()).To(ContainSubstring(wantGuidance))

				expectNothingWritten(artID)
			},
			Entry("the existing Artifact", artID,
				"checking existing Artifact "+artID+" for conflicts (retry once the graph is reachable; re-running the job is safe)"),
			Entry("the referenced control", "ac-1",
				"checking control ac-1 exists (retry once the graph is reachable; re-running the job is safe)"),
		)

		// A node under another label holding an ID Materialize needs is a
		// conflict, not an idempotent re-write: it must never pass as
		// ErrNodeExists, and the other node must be left alone.
		DescribeTable("rejects an ID already used under another label, writing nothing",
			func(id, label string) {
				owner := graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now(),
					Properties: map[string]any{"title": "pre-existing"}}
				Expect(graph.CreateNode(ctx, tenantID, owner)).To(Succeed())
				before, getErr := graph.GetNode(ctx, tenantID, id)
				Expect(getErr).NotTo(HaveOccurred())

				err := m.Materialize(ctx, tenantID, "job-1")
				Expect(err).To(MatchError(graphdb.ErrNodeIDConflict))
				Expect(errors.Is(err, graphdb.ErrNodeExists)).To(BeFalse(), "a conflict must not pass as ErrNodeExists")
				Expect(err.Error()).To(ContainSubstring(
					fmt.Sprintf("node ID %q belongs to a node labeled Control, so it cannot also identify a node labeled %s", id, label)))
				Expect(err.Error()).To(ContainSubstring("Give the " + label + " node an ID no other node uses"))

				after, getErr := graph.GetNode(ctx, tenantID, id)
				Expect(getErr).NotTo(HaveOccurred())
				Expect(after).To(Equal(before))

				if id != artID {
					_, getErr = graph.GetNode(ctx, tenantID, artID)
					Expect(getErr).To(MatchError(graphdb.ErrNodeNotFound))
				}
				for _, at := range artifacts.AllArtifactTypes() {
					if typeID := strings.ToLower(at.String()); typeID != id {
						_, getErr = graph.GetNode(ctx, tenantID, typeID)
						Expect(getErr).To(MatchError(graphdb.ErrNodeNotFound), "ArtifactType %s", at)
					}
				}
				Expect(edgeCount("DEMANDS")).To(BeZero())
				Expect(edgeCount("IS_TYPE")).To(BeZero())
			},
			Entry("an ArtifactType ID", "policy", "ArtifactType"),
			Entry("the Artifact ID", artID, "Artifact"),
		)

		// Writes are not atomic across calls: a failed write keeps everything
		// written before it, and re-running the job completes it.
		DescribeTable("fails on a graph write error, keeps earlier writes, and completes on re-run",
			func(nodeLabel, edgeLabel, wantMsg string, wantTypes, wantArtifact bool, wantDemands int) {
				injected := errors.New("graph connection reset")
				failing := failingWrite{GraphDB: graph, nodeLabel: nodeLabel, edgeLabel: edgeLabel, err: injected}

				err := artifacts.NewGraphMaterializer(failing, store, cfg).Materialize(ctx, tenantID, "job-1")
				Expect(err).To(MatchError(injected))
				Expect(err.Error()).To(ContainSubstring(wantMsg))

				typesStored := 0
				for _, at := range artifacts.AllArtifactTypes() {
					if _, err := graph.GetNode(ctx, tenantID, strings.ToLower(at.String())); err == nil {
						typesStored++
					}
				}
				if wantTypes {
					Expect(typesStored).To(Equal(len(artifacts.AllArtifactTypes())))
				} else {
					Expect(typesStored).To(BeZero())
				}
				_, getErr := graph.GetNode(ctx, tenantID, artID)
				if wantArtifact {
					Expect(getErr).NotTo(HaveOccurred())
				} else {
					Expect(getErr).To(MatchError(graphdb.ErrNodeNotFound))
				}
				Expect(edgeCount("DEMANDS")).To(Equal(wantDemands))
				Expect(edgeCount("IS_TYPE")).To(BeZero())

				Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())
				Expect(artifactCount()).To(Equal(1))
				Expect(edgeCount("DEMANDS")).To(Equal(1))
				Expect(edgeCount("IS_TYPE")).To(Equal(1))
				for _, at := range artifacts.AllArtifactTypes() {
					_, err := graph.GetNode(ctx, tenantID, strings.ToLower(at.String()))
					Expect(err).NotTo(HaveOccurred(), "ArtifactType %s", at)
				}
			},
			Entry("creating an ArtifactType node", "ArtifactType", "",
				"materializer: creating ArtifactType policy: graph connection reset", false, false, 0),
			Entry("creating the Artifact node", "Artifact", "",
				"materializer: creating Artifact "+artID+": graph connection reset", true, false, 0),
			Entry("creating the DEMANDS edge", "", "DEMANDS",
				"materializer: creating DEMANDS edge ac-1->"+artID+": graph connection reset", true, true, 0),
			Entry("creating the IS_TYPE edge", "", "IS_TYPE",
				"materializer: creating IS_TYPE edge "+artID+"->policy: graph connection reset", true, true, 1),
		)

		It("counts only the nodes and edges it creates across runs", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			instrumented := artifacts.NewGraphMaterializer(graph, store, cfg,
				artifacts.WithMaterializerTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			Expect(instrumented.Materialize(ctx, tenantID, "job-1")).To(Succeed())
			Expect(instrumented.Materialize(ctx, tenantID, "job-1")).To(Succeed())

			Expect(counterByAttr(tp, "artifacts.nodes.materialized", "node.label", "Artifact")).To(Equal(int64(1)))
			Expect(counterByAttr(tp, "artifacts.nodes.materialized", "node.label", "ArtifactType")).
				To(Equal(int64(len(artifacts.AllArtifactTypes()))))
			Expect(counterByAttr(tp, "artifacts.edges.materialized", "edge.label", "DEMANDS+IS_TYPE")).To(Equal(int64(2)))
		})
	})

	DescribeTable("rejects an invalid tenant ID before touching storage or the graph",
		func(badTenant, wantText string) {
			err := m.Materialize(ctx, badTenant, "job-1")
			Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			Expect(err.Error()).To(HavePrefix("materializer.Materialize: tenant ID"),
				"the materializer must reject the tenant itself, not leave it to the driver")
			Expect(err.Error()).To(ContainSubstring(wantText))
			expectNothingWritten(artifacts.ExportArtifactID("ac-1",
				artifacts.ConsensusArtifact{Name: "access control policy", Type: artifacts.ArtifactPolicy}))
		},
		Entry("empty", "", "tenant ID must not be empty"),
		Entry("uppercase", "Test-Tenant", tenant.IDRule),
		Entry("53 characters", "t"+strings.Repeat("a", 52), tenant.IDRule),
	)

	It("fails when the control node does not exist", func() {
		g := memdriver.New()
		Expect(g.CreateGraph(ctx, tenantID)).To(Succeed())
		err := artifacts.NewGraphMaterializer(g, store, cfg).Materialize(ctx, tenantID, "job-1")
		Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
		Expect(err.Error()).To(ContainSubstring("ac-1"))
	})

	It("writes nothing when any control in the job is missing", func() {
		crJSON, err := json.Marshal(artifacts.ControlResult{
			ControlID: "ac-2",
			Artifacts: []artifacts.ConsensusArtifact{{Name: "audit log", Type: artifacts.ArtifactRecord, Confidence: 1.0}},
		})
		Expect(err).NotTo(HaveOccurred())
		store.files["test-tenant/analysis/artifacts/job-1/ac-2.json"] = crJSON

		err = m.Materialize(ctx, tenantID, "job-1")
		Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
		Expect(err.Error()).To(ContainSubstring("control ac-2"))
		Expect(err.Error()).To(ContainSubstring("import the catalog first"))

		expectNothingWritten(
			artifacts.ExportArtifactID("ac-1", artifacts.ConsensusArtifact{Name: "access control policy", Type: artifacts.ArtifactPolicy}),
			artifacts.ExportArtifactID("ac-2", artifacts.ConsensusArtifact{Name: "audit log", Type: artifacts.ArtifactRecord}),
		)
	})

	Context("validating a job before writing", func() {
		// storeJob replaces job-1's stored results with one control result.
		storeJob := func(cr artifacts.ControlResult) {
			crJSON, err := json.Marshal(cr)
			Expect(err).NotTo(HaveOccurred())
			store.files = map[string][]byte{"test-tenant/analysis/artifacts/job-1/ac-1.json": crJSON}
		}

		It("writes nothing when an artifact has an unknown type", func() {
			store.files = map[string][]byte{"test-tenant/analysis/artifacts/job-1/ac-1.json": []byte(
				`{"control_id":"ac-1","artifacts":[` +
					`{"name":"access control policy","type":0,"confidence":1},` +
					`{"name":"mystery artifact","type":42,"confidence":1}]}`)}

			err := m.Materialize(ctx, tenantID, "job-1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`artifact "mystery artifact" of control ac-1 for job "job-1"`))
			Expect(err.Error()).To(ContainSubstring("artifact type 42 is not a known ArtifactType"))
			Expect(err.Error()).To(ContainSubstring("nothing was written"))

			expectNothingWritten(artifacts.ExportArtifactID("ac-1", artifacts.ConsensusArtifact{
				Name: "access control policy", Type: artifacts.ArtifactPolicy,
			}))
		})

		It("writes nothing when two artifacts in the job share an ID but differ", func() {
			first := artifacts.ConsensusArtifact{Name: "access control policy", Type: artifacts.ArtifactPolicy, Confidence: 1.0}
			second := artifacts.ConsensusArtifact{Name: "The Access Control Policy.", Type: artifacts.ArtifactPolicy,
				Confidence: 1.0, Description: "a different reading"}
			sharedID := artifacts.ExportArtifactID("ac-1", first)
			Expect(artifacts.ExportArtifactID("ac-1", second)).To(Equal(sharedID))
			storeJob(artifacts.ControlResult{ControlID: "ac-1", Artifacts: []artifacts.ConsensusArtifact{first, second}})

			err := m.Materialize(ctx, tenantID, "job-1")
			Expect(err).To(MatchError(graphdb.ErrNodeExists))
			Expect(err.Error()).To(ContainSubstring(sharedID))
			Expect(err.Error()).To(ContainSubstring(`artifacts "access control policy" and "The Access Control Policy."`))
			Expect(err.Error()).To(ContainSubstring("normalize to the same ID"))
			Expect(err.Error()).To(ContainSubstring("nothing was written"))
			Expect(err.Error()).To(ContainSubstring("bug in the analyzer output"))

			expectNothingWritten(sharedID)
		})

		It("writes an exact duplicate within the job once", func() {
			art := artifacts.ConsensusArtifact{Name: "access control policy", Type: artifacts.ArtifactPolicy, Confidence: 1.0}
			storeJob(artifacts.ControlResult{ControlID: "ac-1", Artifacts: []artifacts.ConsensusArtifact{art, art}})

			Expect(m.Materialize(ctx, tenantID, "job-1")).To(Succeed())

			Expect(artifactCount()).To(Equal(1))
			rels := demands(graph)
			Expect(rels).To(HaveLen(1))
			Expect(rels[0].Target.ID).To(Equal(artifacts.ExportArtifactID("ac-1", art)))
			isType, err := graph.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "IS_TYPE"})
			Expect(err).NotTo(HaveOccurred())
			Expect(isType).To(HaveLen(1))
		})
	})
})
