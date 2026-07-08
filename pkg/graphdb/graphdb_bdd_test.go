//go:build !integration

package graphdb_test

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestGraphDBBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GraphDB BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
// NOTE: Uses BeforeEach (not BeforeSuite) to avoid conflicts with
// SynchronizedBeforeSuite in the integration BDD file when building
// with -tags integration.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("GraphDB System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting GraphDB BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("GraphDB BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" — what business behaviors the graph
	// database layer supports in the compliance mapping domain.
	// =================================================================

	Describe("AGType Parsing Behaviors", func() {
		Context("when reconstructing compliance graph nodes from Apache AGE wire format", func() {
			It("deserializes a requirement vertex into a domain Node with temporal attributes", func() {
				By("parsing a valid AGE vertex representation")
				raw := `{"id": 123, "label": "Requirement", "properties": {"id": "req-1", "valid_from": "2025-01-01T00:00:00Z", "created_by": "test"}}::vertex`
				node, err := graphdb.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())

				By("extracting the domain-level node identity")
				Expect(node.ID).To(Equal("req-1"))
				Expect(node.Label).To(Equal("Requirement"))

				By("preserving audit metadata for compliance traceability")
				Expect(node.CreatedBy).To(Equal("test"))
				expectedTime, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				Expect(node.ValidFrom).To(Equal(expectedTime))
			})

			It("deserializes a compliance edge with confidence scoring", func() {
				By("parsing a SATISFIES relationship from AGE format")
				raw := `{"id": 789, "label": "SATISFIES", "start_id": 100, "end_id": 200, "properties": {"id": "edge-1", "source": "req-1", "target": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z", "confidence": 0.95}}::edge`
				edge, err := graphdb.ParseAGEdge(raw)
				Expect(err).NotTo(HaveOccurred())

				By("extracting edge identity and compliance relationship type")
				Expect(edge.ID).To(Equal("edge-1"))
				Expect(edge.Label).To(Equal("SATISFIES"))

				By("preserving source and target for graph traversal")
				Expect(edge.Source).To(Equal("req-1"))
				Expect(edge.Target).To(Equal("ctrl-1"))

				By("retaining AI-generated confidence scores for mapping quality")
				Expect(edge.Confidence).To(Equal(0.95))
			})

			It("reconstructs a compliance path traversal from AGE path format", func() {
				By("building a vertex-edge-vertex path as AGE would return it")
				vertex1 := `{"id": 1, "label": "Requirement", "properties": {"id": "req-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
				edge := `{"id": 10, "label": "SATISFIES", "start_id": 1, "end_id": 2, "properties": {"id": "e-1", "source": "req-1", "target": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z"}}::edge`
				vertex2 := `{"id": 2, "label": "Control", "properties": {"id": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
				raw := "[" + vertex1 + ", " + edge + ", " + vertex2 + "]::path"

				By("parsing the complete path")
				path, err := graphdb.ParseAGPath(raw)
				Expect(err).NotTo(HaveOccurred())

				By("verifying the path contains the correct number of nodes and edges")
				Expect(path.Nodes).To(HaveLen(2))
				Expect(path.Edges).To(HaveLen(1))

				By("confirming node ordering matches the traversal direction")
				Expect(path.Nodes[0].ID).To(Equal("req-1"))
				Expect(path.Nodes[1].ID).To(Equal("ctrl-1"))
				Expect(path.Edges[0].Label).To(Equal("SATISFIES"))
			})
		})

		Context("when enforcing input validation for graph mutations", func() {
			var client graphdb.GraphDB

			BeforeEach(func() {
				// nil db is safe because validation fires before any SQL
				var newErr error
				client, newErr = graphdb.New(nil)
				Expect(newErr).NotTo(HaveOccurred())
			})

			It("rejects nodes missing required identity to prevent orphaned vertices", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")

				By("rejecting a node with empty ID")
				err := client.CreateNode(nil, "test-tenant", graphdb.Node{Label: "X", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create node: id is required"))

				By("rejecting a node with empty label")
				err = client.CreateNode(nil, "test-tenant", graphdb.Node{ID: "n-1", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create node: label is required"))

				By("rejecting a node missing temporal validity")
				err = client.CreateNode(nil, "test-tenant", graphdb.Node{ID: "n-1", Label: "X"}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create node: valid_from is required"))
			})

			It("rejects edges missing required fields to maintain graph integrity", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")

				By("rejecting an edge with empty label")
				err := client.CreateEdge(nil, "test-tenant", graphdb.Edge{Source: "a", Target: "b", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: label is required"))

				By("rejecting an edge with empty source")
				err = client.CreateEdge(nil, "test-tenant", graphdb.Edge{Label: "R", Target: "b", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))

				By("rejecting an edge with empty target")
				err = client.CreateEdge(nil, "test-tenant", graphdb.Edge{Label: "R", Source: "a", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))

				By("rejecting an edge missing temporal validity")
				err = client.CreateEdge(nil, "test-tenant", graphdb.Edge{Label: "R", Source: "a", Target: "b"}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: valid_from is required"))
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// These specs verify that serialization round-trips preserve
	// all domain-relevant attributes without data loss.
	// =================================================================

	Describe("Cypher Serialization Compliance", func() {
		Context("when serializing Node properties to AGE Cypher format", func() {
			It("serializes a minimal node with only required fields", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				n := graphdb.Node{
					ID:        "req-1",
					ValidFrom: validFrom,
				}
				got := graphdb.NodeToAGProperties(n)

				By("including required identity and temporal fields")
				Expect(got).To(ContainSubstring("id: 'req-1'"))
				Expect(got).To(ContainSubstring("valid_from: '2025-01-01T00:00:00Z'"))

				By("wrapping output in Cypher property map braces")
				Expect(got).To(HavePrefix("{"))
				Expect(got).To(HaveSuffix("}"))

				By("omitting optional fields that are not set")
				Expect(got).NotTo(ContainSubstring("valid_to"))
				Expect(got).NotTo(ContainSubstring("created_by"))
			})

			It("serializes a fully-populated node with all optional fields", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				validTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
				n := graphdb.Node{
					ID:             "req-2",
					ValidFrom:      validFrom,
					ValidTo:        &validTo,
					CreatedBy:      "admin",
					CreationMethod: "import",
					Properties:     map[string]any{"severity": "high"},
				}
				got := graphdb.NodeToAGProperties(n)

				Expect(got).To(ContainSubstring("id: 'req-2'"))
				Expect(got).To(ContainSubstring("valid_to: '2025-12-31T23:59:59Z'"))
				Expect(got).To(ContainSubstring("created_by: 'admin'"))
				Expect(got).To(ContainSubstring("creation_method: 'import'"))
				Expect(got).To(ContainSubstring("severity: 'high'"))
			})
		})

		Context("when serializing Edge properties to AGE Cypher format", func() {
			It("serializes a minimal edge with only required fields", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				e := graphdb.Edge{
					ID:        "e-1",
					Source:    "a",
					Target:    "b",
					ValidFrom: validFrom,
				}
				got := graphdb.EdgeToAGProperties(e)

				Expect(got).To(ContainSubstring("id: 'e-1'"))
				Expect(got).To(ContainSubstring("source: 'a'"))
				Expect(got).To(ContainSubstring("target: 'b'"))
				Expect(got).To(HavePrefix("{"))
				Expect(got).To(HaveSuffix("}"))
			})

			It("serializes an edge with all optional fields", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				validTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
				e := graphdb.Edge{
					ID:                "e-2",
					Source:            "src",
					Target:            "tgt",
					ValidFrom:         validFrom,
					ValidTo:           &validTo,
					DeterminedBy:      "scanner",
					DeterminationType: "automated",
					Confidence:        0.85,
					Supersedes:        "e-0",
				}
				got := graphdb.EdgeToAGProperties(e)

				Expect(got).To(ContainSubstring("determined_by: 'scanner'"))
				Expect(got).To(ContainSubstring("determination_type: 'automated'"))
				Expect(got).To(ContainSubstring("confidence: 0.85"))
				Expect(got).To(ContainSubstring("supersedes: 'e-0'"))
			})
		})

		Context("when escaping values for Cypher string literals", func() {
			DescribeTable("escapeCypher produces safe Cypher strings",
				func(input, expected string) {
					Expect(graphdb.EscapeCypher(input)).To(Equal(expected))
				},
				Entry("no special chars", "hello", "hello"),
				Entry("backslash", `a\b`, `a\\b`),
				Entry("single quote", "it's", `it\'s`),
				Entry("both backslash and quote", `it's a\b`, `it\'s a\\b`),
				Entry("empty string", "", ""),
				Entry("multiple backslashes", `a\\b`, `a\\\\b`),
				Entry("dollar-quote tag stripped",
					"prefix"+graphdb.ExportCypherDollarTag+"suffix",
					"prefixsuffix"),
				Entry("bare dollar signs preserved", "cost is $100", "cost is $100"),
				Entry("bare $$ preserved", "foo $$ bar", "foo $$ bar"),
			)

			DescribeTable("cypherValue formats Go values as Cypher literals",
				func(input any, expected string) {
					Expect(graphdb.CypherValue(input)).To(Equal(expected))
				},
				Entry("string", "hello", "'hello'"),
				Entry("string with quote", "it's", `'it\'s'`),
				Entry("float64", float64(3.14), "3.14"),
				Entry("float64 integer", float64(42), "42"),
				Entry("float32", float32(2.5), "2.5"),
				Entry("int", 7, "7"),
				Entry("int64", int64(99), "99"),
				Entry("bool true", true, "true"),
				Entry("bool false", false, "false"),
				Entry("other type", []int{1, 2}, "'[1 2]'"),
			)
		})
	})

	Describe("Telemetry Integration", func() {
		Context("when creating a client without telemetry", func() {
			It("initializes with nil telemetry fields", func() {
				client, err := graphdb.New(nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(client).NotTo(BeNil())

				tf := graphdb.ExportTelemetryFields(client)
				Expect(tf.HasTracer).To(BeFalse(), "tracer should be nil without telemetry")
				Expect(tf.HasMeter).To(BeFalse(), "meter should be nil without telemetry")
				Expect(tf.HasQueryCounter).To(BeFalse(), "queryCounter should be nil without telemetry")
				Expect(tf.HasQueryLatency).To(BeFalse(), "queryLatency should be nil without telemetry")
			})
		})

		Context("when creating a client with telemetry", func() {
			It("initializes all telemetry instruments", func() {
				tp := tracenoop.NewTracerProvider()
				tracer := tp.Tracer("graphdb-test")
				mp := metricnoop.NewMeterProvider()
				meter := mp.Meter("graphdb-test")

				client, err := graphdb.New(nil, graphdb.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())
				Expect(client).NotTo(BeNil())

				tf := graphdb.ExportTelemetryFields(client)
				Expect(tf.HasTracer).To(BeTrue(), "tracer should be set with telemetry")
				Expect(tf.HasMeter).To(BeTrue(), "meter should be set with telemetry")
				Expect(tf.HasQueryCounter).To(BeTrue(), "queryCounter should be set with telemetry")
				Expect(tf.HasQueryLatency).To(BeTrue(), "queryLatency should be set with telemetry")
			})
		})

		Context("when operations produce spans (error path, no DB)", func() {
			var (
				tp     *telemetrytest.TestProvider
				client graphdb.GraphDB
			)

			BeforeEach(func() {
				var err error
				tp, err = telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())

				tracer := tp.TracerProvider().Tracer("graphdb-test")
				meter := tp.MeterProvider().Meter("graphdb-test")
				client, err = graphdb.New(nil, graphdb.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())
			})

			AfterEach(func() {
				Expect(tp.Shutdown(context.Background())).To(Succeed())
			})

			It("emits a graphdb.CreateNode span with Error status on empty tenant", func() {
				By("calling CreateNode with valid fields but empty tenant")
				err := client.CreateNode(context.Background(), "", graphdb.Node{
					ID:        "n1",
					Label:     "Test",
					ValidFrom: time.Now(),
				})
				Expect(err).To(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, "graphdb.CreateNode")
				Expect(span).NotTo(BeNil(), "expected graphdb.CreateNode span")
				Expect(span.Status().Code.String()).To(Equal("Error"))
			})

			It("emits a graphdb.CreateEdge span with Error status on empty tenant", func() {
				By("calling CreateEdge with valid fields but empty tenant")
				err := client.CreateEdge(context.Background(), "", graphdb.Edge{
					Label:     "SATISFIES",
					Source:    "req-1",
					Target:    "ctrl-1",
					ValidFrom: time.Now(),
				})
				Expect(err).To(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, "graphdb.CreateEdge")
				Expect(span).NotTo(BeNil(), "expected graphdb.CreateEdge span")
				Expect(span.Status().Code.String()).To(Equal("Error"))
			})

			It("sets tenant.id span attribute even on error paths", func() {
				By("calling CreateEdge with valid fields and a specific tenant that fails at beginTx")
				_ = client.CreateNode(context.Background(), "", graphdb.Node{
					ID:        "n1",
					Label:     "Test",
					ValidFrom: time.Now(),
				})

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, "graphdb.CreateNode")
				Expect(span).NotTo(BeNil(), "expected graphdb.CreateNode span")

				tenantAttr, found := telemetrytest.SpanAttribute(span, "tenant.id")
				Expect(found).To(BeTrue(), "expected tenant.id attribute on span")
				Expect(tenantAttr.AsString()).To(Equal(""))
			})
		})
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES AND INTEGRATION SCENARIOS
	// These specs cover the "what" — detailed edge case coverage
	// ported from the original agtype_test.go and client_test.go.
	// =================================================================

	Describe("AGType Vertex Parsing Edge Cases", func() {
		Context("when parsing valid vertex representations", func() {
			It("extracts the property-level id over the graph-internal id", func() {
				raw := `{"id": 123, "label": "Requirement", "properties": {"id": "req-1", "valid_from": "2025-01-01T00:00:00Z", "created_by": "test"}}::vertex`
				node, err := graphdb.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.ID).To(Equal("req-1"))
				Expect(node.Label).To(Equal("Requirement"))
				Expect(node.CreatedBy).To(Equal("test"))

				expectedTime, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				Expect(node.ValidFrom).To(Equal(expectedTime))
			})

			It("falls back to graph-internal id when property id is absent", func() {
				raw := `{"id": 456, "label": "Control", "properties": {"valid_from": "2025-06-01T00:00:00Z"}}::vertex`
				node, err := graphdb.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.ID).To(Equal("456"))
			})

			It("parses valid_to temporal bound when present", func() {
				raw := `{"id": 10, "label": "Policy", "properties": {"id": "pol-1", "valid_from": "2025-01-01T00:00:00Z", "valid_to": "2025-12-31T23:59:59Z"}}::vertex`
				node, err := graphdb.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.ValidTo).NotTo(BeNil())

				expectedTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
				Expect(*node.ValidTo).To(Equal(expectedTo))
			})
		})

		Context("when handling malformed vertex input", func() {
			It("rejects input missing the ::vertex suffix", func() {
				raw := `{"id": 1, "label": "X", "properties": {}}`
				_, err := graphdb.ParseAGVertex(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects invalid JSON body", func() {
				raw := `{bad json}::vertex`
				_, err := graphdb.ParseAGVertex(raw)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("AGType Edge Parsing Edge Cases", func() {
		Context("when parsing valid edge representations", func() {
			It("extracts full edge properties including source, target, and confidence", func() {
				raw := `{"id": 789, "label": "SATISFIES", "start_id": 100, "end_id": 200, "properties": {"id": "edge-1", "source": "req-1", "target": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z", "confidence": 0.95}}::edge`
				edge, err := graphdb.ParseAGEdge(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(edge.ID).To(Equal("edge-1"))
				Expect(edge.Label).To(Equal("SATISFIES"))
				Expect(edge.Source).To(Equal("req-1"))
				Expect(edge.Target).To(Equal("ctrl-1"))
				Expect(edge.Confidence).To(Equal(0.95))
			})

			It("falls back to start_id/end_id when property source/target are absent", func() {
				raw := `{"id": 50, "label": "RELATES", "start_id": 10, "end_id": 20, "properties": {"valid_from": "2025-01-01T00:00:00Z"}}::edge`
				edge, err := graphdb.ParseAGEdge(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(edge.Source).To(Equal("10"))
				Expect(edge.Target).To(Equal("20"))
			})
		})

		Context("when handling malformed edge input", func() {
			It("rejects input missing the ::edge suffix", func() {
				raw := `{"id": 1, "label": "X", "start_id": 1, "end_id": 2, "properties": {}}`
				_, err := graphdb.ParseAGEdge(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects invalid JSON body", func() {
				raw := `not json::edge`
				_, err := graphdb.ParseAGEdge(raw)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("AGType Path Parsing Edge Cases", func() {
		Context("when handling malformed path input", func() {
			It("rejects input missing the ::path suffix", func() {
				raw := `[{"id": 1, "label": "X", "properties": {}}::vertex]`
				_, err := graphdb.ParseAGPath(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects a non-array path body", func() {
				raw := `{"id": 1}::path`
				_, err := graphdb.ParseAGPath(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects elements with unknown type suffixes", func() {
				raw := `[{"id": 1}::unknown]::path`
				_, err := graphdb.ParseAGPath(raw)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("AGType Path Element Splitting Edge Cases", func() {
		Context("when splitting composite AGE path strings", func() {
			It("handles a single element", func() {
				result := graphdb.SplitAGPathElements(`{"id": 1, "label": "X", "properties": {}}::vertex`)
				Expect(result).To(HaveLen(1))
			})

			It("splits three elements with nested JSON correctly", func() {
				v1 := `{"id": 1, "label": "A", "properties": {"key": "val"}}::vertex`
				e := `{"id": 10, "label": "R", "start_id": 1, "end_id": 2, "properties": {"x": "y"}}::edge`
				v2 := `{"id": 2, "label": "B", "properties": {}}::vertex`
				input := v1 + ", " + e + ", " + v2

				result := graphdb.SplitAGPathElements(input)
				Expect(result).To(HaveLen(3))
			})

			It("does not split on commas inside JSON braces", func() {
				result := graphdb.SplitAGPathElements(`{"a": 1, "b": 2}::vertex`)
				Expect(result).To(HaveLen(1))
			})

			It("returns empty slice for empty input", func() {
				result := graphdb.SplitAGPathElements("")
				Expect(result).To(HaveLen(0))
			})
		})
	})

	Describe("Suffix Stripping Edge Cases", func() {
		Context("when stripping AGE type suffixes", func() {
			It("strips a valid suffix and returns the body", func() {
				body, err := graphdb.StripSuffix(`{"id": 1}::vertex`, "::vertex")
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(Equal(`{"id": 1}`))
			})

			It("strips surrounding whitespace before suffix detection", func() {
				body, err := graphdb.StripSuffix(`  {"id": 1}::edge  `, "::edge")
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(Equal(`{"id": 1}`))
			})

			It("returns an error for wrong suffix", func() {
				_, err := graphdb.StripSuffix(`{"id": 1}::vertex`, "::edge")
				Expect(err).To(HaveOccurred())
			})

			It("returns an error when no suffix is present", func() {
				_, err := graphdb.StripSuffix(`{"id": 1}`, "::vertex")
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("CreateNode Validation Edge Cases", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var newErr error
			client, newErr = graphdb.New(nil)
			Expect(newErr).NotTo(HaveOccurred())
		})

		Context("when validating node creation inputs", func() {
			It("rejects a node with empty id", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateNode(nil, "test-tenant", graphdb.Node{Label: "X", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create node: id is required"))
			})

			It("rejects a node with empty label", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateNode(nil, "test-tenant", graphdb.Node{ID: "n-1", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create node: label is required"))
			})

			It("rejects a node with zero valid_from", func() {
				err := client.CreateNode(nil, "test-tenant", graphdb.Node{ID: "n-1", Label: "X"}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create node: valid_from is required"))
			})
		})
	})

	Describe("CreateEdge Validation Edge Cases", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var newErr error
			client, newErr = graphdb.New(nil)
			Expect(newErr).NotTo(HaveOccurred())
		})

		Context("when validating edge creation inputs", func() {
			It("rejects an edge with empty label", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateEdge(nil, "test-tenant", graphdb.Edge{Source: "a", Target: "b", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: label is required"))
			})

			It("rejects an edge with empty source", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateEdge(nil, "test-tenant", graphdb.Edge{Label: "R", Target: "b", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))
			})

			It("rejects an edge with empty target", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateEdge(nil, "test-tenant", graphdb.Edge{Label: "R", Source: "a", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))
			})

			It("rejects an edge with zero valid_from", func() {
				err := client.CreateEdge(nil, "test-tenant", graphdb.Edge{Label: "R", Source: "a", Target: "b"}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: valid_from is required"))
			})
		})
	})

	Describe("Node Property Serialization Edge Cases", func() {
		Context("when serializing node properties with varying field populations", func() {
			It("includes valid_from in RFC3339 format", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				n := graphdb.Node{ID: "n-1", ValidFrom: validFrom}
				got := graphdb.NodeToAGProperties(n)
				Expect(got).To(ContainSubstring("valid_from: '2025-01-01T00:00:00Z'"))
			})
		})
	})

	Describe("Edge Property Serialization Edge Cases", func() {
		Context("when serializing edge properties with varying field populations", func() {
			It("includes valid_from and valid_to when set", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				validTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
				e := graphdb.Edge{
					ID:        "e-1",
					Source:    "a",
					Target:    "b",
					ValidFrom: validFrom,
					ValidTo:   &validTo,
				}
				got := graphdb.EdgeToAGProperties(e)
				Expect(got).To(ContainSubstring("valid_to: '2025-12-31T23:59:59Z'"))
			})
		})
	})

	// Use testspecs for the unused import (compile check)
	Describe("Test Infrastructure Verification", func() {
		It("confirms testspecs helpers are available", func() {
			_ = strings.TrimSpace("ok")
			testspecs.LogTestProgress("testspecs integration confirmed")
		})
	})
})
