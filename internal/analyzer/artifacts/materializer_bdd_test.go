package artifacts_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// capturedEdge records a CreateEdge call with its structural endpoint IDs.
type capturedEdge struct {
	SourceID string
	TargetID string
	Edge     graphdb.Edge
}

// fakeGraphDB records all created nodes and edges for assertion.
type fakeGraphDB struct {
	graphdb.GraphDB
	nodes    []graphdb.Node
	captured []capturedEdge
	nodeIDs  map[string]bool
}

func (f *fakeGraphDB) CreateNode(_ context.Context, _ string, n graphdb.Node) error {
	key := n.Label + "/" + n.ID
	if f.nodeIDs == nil {
		f.nodeIDs = make(map[string]bool)
	}
	if f.nodeIDs[key] {
		return fmt.Errorf("create node %s: %w", key, graphdb.ErrNodeExists)
	}
	f.nodeIDs[key] = true
	f.nodes = append(f.nodes, n)
	return nil
}

func (f *fakeGraphDB) CreateEdge(_ context.Context, _, sourceID, targetID string, e graphdb.Edge) error {
	f.captured = append(f.captured, capturedEdge{SourceID: sourceID, TargetID: targetID, Edge: e})
	return nil
}

func (f *fakeGraphDB) QueryRelationships(_ context.Context, _ string, _ graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	return nil, nil
}

// fakeStorage serves pre-loaded ControlResult JSON files.
type fakeStorage struct {
	storage.Provider
	files map[string][]byte
}

func (f *fakeStorage) List(_ context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	var result []storage.ObjectMetadata
	for key := range f.files {
		if strings.HasPrefix(key, prefix) {
			result = append(result, storage.ObjectMetadata{Key: key})
		}
	}
	return result, nil
}

func (f *fakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.files[key]
	if !ok {
		return nil, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

var _ = Describe("GraphMaterializer", func() {
	var (
		graph *fakeGraphDB
		store *fakeStorage
		cfg   config.ArtifactsConfig
		m     *artifacts.GraphMaterializer
	)

	BeforeEach(func() {
		graph = &fakeGraphDB{}
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
		crJSON, _ := json.Marshal(cr)

		store = &fakeStorage{
			files: map[string][]byte{
				"test-tenant/analysis/artifacts/job-1/ac-1.json": crJSON,
			},
		}

		m = artifacts.NewGraphMaterializer(graph, store, cfg)
	})

	It("creates ArtifactType nodes", func() {
		err := m.Materialize(context.Background(), "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		typeNodes := 0
		for _, n := range graph.nodes {
			if n.Label == "ArtifactType" {
				typeNodes++
			}
		}
		Expect(typeNodes).To(Equal(9))
	})

	It("creates Artifact node with dedup_generation 0", func() {
		err := m.Materialize(context.Background(), "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		var artifactNode *graphdb.Node
		for i, n := range graph.nodes {
			if n.Label == "Artifact" {
				artifactNode = &graph.nodes[i]
				break
			}
		}
		Expect(artifactNode).NotTo(BeNil())
		Expect(artifactNode.Properties["dedup_generation"]).To(Equal(0))
		Expect(artifactNode.Properties["name"]).To(Equal("access control policy"))
		Expect(artifactNode.Properties["confidence"]).To(Equal(1.0))
	})

	It("creates DEMANDS and IS_TYPE edges", func() {
		err := m.Materialize(context.Background(), "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		var demandsCount, isTypeCount int
		for _, c := range graph.captured {
			switch c.Edge.Label {
			case "DEMANDS":
				demandsCount++
				Expect(c.SourceID).To(Equal("ac-1"))
			case "IS_TYPE":
				isTypeCount++
				Expect(c.TargetID).To(Equal("policy"))
			}
		}
		Expect(demandsCount).To(Equal(1))
		Expect(isTypeCount).To(Equal(1))
	})

	It("generates stable artifact IDs from content hash", func() {
		err := m.Materialize(context.Background(), "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		var id1 string
		for _, n := range graph.nodes {
			if n.Label == "Artifact" {
				id1 = n.ID
				break
			}
		}

		// Re-materialize — should produce the same ID.
		graph2 := &fakeGraphDB{}
		m2 := artifacts.NewGraphMaterializer(graph2, store, cfg)
		err = m2.Materialize(context.Background(), "test-tenant", "job-1")
		Expect(err).NotTo(HaveOccurred())

		var id2 string
		for _, n := range graph2.nodes {
			if n.Label == "Artifact" {
				id2 = n.ID
				break
			}
		}
		Expect(id2).To(Equal(id1))
	})
})
