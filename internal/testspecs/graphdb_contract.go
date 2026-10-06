package testspecs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// GraphDBSetup returns a client and two tenants, tenant and other, whose
// graphs already exist on that same client. Both graphs must be fresh, empty,
// and private to the calling spec: the contract specs assert exact edge sets
// and emptiness, so a graph shared across specs, or one carrying leftover
// data, produces false failures. other exists only so the "tenant isolation"
// spec can prove tenant's data is invisible from it; no other spec writes to
// or reads from other. It runs inside a BeforeEach, so it should register
// teardown with ginkgo.DeferCleanup.
type GraphDBSetup func() (db graphdb.GraphDB, tenant, other string)

// contractT0 is the base instant for every contract fixture. It falls on a
// whole second so sub-second ordering bugs are observable.
var contractT0 = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

// contractHostileValue holds a single quote and a backslash, the characters a
// driver must escape when it embeds a string in a query. Filtering on it must
// match exactly the edge written with it.
const contractHostileValue = `job-'1\`

type graphOp func(ctx context.Context, db graphdb.GraphDB, tenant string) error

// tableArgs combines a DescribeTable body function with its Entry list into a
// single slice so the two can be passed to ginkgo.DescribeTable with one
// trailing "...". Go forbids spreading a slice into a variadic parameter
// alongside another positional argument for that same parameter, so the body
// function and the entries must be merged into one slice first.
func tableArgs(body any, entries []any) []any {
	return append([]any{body}, entries...)
}

// namedOp is a graphOp with the name its table entries use.
type namedOp struct {
	name string
	op   graphOp
}

// tenantScopedOpList calls every tenant-scoped GraphDB method with otherwise
// valid arguments. CreateNode and UpsertNode write the fresh node "bad", so a
// write that leaks past a check is detectable; the other writes use nodes "a"/"b" and
// edge "e".
func tenantScopedOpList() []namedOp {
	return []namedOp{
		{"CreateNode", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateNode(ctx, tenant, graphdb.Node{ID: "bad", Label: "Control", ValidFrom: contractT0})
		})},
		{"UpsertNode", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.UpsertNode(ctx, tenant, graphdb.Node{ID: "bad", Label: "Control", ValidFrom: contractT0})
			return err
		})},
		{"CreateEdge", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateEdge(ctx, tenant, "a", "b", graphdb.Edge{ID: "e", Label: "MAPS", ValidFrom: contractT0})
		})},
		{"CreateRequiresEdge", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateRequiresEdge(ctx, tenant, graphdb.RequiresEdge{SourceID: "a", TargetID: "b", AnalyzedAt: contractT0, JobID: "j"})
		})},
		{"QueryRelationships", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.QueryRelationships(ctx, tenant, graphdb.RelationshipQuery{})
			return err
		})},
		{"QueryAsOf", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{}, contractT0)
			return err
		})},
		{"Traverse", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.Traverse(ctx, tenant, graphdb.TraversalQuery{StartNode: "a"})
			return err
		})},
		{"GetNode", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.GetNode(ctx, tenant, "a")
			return err
		})},
		{"GetEdge", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.GetEdge(ctx, tenant, "e")
			return err
		})},
		{"BulkCreateEdges", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{
				{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{ID: "e", Label: "MAPS", ValidFrom: contractT0}},
			})
			return err
		})},
		{"ExecuteQuery", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.ExecuteQuery(ctx, tenant, "MATCH (n) RETURN n", nil)
			return err
		})},
		{"SupersedeFact", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "a", SupersededAt: contractT0})
			return err
		})},
	}
}

// tenantScopedOps is tenantScopedOpList as table entries.
func tenantScopedOps() []any {
	var entries []any
	for _, n := range tenantScopedOpList() {
		entries = append(entries, ginkgo.Entry(n.name, n.op))
	}
	return entries
}

// invalidFieldOps calls methods with an invalid field AND an empty tenant.
// Field validation must win. Each entry carries the substring expected in
// the resulting error message, verified against agedriver's wording.
func invalidFieldOps() []any {
	return []any{
		ginkgo.Entry("CreateNode without ID", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateNode(ctx, tenant, graphdb.Node{Label: "Control", ValidFrom: contractT0})
		}), "id is required"),
		ginkgo.Entry("CreateNode without label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateNode(ctx, tenant, graphdb.Node{ID: "a", ValidFrom: contractT0})
		}), "label is required"),
		ginkgo.Entry("CreateNode without valid_from", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateNode(ctx, tenant, graphdb.Node{ID: "a", Label: "Control"})
		}), "valid_from is required"),
		ginkgo.Entry("UpsertNode without ID", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.UpsertNode(ctx, tenant, graphdb.Node{Label: "Control", ValidFrom: contractT0})
			return err
		}), "id is required"),
		ginkgo.Entry("UpsertNode without label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.UpsertNode(ctx, tenant, graphdb.Node{ID: "a", ValidFrom: contractT0})
			return err
		}), "label is required"),
		ginkgo.Entry("UpsertNode without valid_from", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.UpsertNode(ctx, tenant, graphdb.Node{ID: "a", Label: "Control"})
			return err
		}), "valid_from is required"),
		ginkgo.Entry("CreateEdge without label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateEdge(ctx, tenant, "a", "b", graphdb.Edge{ValidFrom: contractT0})
		}), "label is required"),
		ginkgo.Entry("CreateEdge without source and target", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateEdge(ctx, tenant, "", "", graphdb.Edge{Label: "MAPS", ValidFrom: contractT0})
		}), "source and target are required"),
		ginkgo.Entry("CreateEdge without valid_from", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateEdge(ctx, tenant, "a", "b", graphdb.Edge{Label: "MAPS"})
		}), "valid_from is required"),
		ginkgo.Entry("CreateRequiresEdge without source", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateRequiresEdge(ctx, tenant, graphdb.RequiresEdge{TargetID: "b", AnalyzedAt: contractT0})
		}), "source_id is required"),
		ginkgo.Entry("CreateRequiresEdge without target", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateRequiresEdge(ctx, tenant, graphdb.RequiresEdge{SourceID: "a", AnalyzedAt: contractT0})
		}), "target_id is required"),
		ginkgo.Entry("CreateRequiresEdge without analyzed_at", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateRequiresEdge(ctx, tenant, graphdb.RequiresEdge{SourceID: "a", TargetID: "b"})
		}), "analyzed_at is required"),
		ginkgo.Entry("GetNode without ID", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.GetNode(ctx, tenant, "")
			return err
		}), "node_id is required"),
		ginkgo.Entry("GetEdge without ID", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.GetEdge(ctx, tenant, "")
			return err
		}), "edge_id is required"),
		ginkgo.Entry("BulkCreateEdges without label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{ValidFrom: contractT0}}})
			return err
		}), "label is required"),
		ginkgo.Entry("BulkCreateEdges without source and target", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{{Edge: graphdb.Edge{Label: "MAPS", ValidFrom: contractT0}}})
			return err
		}), "source and target are required"),
		ginkgo.Entry("BulkCreateEdges without valid_from", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{Label: "MAPS"}}})
			return err
		}), "valid_from is required"),
		ginkgo.Entry("ExecuteQuery without cypher", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.ExecuteQuery(ctx, tenant, "", nil)
			return err
		}), "cypher is required"),
		ginkgo.Entry("SupersedeFact without target", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{SupersededAt: contractT0})
			return err
		}), "node_id or edge_id is required"),
		ginkgo.Entry("SupersedeFact without superseded_at", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "a"})
			return err
		}), "superseded_at is required"),
	}
}

// malformedTenantOps calls CreateGraph and every tenantScopedOpList method
// with each tenant that tenant.ValidateTenantID rejects: one that would end
// agedriver's quoted graph name in SQL, one with uppercase letters, and one
// 53 characters long, whose graph name PostgreSQL would truncate.
func malformedTenantOps() []any {
	ops := append([]namedOp{{"CreateGraph", func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
		return db.CreateGraph(ctx, tenant)
	}}}, tenantScopedOpList()...)
	var entries []any
	for _, bad := range []struct{ name, id string }{
		{"a SQL breakout tenant", "x'); --"},
		{"an uppercase tenant", "Acme-Corp"},
		{"a 53-character tenant", "a" + strings.Repeat("b", 51) + "c"},
	} {
		for _, n := range ops {
			entries = append(entries, ginkgo.Entry(n.name+" with "+bad.name, bad.id, n.op))
		}
	}
	return entries
}

// reservedKeyOps writes Properties that use a key the driver sets itself
// from a Node or Edge field. Each entry carries that key. Writes use node ID
// "bad" and leave Edge.ID empty or "bad-e", so the caller can confirm nothing
// was stored and that existing edge "e1" was not overwritten. UpsertNode is
// also aimed at existing node "a" with an extra "leaked" property and
// CreatedBy set, so a leaked update changes node a, which the caller compares
// in full (every Node field and Properties) against a snapshot taken before
// the op.
func reservedKeyOps() []any {
	node := func(props map[string]any) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateNode(ctx, tenant, graphdb.Node{ID: "bad", Label: "Control", ValidFrom: contractT0, Properties: props})
		}
	}
	upsert := func(id string, reserved map[string]any) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			props := maps.Clone(reserved)
			props["leaked"] = "yes"
			_, err := db.UpsertNode(ctx, tenant, graphdb.Node{ID: id, Label: "Control", ValidFrom: contractT0, CreatedBy: "leaked", Properties: props})
			return err
		}
	}
	edge := func(e graphdb.Edge) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateEdge(ctx, tenant, "a", "b", e)
		}
	}
	bulk := func(edges ...graphdb.Edge) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			batch := make([]graphdb.BulkEdge, len(edges))
			for i, e := range edges {
				batch[i] = graphdb.BulkEdge{SourceID: "a", TargetID: "b", Edge: e}
			}
			_, err := db.BulkCreateEdges(ctx, tenant, batch)
			return err
		}
	}
	return []any{
		ginkgo.Entry("CreateNode id naming an existing node", node(map[string]any{"id": "a"}), "id", ""),
		ginkgo.Entry("CreateNode valid_from", node(map[string]any{"valid_from": "1970-01-01T00:00:00.000000000Z"}), "valid_from", ""),
		ginkgo.Entry("CreateNode superseded_by", node(map[string]any{"superseded_by": "job-x"}), "superseded_by", ""),
		ginkgo.Entry("UpsertNode id naming another node", upsert("bad", map[string]any{"id": "a"}), "id", ""),
		ginkgo.Entry("UpsertNode valid_to on an existing node", upsert("a", map[string]any{"valid_to": "1970-01-01T00:00:00.000000000Z"}), "valid_to", ""),
		ginkgo.Entry("UpsertNode superseded_by on an existing node", upsert("a", map[string]any{"superseded_by": "job-x"}), "superseded_by", ""),
		ginkgo.Entry("CreateEdge id naming an existing edge, with an empty Edge.ID",
			edge(graphdb.Edge{Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"id": "e1"}}), "id", ""),
		ginkgo.Entry("CreateEdge valid_to",
			edge(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"valid_to": "1970-01-01T00:00:00.000000000Z"}}), "valid_to", ""),
		ginkgo.Entry("BulkCreateEdges id naming an existing edge, with an empty Edge.ID",
			bulk(graphdb.Edge{Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"id": "e1"}}), "id", ""),
		ginkgo.Entry("BulkCreateEdges id on the second edge of a batch",
			bulk(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0},
				graphdb.Edge{Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"id": "e1"}}), "id", "[1]"),
		ginkgo.Entry("CreateEdge confidence, which is reserved on edges only",
			edge(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"confidence": 0.9}}), "confidence", ""),
		ginkgo.Entry("BulkCreateEdges confidence, which is reserved on edges only",
			bulk(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"confidence": 0.9}}), "confidence", ""),
		ginkgo.Entry("CreateNode created_by",
			node(map[string]any{"created_by": "someone-else"}), "created_by", ""),
		ginkgo.Entry("CreateNode creation_method",
			node(map[string]any{"creation_method": "manual"}), "creation_method", ""),
		ginkgo.Entry("CreateEdge determined_by",
			edge(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"determined_by": "job-x"}}), "determined_by", ""),
		ginkgo.Entry("CreateEdge determination_type",
			edge(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"determination_type": "manual"}}), "determination_type", ""),
		ginkgo.Entry("CreateEdge supersedes",
			edge(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"supersedes": "e1"}}), "supersedes", ""),
		ginkgo.Entry("UpsertNode created_by on an existing node", upsert("a", map[string]any{"created_by": "someone-else"}), "created_by", ""),
		ginkgo.Entry("UpsertNode creation_method on an existing node", upsert("a", map[string]any{"creation_method": "manual"}), "creation_method", ""),
		ginkgo.Entry("BulkCreateEdges supersedes",
			bulk(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"supersedes": "e1"}}), "supersedes", ""),
		ginkgo.Entry("BulkCreateEdges determination_type",
			bulk(graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"determination_type": "manual"}}), "determination_type", ""),
	}
}

// cypherBreakout is a label that would end the pattern it is spliced into
// and run a destructive clause, if a driver accepted it.
const cypherBreakout = "Control) DETACH DELETE n //"

// cypherBreakoutKey is cypherBreakout for a property key: it would end the
// property map instead of the label.
const cypherBreakoutKey = "k: 1}) DETACH DELETE n //"

// nonIdentifierOps calls methods with a label or property key that is not an
// identifier. Writes use node ID "bad" and edge ID "bad-e" so the caller can
// confirm nothing was stored.
func nonIdentifierOps() []any {
	node := func(label string, props map[string]any) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateNode(ctx, tenant, graphdb.Node{ID: "bad", Label: label, ValidFrom: contractT0, Properties: props})
		}
	}
	upsert := func(label string, props map[string]any) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.UpsertNode(ctx, tenant, graphdb.Node{ID: "bad", Label: label, ValidFrom: contractT0, Properties: props})
			return err
		}
	}
	edge := func(label string, props map[string]any) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			return db.CreateEdge(ctx, tenant, "a", "b", graphdb.Edge{ID: "bad-e", Label: label, ValidFrom: contractT0, Properties: props})
		}
	}
	query := func(q graphdb.RelationshipQuery) graphOp {
		return func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.QueryRelationships(ctx, tenant, q)
			return err
		}
	}
	return []any{
		ginkgo.Entry("CreateNode label", node(cypherBreakout, nil)),
		ginkgo.Entry("CreateNode label starting with a digit", node("1Control", nil)),
		ginkgo.Entry("CreateNode property key", node("Control", map[string]any{cypherBreakoutKey: "v"})),
		ginkgo.Entry("UpsertNode label", upsert(cypherBreakout, nil)),
		ginkgo.Entry("UpsertNode property key", upsert("Control", map[string]any{cypherBreakoutKey: "v"})),
		ginkgo.Entry("CreateEdge label", edge(cypherBreakout, nil)),
		ginkgo.Entry("CreateEdge property key", edge("MAPS", map[string]any{"has space": "v"})),
		ginkgo.Entry("BulkCreateEdges label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{ID: "bad-e", Label: cypherBreakout, ValidFrom: contractT0}}})
			return err
		})),
		ginkgo.Entry("BulkCreateEdges property key", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{ID: "bad-e", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"a-b": "v"}}}})
			return err
		})),
		ginkgo.Entry("QueryRelationships source label", query(graphdb.RelationshipQuery{SourceLabel: cypherBreakout})),
		ginkgo.Entry("QueryRelationships target label", query(graphdb.RelationshipQuery{TargetLabel: cypherBreakout})),
		ginkgo.Entry("QueryRelationships edge label", query(graphdb.RelationshipQuery{EdgeLabel: cypherBreakout})),
		ginkgo.Entry("QueryRelationships property key", query(graphdb.RelationshipQuery{Properties: map[string]any{"k = 1 OR 1": 1}})),
		ginkgo.Entry("QueryAsOf edge label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{EdgeLabel: cypherBreakout}, contractT0)
			return err
		})),
		ginkgo.Entry("Traverse edge label", graphOp(func(ctx context.Context, db graphdb.GraphDB, tenant string) error {
			_, err := db.Traverse(ctx, tenant, graphdb.TraversalQuery{StartNode: "a", EdgeLabels: []string{"MAPS", cypherBreakout}, MaxDepth: 1})
			return err
		})),
	}
}

func randomTenantID(prefix string) string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return prefix + "-" + hex.EncodeToString(b)
}

func relEdgeIDs(rels []graphdb.Relationship) []string {
	ids := make([]string, len(rels))
	for i, r := range rels {
		ids[i] = r.Edge.ID
	}
	return ids
}

func pathNodeIDs(paths []graphdb.Path) [][]string {
	out := make([][]string, len(paths))
	for i, p := range paths {
		for _, n := range p.Nodes {
			out[i] = append(out[i], n.ID)
		}
	}
	return out
}

func timePtr(t time.Time) *time.Time { return &t }

// GraphDBContractBehavior is the executable form of the contract documented
// on graphdb.GraphDB. Every driver runs it: memdriver in its unit suite,
// agedriver in its integration suite.
//
// Three clauses are tested per driver instead of here:
//   - ExecuteQuery's query-execution clauses (whole-token $name
//     substitution; ErrNotSupported from drivers without an openCypher
//     engine), because the in-memory driver cannot execute Cypher.
//   - Concurrent first-time CreateGraph, because agedriver's restricted
//     graph_user cannot create a graph (the tenant trigger does), so the
//     shared specs run against a provisioned tenant.
//   - The "ambiguous node ID" error for graphs written before #148, because
//     the contract forbids building such a graph through the API; agedriver
//     seeds one with raw Cypher.
//
// A new driver must add its own specs for these three.
//
// Usage:
//
//	ginkgo.Describe("GraphDB contract", testspecs.GraphDBContractBehavior(setup))
func GraphDBContractBehavior(setup GraphDBSetup) func() {
	return func() {
		var (
			ctx           context.Context
			db            graphdb.GraphDB
			tenant, other string
		)

		ginkgo.BeforeEach(func() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			ginkgo.DeferCleanup(cancel)
			db, tenant, other = setup()
		})

		mustNode := func(id, label string) {
			gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{ID: id, Label: label, ValidFrom: contractT0})).To(gomega.Succeed())
		}
		mustEdge := func(src, tgt string, e graphdb.Edge) {
			gomega.Expect(db.CreateEdge(ctx, tenant, src, tgt, e)).To(gomega.Succeed())
		}
		currentEdges := func(q graphdb.RelationshipQuery) []graphdb.Relationship {
			rels, err := db.QueryRelationships(ctx, tenant, q)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return rels
		}

		ginkgo.Describe("tenant and graph lifecycle", func() {
			ginkgo.It("treats CreateGraph as idempotent and keeps existing data", func() {
				mustNode("a", "Control")
				gomega.Expect(db.CreateGraph(ctx, tenant)).To(gomega.Succeed())
				gomega.Expect(db.CreateGraph(ctx, tenant)).To(gomega.Succeed())
				_, err := db.GetNode(ctx, tenant, "a")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			// agedriver's restricted graph_user cannot create a graph (the
			// tenant trigger does), so this runs against the provisioned
			// tenant; first-time concurrent creation is covered per driver.
			ginkgo.It("returns nil from every one of several concurrent CreateGraph calls and keeps existing data", func() {
				mustNode("a", "Control")
				const callers = 8
				errs := make([]error, callers)
				var wg sync.WaitGroup
				for i := range callers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						errs[i] = db.CreateGraph(ctx, tenant)
					}()
				}
				wg.Wait()
				for _, err := range errs {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				_, err := db.GetNode(ctx, tenant, "a")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})

			ginkgo.It("rejects CreateGraph without a tenant", func() {
				gomega.Expect(db.CreateGraph(ctx, "")).To(gomega.MatchError(graphdb.ErrTenantRequired))
			})

			ginkgo.DescribeTable("returns ErrTenantRequired for an empty tenant",
				tableArgs(func(op graphOp) {
					gomega.Expect(op(ctx, db, "")).To(gomega.MatchError(graphdb.ErrTenantRequired))
				}, tenantScopedOps())...,
			)

			ginkgo.DescribeTable("rejects a malformed tenant with ErrTenantRequired and an actionable message, and stores nothing",
				tableArgs(func(bad string, op graphOp) {
					mustNode("a", "Control")
					mustNode("b", "Control")
					err := op(ctx, db, bad)
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrTenantRequired))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("3-52 characters"))
					n, err := db.GetNode(ctx, tenant, "a")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(n.ValidTo).To(gomega.BeNil())
					_, err = db.GetNode(ctx, tenant, "bad")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))
					gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.BeEmpty())
				}, malformedTenantOps())...,
			)

			ginkgo.DescribeTable("returns ErrGraphNotFound for a tenant whose graph does not exist",
				tableArgs(func(op graphOp) {
					missing := randomTenantID("contract-missing")
					err := op(ctx, db, missing)
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrGraphNotFound))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(missing))

					// Proves the failed operation did not create the graph
					// implicitly: a subsequent read still sees no graph.
					_, err = db.GetNode(ctx, missing, "a")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrGraphNotFound))
				}, tenantScopedOps())...,
			)

			ginkgo.DescribeTable("validates fields before checking the tenant",
				tableArgs(func(op graphOp, want string) {
					err := op(ctx, db, "")
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).NotTo(gomega.MatchError(graphdb.ErrTenantRequired))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(want))
				}, invalidFieldOps())...,
			)

			ginkgo.DescribeTable("validates labels and property keys before checking the tenant",
				tableArgs(func(op graphOp) {
					err := op(ctx, db, "")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrInvalidCypher))
					gomega.Expect(err).NotTo(gomega.MatchError(graphdb.ErrTenantRequired))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("must be an identifier"))
				}, nonIdentifierOps())...,
			)

			ginkgo.DescribeTable("validates reserved property keys before checking the tenant",
				tableArgs(func(op graphOp, key, _ string) {
					err := op(ctx, db, "")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrInvalidCypher))
					gomega.Expect(err).NotTo(gomega.MatchError(graphdb.ErrTenantRequired))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(fmt.Sprintf("property key %q is reserved", key)))
				}, reservedKeyOps())...,
			)
		})

		ginkgo.Describe("tenant isolation", func() {
			ginkgo.It("does not expose one tenant's graph data to another", func() {
				// other's graph already exists: setup provisions it the same
				// way it provisions tenant's, so this spec never needs to
				// create (or drop) a graph itself.
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})

				_, err := db.GetNode(ctx, other, "a")
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))

				_, err = db.GetEdge(ctx, other, "e1")
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrEdgeNotFound))

				rels, err := db.QueryRelationships(ctx, other, graphdb.RelationshipQuery{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(rels).To(gomega.BeEmpty())

				asOf, err := db.QueryAsOf(ctx, other, graphdb.RelationshipQuery{}, contractT0.Add(time.Hour))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(asOf).To(gomega.BeEmpty())

				paths, err := db.Traverse(ctx, other, graphdb.TraversalQuery{StartNode: "a"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(paths).To(gomega.BeEmpty())

				ok, err := db.SupersedeFact(ctx, other, graphdb.SupersedeRequest{NodeID: "a", SupersededAt: contractT0.Add(time.Hour)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ok).To(gomega.BeFalse())

				err = db.CreateEdge(ctx, other, "a", "b", graphdb.Edge{ID: "e2", Label: "MAPS", ValidFrom: contractT0})
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))

				// The probes against other must not have perturbed tenant's data.
				node, err := db.GetNode(ctx, tenant, "a")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(node.ValidTo).To(gomega.BeNil())

				edge, err := db.GetEdge(ctx, tenant, "e1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(edge.ValidTo).To(gomega.BeNil())
			})
		})

		ginkgo.Describe("reserved property keys", func() {
			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0, Properties: map[string]any{"kind": "original"}})
			})

			// expectEveryFieldSet fails when a struct field other than
			// Properties is zero, so a field added to Node or Edge later
			// cannot escape the read-back spec below.
			expectEveryFieldSet := func(v any) {
				rv := reflect.ValueOf(v)
				for i := range rv.NumField() {
					if name := rv.Type().Field(i).Name; name != "Properties" {
						gomega.Expect(rv.Field(i).IsZero()).To(gomega.BeFalse(), "set %s in this spec", name)
					}
				}
			}

			ginkgo.It("reserves exactly the keys the driver writes for a node and an edge with every field set, including SupersedeFact's", func() {
				validTo := contractT0.Add(time.Hour)
				n := graphdb.Node{ID: "full", Label: "Control", ValidFrom: contractT0, ValidTo: &validTo, CreatedBy: "u", CreationMethod: "m"}
				expectEveryFieldSet(n)
				gomega.Expect(db.CreateNode(ctx, tenant, n)).To(gomega.Succeed())
				e := graphdb.Edge{ID: "full-e", Label: "MAPS", ValidFrom: contractT0, ValidTo: &validTo,
					DeterminedBy: "j", DeterminationType: "t", Confidence: 0.5, Supersedes: "e1"}
				expectEveryFieldSet(e)
				mustEdge("a", "b", e)
				for _, req := range []graphdb.SupersedeRequest{{NodeID: "full"}, {EdgeID: "full-e"}} {
					req.SupersededAt = validTo
					req.SupersededByJobID = "job-s"
					updated, err := db.SupersedeFact(ctx, tenant, req)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(updated).To(gomega.BeTrue())
				}

				gotNode, err := db.GetNode(ctx, tenant, "full")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(slices.Collect(maps.Keys(gotNode.Properties))).To(gomega.ConsistOf(slices.Collect(maps.Keys(graphdb.ReservedNodeKeys()))))
				gotEdge, err := db.GetEdge(ctx, tenant, "full-e")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(slices.Collect(maps.Keys(gotEdge.Properties))).To(gomega.ConsistOf(slices.Collect(maps.Keys(graphdb.ReservedEdgeKeys()))))
			})

			ginkgo.It("lets nodes carry a confidence property, because confidence is reserved on edges only", func() {
				gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{ID: "conf", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"confidence": 0.9}})).To(gomega.Succeed())
				got, err := db.GetNode(ctx, tenant, "conf")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("confidence", 0.9))

				created, err := db.UpsertNode(ctx, tenant, graphdb.Node{ID: "conf", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"confidence": 0.25}})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(created).To(gomega.BeFalse())
				got, err = db.GetNode(ctx, tenant, "conf")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("confidence", 0.25))

				created, err = db.UpsertNode(ctx, tenant, graphdb.Node{ID: "conf-new", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"confidence": 0.5}})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(created).To(gomega.BeTrue())
				got, err = db.GetNode(ctx, tenant, "conf-new")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("confidence", 0.5))
			})

			ginkgo.DescribeTable("rejects a key the driver writes itself with ErrInvalidCypher and stores nothing",
				tableArgs(func(op graphOp, key, index string) {
					before := map[string]*graphdb.Node{}
					for _, id := range []string{"a", "b"} {
						n, err := db.GetNode(ctx, tenant, id)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						before[id] = n
					}

					err := op(ctx, db, tenant)
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrInvalidCypher))
					if index != "" {
						gomega.Expect(err.Error()).To(gomega.ContainSubstring(index))
						var be *graphdb.BulkEdgeError
						gomega.Expect(errors.As(err, &be)).To(gomega.BeTrue(), "a per-edge BulkCreateEdges error must be a *graphdb.BulkEdgeError")
						gomega.Expect(fmt.Sprintf("[%d]", be.Index)).To(gomega.Equal(index))
					}
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(fmt.Sprintf("property key %q is reserved", key)))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("Set that field instead"))

					_, err = db.GetNode(ctx, tenant, "bad")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))
					for _, id := range []string{"a", "b"} {
						n, err := db.GetNode(ctx, tenant, id)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(n).To(gomega.Equal(before[id]), "node %s changed", id)
					}
					rels := currentEdges(graphdb.RelationshipQuery{})
					gomega.Expect(relEdgeIDs(rels)).To(gomega.Equal([]string{"e1"}))
					gomega.Expect(rels[0].Edge.Properties).To(gomega.HaveKeyWithValue("kind", "original"))
				}, reservedKeyOps())...,
			)
		})

		ginkgo.Describe("labels and property keys", func() {
			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
			})

			ginkgo.DescribeTable("rejects a non-identifier with ErrInvalidCypher and stores nothing",
				tableArgs(func(op graphOp) {
					err := op(ctx, db, tenant)
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrInvalidCypher))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("must be an identifier"))
					_, err = db.GetNode(ctx, tenant, "bad")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))
					gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.BeEmpty())
					for _, id := range []string{"a", "b"} {
						_, err := db.GetNode(ctx, tenant, id)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
				}, nonIdentifierOps())...,
			)

			ginkgo.It("accepts identifiers with underscores and digits", func() {
				gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{ID: "c", Label: "_Control_2", ValidFrom: contractT0, Properties: map[string]any{"_k9": "v"}})).To(gomega.Succeed())
				mustEdge("a", "c", graphdb.Edge{ID: "e1", Label: "MAPS_TO_2", ValidFrom: contractT0, Properties: map[string]any{"kind_1": "x"}})
				gomega.Expect(relEdgeIDs(currentEdges(graphdb.RelationshipQuery{TargetLabel: "_Control_2", EdgeLabel: "MAPS_TO_2", Properties: map[string]any{"kind_1": "x"}}))).To(gomega.Equal([]string{"e1"}))
			})
		})

		ginkgo.Describe("nodes", func() {
			ginkgo.It("round-trips every field and normalizes properties the way AGE reads them back", func() {
				validFrom := contractT0.Add(500 * time.Millisecond)
				validTo := contractT0.Add(time.Hour)
				gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{
					ID:             "n1",
					Label:          "Control",
					ValidFrom:      validFrom,
					ValidTo:        &validTo,
					CreatedBy:      "job-1",
					CreationMethod: "import",
					Properties: map[string]any{
						"title":  "Access Control",
						"count":  3,
						"ratio":  0.25,
						"active": true,
						"tags":   []string{"a", "b"},
						"f32":    float32(0.1),
						"big":    int64(1) << 40,
					},
				})).To(gomega.Succeed())

				got, err := db.GetNode(ctx, tenant, "n1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.ID).To(gomega.Equal("n1"))
				gomega.Expect(got.Label).To(gomega.Equal("Control"))
				gomega.Expect(got.ValidFrom.Equal(validFrom)).To(gomega.BeTrue(), "valid_from %s", got.ValidFrom)
				gomega.Expect(got.ValidTo).NotTo(gomega.BeNil())
				gomega.Expect(got.ValidTo.Equal(validTo)).To(gomega.BeTrue(), "valid_to %s", got.ValidTo)
				gomega.Expect(got.CreatedBy).To(gomega.Equal("job-1"))
				gomega.Expect(got.CreationMethod).To(gomega.Equal("import"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("title", "Access Control"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("count", float64(3)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("ratio", 0.25))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("active", true))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("tags", "[a b]"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("f32", 0.1))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("big", float64(1<<40)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("id", "n1"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("valid_from", graphdb.FormatTime(validFrom)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("valid_to", graphdb.FormatTime(validTo)))
			})

			ginkgo.It("rejects a duplicate label and ID with ErrNodeExists and leaves the original unchanged", func() {
				gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"v": "first"},
				})).To(gomega.Succeed())

				err := db.CreateNode(ctx, tenant, graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"v": "second"},
				})
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeExists))
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("n1"))

				got, err := db.GetNode(ctx, tenant, "n1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("v", "first"))
			})

			ginkgo.DescribeTable("rejects an ID already used under another label with a NodeIDConflictError and leaves the graph unchanged",
				func(write func(graphdb.Node) error) {
					gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{
						ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"v": "control"},
					})).To(gomega.Succeed())
					before, err := db.GetNode(ctx, tenant, "n1")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					err = write(graphdb.Node{ID: "n1", Label: "Artifact", ValidFrom: contractT0, Properties: map[string]any{"v": "artifact"}})
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeIDConflict))
					gomega.Expect(errors.Is(err, graphdb.ErrNodeExists)).To(gomega.BeFalse(), "graph writers treat ErrNodeExists as success")
					var conflict *graphdb.NodeIDConflictError
					gomega.Expect(errors.As(err, &conflict)).To(gomega.BeTrue())
					gomega.Expect(*conflict).To(gomega.Equal(graphdb.NodeIDConflictError{ID: "n1", Label: "Artifact", StoredLabel: "Control"}))

					after, err := db.GetNode(ctx, tenant, "n1")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(after).To(gomega.Equal(before))
					// An edge from "n1" resolves only while exactly one node has the ID.
					mustNode("n2", "Control")
					mustEdge("n1", "n2", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})
				},
				ginkgo.Entry("CreateNode", func(n graphdb.Node) error { return db.CreateNode(ctx, tenant, n) }),
				ginkgo.Entry("UpsertNode", func(n graphdb.Node) error {
					_, err := db.UpsertNode(ctx, tenant, n)
					return err
				}),
			)

			ginkgo.It("returns ErrNodeNotFound for an unknown ID", func() {
				_, err := db.GetNode(ctx, tenant, "nope")
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))
			})
		})

		ginkgo.Describe("UpsertNode", func() {
			upsert := func(n graphdb.Node) bool {
				created, err := db.UpsertNode(ctx, tenant, n)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return created
			}
			getProps := func(id string) map[string]any {
				got, err := db.GetNode(ctx, tenant, id)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return got.Properties
			}
			withoutID := func(props map[string]any) map[string]any {
				out := maps.Clone(props)
				delete(out, "id")
				return out
			}

			ginkgo.It("creates a missing node exactly as CreateNode would and reports created", func() {
				validTo := contractT0.Add(time.Hour)
				n := graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, ValidTo: &validTo,
					CreatedBy: "job-1", CreationMethod: "import",
					Properties: map[string]any{"title": "Access Control", "count": 3, "active": true},
				}
				gomega.Expect(upsert(n)).To(gomega.BeTrue())

				twin := n
				twin.ID = "twin"
				gomega.Expect(db.CreateNode(ctx, tenant, twin)).To(gomega.Succeed())

				got, err := db.GetNode(ctx, tenant, "n1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Label).To(gomega.Equal("Control"))
				gomega.Expect(got.Properties).To(gomega.Equal(map[string]any{
					"id": "n1", "valid_from": graphdb.FormatTime(contractT0), "valid_to": graphdb.FormatTime(validTo),
					"created_by": "job-1", "creation_method": "import",
					"title": "Access Control", "count": float64(3), "active": true,
				}))
				gomega.Expect(withoutID(got.Properties)).To(gomega.Equal(withoutID(getProps("twin"))))
			})

			ginkgo.It("replaces an existing node's properties and reserved fields except valid_from, removing keys absent from Properties", func() {
				gomega.Expect(upsert(graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, CreatedBy: "job-1", CreationMethod: "import",
					Properties: map[string]any{"title": "old", "statement": "old", "obsolete": "x"},
				})).To(gomega.BeTrue())

				validFrom := contractT0.Add(time.Hour)
				gomega.Expect(upsert(graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: validFrom, CreatedBy: "job-2",
					Properties: map[string]any{"title": "new", "statement": "new", "count": 3},
				})).To(gomega.BeFalse())

				got, err := db.GetNode(ctx, tenant, "n1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Properties).To(gomega.Equal(map[string]any{
					"id": "n1", "valid_from": graphdb.FormatTime(contractT0), "created_by": "job-2",
					"title": "new", "statement": "new", "count": float64(3),
				}))
				gomega.Expect(got.ValidFrom.Equal(contractT0)).To(gomega.BeTrue(), "valid_from %s", got.ValidFrom)
				gomega.Expect(got.CreationMethod).To(gomega.BeEmpty())
			})

			ginkgo.It("keeps the stored valid_from on update, so AsOf reads between the two times still see the node", func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})

				later := contractT0.Add(2 * time.Hour)
				gomega.Expect(upsert(graphdb.Node{ID: "a", Label: "Control", ValidFrom: later, Properties: map[string]any{"title": "new"}})).To(gomega.BeFalse())

				got, err := db.GetNode(ctx, tenant, "a")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.ValidFrom.Equal(contractT0)).To(gomega.BeTrue(), "valid_from %s", got.ValidFrom)
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("valid_from", graphdb.FormatTime(contractT0)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("title", "new"))

				between := contractT0.Add(time.Hour)
				paths, err := db.Traverse(ctx, tenant, graphdb.TraversalQuery{StartNode: "a", MaxDepth: 1, AsOf: &between})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(pathNodeIDs(paths)).To(gomega.Equal([][]string{{"a", "b"}}))
			})

			ginkgo.It("is idempotent: repeating an upsert reports not created and stores the same node", func() {
				n := graphdb.Node{ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"title": "t"}}
				gomega.Expect(upsert(n)).To(gomega.BeTrue())
				first := getProps("n1")
				gomega.Expect(upsert(n)).To(gomega.BeFalse())
				gomega.Expect(getProps("n1")).To(gomega.Equal(first))
			})

			ginkgo.It("updates a node created by CreateNode", func() {
				gomega.Expect(db.CreateNode(ctx, tenant, graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"title": "old"},
				})).To(gomega.Succeed())
				gomega.Expect(upsert(graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"title": "new"},
				})).To(gomega.BeFalse())
				gomega.Expect(getProps("n1")).To(gomega.HaveKeyWithValue("title", "new"))
			})

			ginkgo.It("keeps SupersedeFact's valid_to and superseded_by when ValidTo is unset", func() {
				mustNode("n1", "Control")
				supersededAt := contractT0.Add(2 * time.Hour)
				updated, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "n1", SupersededAt: supersededAt, SupersededByJobID: "job-s"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(updated).To(gomega.BeTrue())

				gomega.Expect(upsert(graphdb.Node{
					ID: "n1", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"title": "new"},
				})).To(gomega.BeFalse())

				gomega.Expect(getProps("n1")).To(gomega.Equal(map[string]any{
					"id": "n1", "valid_from": graphdb.FormatTime(contractT0),
					"valid_to": graphdb.FormatTime(supersededAt), "superseded_by": "job-s",
					"title": "new",
				}))
			})

			ginkgo.It("overwrites valid_to and removes superseded_by when ValidTo is set", func() {
				mustNode("n1", "Control")
				_, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "n1", SupersededAt: contractT0.Add(2 * time.Hour), SupersededByJobID: "job-s"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				validTo := contractT0.Add(3 * time.Hour)
				gomega.Expect(upsert(graphdb.Node{ID: "n1", Label: "Control", ValidFrom: contractT0, ValidTo: &validTo})).To(gomega.BeFalse())

				gomega.Expect(getProps("n1")).To(gomega.Equal(map[string]any{
					"id": "n1", "valid_from": graphdb.FormatTime(contractT0), "valid_to": graphdb.FormatTime(validTo),
				}))
			})

			ginkgo.It("keeps the edges attached to an updated node", func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})

				gomega.Expect(upsert(graphdb.Node{ID: "a", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"title": "new"}})).To(gomega.BeFalse())

				rels := currentEdges(graphdb.RelationshipQuery{})
				gomega.Expect(relEdgeIDs(rels)).To(gomega.Equal([]string{"e1"}))
				gomega.Expect(rels[0].Source.Properties).To(gomega.HaveKeyWithValue("title", "new"))
			})

			// expectSingleNode proves exactly one node has ID id: an edge from
			// it resolves its source unambiguously only then.
			expectSingleNode := func(id string) {
				mustNode("single-target", "Artifact")
				gomega.Expect(db.CreateEdge(ctx, tenant, id, "single-target", graphdb.Edge{ID: "single-e", Label: "MAPS", ValidFrom: contractT0})).To(gomega.Succeed())
			}
			const racers = 8

			ginkgo.It("creates a new node exactly once under concurrent UpsertNode calls", func() {
				created := make([]bool, racers)
				errs := make([]error, racers)
				var wg sync.WaitGroup
				for i := range racers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						created[i], errs[i] = db.UpsertNode(ctx, tenant, graphdb.Node{ID: "race", Label: "Control", ValidFrom: contractT0, Properties: map[string]any{"rev": i}})
					}()
				}
				wg.Wait()
				for _, err := range errs {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				createdCount := 0
				for _, c := range created {
					if c {
						createdCount++
					}
				}
				gomega.Expect(createdCount).To(gomega.Equal(1), "exactly one call reports created=true")
				expectSingleNode("race")
			})

			ginkgo.It("lets exactly one of several concurrent CreateNode calls for a new node succeed", func() {
				errs := make([]error, racers)
				var wg sync.WaitGroup
				for i := range racers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						errs[i] = db.CreateNode(ctx, tenant, graphdb.Node{ID: "race", Label: "Control", ValidFrom: contractT0})
					}()
				}
				wg.Wait()
				succeeded := 0
				for _, err := range errs {
					if err == nil {
						succeeded++
						continue
					}
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeExists))
				}
				gomega.Expect(succeeded).To(gomega.Equal(1))
				expectSingleNode("race")
			})

			// raceMixed runs racers concurrent writes of a new node "race". Even
			// racers call CreateNode and odd racers UpsertNode, and each pair
			// (0-1, 2-3, ...) takes the next label in labels, so every label is
			// written by both methods.
			type raceResult struct {
				label   string
				created bool
				err     error
			}
			raceMixed := func(labels ...string) []raceResult {
				results := make([]raceResult, racers)
				var wg sync.WaitGroup
				for i := range racers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						n := graphdb.Node{ID: "race", Label: labels[(i/2)%len(labels)], ValidFrom: contractT0}
						r := raceResult{label: n.Label}
						if i%2 == 0 {
							r.err = db.CreateNode(ctx, tenant, n)
							r.created = r.err == nil
						} else {
							r.created, r.err = db.UpsertNode(ctx, tenant, n)
						}
						results[i] = r
					}()
				}
				wg.Wait()
				return results
			}

			ginkgo.It("creates a new node exactly once when CreateNode and UpsertNode race on it", func() {
				results := raceMixed("Control")
				creators := 0
				for i, r := range results {
					if r.created {
						creators++
						continue
					}
					if i%2 == 0 {
						gomega.Expect(r.err).To(gomega.MatchError(graphdb.ErrNodeExists), "CreateNode racer %d", i)
					} else {
						gomega.Expect(r.err).NotTo(gomega.HaveOccurred(), "UpsertNode racer %d", i)
					}
				}
				gomega.Expect(creators).To(gomega.Equal(1), "exactly one call creates the node")
				expectSingleNode("race")
			})

			ginkgo.It("lets only one label claim a new ID when writers under two labels race on it", func() {
				results := raceMixed("Control", "Artifact")
				creators := 0
				var winner string
				for _, r := range results {
					if r.created {
						creators++
						winner = r.label
					}
				}
				gomega.Expect(creators).To(gomega.Equal(1), "exactly one call creates the node")
				for i, r := range results {
					switch {
					case r.created:
					case r.label != winner:
						gomega.Expect(r.err).To(gomega.MatchError(graphdb.ErrNodeIDConflict), "racer %d (%s)", i, r.label)
					case i%2 == 0:
						gomega.Expect(r.err).To(gomega.MatchError(graphdb.ErrNodeExists), "CreateNode racer %d", i)
					default:
						gomega.Expect(r.err).NotTo(gomega.HaveOccurred(), "UpsertNode racer %d", i)
					}
				}
				got, err := db.GetNode(ctx, tenant, "race")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Label).To(gomega.Equal(winner))
				expectSingleNode("race")
			})
		})

		ginkgo.Describe("edges", func() {
			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
			})

			ginkgo.It("round-trips an edge and its endpoints through GetEdge", func() {
				mustEdge("a", "b", graphdb.Edge{
					ID:                "e1",
					Label:             "MAPS",
					ValidFrom:         contractT0,
					DeterminedBy:      "job-1",
					DeterminationType: "llm",
					Confidence:        0.9,
					Supersedes:        "e0",
					Properties:        map[string]any{"kind": "strong"},
				})

				got, err := db.GetEdge(ctx, tenant, "e1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.SourceID).To(gomega.Equal("a"))
				gomega.Expect(got.TargetID).To(gomega.Equal("b"))
				gomega.Expect(got.ID).To(gomega.Equal("e1"))
				gomega.Expect(got.Label).To(gomega.Equal("MAPS"))
				gomega.Expect(got.ValidFrom.Equal(contractT0)).To(gomega.BeTrue())
				gomega.Expect(got.ValidTo).To(gomega.BeNil())
				gomega.Expect(got.DeterminedBy).To(gomega.Equal("job-1"))
				gomega.Expect(got.DeterminationType).To(gomega.Equal("llm"))
				gomega.Expect(got.Confidence).To(gomega.Equal(0.9))
				gomega.Expect(got.Supersedes).To(gomega.Equal("e0"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("kind", "strong"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("id", "e1"))
			})

			ginkgo.It("returns ErrEdgeNotFound for an unknown ID", func() {
				_, err := db.GetEdge(ctx, tenant, "nope")
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrEdgeNotFound))
			})

			ginkgo.DescribeTable("rejects a missing endpoint with ErrNodeNotFound and creates nothing",
				func(src, tgt, missing string) {
					err := db.CreateEdge(ctx, tenant, src, tgt, graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(missing))
					gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.BeEmpty())
				},
				ginkgo.Entry("missing source", "zz", "b", "zz"),
				ginkgo.Entry("missing target", "a", "zz", "zz"),
			)
			ginkgo.It("rejects a duplicate edge ID with ErrEdgeExists and keeps the original", func() {
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0, Confidence: 0.5})
				err := db.CreateEdge(ctx, tenant, "a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0, Confidence: 0.9})
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrEdgeExists))
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("e1"))

				rels := currentEdges(graphdb.RelationshipQuery{})
				gomega.Expect(rels).To(gomega.HaveLen(1))
				gomega.Expect(rels[0].Edge.Confidence).To(gomega.Equal(0.5))
			})

			ginkgo.It("does not deduplicate empty edge IDs", func() {
				mustEdge("a", "b", graphdb.Edge{Label: "MAPS", ValidFrom: contractT0})
				mustEdge("a", "b", graphdb.Edge{Label: "MAPS", ValidFrom: contractT0})
				gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.HaveLen(2))
			})
		})

		ginkgo.Describe("CreateRequiresEdge", func() {
			var req graphdb.RequiresEdge

			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				req = graphdb.RequiresEdge{
					SourceID:        "a",
					TargetID:        "b",
					Confidence:      0.75,
					Unanimous:       true,
					ValidVotes:      3,
					TotalVotes:      4,
					VoteWeight:      3.0,
					Models:          []string{"m1", "m2"},
					SamplesPerModel: 2,
					PromptVersion:   "1.0.0",
					AnalyzedAt:      contractT0.Add(250 * time.Millisecond),
					JobID:           "job-1",
				}
			})

			ginkgo.It("writes a deterministic ID, valid_from and consensus properties", func() {
				gomega.Expect(db.CreateRequiresEdge(ctx, tenant, req)).To(gomega.Succeed())

				got, err := db.GetEdge(ctx, tenant, req.EdgeID())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.SourceID).To(gomega.Equal("a"))
				gomega.Expect(got.TargetID).To(gomega.Equal("b"))
				gomega.Expect(got.Label).To(gomega.Equal("REQUIRES"))
				gomega.Expect(got.ValidFrom.Equal(req.AnalyzedAt)).To(gomega.BeTrue(), "valid_from %s", got.ValidFrom)
				gomega.Expect(got.Confidence).To(gomega.Equal(0.75))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("unanimous", true))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("valid_votes", float64(3)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("total_votes", float64(4)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("vote_weight", 3.0))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("samples_per_model", float64(2)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("prompt_version", "1.0.0"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("job_id", "job-1"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("analyzed_at", graphdb.FormatTime(req.AnalyzedAt)))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("models", []any{"m1", "m2"}))
			})

			ginkgo.It("rejects re-materialization with ErrEdgeExists and keeps the original edge", func() {
				gomega.Expect(db.CreateRequiresEdge(ctx, tenant, req)).To(gomega.Succeed())

				// Same JobID, SourceID and TargetID, so the same EdgeID, but
				// every other field changed: the stored edge must not move.
				again := req
				again.Confidence = 0.25
				again.Unanimous = false
				again.Models = []string{"m3"}
				again.PromptVersion = "2.0.0"
				again.AnalyzedAt = req.AnalyzedAt.Add(time.Hour)
				gomega.Expect(again.EdgeID()).To(gomega.Equal(req.EdgeID()))
				err := db.CreateRequiresEdge(ctx, tenant, again)
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrEdgeExists))
				gomega.Expect(err.Error()).To(gomega.ContainSubstring(req.EdgeID()))

				gomega.Expect(currentEdges(graphdb.RelationshipQuery{EdgeLabel: "REQUIRES"})).To(gomega.HaveLen(1))
				got, err := db.GetEdge(ctx, tenant, req.EdgeID())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Confidence).To(gomega.Equal(0.75))
				gomega.Expect(got.ValidFrom.Equal(req.AnalyzedAt)).To(gomega.BeTrue(), "valid_from %s", got.ValidFrom)
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("unanimous", true))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("models", []any{"m1", "m2"}))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("prompt_version", "1.0.0"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("analyzed_at", graphdb.FormatTime(req.AnalyzedAt)))
			})

			ginkgo.It("is visible to QueryAsOf from AnalyzedAt onward", func() {
				gomega.Expect(db.CreateRequiresEdge(ctx, tenant, req)).To(gomega.Succeed())
				q := graphdb.RelationshipQuery{EdgeLabel: "REQUIRES"}

				at, err := db.QueryAsOf(ctx, tenant, q, req.AnalyzedAt)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(at).To(gomega.HaveLen(1))

				before, err := db.QueryAsOf(ctx, tenant, q, req.AnalyzedAt.Add(-time.Nanosecond))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(before).To(gomega.BeEmpty())
			})
			ginkgo.It("rejects a missing endpoint with ErrNodeNotFound", func() {
				req.TargetID = "zz"
				err := db.CreateRequiresEdge(ctx, tenant, req)
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrNodeNotFound))
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("zz"))
				gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.BeEmpty())
			})
		})

		ginkgo.Describe("BulkCreateEdges", func() {
			bulk := func(src, tgt, id string) graphdb.BulkEdge {
				return graphdb.BulkEdge{SourceID: src, TargetID: tgt, Edge: graphdb.Edge{ID: id, Label: "MAPS", ValidFrom: contractT0}}
			}

			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustNode("c", "Control")
			})

			ginkgo.DescribeTable("returns nil, nil for empty input and stores nothing",
				func(batch []graphdb.BulkEdge) {
					ids, err := db.BulkCreateEdges(ctx, tenant, batch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ids).To(gomega.BeNil())
					gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.BeEmpty())
				},
				ginkgo.Entry("nil slice", []graphdb.BulkEdge(nil)),
				ginkgo.Entry("empty slice", []graphdb.BulkEdge{}),
			)

			ginkgo.DescribeTable("rejects a malformed tenant even for empty input",
				func(batch []graphdb.BulkEdge, bad, wantMsg string) {
					ids, err := db.BulkCreateEdges(ctx, bad, batch)
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrTenantRequired))
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(wantMsg))
					gomega.Expect(ids).To(gomega.BeNil())
				},
				ginkgo.Entry("nil slice with an empty tenant", []graphdb.BulkEdge(nil), "", "tenant ID required"),
				ginkgo.Entry("empty slice with an empty tenant", []graphdb.BulkEdge{}, "", "tenant ID required"),
				ginkgo.Entry("nil slice with an uppercase tenant", []graphdb.BulkEdge(nil), "Acme-Corp", "3-52 characters"),
				ginkgo.Entry("empty slice with a SQL breakout tenant", []graphdb.BulkEdge{}, "x'); --", "3-52 characters"),
			)

			ginkgo.DescribeTable("returns nil, nil for empty input with a valid tenant that has no graph, and creates none",
				func(batch []graphdb.BulkEdge) {
					missing := randomTenantID("contract-missing")
					ids, err := db.BulkCreateEdges(ctx, missing, batch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ids).To(gomega.BeNil())

					_, err = db.GetNode(ctx, missing, "a")
					gomega.Expect(err).To(gomega.MatchError(graphdb.ErrGraphNotFound))
				},
				ginkgo.Entry("nil slice", []graphdb.BulkEdge(nil)),
				ginkgo.Entry("empty slice", []graphdb.BulkEdge{}),
			)

			ginkgo.It("creates every edge and returns the IDs in order", func() {
				ids, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{bulk("a", "b", "e1"), bulk("b", "c", "e2")})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ids).To(gomega.Equal([]string{"e1", "e2"}))
				gomega.Expect(relEdgeIDs(currentEdges(graphdb.RelationshipQuery{}))).To(gomega.ConsistOf("e1", "e2"))
			})

			ginkgo.DescribeTable("persists nothing when any edge fails",
				func(batch []graphdb.BulkEdge, sentinel error) {
					ids, err := db.BulkCreateEdges(ctx, tenant, batch)
					gomega.Expect(err).To(gomega.MatchError(sentinel))
					gomega.Expect(ids).To(gomega.BeNil())
					gomega.Expect(currentEdges(graphdb.RelationshipQuery{})).To(gomega.BeEmpty())
				},
				ginkgo.Entry("missing endpoint mid-batch",
					[]graphdb.BulkEdge{bulk("a", "b", "e1"), bulk("a", "zz", "e2"), bulk("b", "c", "e3")}, graphdb.ErrNodeNotFound),
				ginkgo.Entry("duplicate ID within the batch",
					[]graphdb.BulkEdge{bulk("a", "b", "e1"), bulk("b", "c", "e1")}, graphdb.ErrEdgeExists),
			)

			ginkgo.DescribeTable("reports the index of the first failing edge as a *graphdb.BulkEdgeError",
				func(batch []graphdb.BulkEdge, index int, sentinel error) {
					_, err := db.BulkCreateEdges(ctx, tenant, batch)
					gomega.Expect(err).To(gomega.MatchError(sentinel))
					var be *graphdb.BulkEdgeError
					gomega.Expect(errors.As(err, &be)).To(gomega.BeTrue(), "a per-edge BulkCreateEdges error must be a *graphdb.BulkEdgeError")
					gomega.Expect(be.Index).To(gomega.Equal(index))
					gomega.Expect(err.Error()).To(gomega.HavePrefix(fmt.Sprintf("bulk create edges [%d]: ", index)))
				},
				ginkgo.Entry("missing endpoint after a valid edge",
					[]graphdb.BulkEdge{bulk("a", "b", "e1"), bulk("a", "zz", "e2")}, 1, graphdb.ErrNodeNotFound),
				ginkgo.Entry("duplicate ID after valid edges",
					[]graphdb.BulkEdge{bulk("a", "b", "e1"), bulk("b", "c", "e2"), bulk("a", "c", "e1")}, 2, graphdb.ErrEdgeExists),
				ginkgo.Entry("first of two failures",
					[]graphdb.BulkEdge{bulk("a", "b", "e1"), bulk("b", "c", "e2"), bulk("zz", "c", "e3"), bulk("a", "c", "e1")}, 2, graphdb.ErrNodeNotFound),
			)

			ginkgo.It("persists nothing when an ID already exists in the graph", func() {
				mustEdge("a", "b", graphdb.Edge{ID: "e0", Label: "MAPS", ValidFrom: contractT0})
				ids, err := db.BulkCreateEdges(ctx, tenant, []graphdb.BulkEdge{bulk("b", "c", "e9"), bulk("a", "c", "e0")})
				gomega.Expect(err).To(gomega.MatchError(graphdb.ErrEdgeExists))
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("[1]"))
				var be *graphdb.BulkEdgeError
				gomega.Expect(errors.As(err, &be)).To(gomega.BeTrue())
				gomega.Expect(be.Index).To(gomega.Equal(1))
				gomega.Expect(ids).To(gomega.BeNil())
				gomega.Expect(relEdgeIDs(currentEdges(graphdb.RelationshipQuery{}))).To(gomega.ConsistOf("e0"))
			})
		})

		ginkgo.Describe("QueryRelationships", func() {
			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustNode("x", "Artifact")
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0, DeterminedBy: "job-1", Properties: map[string]any{"kind": "strong", "count": 3}})
				mustEdge("a", "x", graphdb.Edge{ID: "e2", Label: "DEMANDS", ValidFrom: contractT0, DeterminedBy: contractHostileValue, Properties: map[string]any{"kind": "weak"}})
			})

			ginkgo.DescribeTable("filters by labels and properties",
				func(q graphdb.RelationshipQuery, want []string) {
					gomega.Expect(relEdgeIDs(currentEdges(q))).To(gomega.ConsistOf(want))
				},
				ginkgo.Entry("no filter", graphdb.RelationshipQuery{}, []string{"e1", "e2"}),
				ginkgo.Entry("edge label", graphdb.RelationshipQuery{EdgeLabel: "MAPS"}, []string{"e1"}),
				ginkgo.Entry("target label", graphdb.RelationshipQuery{TargetLabel: "Artifact"}, []string{"e2"}),
				ginkgo.Entry("source label", graphdb.RelationshipQuery{SourceLabel: "Control"}, []string{"e1", "e2"}),
				ginkgo.Entry("source label without matches", graphdb.RelationshipQuery{SourceLabel: "Artifact"}, []string{}),
				ginkgo.Entry("property", graphdb.RelationshipQuery{Properties: map[string]any{"kind": "weak"}}, []string{"e2"}),
				ginkgo.Entry("numeric property", graphdb.RelationshipQuery{Properties: map[string]any{"count": 3}}, []string{"e1"}),
				ginkgo.Entry("reserved key id", graphdb.RelationshipQuery{Properties: map[string]any{"id": "e2"}}, []string{"e2"}),
				ginkgo.Entry("reserved key determined_by", graphdb.RelationshipQuery{Properties: map[string]any{"determined_by": "job-1"}}, []string{"e1"}),
				ginkgo.Entry("value needing escapes", graphdb.RelationshipQuery{Properties: map[string]any{"determined_by": contractHostileValue}}, []string{"e2"}),
			)

			ginkgo.It("returns the source and target nodes", func() {
				rels := currentEdges(graphdb.RelationshipQuery{EdgeLabel: "MAPS"})
				gomega.Expect(rels).To(gomega.HaveLen(1))
				gomega.Expect(rels[0].Source.ID).To(gomega.Equal("a"))
				gomega.Expect(rels[0].Source.Label).To(gomega.Equal("Control"))
				gomega.Expect(rels[0].Target.ID).To(gomega.Equal("b"))
			})

			ginkgo.It("excludes superseded edges but ignores node validity", func() {
				ok, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{EdgeID: "e1", SupersededAt: contractT0.Add(time.Hour)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ok).To(gomega.BeTrue())
				ok, err = db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "x", SupersededAt: contractT0.Add(time.Hour)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ok).To(gomega.BeTrue())

				gomega.Expect(relEdgeIDs(currentEdges(graphdb.RelationshipQuery{}))).To(gomega.ConsistOf("e2"))
			})

			ginkgo.It("excludes edges created with valid_to set", func() {
				mustEdge("b", "a", graphdb.Edge{ID: "e3", Label: "MAPS", ValidFrom: contractT0, ValidTo: timePtr(contractT0.Add(time.Hour))})
				gomega.Expect(relEdgeIDs(currentEdges(graphdb.RelationshipQuery{}))).To(gomega.ConsistOf("e1", "e2"))
			})
		})

		ginkgo.Describe("QueryAsOf", func() {
			asOf := func(t time.Time) []string {
				rels, err := db.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{}, t)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return relEdgeIDs(rels)
			}

			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
			})

			ginkgo.It("orders sub-second timestamps chronologically", func() {
				mustEdge("a", "b", graphdb.Edge{ID: "early", Label: "MAPS", ValidFrom: contractT0})
				mustEdge("a", "b", graphdb.Edge{ID: "late", Label: "MAPS", ValidFrom: contractT0.Add(500 * time.Millisecond)})

				gomega.Expect(asOf(contractT0.Add(250 * time.Millisecond))).To(gomega.ConsistOf("early"))
				gomega.Expect(asOf(contractT0.Add(750 * time.Millisecond))).To(gomega.ConsistOf("early", "late"))
			})

			ginkgo.It("treats valid_to as exclusive", func() {
				end := contractT0.Add(time.Second)
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0, ValidTo: &end})

				gomega.Expect(asOf(end.Add(-time.Nanosecond))).To(gomega.ConsistOf("e1"))
				gomega.Expect(asOf(end)).To(gomega.BeEmpty())
			})

			ginkgo.It("filters edges only and ignores node validity", func() {
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})
				ok, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "a", SupersededAt: contractT0.Add(time.Second)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ok).To(gomega.BeTrue())

				gomega.Expect(asOf(contractT0.Add(2 * time.Second))).To(gomega.ConsistOf("e1"))
			})

			ginkgo.It("applies the relationship query filters", func() {
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})
				mustEdge("a", "b", graphdb.Edge{ID: "e2", Label: "OTHER", ValidFrom: contractT0})

				rels, err := db.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{EdgeLabel: "MAPS"}, contractT0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(relEdgeIDs(rels)).To(gomega.ConsistOf("e1"))
			})

			ginkgo.DescribeTable("accepts reserved keys in the property filter",
				func(props map[string]any, want []string) {
					mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0, DeterminedBy: "job-1"})
					mustEdge("a", "b", graphdb.Edge{ID: "e2", Label: "MAPS", ValidFrom: contractT0, DeterminedBy: "job-2"})
					mustEdge("a", "b", graphdb.Edge{ID: "e3", Label: "MAPS", ValidFrom: contractT0, DeterminedBy: contractHostileValue})

					rels, err := db.QueryAsOf(ctx, tenant, graphdb.RelationshipQuery{Properties: props}, contractT0)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(relEdgeIDs(rels)).To(gomega.ConsistOf(want))
				},
				ginkgo.Entry("id", map[string]any{"id": "e2"}, []string{"e2"}),
				ginkgo.Entry("determined_by", map[string]any{"determined_by": "job-1"}, []string{"e1"}),
				ginkgo.Entry("value needing escapes", map[string]any{"determined_by": contractHostileValue}, []string{"e3"}),
			)
		})

		ginkgo.Describe("Traverse", func() {
			traverse := func(q graphdb.TraversalQuery) [][]string {
				paths, err := db.Traverse(ctx, tenant, q)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return pathNodeIDs(paths)
			}

			// a -NEXT-> b -NEXT-> c -OTHER-> d, where c->d starts an hour later.
			ginkgo.BeforeEach(func() {
				for _, id := range []string{"a", "b", "c", "d"} {
					mustNode(id, "Control")
				}
				mustEdge("a", "b", graphdb.Edge{ID: "ab", Label: "NEXT", ValidFrom: contractT0})
				mustEdge("b", "c", graphdb.Edge{ID: "bc", Label: "NEXT", ValidFrom: contractT0})
				mustEdge("c", "d", graphdb.Edge{ID: "cd", Label: "OTHER", ValidFrom: contractT0.Add(time.Hour)})
			})

			ginkgo.DescribeTable("returns every path within the query bounds",
				func(q graphdb.TraversalQuery, want [][]string) {
					gomega.Expect(traverse(q)).To(gomega.ConsistOf(want))
				},
				ginkgo.Entry("outbound, unlimited depth",
					graphdb.TraversalQuery{StartNode: "a", Direction: "outbound"},
					[][]string{{"a", "b"}, {"a", "b", "c"}, {"a", "b", "c", "d"}}),
				ginkgo.Entry("max depth 2",
					graphdb.TraversalQuery{StartNode: "a", MaxDepth: 2},
					[][]string{{"a", "b"}, {"a", "b", "c"}}),
				ginkgo.Entry("edge label filter",
					graphdb.TraversalQuery{StartNode: "a", EdgeLabels: []string{"NEXT"}},
					[][]string{{"a", "b"}, {"a", "b", "c"}}),
				ginkgo.Entry("inbound",
					graphdb.TraversalQuery{StartNode: "c", Direction: "inbound"},
					[][]string{{"c", "b"}, {"c", "b", "a"}}),
				ginkgo.Entry("both directions, depth 1",
					graphdb.TraversalQuery{StartNode: "b", Direction: "both", MaxDepth: 1},
					[][]string{{"b", "a"}, {"b", "c"}}),
				ginkgo.Entry("unknown direction means outbound",
					graphdb.TraversalQuery{StartNode: "a", Direction: "sideways", MaxDepth: 1},
					[][]string{{"a", "b"}}),
				ginkgo.Entry("AsOf drops paths containing an edge not yet valid",
					graphdb.TraversalQuery{StartNode: "a", AsOf: timePtr(contractT0.Add(time.Minute))},
					[][]string{{"a", "b"}, {"a", "b", "c"}}),
			)

			ginkgo.It("returns no paths and no error for a missing start node", func() {
				gomega.Expect(traverse(graphdb.TraversalQuery{StartNode: "nope"})).To(gomega.BeEmpty())
			})

			ginkgo.It("never reuses an edge within a path", func() {
				mustEdge("b", "a", graphdb.Edge{ID: "ba", Label: "NEXT", ValidFrom: contractT0})
				gomega.Expect(traverse(graphdb.TraversalQuery{StartNode: "a", EdgeLabels: []string{"NEXT"}})).To(gomega.ConsistOf(
					[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"a", "b", "a"},
				))
			})
		})

		ginkgo.Describe("SupersedeFact", func() {
			ginkgo.BeforeEach(func() {
				mustNode("a", "Control")
				mustNode("b", "Control")
				mustEdge("a", "b", graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: contractT0})
			})

			ginkgo.It("marks an edge superseded and records superseded_by", func() {
				at := contractT0.Add(time.Hour)
				ok, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{EdgeID: "e1", SupersededAt: at, SupersededByJobID: "job-2"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ok).To(gomega.BeTrue())

				got, err := db.GetEdge(ctx, tenant, "e1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.ValidTo).NotTo(gomega.BeNil())
				gomega.Expect(got.ValidTo.Equal(at)).To(gomega.BeTrue())
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("superseded_by", "job-2"))
				gomega.Expect(got.Properties).To(gomega.HaveKeyWithValue("valid_to", graphdb.FormatTime(at)))
			})

			ginkgo.It("overwrites valid_to unconditionally", func() {
				for _, at := range []time.Time{contractT0.Add(2 * time.Hour), contractT0.Add(time.Hour)} {
					_, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{EdgeID: "e1", SupersededAt: at})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				got, err := db.GetEdge(ctx, tenant, "e1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.ValidTo.Equal(contractT0.Add(time.Hour))).To(gomega.BeTrue())
			})

			ginkgo.It("marks a node superseded", func() {
				at := contractT0.Add(time.Hour)
				ok, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "a", SupersededAt: at})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ok).To(gomega.BeTrue())

				got, err := db.GetNode(ctx, tenant, "a")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.ValidTo).NotTo(gomega.BeNil())
				gomega.Expect(got.ValidTo.Equal(at)).To(gomega.BeTrue())
			})

			ginkgo.DescribeTable("returns false without error for an unknown ID",
				func(req graphdb.SupersedeRequest) {
					ok, err := db.SupersedeFact(ctx, tenant, req)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(ok).To(gomega.BeFalse())
				},
				ginkgo.Entry("edge", graphdb.SupersedeRequest{EdgeID: "nope", SupersededAt: contractT0}),
				ginkgo.Entry("node", graphdb.SupersedeRequest{NodeID: "nope", SupersededAt: contractT0}),
			)

			ginkgo.It("rejects a request naming both a node and an edge", func() {
				_, err := db.SupersedeFact(ctx, tenant, graphdb.SupersedeRequest{NodeID: "a", EdgeID: "e1", SupersededAt: contractT0})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("not both"))

				node, err := db.GetNode(ctx, tenant, "a")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(node.ValidTo).To(gomega.BeNil())

				edge, err := db.GetEdge(ctx, tenant, "e1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(edge.ValidTo).To(gomega.BeNil())
			})
		})
	}
}
