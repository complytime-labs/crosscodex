//go:build !integration

package artifacts_test

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

const reconcileTenant = "reconcile-test"

// reconcileFaultyGraph injects errors into, and counts calls to, the graph
// methods Reconcile uses for reading and bulk writing.
type reconcileFaultyGraph struct {
	graphdb.GraphDB
	queryErr      error
	bulkErr       error
	bulkCalls     int
	createNodeErr error
}

func (f *reconcileFaultyGraph) QueryRelationships(ctx context.Context, tenantID string, q graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.GraphDB.QueryRelationships(ctx, tenantID, q)
}

func (f *reconcileFaultyGraph) BulkCreateEdges(ctx context.Context, tenantID string, edges []graphdb.BulkEdge) ([]string, error) {
	f.bulkCalls++
	if f.bulkErr != nil {
		return nil, f.bulkErr
	}
	return f.GraphDB.BulkCreateEdges(ctx, tenantID, edges)
}

func (f *reconcileFaultyGraph) CreateNode(ctx context.Context, tenantID string, node graphdb.Node) error {
	if f.createNodeErr != nil {
		return f.createNodeErr
	}
	return f.GraphDB.CreateNode(ctx, tenantID, node)
}

var _ = Describe("Reconcile", func() {
	var (
		ctx  context.Context
		g    graphdb.GraphDB
		opts artifacts.ReconcileOptions
	)

	groupID := func(typ, normalizedName string) string {
		return graphdb.DerivedID("artifact-group", typ, normalizedName)
	}

	BeforeEach(func() {
		ctx = context.Background()
		g = memdriver.New()
		Expect(g.CreateGraph(ctx, reconcileTenant)).To(Succeed())
		opts = artifacts.ReconcileOptions{MaxEdges: 1000, ChunkSize: 100, Now: reconcileNow}
	})

	It("groups identical normalized names of one type across controls", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "the access control policy.", "Document")

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 2, Groups: 1, NewGroups: 1, NewMemberships: 2}))

		want := groupID("document", "access control policy")
		rels := edgesOf(ctx, g, reconcileTenant, "Artifact", "MEMBER_OF", "ArtifactGroup")
		Expect(rels).To(HaveLen(2))
		for _, r := range rels {
			Expect(r.Target.ID).To(Equal(want))
			Expect(r.Target.Properties).To(HaveKeyWithValue("name", "access control policy"))
			Expect(r.Target.Properties).To(HaveKeyWithValue("type", "document"))
			Expect(r.Target.CreatedBy).To(Equal("artifact-reconciler"))
			Expect(r.Target.CreationMethod).To(Equal("exact_name"))
			Expect(r.Edge.ID).To(Equal(graphdb.DerivedID("member-of", r.Source.ID, want)))
			Expect(r.Edge.DeterminedBy).To(Equal("artifact-reconciler"))
			Expect(r.Edge.DeterminationType).To(Equal("exact_name"))
			Expect(r.Edge.Confidence).To(Equal(1.0))
		}
	})

	It("links overlapping names of one type with a single SAME_AS edge from the lower group ID", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c3", "c3__art_0", "Access Control Policy", "record")

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Groups).To(Equal(3))
		Expect(res.Matches).To(Equal(1))
		Expect(res.NewMatches).To(Equal(1))

		rels := edgesOf(ctx, g, reconcileTenant, "ArtifactGroup", "SAME_AS", "ArtifactGroup")
		Expect(rels).To(HaveLen(1))
		r := rels[0]
		a, b := groupID("document", "access control policy"), groupID("document", "access policy")
		low, high := min(a, b), max(a, b)
		Expect(r.Source.ID).To(Equal(low))
		Expect(r.Target.ID).To(Equal(high))
		Expect(r.Edge.ID).To(Equal(graphdb.DerivedID("same-as", artifacts.ReconcileAlgorithmVersion, low, high)))
		Expect(r.Edge.DeterminedBy).To(Equal("artifact-reconciler"))
		Expect(r.Edge.DeterminationType).To(Equal("token_overlap"))
		Expect(r.Edge.Confidence).To(Equal(1.0))
		Expect(r.Edge.Properties).To(HaveKeyWithValue("similarity_score", 1.0))
		Expect(r.Edge.Properties).To(HaveKeyWithValue("algorithm_version", artifacts.ReconcileAlgorithmVersion))
		Expect(r.Edge.Properties).To(HaveKeyWithValue("matched_at", graphdb.FormatTime(reconcileNow)))
	})

	It("does not link names below the threshold", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Audit Log Retention Plan", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Audit Report", "document")

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Matches).To(Equal(0))
		Expect(edgesOf(ctx, g, reconcileTenant, "ArtifactGroup", "SAME_AS", "ArtifactGroup")).To(BeEmpty())
	})

	It("skips artifacts without a usable name or type", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "the", "document")
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_1", "Access Policy", "")
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_2", "Access Policy", "   ")

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Skipped: 3}))
	})

	It("skips an artifact whose type is not a string", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		Expect(g.CreateNode(ctx, reconcileTenant, graphdb.Node{ID: "c1__art_1", Label: "Artifact", ValidFrom: reconcileNow,
			Properties: map[string]any{"name": "Access Policy", "type": 3}})).To(Succeed())
		Expect(g.CreateEdge(ctx, reconcileTenant, "c1", "c1__art_1", graphdb.Edge{
			ID: graphdb.DerivedID("demands", "c1", "c1__art_1"), Label: "DEMANDS", ValidFrom: reconcileNow,
		})).To(Succeed())

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 1, Skipped: 1, Groups: 1, NewGroups: 1, NewMemberships: 1}))
	})

	It("groups identical artifacts under the same control", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_1", "access policy", "document")

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 2, Groups: 1, NewGroups: 1, NewMemberships: 2}))
	})

	It("ignores superseded artifacts", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		_, err := g.SupersedeFact(ctx, reconcileTenant, graphdb.SupersedeRequest{NodeID: "c2__art_0", SupersededAt: reconcileNow})
		Expect(err).NotTo(HaveOccurred())

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Artifacts).To(Equal(1))
		Expect(res.NewMemberships).To(Equal(1))
	})

	It("writes nothing on a second run over the same graph", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		_, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 2, Groups: 2, Matches: 1}))
		Expect(edgesOf(ctx, g, reconcileTenant, "Artifact", "MEMBER_OF", "ArtifactGroup")).To(HaveLen(2))
		Expect(edgesOf(ctx, g, reconcileTenant, "ArtifactGroup", "SAME_AS", "ArtifactGroup")).To(HaveLen(1))
	})

	It("adds only the new artifact's membership on a later run", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		_, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 2, Groups: 1, NewMemberships: 1}))
	})

	It("reports a dry run without writing", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		opts.DryRun = true

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 2, Groups: 2, NewGroups: 2, NewMemberships: 2, Matches: 1, NewMatches: 1}))
		Expect(edgesOf(ctx, g, reconcileTenant, "Artifact", "MEMBER_OF", "ArtifactGroup")).To(BeEmpty())
		_, err = g.GetNode(ctx, reconcileTenant, groupID("document", "access policy"))
		Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
	})

	It("fails before writing when new SAME_AS edges exceed MaxEdges", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c3", "c3__art_0", "Access Review Policy", "document")
		opts.MaxEdges = 2

		_, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).To(MatchError(ContainSubstring("3 new SAME_AS edges exceed the limit of 2")))
		Expect(edgesOf(ctx, g, reconcileTenant, "Artifact", "MEMBER_OF", "ArtifactGroup")).To(BeEmpty())
	})

	It("writes in chunks of ChunkSize", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		fg := &reconcileFaultyGraph{GraphDB: g}
		opts.ChunkSize = 1

		res, err := artifacts.Reconcile(ctx, fg, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(fg.bulkCalls).To(Equal(3), "2 MEMBER_OF + 1 SAME_AS, one edge per call")
		Expect(res.NewMemberships + res.NewMatches).To(Equal(3))
	})

	It("drops an edge the stored-edge query cannot see and writes the rest of its chunk", func() {
		// A superseded MEMBER_OF edge is hidden from QueryRelationships but
		// still owns its ID, so BulkCreateEdges reports ErrEdgeExists for it.
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		gid := groupID("document", "access policy")
		Expect(g.CreateNode(ctx, reconcileTenant, graphdb.Node{ID: gid, Label: "ArtifactGroup", ValidFrom: reconcileNow,
			Properties: map[string]any{"name": "access policy", "type": "document"}})).To(Succeed())
		hidden := graphdb.DerivedID("member-of", "c1__art_0", gid)
		Expect(g.CreateEdge(ctx, reconcileTenant, "c1__art_0", gid, graphdb.Edge{ID: hidden, Label: "MEMBER_OF", ValidFrom: reconcileNow})).To(Succeed())
		_, err := g.SupersedeFact(ctx, reconcileTenant, graphdb.SupersedeRequest{EdgeID: hidden, SupersededAt: reconcileNow})
		Expect(err).NotTo(HaveOccurred())

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.NewMemberships).To(Equal(1))
		rels := edgesOf(ctx, g, reconcileTenant, "Artifact", "MEMBER_OF", "ArtifactGroup")
		Expect(rels).To(HaveLen(1))
		Expect(rels[0].Source.ID).To(Equal("c2__art_0"))
	})

	DescribeTable("rejects invalid options",
		func(mutate func(*artifacts.ReconcileOptions)) {
			mutate(&opts)
			_, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
			Expect(err).To(MatchError(ContainSubstring("reconcile artifacts: invalid options")))
		},
		Entry("zero MaxEdges", func(o *artifacts.ReconcileOptions) { o.MaxEdges = 0 }),
		Entry("zero ChunkSize", func(o *artifacts.ReconcileOptions) { o.ChunkSize = 0 }),
		Entry("zero Now", func(o *artifacts.ReconcileOptions) { o.Now = time.Time{} }),
	)

	It("wraps a read failure with the failing step", func() {
		boom := errors.New("boom")
		_, err := artifacts.Reconcile(ctx, &reconcileFaultyGraph{GraphDB: g, queryErr: boom}, reconcileTenant, opts)
		Expect(err).To(MatchError(boom))
		Expect(err).To(MatchError(ContainSubstring("reconcile artifacts: load artifacts")))
	})

	It("wraps a write failure with the failing step", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		boom := errors.New("boom")
		_, err := artifacts.Reconcile(ctx, &reconcileFaultyGraph{GraphDB: g, bulkErr: boom}, reconcileTenant, opts)
		Expect(err).To(MatchError(boom))
		Expect(err).To(MatchError(ContainSubstring("reconcile artifacts: write MEMBER_OF edges")))
	})

	It("counts only the groups it actually created", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Audit Plan", "document")
		pregroup := groupID("document", "access policy")
		Expect(g.CreateNode(ctx, reconcileTenant, graphdb.Node{ID: pregroup, Label: "ArtifactGroup", ValidFrom: reconcileNow,
			Properties: map[string]any{"name": "access policy", "type": "document"}})).To(Succeed())

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.NewGroups).To(Equal(1))
	})

	It("reports zero new memberships and matches when the write fails before writing anything", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Control Policy", "document")
		boom := errors.New("boom")
		fg := &reconcileFaultyGraph{GraphDB: g, bulkErr: boom}

		res, err := artifacts.Reconcile(ctx, fg, reconcileTenant, opts)
		Expect(err).To(MatchError(boom))
		Expect(res.NewMemberships).To(Equal(0))
		Expect(res.NewMatches).To(Equal(0))
	})

	It("wraps a CreateNode failure and reports no new memberships", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		boom := errors.New("boom")
		fg := &reconcileFaultyGraph{GraphDB: g, createNodeErr: boom}

		res, err := artifacts.Reconcile(ctx, fg, reconcileTenant, opts)
		Expect(err).To(MatchError(boom))
		Expect(err).To(MatchError(ContainSubstring("create ArtifactGroup")))
		Expect(res.NewMemberships).To(Equal(0))
	})

	It("does not retry a BulkEdgeError that is not ErrEdgeExists", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		fg := &reconcileFaultyGraph{GraphDB: g, bulkErr: &graphdb.BulkEdgeError{Index: 0, Err: graphdb.ErrNodeNotFound}}

		_, err := artifacts.Reconcile(ctx, fg, reconcileTenant, opts)
		Expect(err).To(MatchError(graphdb.ErrNodeNotFound))
		Expect(fg.bulkCalls).To(Equal(1))
	})

	It("drops several hidden edges inside one chunk and writes the rest", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c3", "c3__art_0", "Access Policy", "document")
		gid := groupID("document", "access policy")
		Expect(g.CreateNode(ctx, reconcileTenant, graphdb.Node{ID: gid, Label: "ArtifactGroup", ValidFrom: reconcileNow,
			Properties: map[string]any{"name": "access policy", "type": "document"}})).To(Succeed())
		for _, artID := range []string{"c1__art_0", "c2__art_0"} {
			hidden := graphdb.DerivedID("member-of", artID, gid)
			Expect(g.CreateEdge(ctx, reconcileTenant, artID, gid, graphdb.Edge{ID: hidden, Label: "MEMBER_OF", ValidFrom: reconcileNow})).To(Succeed())
			_, err := g.SupersedeFact(ctx, reconcileTenant, graphdb.SupersedeRequest{EdgeID: hidden, SupersededAt: reconcileNow})
			Expect(err).NotTo(HaveOccurred())
		}

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.NewMemberships).To(Equal(1))
		rels := edgesOf(ctx, g, reconcileTenant, "Artifact", "MEMBER_OF", "ArtifactGroup")
		Expect(rels).To(HaveLen(1))
		Expect(rels[0].Source.ID).To(Equal("c3__art_0"))
	})

	It("writes MEMBER_OF and SAME_AS in separate chunked calls", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c3", "c3__art_0", "Access Control Policy", "record")
		fg := &reconcileFaultyGraph{GraphDB: g}
		opts.ChunkSize = 2

		res, err := artifacts.Reconcile(ctx, fg, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(fg.bulkCalls).To(Equal(3), "2 MEMBER_OF chunks (2+1) + 1 SAME_AS chunk")
		Expect(res.NewMemberships).To(Equal(3))
		Expect(res.NewMatches).To(Equal(1))
	})

	It("succeeds when new SAME_AS edges equal MaxEdges", func() {
		seedArtifact(ctx, g, reconcileTenant, "c1", "c1__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c2", "c2__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, reconcileTenant, "c3", "c3__art_0", "Access Review Policy", "document")
		opts.MaxEdges = 3

		res, err := artifacts.Reconcile(ctx, g, reconcileTenant, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.NewMatches).To(Equal(3))
	})
})
