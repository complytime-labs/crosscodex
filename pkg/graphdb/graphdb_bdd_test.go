package graphdb_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestGraphDB(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GraphDB Contract Types Suite")
}

var _ = Describe("FormatTime", func() {
	It("writes fixed-width nanosecond UTC timestamps", func() {
		t := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
		Expect(graphdb.FormatTime(t)).To(Equal("2025-01-01T12:00:00.000000000Z"))
		Expect(graphdb.FormatTime(t.Add(500 * time.Millisecond))).To(Equal("2025-01-01T12:00:00.500000000Z"))
	})

	It("converts non-UTC times to UTC", func() {
		est := time.FixedZone("EST", -5*3600)
		t := time.Date(2025, 1, 1, 7, 0, 0, 1, est)
		Expect(graphdb.FormatTime(t)).To(Equal("2025-01-01T12:00:00.000000001Z"))
	})

	It("is parseable as RFC3339Nano", func() {
		t := time.Date(2025, 6, 30, 23, 59, 59, 123456789, time.UTC)
		parsed, err := time.Parse(time.RFC3339Nano, graphdb.FormatTime(t))
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.Equal(t)).To(BeTrue())
	})
})

var _ = Describe("RequiresEdge.EdgeID", func() {
	It("derives a deterministic ID from job, source and target", func() {
		r := graphdb.RequiresEdge{SourceID: "ac-2", TargetID: "ac-1", JobID: "job-9"}
		Expect(r.EdgeID()).To(Equal("requires_job-9_ac-2_ac-1"))
	})

	It("distinguishes direction and job", func() {
		r := graphdb.RequiresEdge{SourceID: "ac-2", TargetID: "ac-1", JobID: "job-9"}
		swapped := graphdb.RequiresEdge{SourceID: "ac-1", TargetID: "ac-2", JobID: "job-9"}
		Expect(swapped.EdgeID()).NotTo(Equal(r.EdgeID()))

		otherJob := graphdb.RequiresEdge{SourceID: "ac-2", TargetID: "ac-1", JobID: "job-10"}
		Expect(otherJob.EdgeID()).NotTo(Equal(r.EdgeID()))
	})

	It("does not collide when an underscore moves between source and target", func() {
		a := graphdb.RequiresEdge{SourceID: "a_b", TargetID: "c", JobID: "j"}
		b := graphdb.RequiresEdge{SourceID: "a", TargetID: "b_c", JobID: "j"}
		Expect(a.EdgeID()).To(Equal("requires_j_a%5Fb_c"))
		Expect(b.EdgeID()).To(Equal("requires_j_a_b%5Fc"))
	})
})

var _ = Describe("DerivedID", func() {
	DescribeTable("escapes each part and joins the parts with underscores",
		func(parts []string, want string) {
			Expect(graphdb.DerivedID(parts...)).To(Equal(want))
		},
		Entry("plain parts stay readable", []string{"requires", "job-9", "ac-2", "ac-1"}, "requires_job-9_ac-2_ac-1"),
		Entry("underscore in a part", []string{"a_b", "c"}, "a%5Fb_c"),
		Entry("underscore moved to the other part", []string{"a", "b_c"}, "a_b%5Fc"),
		Entry("percent in a part", []string{"50%", "x"}, "50%25_x"),
		Entry("literal %5F in a part", []string{"a%5Fb", "c"}, "a%255Fb_c"),
		Entry("empty part", []string{"a", "", "b"}, "a__b"),
		Entry("single part", []string{"a_b"}, "a%5Fb"),
	)

	DescribeTable("gives distinct IDs to distinct part lists",
		func(a, b []string) {
			Expect(graphdb.DerivedID(a...)).NotTo(Equal(graphdb.DerivedID(b...)))
		},
		Entry("underscore split point", []string{"a_b", "c"}, []string{"a", "b_c"}),
		Entry("escaped underscore vs literal escape", []string{"a_b"}, []string{"a%5Fb"}),
		Entry("escaped percent vs literal escape", []string{"a%"}, []string{"a%25"}),
		Entry("different part counts", []string{"a", "b"}, []string{"a_b"}),
		Entry("empty part vs no part", []string{"a", ""}, []string{"a"}),
	)
})

var _ = Describe("CheckIdentifier", func() {
	DescribeTable("accepts plain identifiers",
		func(s string) {
			Expect(graphdb.CheckIdentifier("label", s)).To(Succeed())
		},
		Entry("letters", "Control"),
		Entry("underscores and digits", "MAPS_TO_2"),
		Entry("leading underscore", "_internal"),
	)

	DescribeTable("rejects anything else with ErrInvalidCypher and names the field and value",
		func(s string) {
			err := graphdb.CheckIdentifier("label", s)
			Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
			Expect(err.Error()).To(ContainSubstring("label %q must be an identifier", s))
			Expect(err.Error()).To(ContainSubstring("Rename it"))
		},
		Entry("empty", ""),
		Entry("leading digit", "1Control"),
		Entry("space", "has space"),
		Entry("dash", "a-b"),
		Entry("pattern break-out", "Control) DETACH DELETE n //"),
		Entry("dollar-quote tag", "a$cypher$b"),
		Entry("backtick", "`x`"),
		Entry("non-ASCII letter", "Contrôle"),
		Entry("trailing newline", "Control\n"),
	)
})

var _ = Describe("CheckRelationshipQuery", func() {
	It("accepts an empty query", func() {
		Expect(graphdb.CheckRelationshipQuery(graphdb.RelationshipQuery{})).To(Succeed())
	})

	It("accepts every reserved node and edge key in the property filter, because filters only read", func() {
		props := map[string]any{}
		for k := range graphdb.ReservedNodeKeys() {
			props[k] = "v"
		}
		for k := range graphdb.ReservedEdgeKeys() {
			props[k] = "v"
		}
		Expect(graphdb.CheckRelationshipQuery(graphdb.RelationshipQuery{Properties: props})).To(Succeed())
	})

	DescribeTable("names the offending field",
		func(q graphdb.RelationshipQuery, want string) {
			err := graphdb.CheckRelationshipQuery(q)
			Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
			Expect(err.Error()).To(ContainSubstring(want))
		},
		Entry("source label", graphdb.RelationshipQuery{SourceLabel: "a b"}, `source label "a b"`),
		Entry("target label", graphdb.RelationshipQuery{TargetLabel: "a b"}, `target label "a b"`),
		Entry("edge label", graphdb.RelationshipQuery{EdgeLabel: "a b"}, `edge label "a b"`),
		Entry("property key", graphdb.RelationshipQuery{Properties: map[string]any{"ok": 1, "a b": 2}}, `property key "a b"`),
	)
})

var _ = Describe("CheckNode", func() {
	valid := func() graphdb.Node {
		return graphdb.Node{ID: "n1", Label: "Control", ValidFrom: time.Unix(0, 1), Properties: map[string]any{"title": "t"}}
	}

	It("accepts a node with ID, label and valid_from", func() {
		Expect(graphdb.CheckNode(valid())).To(Succeed())
	})

	DescribeTable("rejects an invalid node with an unprefixed, actionable message",
		func(mutate func(*graphdb.Node), want string, invalidCypher bool) {
			n := valid()
			mutate(&n)
			err := graphdb.CheckNode(n)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(errors.Is(err, graphdb.ErrInvalidCypher)).To(Equal(invalidCypher))
		},
		Entry("missing ID", func(n *graphdb.Node) { n.ID = "" }, "id is required", false),
		Entry("missing label", func(n *graphdb.Node) { n.Label = "" }, "label is required", false),
		Entry("non-identifier label", func(n *graphdb.Node) { n.Label = "a b" }, `label "a b" must be an identifier`, true),
		Entry("non-identifier property key", func(n *graphdb.Node) { n.Properties = map[string]any{"a b": 1} }, `property key "a b"`, true),
		Entry("reserved property key", func(n *graphdb.Node) { n.Properties = map[string]any{"valid_to": "x"} }, `property key "valid_to" is reserved`, true),
		Entry("missing valid_from", func(n *graphdb.Node) { n.ValidFrom = time.Time{} }, "valid_from is required", false),
	)

	It("reports the first defect in check order: id, label, label identifier, properties, valid_from", func() {
		Expect(graphdb.CheckNode(graphdb.Node{})).To(MatchError("id is required"))
		Expect(graphdb.CheckNode(graphdb.Node{ID: "n1"})).To(MatchError("label is required"))
		Expect(graphdb.CheckNode(graphdb.Node{ID: "n1", Label: "a b", Properties: map[string]any{"valid_to": "x"}})).
			To(MatchError(ContainSubstring(`label "a b" must be an identifier`)))
		Expect(graphdb.CheckNode(graphdb.Node{ID: "n1", Label: "Control", Properties: map[string]any{"valid_to": "x"}})).
			To(MatchError(ContainSubstring(`property key "valid_to" is reserved`)))
	})
})

var _ = Describe("CheckEdge", func() {
	valid := func() graphdb.Edge {
		return graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: time.Unix(0, 1), Properties: map[string]any{"kind": "strong"}}
	}

	It("accepts an edge with label, endpoints and valid_from", func() {
		Expect(graphdb.CheckEdge("a", "b", valid())).To(Succeed())
	})

	It("accepts an edge without an ID, because the edge ID is optional", func() {
		e := valid()
		e.ID = ""
		Expect(graphdb.CheckEdge("a", "b", e)).To(Succeed())
	})

	DescribeTable("rejects an invalid edge with an unprefixed, actionable message",
		func(src, tgt string, mutate func(*graphdb.Edge), want string, invalidCypher bool) {
			e := valid()
			mutate(&e)
			err := graphdb.CheckEdge(src, tgt, e)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(errors.Is(err, graphdb.ErrInvalidCypher)).To(Equal(invalidCypher))
		},
		Entry("missing label", "a", "b", func(e *graphdb.Edge) { e.Label = "" }, "label is required", false),
		Entry("non-identifier label", "a", "b", func(e *graphdb.Edge) { e.Label = "a b" }, `label "a b" must be an identifier`, true),
		Entry("non-identifier property key", "a", "b", func(e *graphdb.Edge) { e.Properties = map[string]any{"a b": 1} }, `property key "a b"`, true),
		Entry("reserved property key", "a", "b", func(e *graphdb.Edge) { e.Properties = map[string]any{"confidence": 0.9} }, `property key "confidence" is reserved`, true),
		Entry("missing source", "", "b", func(*graphdb.Edge) {}, "source and target are required", false),
		Entry("missing target", "a", "", func(*graphdb.Edge) {}, "source and target are required", false),
		Entry("missing valid_from", "a", "b", func(e *graphdb.Edge) { e.ValidFrom = time.Time{} }, "valid_from is required", false),
	)

	It("reports the label before the other defects, matching the drivers' historical order", func() {
		Expect(graphdb.CheckEdge("", "", graphdb.Edge{})).To(MatchError("label is required"))
	})
})

var _ = Describe("CheckSupersedeRequest", func() {
	at := time.Unix(0, 1)

	It("accepts a node or an edge target with superseded_at", func() {
		Expect(graphdb.CheckSupersedeRequest(graphdb.SupersedeRequest{NodeID: "a", SupersededAt: at})).To(Succeed())
		Expect(graphdb.CheckSupersedeRequest(graphdb.SupersedeRequest{EdgeID: "e1", SupersededAt: at, SupersededByJobID: "job-1"})).To(Succeed())
	})

	DescribeTable("rejects an invalid request with an unprefixed message",
		func(req graphdb.SupersedeRequest, want string) {
			Expect(graphdb.CheckSupersedeRequest(req)).To(MatchError(want))
		},
		Entry("no target", graphdb.SupersedeRequest{SupersededAt: at}, "node_id or edge_id is required"),
		Entry("both targets", graphdb.SupersedeRequest{NodeID: "a", EdgeID: "e1", SupersededAt: at}, "set node_id or edge_id, not both"),
		Entry("no superseded_at", graphdb.SupersedeRequest{NodeID: "a"}, "superseded_at is required"),
	)

	It("reports the target before superseded_at, matching the drivers' historical order", func() {
		Expect(graphdb.CheckSupersedeRequest(graphdb.SupersedeRequest{})).To(MatchError("node_id or edge_id is required"))
		Expect(graphdb.CheckSupersedeRequest(graphdb.SupersedeRequest{NodeID: "a", EdgeID: "e1"})).To(MatchError("set node_id or edge_id, not both"))
	})
})

var _ = Describe("CheckRequiresEdge", func() {
	valid := func() graphdb.RequiresEdge {
		return graphdb.RequiresEdge{SourceID: "ac-2", TargetID: "ac-1", AnalyzedAt: time.Unix(0, 1), JobID: "job-1"}
	}

	It("accepts an edge with both endpoints and analyzed_at", func() {
		Expect(graphdb.CheckRequiresEdge(valid())).To(Succeed())
	})

	DescribeTable("rejects an invalid edge with an unprefixed message",
		func(mutate func(*graphdb.RequiresEdge), want string) {
			e := valid()
			mutate(&e)
			Expect(graphdb.CheckRequiresEdge(e)).To(MatchError(want))
		},
		Entry("missing source", func(e *graphdb.RequiresEdge) { e.SourceID = "" }, "source_id is required"),
		Entry("missing target", func(e *graphdb.RequiresEdge) { e.TargetID = "" }, "target_id is required"),
		Entry("missing analyzed_at", func(e *graphdb.RequiresEdge) { e.AnalyzedAt = time.Time{} }, "analyzed_at is required"),
	)

	It("reports source, then target, then analyzed_at, matching the drivers' historical order", func() {
		Expect(graphdb.CheckRequiresEdge(graphdb.RequiresEdge{})).To(MatchError("source_id is required"))
		Expect(graphdb.CheckRequiresEdge(graphdb.RequiresEdge{SourceID: "ac-2"})).To(MatchError("target_id is required"))
	})
})

var _ = Describe("CheckPropertyKeys", func() {
	It("accepts nil and identifier keys", func() {
		Expect(graphdb.CheckPropertyKeys(nil)).To(Succeed())
		Expect(graphdb.CheckPropertyKeys(map[string]any{"a": 1, "b_2": "x"})).To(Succeed())
	})

	It("reports the first offending key in sorted order", func() {
		err := graphdb.CheckPropertyKeys(map[string]any{"z z": 1, "a a": 2, "ok": 3})
		Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
		Expect(err.Error()).To(ContainSubstring(`property key "a a"`))
	})
})

var _ = Describe("CheckTenant", func() {
	It("accepts a valid tenant ID, including the 52-character maximum", func() {
		Expect(graphdb.CheckTenant("acme-corp")).To(Succeed())
		Expect(graphdb.CheckTenant("a" + strings.Repeat("b", 50) + "c")).To(Succeed())
	})

	It("returns ErrTenantRequired itself for an empty tenant", func() {
		Expect(graphdb.CheckTenant("")).To(BeIdenticalTo(graphdb.ErrTenantRequired))
	})

	DescribeTable("rejects a malformed tenant with an error wrapping ErrTenantRequired and ErrInvalidTenant that states the rule",
		func(id string) {
			err := graphdb.CheckTenant(id)
			Expect(err).To(MatchError(graphdb.ErrTenantRequired))
			Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			Expect(err.Error()).To(Equal(tenant.ValidateTenantID(id).Error()))
			Expect(err.Error()).To(Equal(fmt.Sprintf("tenant ID %q is invalid: it must be %s. Choose an ID that matches", id, tenant.IDRule)))
		},
		Entry("SQL breakout", "x'); --"),
		Entry("uppercase", "Acme"),
		Entry("53 characters", "a"+strings.Repeat("b", 51)+"c"),
		Entry("dollar-quote tag", "a$cypher$b"),
	)
})

var _ = Describe("NodeIDConflictError", func() {
	conflict := &graphdb.NodeIDConflictError{ID: "cat/ac-2", Label: "Artifact", StoredLabel: "Control"}

	It("matches ErrNodeIDConflict and not ErrNodeExists, which graph writers treat as success", func() {
		var err error = conflict
		Expect(err).To(MatchError(graphdb.ErrNodeIDConflict))
		Expect(errors.Is(err, graphdb.ErrNodeExists)).To(BeFalse())
	})

	It("names the ID, both labels, why it was blocked and what to do instead", func() {
		Expect(conflict.Error()).To(Equal(`node ID already used under another label: node ID "cat/ac-2" belongs to a node labeled Control, so it cannot also identify a node labeled Artifact. ` +
			`Node IDs are unique across labels in a tenant's graph so that edge endpoints resolve to one node. ` +
			`Give the Artifact node an ID no other node uses (for example graphdb.DerivedID with a kind tag naming the label), or write the existing Control node instead`))
	})
})

var _ = Describe("BulkEdgeError", func() {
	It("prefixes the wrapped error with the zero-based edge index", func() {
		err := &graphdb.BulkEdgeError{Index: 2, Err: fmt.Errorf("edge %q: %w", "e1", graphdb.ErrEdgeExists)}
		Expect(err.Error()).To(Equal(`bulk create edges [2]: edge "e1": edge already exists`))
	})

	It("keeps the wrapped sentinel and typed errors reachable", func() {
		var err error = &graphdb.BulkEdgeError{Index: 0, Err: fmt.Errorf("create edge: %w", graphdb.ErrNodeNotFound)}
		Expect(err).To(MatchError(graphdb.ErrNodeNotFound))

		rk := graphdb.CheckEdgeProperties(map[string]any{"valid_from": "x"})
		Expect(rk).To(HaveOccurred())
		err = &graphdb.BulkEdgeError{Index: 3, Err: rk}
		var got *graphdb.ReservedPropertyError
		Expect(errors.As(err, &got)).To(BeTrue())
		Expect(got.Key).To(Equal("valid_from"))
		Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
	})

	It("is found through further wrapping", func() {
		err := fmt.Errorf("outer: %w", &graphdb.BulkEdgeError{Index: 7, Err: graphdb.ErrEdgeExists})
		var be *graphdb.BulkEdgeError
		Expect(errors.As(err, &be)).To(BeTrue())
		Expect(be.Index).To(Equal(7))
	})
})

var _ = Describe("CheckNodeProperties and CheckEdgeProperties", func() {
	It("accept nil and unreserved identifier keys", func() {
		Expect(graphdb.CheckNodeProperties(nil)).To(Succeed())
		Expect(graphdb.CheckNodeProperties(map[string]any{"name": "x", "confidence": 0.5, "job_id": "j"})).To(Succeed())
		Expect(graphdb.CheckEdgeProperties(nil)).To(Succeed())
		Expect(graphdb.CheckEdgeProperties(map[string]any{"kind": "x", "job_id": "j", "created_by": "u"})).To(Succeed())
	})

	DescribeTable("still reject a non-identifier key",
		func(check func(map[string]any) error) {
			err := check(map[string]any{"a b": 1})
			Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
			Expect(err.Error()).To(ContainSubstring("must be an identifier"))
		},
		Entry("CheckNodeProperties", graphdb.CheckNodeProperties),
		Entry("CheckEdgeProperties", graphdb.CheckEdgeProperties),
	)

	DescribeTable("rejects every reserved key with a ReservedPropertyError naming its field",
		func(check func(map[string]any) error, reserved map[string]string) {
			Expect(reserved).NotTo(BeEmpty())
			for key, field := range reserved {
				err := check(map[string]any{"ok": 1, key: "v"})
				Expect(err).To(MatchError(graphdb.ErrInvalidCypher), "key %q", key)
				var rk *graphdb.ReservedPropertyError
				Expect(errors.As(err, &rk)).To(BeTrue(), "key %q", key)
				Expect(*rk).To(Equal(graphdb.ReservedPropertyError{Key: key, Field: field}))
				Expect(err.Error()).To(ContainSubstring(`property key %q is reserved`, key))
				Expect(err.Error()).To(ContainSubstring(field))
			}
		},
		Entry("CheckNodeProperties", graphdb.CheckNodeProperties, graphdb.ReservedNodeKeys()),
		Entry("CheckEdgeProperties", graphdb.CheckEdgeProperties, graphdb.ReservedEdgeKeys()),
	)

	It("reserves confidence on edges only, because nodes carry a caller confidence property", func() {
		Expect(graphdb.ReservedEdgeKeys()).To(HaveKey("confidence"))
		Expect(graphdb.ReservedNodeKeys()).NotTo(HaveKey("confidence"))
	})

	It("returns copies, so callers cannot change the reserved sets", func() {
		graphdb.ReservedEdgeKeys()["kind"] = "x"
		delete(graphdb.ReservedEdgeKeys(), "id")
		Expect(graphdb.CheckEdgeProperties(map[string]any{"kind": 1})).To(Succeed())
		Expect(graphdb.CheckEdgeProperties(map[string]any{"id": 1})).To(MatchError(graphdb.ErrInvalidCypher))
	})

	It("reports the first offending key in sorted order", func() {
		err := graphdb.CheckEdgeProperties(map[string]any{"valid_from": 1, "id": 2, "kind": 3})
		Expect(err.Error()).To(ContainSubstring(`property key "id"`))
	})
})
