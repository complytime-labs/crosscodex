//go:build integration

package artifacts_test

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("Adjudicate on Apache AGE", Ordered, func() {
	var (
		ctx          context.Context
		db           *sql.DB
		g            graphdb.GraphDB
		cleanup      testspecs.TestCleanup
		tenantID     string
		graphCreated bool
		store        *fakeVerdictStore
		adj          *artifacts.Adjudicator
		opts         artifacts.AdjudicateOptions
		accessLow    string
		accessHigh   string
		passLow      string
		passHigh     string
	)

	candidateOf := func(low, high string) string {
		return graphdb.DerivedID("same-as", artifacts.ReconcileAlgorithmVersion, low, high)
	}

	BeforeAll(func() {
		ctx = context.Background()
		db, cleanup = testspecs.SetupTestDatabase()
		var err error
		g, err = agedriver.New(db)
		Expect(err).NotTo(HaveOccurred())
		tenantID = "adjudicate-int-" + uuid.New().String()
		Expect(g.CreateGraph(ctx, tenantID)).To(Succeed())
		graphCreated = true

		seedArtifact(ctx, g, tenantID, "c1", "c1__art_0", "Access Control Policy", "document")
		seedArtifact(ctx, g, tenantID, "c2", "c2__art_0", "Access Policy", "document")
		seedArtifact(ctx, g, tenantID, "c3", "c3__art_0", "Password Policy", "document")
		seedArtifact(ctx, g, tenantID, "c4", "c4__art_0", "Password Policy Standard", "document")
		accessLow = graphdb.DerivedID("artifact-group", "document", "access control policy")
		accessHigh = graphdb.DerivedID("artifact-group", "document", "access policy")
		passLow = graphdb.DerivedID("artifact-group", "document", "password policy")
		passHigh = graphdb.DerivedID("artifact-group", "document", "password policy standard")

		store = newFakeVerdictStore()
		res, err := artifacts.Reconcile(ctx, g, store, tenantID, artifacts.ReconcileOptions{MaxEdges: 100, ChunkSize: 2, Now: reconcileNow})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.NewMatches).To(Equal(2))

		llm := &scriptedLLM{reply: func(req *llmclient.CompletionRequest) (string, error) {
			if strings.Contains(req.Messages[len(req.Messages)-1].Content, "access") {
				return answer("YES"), nil
			}
			return answer("NO"), nil
		}}
		cfg := config.ArtifactAdjudicationConfig{Enabled: true, Models: []string{"m1"}, SamplesPerModel: 1,
			MaxTokens: 200, ConsensusThreshold: 0.6, MaxErrorRate: 0, MaxAttempts: 3}
		adj = artifacts.NewAdjudicator(g, store, llm, &fixedPrompts{}, cfg)
		now := reconcileNow.Add(time.Hour)
		opts = artifacts.AdjudicateOptions{MaxPairs: 10, Now: func() time.Time { return now }}
	})

	AfterAll(func() {
		if db != nil && graphCreated {
			// Mirrors agedriver's graph naming (crosscodex_<tenant>), as in reconcile_integration_bdd_test.go.
			_, err := db.ExecContext(ctx, "SELECT ag_catalog.drop_graph($1, true)", "crosscodex_"+tenantID)
			Expect(err).NotTo(HaveOccurred())
		}
		if cleanup != nil {
			cleanup()
		}
	})

	superseded := func(edgeID string) bool {
		e, err := g.GetEdge(ctx, tenantID, edgeID)
		Expect(err).NotTo(HaveOccurred())
		return e.ValidTo != nil
	}

	It("confirms one pair and rejects the other", func() {
		res, err := adj.Run(ctx, tenantID, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{Leased: 2, Confirmed: 1, Rejected: 1}))

		live := edgesOf(ctx, g, tenantID, "ArtifactGroup", "SAME_AS", "ArtifactGroup")
		Expect(live).To(HaveLen(1))
		e := live[0].Edge
		Expect(e.ID).To(Equal(graphdb.DerivedID("same-as", "llm_panel", accessLow, accessHigh)))
		Expect(e.DeterminationType).To(Equal("llm_panel"))
		Expect(e.DeterminedBy).To(Equal("artifact-adjudicator"))
		Expect(e.Supersedes).To(Equal(candidateOf(accessLow, accessHigh)))
		Expect(e.Properties).To(HaveKeyWithValue("prompt_name", "artifact_same_as"))
		Expect(superseded(candidateOf(accessLow, accessHigh))).To(BeTrue())
		Expect(superseded(candidateOf(passLow, passHigh))).To(BeTrue())
	})

	It("re-projects idempotently", func() {
		store.resetProjected(tenantID)
		res, err := adj.Run(ctx, tenantID, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(artifacts.AdjudicateResult{Recovered: 2}))
		Expect(edgesOf(ctx, g, tenantID, "ArtifactGroup", "SAME_AS", "ArtifactGroup")).To(HaveLen(1))
	})

	It("keeps the reconciler from re-planning closed pairs", func() {
		res, err := artifacts.Reconcile(ctx, g, store, tenantID, artifacts.ReconcileOptions{MaxEdges: 100, ChunkSize: 2, Now: reconcileNow})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Closed).To(Equal(2))
		Expect(res.NewMatches).To(Equal(0))
	})
})
