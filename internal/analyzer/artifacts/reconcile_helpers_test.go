package artifacts_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

var reconcileNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// seedArtifact writes what internal/graph's live writer writes for one
// artifact: the Control node (if absent), the Artifact node and its DEMANDS
// edge. An empty typ omits the type property.
func seedArtifact(ctx context.Context, g graphdb.GraphDB, tenantID, controlID, artID, name, typ string) {
	err := g.CreateNode(ctx, tenantID, graphdb.Node{ID: controlID, Label: "Control", ValidFrom: reconcileNow})
	if !errors.Is(err, graphdb.ErrNodeExists) {
		Expect(err).NotTo(HaveOccurred())
	}
	props := map[string]any{"name": name}
	if typ != "" {
		props["type"] = typ
	}
	Expect(g.CreateNode(ctx, tenantID, graphdb.Node{ID: artID, Label: "Artifact", ValidFrom: reconcileNow, Properties: props})).To(Succeed())
	Expect(g.CreateEdge(ctx, tenantID, controlID, artID, graphdb.Edge{
		ID: graphdb.DerivedID("demands", controlID, artID), Label: "DEMANDS", ValidFrom: reconcileNow,
	})).To(Succeed())
}

// edgesOf returns the current relationships source -label-> target.
func edgesOf(ctx context.Context, g graphdb.GraphDB, tenantID, source, label, target string) []graphdb.Relationship {
	rels, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{SourceLabel: source, EdgeLabel: label, TargetLabel: target})
	Expect(err).NotTo(HaveOccurred())
	return rels
}
