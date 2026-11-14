package graph_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// uriResolver serves each pg:// URI its own data and counts the calls per
// URI. A URI with no data resolves as the PG resolver does for a job without
// that analyzer's row: an error wrapping sql.ErrNoRows.
type uriResolver struct {
	mu    sync.Mutex
	data  map[string][]byte
	errs  map[string]error
	calls map[string]int
}

func (r *uriResolver) Scheme() string { return "pg" }

func (r *uriResolver) Resolve(_ context.Context, ref graph.ExportResourceRef) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[ref.URI]++
	if err := r.errs[ref.URI]; err != nil {
		return nil, err
	}
	data, ok := r.data[ref.URI]
	if !ok {
		return nil, fmt.Errorf("pg resolve %s: %w", ref.URI, sql.ErrNoRows)
	}
	return data, nil
}

func (r *uriResolver) callsTo(uri string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[uri]
}

var _ = Describe("Subscriber critique verdicts", func() {
	const (
		tenantID = "test-tenant"
		jobID    = "job-1"
	)
	uri := func(analyzer string) string { return "pg://results/" + jobID + "/" + analyzer }

	var (
		ctx      context.Context
		gdb      graphdb.GraphDB
		resolver *uriResolver
		svc      *graph.Service
	)

	put := func(analyzer string, v any) {
		data, err := json.Marshal(v)
		Expect(err).NotTo(HaveOccurred())
		resolver.data[uri(analyzer)] = data
	}

	deliver := func(analyzer string, metadata map[string]string) error {
		event := map[string]any{"analyzer": analyzer, "job_id": jobID, "stage": "completed"}
		if metadata != nil {
			event["metadata"] = metadata
		}
		data, err := json.Marshal(event)
		Expect(err).NotTo(HaveOccurred())
		return graph.ExportHandleEvent(svc, ctx, &natsbus.Message{
			Subject: "crosscodex.pipeline." + tenantID + "." + jobID + ".stage.completed",
			Data:    data,
		})
	}
	deferred := map[string]string{results.GraphDeferredMetadataKey: "true", "total_count": "3"}

	edges := func(label string) map[[2]string]graphdb.Edge {
		rels, err := gdb.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: label})
		Expect(err).NotTo(HaveOccurred())
		out := map[[2]string]graphdb.Edge{}
		for _, r := range rels {
			out[[2]string{r.Source.ID, r.Target.ID}] = r.Edge
		}
		return out
	}

	match := func(src, tgt string) results.SemanticMatchResult {
		return results.SemanticMatchResult{SourceID: src, TargetID: tgt, RelationshipType: "EQUIVALENT", Confidence: 0.9}
	}
	prerequisite := func(src, tgt string) results.RequiresResult {
		return results.RequiresResult{SourceID: src, TargetID: tgt, Confidence: 1, Unanimous: true, ValidVotes: 3, TotalVotes: 3, Models: []string{"m"}}
	}
	critique := func(kind, src, tgt, outcome string, upheld, rejected int, reasons ...string) results.CritiqueResult {
		return results.CritiqueResult{Kind: kind, SourceControlID: src, TargetControlID: tgt, Upheld: upheld, Rejected: rejected,
			Outcome: outcome, Vetoed: outcome == results.CritiqueVetoed, Reasons: reasons}
	}

	BeforeEach(func() {
		ctx = context.Background()
		gdb = memdriver.New()
		Expect(gdb.CreateGraph(ctx, tenantID)).To(Succeed())
		for _, id := range []string{"a", "b", "c", "d"} {
			Expect(gdb.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now().UTC()})).To(Succeed())
		}
		resolver = &uriResolver{data: map[string][]byte{}, errs: map[string]error{}, calls: map[string]int{}}
		registry := graph.NewResolverRegistry()
		registry.Register(resolver)
		svc = graph.New(gdb, &mockVectorDB{}, nil, graph.WithResolverRegistry(registry))

		// a->b: relationship vetoed, requires upheld. c->d: relationship
		// split, requires vetoed. b->c: no critique result for either.
		put(results.RelationshipAnalyzer, []results.SemanticMatchResult{match("a", "b"), match("c", "d"), match("b", "c")})
		put(results.RequiresAnalyzer, []results.RequiresResult{prerequisite("a", "b"), prerequisite("c", "d"), prerequisite("b", "c")})
		put(results.CritiqueAnalyzer, []results.CritiqueResult{
			critique(results.RelationshipAnalyzer, "a", "b", results.CritiqueVetoed, 0, 3,
				"panel-1 s0 REJECT: x", "panel-2 s0 REJECT: y", "panel-3 s0 REJECT: z"),
			critique(results.RelationshipAnalyzer, "c", "d", results.CritiqueSplit, 2, 1,
				"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: scope differs", "panel-3 s0 UPHOLD: ok"),
			critique(results.RequiresAnalyzer, "a", "b", results.CritiqueUpheld, 3, 0,
				"panel-1 s0 UPHOLD: ok", "panel-2 s0 UPHOLD: ok", "panel-3 s0 UPHOLD: ok"),
			critique(results.RequiresAnalyzer, "c", "d", results.CritiqueVetoed, 0, 3,
				"panel-1 s0 REJECT: x", "panel-2 s0 REJECT: y", "panel-3 s0 REJECT: z"),
		})
	})

	DescribeTable("a deferred stage event writes no edges and resolves nothing",
		func(analyzer, label string) {
			Expect(deliver(analyzer, deferred)).To(Succeed())
			Expect(edges(label)).To(BeEmpty())
			Expect(resolver.callsTo(uri(analyzer))).To(BeZero())
		},
		Entry("relationship", results.RelationshipAnalyzer, results.SemanticMatchEdgeLabel),
		Entry("requires", results.RequiresAnalyzer, "REQUIRES"),
	)

	DescribeTable("a stage event not marked deferred writes every edge with no critique properties",
		func(analyzer, label string, metadata map[string]string) {
			Expect(deliver(analyzer, metadata)).To(Succeed())
			got := edges(label)
			Expect(got).To(HaveLen(3))
			for _, e := range got {
				Expect(e.Properties).NotTo(HaveKey(results.CritiqueUpheldProperty))
				Expect(e.Properties).NotTo(HaveKey(results.CritiqueNoteProperty))
			}
		},
		Entry("relationship without metadata", results.RelationshipAnalyzer, results.SemanticMatchEdgeLabel, nil),
		Entry("requires with graph_deferred false", results.RequiresAnalyzer, "REQUIRES", map[string]string{results.GraphDeferredMetadataKey: "false"}),
	)

	Describe("the critique stage event", func() {
		It("writes no SEMANTIC_MATCH edge for a relationship-vetoed pair and keeps its upheld REQUIRES edge", func() {
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			Expect(edges(results.SemanticMatchEdgeLabel)).NotTo(HaveKey([2]string{"a", "b"}))
			Expect(edges("REQUIRES")).To(HaveKey([2]string{"a", "b"}))
		})

		It("writes no REQUIRES edge for a requires-vetoed pair and keeps its SEMANTIC_MATCH edge", func() {
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			Expect(edges("REQUIRES")).NotTo(HaveKey([2]string{"c", "d"}))
			Expect(edges(results.SemanticMatchEdgeLabel)).To(HaveKey([2]string{"c", "d"}))
		})

		It("puts the vote counts and the split note on a split pair's edge", func() {
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			e := edges(results.SemanticMatchEdgeLabel)[[2]string{"c", "d"}]
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueUpheldProperty, float64(2)))
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueRejectedProperty, float64(1)))
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueNoteProperty, "critique split: 1 of 3 rejected — panel-2 s0: scope differs"))
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueOutcomeProperty, "split"))
		})

		It("puts the vote counts but no note on an upheld pair's REQUIRES edge", func() {
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			e := edges("REQUIRES")[[2]string{"a", "b"}]
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueUpheldProperty, float64(3)))
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueRejectedProperty, float64(0)))
			Expect(e.Properties).NotTo(HaveKey(results.CritiqueNoteProperty))
			Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueOutcomeProperty, "upheld"))
		})

		DescribeTable("keeps a pair the critique rejected without veto, flagged with its outcome and note",
			func(label string) {
				put(results.CritiqueAnalyzer, []results.CritiqueResult{
					critique(results.RelationshipAnalyzer, "b", "c", results.CritiqueRejected, 1, 2,
						"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: too broad", "panel-3 s0 REJECT: no"),
					critique(results.RequiresAnalyzer, "b", "c", results.CritiqueRejected, 1, 2,
						"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: too broad", "panel-3 s0 REJECT: no"),
				})
				Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
				e, ok := edges(label)[[2]string{"b", "c"}]
				Expect(ok).To(BeTrue(), "%s edge b->c", label)
				Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueOutcomeProperty, "rejected"))
				Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueRejectedProperty, float64(2)))
				Expect(e.Properties).To(HaveKeyWithValue(results.CritiqueNoteProperty, "critique rejected: 2 of 3 rejected — panel-2 s0: too broad"))
			},
			Entry("SEMANTIC_MATCH", results.SemanticMatchEdgeLabel),
			Entry("REQUIRES", "REQUIRES"),
		)

		It("keeps a pair the critique has no result for, without critique properties", func() {
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			for _, label := range []string{results.SemanticMatchEdgeLabel, "REQUIRES"} {
				e, ok := edges(label)[[2]string{"b", "c"}]
				Expect(ok).To(BeTrue(), "%s edge b->c", label)
				Expect(e.Properties).NotTo(HaveKey(results.CritiqueUpheldProperty))
			}
		})

		It("writes the surviving edges after deferred stage events, once, when the critique event is redelivered", func() {
			Expect(deliver(results.RelationshipAnalyzer, deferred)).To(Succeed())
			Expect(deliver(results.RequiresAnalyzer, deferred)).To(Succeed())
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			Expect(edges(results.SemanticMatchEdgeLabel)).To(HaveLen(2))
			Expect(edges("REQUIRES")).To(HaveLen(2))
		})

		It("writes the relationship edges of a job that did not run the requires analyzer", func() {
			delete(resolver.data, uri(results.RequiresAnalyzer))
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			Expect(edges(results.SemanticMatchEdgeLabel)).To(HaveLen(2))
			Expect(edges("REQUIRES")).To(BeEmpty())
		})

		It("writes every edge when the critique reviewed nothing", func() {
			put(results.CritiqueAnalyzer, nil)
			Expect(deliver(results.CritiqueAnalyzer, nil)).To(Succeed())
			Expect(edges(results.SemanticMatchEdgeLabel)).To(HaveLen(3))
			Expect(edges("REQUIRES")).To(HaveLen(3))
		})

		DescribeTable("fails and writes no edge when a result cannot be read",
			func(prepare func(), wantMsg string) {
				prepare()
				err := deliver(results.CritiqueAnalyzer, nil)
				Expect(err).To(MatchError(ContainSubstring(wantMsg)))
				Expect(edges(results.SemanticMatchEdgeLabel)).To(BeEmpty())
				Expect(edges("REQUIRES")).To(BeEmpty())
			},
			Entry("the critique result is corrupt", func() {
				resolver.data[uri(results.CritiqueAnalyzer)] = []byte(`{not json`)
			}, "unmarshal critique results"),
			Entry("a critique vote count is impossible", func() {
				resolver.data[uri(results.CritiqueAnalyzer)] = []byte(`[{"kind":"relationship","source_control_id":"a","target_control_id":"b","rejected":-1}]`)
			}, "gives pair a -> b an impossible vote count"),
			Entry("reading the relationship result fails", func() {
				resolver.errs[uri(results.RelationshipAnalyzer)] = errors.New("connection lost")
			}, "connection lost"),
			Entry("the requires result is corrupt", func() {
				resolver.data[uri(results.RequiresAnalyzer)] = []byte(`{not json`)
			}, "unmarshal requires results"),
		)
	})
})
