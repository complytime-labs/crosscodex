//go:build integration

package artifacts_test

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

var _ = Describe("Reconcile on Apache AGE", Ordered, func() {
	var (
		ctx          context.Context
		db           *sql.DB
		g            graphdb.GraphDB
		store        *fakeVerdictStore
		cleanup      testspecs.TestCleanup
		tenantID     string
		opts         artifacts.ReconcileOptions
		graphCreated bool
	)

	BeforeAll(func() {
		ctx = context.Background()
		db, cleanup = testspecs.SetupTestDatabase()
		var err error
		g, err = agedriver.New(db)
		Expect(err).NotTo(HaveOccurred())
		// A per-run tenant keeps reruns against a persistent database independent.
		tenantID = "reconcile-int-" + uuid.New().String()
		Expect(g.CreateGraph(ctx, tenantID)).To(Succeed())
		graphCreated = true
		store = newFakeVerdictStore()
		// ChunkSize 2 forces several BulkCreateEdges transactions.
		opts = artifacts.ReconcileOptions{MaxEdges: 100, ChunkSize: 2, Now: reconcileNow}

		seedArtifact(ctx, g, tenantID, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, tenantID, "c2", "c2__art_0", "access control policy", "document")
		seedArtifact(ctx, g, tenantID, "c3", "c3__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, tenantID, "c4", "c4__art_0", "Access Control Policy", "record")
	})

	AfterAll(func() {
		if db != nil && graphCreated {
			// Mirrors agedriver's graph naming (crosscodex_<tenant>). agedriver.GraphName
			// exists only in agedriver's export_test.go, so this package cannot use it.
			_, err := db.ExecContext(ctx, "SELECT ag_catalog.drop_graph($1, true)", "crosscodex_"+tenantID)
			Expect(err).NotTo(HaveOccurred())
		}
		if cleanup != nil {
			cleanup()
		}
	})

	It("writes groups, memberships and matches in chunks", func() {
		res, err := artifacts.Reconcile(ctx, g, store, tenantID, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{
			Artifacts: 4, Groups: 3, NewGroups: 3, NewMemberships: 4, Matches: 1, NewMatches: 1,
		}))
		Expect(edgesOf(ctx, g, tenantID, "Artifact", "MEMBER_OF", "ArtifactGroup")).To(HaveLen(4))
		Expect(edgesOf(ctx, g, tenantID, "ArtifactGroup", "SAME_AS", "ArtifactGroup")).To(HaveLen(1))
	})

	It("writes nothing on a second run", func() {
		res, err := artifacts.Reconcile(ctx, g, store, tenantID, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.ReconcileResult{Artifacts: 4, Groups: 3, Matches: 1}))
	})

	It("drops a superseded membership edge without error and writes nothing", func() {
		gid := graphdb.DerivedID("artifact-group", "document", "access policy")
		hidden := graphdb.DerivedID("member-of", "c3__art_0", gid)
		_, err := g.SupersedeFact(ctx, tenantID, graphdb.SupersedeRequest{EdgeID: hidden, SupersededAt: reconcileNow.Add(time.Hour)})
		Expect(err).NotTo(HaveOccurred())

		// The superseded edge still owns its ID, so the write drops it and
		// completes the rest without error.
		res, err := artifacts.Reconcile(ctx, g, store, tenantID, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.NewMemberships).To(Equal(0))
	})
})
