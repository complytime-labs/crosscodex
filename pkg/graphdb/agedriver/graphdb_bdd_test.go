//go:build !integration

package agedriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
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
				node, err := agedriver.ParseAGVertex(raw)
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
				raw := `{"id": 789, "label": "SATISFIES", "start_id": 100, "end_id": 200, "properties": {"id": "edge-1", "valid_from": "2025-01-01T00:00:00Z", "confidence": 0.95}}::edge`
				edge, err := agedriver.ParseAGEdge(raw)
				Expect(err).NotTo(HaveOccurred())

				By("extracting edge identity and compliance relationship type")
				Expect(edge.ID).To(Equal("edge-1"))
				Expect(edge.Label).To(Equal("SATISFIES"))

				By("retaining AI-generated confidence scores for mapping quality")
				Expect(edge.Confidence).To(Equal(0.95))
			})

			It("reconstructs a compliance path traversal from AGE path format", func() {
				By("building a vertex-edge-vertex path as AGE would return it")
				vertex1 := `{"id": 1, "label": "Requirement", "properties": {"id": "req-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
				edge := `{"id": 10, "label": "SATISFIES", "start_id": 1, "end_id": 2, "properties": {"id": "e-1", "valid_from": "2025-01-01T00:00:00Z"}}::edge`
				vertex2 := `{"id": 2, "label": "Control", "properties": {"id": "ctrl-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
				raw := "[" + vertex1 + ", " + edge + ", " + vertex2 + "]::path"

				By("parsing the complete path")
				path, err := agedriver.ParseAGPath(raw)
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
				client, newErr = agedriver.New(nil)
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
				err := client.CreateEdge(nil, "test-tenant", "a", "b", graphdb.Edge{ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: label is required"))

				By("rejecting an edge with empty source")
				err = client.CreateEdge(nil, "test-tenant", "", "b", graphdb.Edge{Label: "R", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))

				By("rejecting an edge with empty target")
				err = client.CreateEdge(nil, "test-tenant", "a", "", graphdb.Edge{Label: "R", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))

				By("rejecting an edge missing temporal validity")
				err = client.CreateEdge(nil, "test-tenant", "a", "b", graphdb.Edge{Label: "R"}) //nolint:staticcheck
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
				got := agedriver.NodeToAGProperties(n)

				By("including required identity and temporal fields")
				Expect(got).To(ContainSubstring("id: 'req-1'"))
				Expect(got).To(ContainSubstring("valid_from: '2025-01-01T00:00:00.000000000Z'"))

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
				got := agedriver.NodeToAGProperties(n)

				Expect(got).To(ContainSubstring("id: 'req-2'"))
				Expect(got).To(ContainSubstring("valid_to: '2025-12-31T23:59:59.000000000Z'"))
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
					ValidFrom: validFrom,
				}
				got := agedriver.EdgeToAGProperties(e)

				Expect(got).To(ContainSubstring("id: 'e-1'"))
				Expect(got).NotTo(ContainSubstring("source:"))
				Expect(got).NotTo(ContainSubstring("target:"))
				Expect(got).To(HavePrefix("{"))
				Expect(got).To(HaveSuffix("}"))
			})

			It("serializes an edge with all optional fields", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				validTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
				e := graphdb.Edge{
					ID:                "e-2",
					ValidFrom:         validFrom,
					ValidTo:           &validTo,
					DeterminedBy:      "scanner",
					DeterminationType: "automated",
					Confidence:        0.85,
					Supersedes:        "e-0",
				}
				got := agedriver.EdgeToAGProperties(e)

				Expect(got).To(ContainSubstring("determined_by: 'scanner'"))
				Expect(got).To(ContainSubstring("determination_type: 'automated'"))
				Expect(got).To(ContainSubstring("confidence: 0.85"))
				Expect(got).To(ContainSubstring("supersedes: 'e-0'"))
			})

			It("writes caller properties in sorted key order, as it does for nodes", func() {
				e := graphdb.Edge{
					ID:        "e-1",
					ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
					Properties: map[string]any{
						"h": 8, "c": 3, "f": 6, "a": 1, "g": 7, "b": 2, "e": 5, "d": 4,
					},
				}
				want := "{id: 'e-1', valid_from: '2025-01-01T00:00:00.000000000Z', a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8}"
				// Map iteration order is randomized; repeating makes an
				// unsorted implementation fail on effectively every run.
				for range 20 {
					Expect(agedriver.EdgeToAGProperties(e)).To(Equal(want))
				}
			})
		})

		Context("when escaping values for Cypher string literals", func() {
			DescribeTable("escapeCypher produces safe Cypher strings",
				func(input, expected string) {
					Expect(agedriver.EscapeCypher(input)).To(Equal(expected))
				},
				Entry("no special chars", "hello", "hello"),
				Entry("backslash", `a\b`, `a\\b`),
				Entry("single quote", "it's", `it\'s`),
				Entry("both backslash and quote", `it's a\b`, `it\'s a\\b`),
				Entry("empty string", "", ""),
				Entry("multiple backslashes", `a\\b`, `a\\\\b`),
				Entry("dollar-quote tag stripped",
					"prefix"+agedriver.ExportCypherDollarTag+"suffix",
					"prefixsuffix"),
				Entry("dollar-quote tag rebuilt by stripping is stripped too",
					"$cyp"+agedriver.ExportCypherDollarTag+"her$",
					""),
				Entry("dollar-quote tag nested twice is stripped",
					"$cyp$cyp"+agedriver.ExportCypherDollarTag+"her$her$",
					""),
				Entry("bare dollar signs preserved", "cost is $100", "cost is $100"),
				Entry("bare $$ preserved", "foo $$ bar", "foo $$ bar"),
			)

			DescribeTable("cypherValue formats Go values as Cypher literals",
				func(input any, expected string) {
					Expect(agedriver.CypherValue(input)).To(Equal(expected))
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
				client, err := agedriver.New(nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(client).NotTo(BeNil())

				tf := agedriver.ExportTelemetryFields(client)
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

				client, err := agedriver.New(nil, agedriver.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())
				Expect(client).NotTo(BeNil())

				tf := agedriver.ExportTelemetryFields(client)
				Expect(tf.HasTracer).To(BeTrue(), "tracer should be set with telemetry")
				Expect(tf.HasMeter).To(BeTrue(), "meter should be set with telemetry")
				Expect(tf.HasQueryCounter).To(BeTrue(), "queryCounter should be set with telemetry")
				Expect(tf.HasQueryLatency).To(BeTrue(), "queryLatency should be set with telemetry")
			})
		})

		Context("when a call ends on an error", func() {
			// endSpan runs markOutcome on a fresh span and returns it ended.
			endSpan := func(err error) sdktrace.ReadOnlySpan {
				tp, tpErr := telemetrytest.NewTestProvider()
				Expect(tpErr).NotTo(HaveOccurred())
				DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
				_, span := tp.TracerProvider().Tracer("test").Start(context.Background(), "op")
				agedriver.MarkOutcome(span, err)
				span.End()
				got := telemetrytest.FindSpan(tp.GetSpans(), "op")
				Expect(got).NotTo(BeNil())
				return got
			}

			DescribeTable("marks a domain sentinel Ok with its result, as record counts it",
				func(err error, wantResult string) {
					got := endSpan(err)
					Expect(got.Status().Code).To(Equal(codes.Ok))
					result, found := telemetrytest.SpanAttribute(got, "graphdb.result")
					Expect(found).To(BeTrue(), "span has no graphdb.result attribute")
					Expect(result.AsString()).To(Equal(wantResult))
				},
				Entry("wrapped ErrNodeExists", fmt.Errorf("create node Control/a: %w", graphdb.ErrNodeExists), "exists"),
				Entry("wrapped ErrEdgeExists", fmt.Errorf("edge %q: %w", "e1", graphdb.ErrEdgeExists), "exists"),
				Entry("wrapped ErrNodeNotFound", fmt.Errorf("target: node %q: %w", "x", graphdb.ErrNodeNotFound), "not_found"),
				Entry("wrapped ErrEdgeNotFound", fmt.Errorf("get edge e1: %w", graphdb.ErrEdgeNotFound), "not_found"),
			)

			DescribeTable("marks any other error Error with its message and no result attribute",
				func(err error) {
					got := endSpan(err)
					Expect(got.Status().Code).To(Equal(codes.Error))
					Expect(got.Status().Description).To(Equal(err.Error()))
					_, found := telemetrytest.SpanAttribute(got, "graphdb.result")
					Expect(found).To(BeFalse())
				},
				Entry("a backend failure", errors.New("create edge: connection reset")),
				// A reused ID is a caller bug, so record counts it as an error.
				Entry("a node ID conflict", fmt.Errorf("create node: %w",
					&graphdb.NodeIDConflictError{ID: "a", Label: "Control", StoredLabel: "Requirement"})),
			)

			It("leaves a tenant error's status to checkTenant, so the rejected tenant is not copied into the span", func() {
				got := endSpan(fmt.Errorf("begin tx: %w", graphdb.ErrTenantRequired))
				Expect(got.Status().Code).To(Equal(codes.Unset))
				Expect(got.Status().Description).To(BeEmpty())
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
				client, err = agedriver.New(nil, agedriver.WithTelemetry(tracer, meter))
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

			It("emits a graphdb.UpsertNode span with Error status on empty tenant", func() {
				created, err := client.UpsertNode(context.Background(), "", graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})
				Expect(err).To(MatchError(graphdb.ErrTenantRequired))
				Expect(created).To(BeFalse())

				span := telemetrytest.FindSpan(tp.GetSpans(), "graphdb.UpsertNode")
				Expect(span).NotTo(BeNil(), "expected graphdb.UpsertNode span")
				Expect(span.Status().Code.String()).To(Equal("Error"))
			})

			It("emits a graphdb.CreateEdge span with Error status on empty tenant", func() {
				By("calling CreateEdge with valid fields but empty tenant")
				err := client.CreateEdge(context.Background(), "", "req-1", "ctrl-1", graphdb.Edge{
					Label:     "SATISFIES",
					ValidFrom: time.Now(),
				})
				Expect(err).To(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, "graphdb.CreateEdge")
				Expect(span).NotTo(BeNil(), "expected graphdb.CreateEdge span")
				Expect(span.Status().Code.String()).To(Equal("Error"))
			})

			DescribeTable("records a fixed tenant.id for a rejected tenant, never the caller's value",
				func(bad string, op func(tenant string) error) {
					Expect(op(bad)).To(MatchError(graphdb.ErrTenantRequired))
					spans := tp.GetSpans()
					Expect(spans).NotTo(BeEmpty())
					span := spans[len(spans)-1]
					tenantAttr, found := telemetrytest.SpanAttribute(span, "tenant.id")
					Expect(found).To(BeTrue(), "expected tenant.id attribute on span %q", span.Name())
					Expect(tenantAttr.AsString()).To(Equal("invalid"))
					Expect(span.Status().Code.String()).To(Equal("Error"))
					Expect(span.Status().Description).To(Equal("invalid tenant ID"))
					if bad != "" {
						Expect(span.Status().Description).NotTo(ContainSubstring(bad))
					}
				},
				Entry("CreateNode with an empty tenant", "", func(tenant string) error {
					return client.CreateNode(context.Background(), tenant, graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})
				}),
				Entry("CreateNode with a SQL breakout tenant", "x'); --", func(tenant string) error {
					return client.CreateNode(context.Background(), tenant, graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})
				}),
				Entry("UpsertNode with a SQL breakout tenant", "x'); --", func(tenant string) error {
					_, err := client.UpsertNode(context.Background(), tenant, graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})
					return err
				}),
				Entry("GetNode with an uppercase tenant", "Acme", func(tenant string) error {
					_, err := client.GetNode(context.Background(), tenant, "n1")
					return err
				}),
				Entry("CreateGraph with a SQL breakout tenant", "x'); --", func(tenant string) error {
					return client.CreateGraph(context.Background(), tenant)
				}),
				Entry("QueryRelationships with a SQL breakout tenant", "x'); --", func(tenant string) error {
					_, err := client.QueryRelationships(context.Background(), tenant, graphdb.RelationshipQuery{})
					return err
				}),
			)

			It("records a valid tenant as its own tenant.id", func() {
				// A nil *sql.DB panics in BeginTx; a closed one fails there with
				// no I/O, after the span has started.
				db, err := sql.Open("pgx", "postgres://localhost/unused")
				Expect(err).NotTo(HaveOccurred())
				Expect(db.Close()).To(Succeed())
				closed, err := agedriver.New(db, agedriver.WithTelemetry(tp.TracerProvider().Tracer("graphdb-test"), tp.MeterProvider().Meter("graphdb-test")))
				Expect(err).NotTo(HaveOccurred())

				_, err = closed.QueryRelationships(context.Background(), "acme-corp", graphdb.RelationshipQuery{})
				Expect(err).To(MatchError(ContainSubstring("database is closed")))
				span := telemetrytest.FindSpan(tp.GetSpans(), "graphdb.QueryRelationships")
				Expect(span).NotTo(BeNil())
				tenantAttr, found := telemetrytest.SpanAttribute(span, "tenant.id")
				Expect(found).To(BeTrue())
				Expect(tenantAttr.AsString()).To(Equal("acme-corp"))
			})

			DescribeTable("counts a call rejected inside its span as an error of its operation",
				func(op string, call func() error) {
					Expect(call()).To(HaveOccurred())
					byOutcome, latencies := queryMetrics(tp, op)
					Expect(byOutcome).To(Equal(map[string]int64{"error/error": 1}))
					Expect(latencies).To(Equal(uint64(1)))
				},
				Entry("CreateNode with an empty tenant", "create_node", func() error {
					return client.CreateNode(context.Background(), "", graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})
				}),
				Entry("UpsertNode with an empty tenant", "upsert_node", func() error {
					_, err := client.UpsertNode(context.Background(), "", graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})
					return err
				}),
				Entry("QueryRelationships with a malformed tenant", "query_relationships", func() error {
					_, err := client.QueryRelationships(context.Background(), "x'); --", graphdb.RelationshipQuery{})
					return err
				}),
				Entry("QueryAsOf with a malformed tenant", "query_as_of", func() error {
					_, err := client.QueryAsOf(context.Background(), "x'); --", graphdb.RelationshipQuery{}, time.Now())
					return err
				}),
				Entry("BulkCreateEdges with an empty tenant", "bulk_create_edges", func() error {
					_, err := client.BulkCreateEdges(context.Background(), "", []graphdb.BulkEdge{
						{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{Label: "MAPS", ValidFrom: time.Now()}},
					})
					return err
				}),
			)

			It("records no metrics for an argument check that runs before the span", func() {
				Expect(client.CreateNode(context.Background(), "acme-corp", graphdb.Node{Label: "Test", ValidFrom: time.Now()})).
					To(MatchError(ContainSubstring("id is required")))
				Expect(telemetrytest.FindSpan(tp.GetSpans(), "graphdb.CreateNode")).To(BeNil())
				byOutcome, latencies := queryMetrics(tp, "create_node")
				Expect(byOutcome).To(BeEmpty())
				Expect(latencies).To(BeZero())
			})

			DescribeTable("records no span or metrics for a query filter check that runs before the span",
				func(spanName, op string, call func() error) {
					Expect(call()).To(MatchError(graphdb.ErrInvalidCypher))
					Expect(telemetrytest.FindSpan(tp.GetSpans(), spanName)).To(BeNil())
					byOutcome, latencies := queryMetrics(tp, op)
					Expect(byOutcome).To(BeEmpty())
					Expect(latencies).To(BeZero())
				},
				Entry("QueryRelationships with an invalid edge label filter", "graphdb.QueryRelationships", "query_relationships", func() error {
					_, err := client.QueryRelationships(context.Background(), "acme-corp", graphdb.RelationshipQuery{EdgeLabel: "a b"})
					return err
				}),
				Entry("QueryAsOf with an invalid property filter key", "graphdb.QueryAsOf", "query_as_of", func() error {
					_, err := client.QueryAsOf(context.Background(), "acme-corp", graphdb.RelationshipQuery{Properties: map[string]any{"a b": 1}}, time.Now())
					return err
				}),
				Entry("Traverse with an invalid edge label", "graphdb.Traverse", "traverse", func() error {
					_, err := client.Traverse(context.Background(), "acme-corp", graphdb.TraversalQuery{StartNode: "a", EdgeLabels: []string{"a b"}})
					return err
				}),
			)

			It("records the batch size as edge.count on the BulkCreateEdges span", func() {
				edges := []graphdb.BulkEdge{
					{SourceID: "a", TargetID: "b", Edge: graphdb.Edge{Label: "MAPS", ValidFrom: time.Now()}},
					{SourceID: "b", TargetID: "c", Edge: graphdb.Edge{Label: "MAPS", ValidFrom: time.Now()}},
					{SourceID: "c", TargetID: "a", Edge: graphdb.Edge{Label: "MAPS", ValidFrom: time.Now()}},
				}
				_, err := client.BulkCreateEdges(context.Background(), "", edges)
				Expect(err).To(MatchError(graphdb.ErrTenantRequired))
				span := telemetrytest.FindSpan(tp.GetSpans(), "graphdb.BulkCreateEdges")
				Expect(span).NotTo(BeNil())
				count, found := telemetrytest.SpanAttribute(span, "edge.count")
				Expect(found).To(BeTrue(), "span has no edge.count attribute")
				Expect(count.AsInt64()).To(Equal(int64(3)))
			})

			It("records no UpsertNode span or metrics for a field check that runs before the span", func() {
				_, err := client.UpsertNode(context.Background(), "acme-corp", graphdb.Node{ID: "n1", Label: "Test"})
				Expect(err).To(MatchError(ContainSubstring("upsert node: valid_from is required")))
				Expect(telemetrytest.FindSpan(tp.GetSpans(), "graphdb.UpsertNode")).To(BeNil())
				byOutcome, latencies := queryMetrics(tp, "upsert_node")
				Expect(byOutcome).To(BeEmpty())
				Expect(latencies).To(BeZero())
			})

			It("describes both graph driver metrics by operation, status and result", func() {
				Expect(client.CreateNode(context.Background(), "", graphdb.Node{ID: "n1", Label: "Test", ValidFrom: time.Now()})).To(HaveOccurred())
				rm := tp.GetMetrics()
				for _, name := range []string{"graphdb.queries.total", "graphdb.query.duration_ms"} {
					m := telemetrytest.FindMetric(rm, name)
					Expect(m).NotTo(BeNil(), "metric %s not recorded", name)
					Expect(m.Description).To(And(
						ContainSubstring("operation"), ContainSubstring("status"), ContainSubstring("result"),
					), "metric %s", name)
				}
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
				node, err := agedriver.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.ID).To(Equal("req-1"))
				Expect(node.Label).To(Equal("Requirement"))
				Expect(node.CreatedBy).To(Equal("test"))

				expectedTime, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				Expect(node.ValidFrom).To(Equal(expectedTime))
			})

			It("falls back to graph-internal id when property id is absent", func() {
				raw := `{"id": 456, "label": "Control", "properties": {"valid_from": "2025-06-01T00:00:00Z"}}::vertex`
				node, err := agedriver.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.ID).To(Equal("456"))
			})

			It("parses valid_to temporal bound when present", func() {
				raw := `{"id": 10, "label": "Policy", "properties": {"id": "pol-1", "valid_from": "2025-01-01T00:00:00Z", "valid_to": "2025-12-31T23:59:59Z"}}::vertex`
				node, err := agedriver.ParseAGVertex(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(node.ValidTo).NotTo(BeNil())

				expectedTo, _ := time.Parse(time.RFC3339, "2025-12-31T23:59:59Z")
				Expect(*node.ValidTo).To(Equal(expectedTo))
			})
		})

		Context("when handling malformed vertex input", func() {
			It("rejects input missing the ::vertex suffix", func() {
				raw := `{"id": 1, "label": "X", "properties": {}}`
				_, err := agedriver.ParseAGVertex(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects invalid JSON body", func() {
				raw := `{bad json}::vertex`
				_, err := agedriver.ParseAGVertex(raw)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("AGType Edge Parsing Edge Cases", func() {
		Context("when parsing valid edge representations", func() {
			It("extracts edge identity, label, and confidence from properties", func() {
				raw := `{"id": 789, "label": "SATISFIES", "start_id": 100, "end_id": 200, "properties": {"id": "edge-1", "valid_from": "2025-01-01T00:00:00Z", "confidence": 0.95}}::edge`
				edge, err := agedriver.ParseAGEdge(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(edge.ID).To(Equal("edge-1"))
				Expect(edge.Label).To(Equal("SATISFIES"))
				Expect(edge.Confidence).To(Equal(0.95))
			})

			It("parses an edge with no custom properties", func() {
				raw := `{"id": 50, "label": "RELATES", "start_id": 10, "end_id": 20, "properties": {"valid_from": "2025-01-01T00:00:00Z"}}::edge`
				edge, err := agedriver.ParseAGEdge(raw)
				Expect(err).NotTo(HaveOccurred())
				Expect(edge.Label).To(Equal("RELATES"))
			})
		})

		Context("when handling malformed edge input", func() {
			It("rejects input missing the ::edge suffix", func() {
				raw := `{"id": 1, "label": "X", "start_id": 1, "end_id": 2, "properties": {}}`
				_, err := agedriver.ParseAGEdge(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects invalid JSON body", func() {
				raw := `not json::edge`
				_, err := agedriver.ParseAGEdge(raw)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("AGType Path Parsing Edge Cases", func() {
		Context("when handling malformed path input", func() {
			It("rejects input missing the ::path suffix", func() {
				raw := `[{"id": 1, "label": "X", "properties": {}}::vertex]`
				_, err := agedriver.ParseAGPath(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects a non-array path body", func() {
				raw := `{"id": 1}::path`
				_, err := agedriver.ParseAGPath(raw)
				Expect(err).To(HaveOccurred())
			})

			It("rejects elements with unknown type suffixes", func() {
				raw := `[{"id": 1}::unknown]::path`
				_, err := agedriver.ParseAGPath(raw)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("AGType Path Element Splitting Edge Cases", func() {
		Context("when splitting composite AGE path strings", func() {
			It("handles a single element", func() {
				result := agedriver.SplitAGPathElements(`{"id": 1, "label": "X", "properties": {}}::vertex`)
				Expect(result).To(HaveLen(1))
			})

			It("splits three elements with nested JSON correctly", func() {
				v1 := `{"id": 1, "label": "A", "properties": {"key": "val"}}::vertex`
				e := `{"id": 10, "label": "R", "start_id": 1, "end_id": 2, "properties": {"x": "y"}}::edge`
				v2 := `{"id": 2, "label": "B", "properties": {}}::vertex`
				input := v1 + ", " + e + ", " + v2

				result := agedriver.SplitAGPathElements(input)
				Expect(result).To(HaveLen(3))
			})

			It("does not split on commas inside JSON braces", func() {
				result := agedriver.SplitAGPathElements(`{"a": 1, "b": 2}::vertex`)
				Expect(result).To(HaveLen(1))
			})

			It("returns empty slice for empty input", func() {
				result := agedriver.SplitAGPathElements("")
				Expect(result).To(HaveLen(0))
			})
		})
	})

	Describe("Suffix Stripping Edge Cases", func() {
		Context("when stripping AGE type suffixes", func() {
			It("strips a valid suffix and returns the body", func() {
				body, err := agedriver.StripSuffix(`{"id": 1}::vertex`, "::vertex")
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(Equal(`{"id": 1}`))
			})

			It("strips surrounding whitespace before suffix detection", func() {
				body, err := agedriver.StripSuffix(`  {"id": 1}::edge  `, "::edge")
				Expect(err).NotTo(HaveOccurred())
				Expect(body).To(Equal(`{"id": 1}`))
			})

			It("returns an error for wrong suffix", func() {
				_, err := agedriver.StripSuffix(`{"id": 1}::vertex`, "::edge")
				Expect(err).To(HaveOccurred())
			})

			It("returns an error when no suffix is present", func() {
				_, err := agedriver.StripSuffix(`{"id": 1}`, "::vertex")
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("CreateNode Validation Edge Cases", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var newErr error
			client, newErr = agedriver.New(nil)
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

			It("rejects a label that is not an identifier before reaching the database", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateNode(nil, "test-tenant", graphdb.Node{ID: "n-1", Label: "X) DETACH DELETE n //", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
				Expect(err.Error()).To(ContainSubstring(`label "X) DETACH DELETE n //" must be an identifier`))
			})
		})
	})

	Describe("CreateEdge Validation Edge Cases", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var newErr error
			client, newErr = agedriver.New(nil)
			Expect(newErr).NotTo(HaveOccurred())
		})

		Context("when validating edge creation inputs", func() {
			It("rejects an edge with empty label", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateEdge(nil, "test-tenant", "a", "b", graphdb.Edge{ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: label is required"))
			})

			It("rejects an edge with empty source", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateEdge(nil, "test-tenant", "", "b", graphdb.Edge{Label: "R", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))
			})

			It("rejects an edge with empty target", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				err := client.CreateEdge(nil, "test-tenant", "a", "", graphdb.Edge{Label: "R", ValidFrom: validFrom}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: source and target are required"))
			})

			It("rejects an edge with zero valid_from", func() {
				err := client.CreateEdge(nil, "test-tenant", "a", "b", graphdb.Edge{Label: "R"}) //nolint:staticcheck
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal("create edge: valid_from is required"))
			})
		})
	})

	Describe("Node Property Serialization Edge Cases", func() {
		Context("when serializing node properties with varying field populations", func() {
			It("includes valid_from in the fixed-width graphdb.TimeLayout format", func() {
				validFrom, _ := time.Parse(time.RFC3339, "2025-01-01T00:00:00Z")
				n := graphdb.Node{ID: "n-1", ValidFrom: validFrom}
				got := agedriver.NodeToAGProperties(n)
				Expect(got).To(ContainSubstring("valid_from: '2025-01-01T00:00:00.000000000Z'"))
			})

			It("writes sub-second times at fixed width so string order matches time order", func() {
				whole, _ := time.Parse(time.RFC3339, "2025-01-01T12:00:00Z")
				half := whole.Add(500 * time.Millisecond)
				wholeProps := agedriver.NodeToAGProperties(graphdb.Node{ID: "n-1", ValidFrom: whole})
				halfProps := agedriver.NodeToAGProperties(graphdb.Node{ID: "n-1", ValidFrom: half})
				Expect(wholeProps).To(ContainSubstring("valid_from: '2025-01-01T12:00:00.000000000Z'"))
				Expect(halfProps).To(ContainSubstring("valid_from: '2025-01-01T12:00:00.500000000Z'"))
			})
		})

		Context("when building the UpsertNode replacement map", func() {
			validFrom := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

			It("keeps the stored valid_from and carries the stored supersede state over when ValidTo is unset", func() {
				got := agedriver.NodeUpsertProperties(graphdb.Node{ID: "n-1", ValidFrom: validFrom})
				Expect(got).To(Equal("{id: 'n-1', valid_from: n.valid_from, valid_to: n.valid_to, superseded_by: n.superseded_by}"))
			})

			It("keeps the stored valid_from, writes ValidTo and omits superseded_by when ValidTo is set", func() {
				validTo := validFrom.Add(time.Hour)
				got := agedriver.NodeUpsertProperties(graphdb.Node{ID: "n-1", ValidFrom: validFrom, ValidTo: &validTo})
				Expect(got).To(Equal("{id: 'n-1', valid_from: n.valid_from, valid_to: '2025-01-01T01:00:00.000000000Z'}"))
			})

			It("escapes values exactly as CreateNode does", func() {
				n := graphdb.Node{ID: "x' }) DETACH DELETE n //", ValidFrom: validFrom, Properties: map[string]any{"title": "a'b\\c"}}
				got := agedriver.NodeUpsertProperties(n)
				Expect(got).To(ContainSubstring(`id: 'x\' }) DETACH DELETE n //'`))
				Expect(got).To(ContainSubstring(`title: 'a\'b\\c'`))
				created := strings.Replace(agedriver.NodeToAGProperties(n), "valid_from: '2025-01-01T00:00:00.000000000Z'", "valid_from: n.valid_from", 1)
				Expect(got).To(Equal(strings.TrimSuffix(created, "}") + ", valid_to: n.valid_to, superseded_by: n.superseded_by}"))
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
					ValidFrom: validFrom,
					ValidTo:   &validTo,
				}
				got := agedriver.EdgeToAGProperties(e)
				Expect(got).To(ContainSubstring("valid_to: '2025-12-31T23:59:59.000000000Z'"))
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

	// =================================================================
	// NEW METHODS: GetNode, GetEdge, BulkCreateEdges, ExecuteQuery, SupersedeFact
	// =================================================================

	Describe("GetNode", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var err error
			client, err = agedriver.New(nil)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when validating input parameters", func() {
			It("rejects empty tenant", func() {
				_, err := client.GetNode(context.Background(), "", "node-1")
				Expect(err).To(MatchError(graphdb.ErrTenantRequired))
			})

			It("rejects empty nodeID", func() {
				_, err := client.GetNode(context.Background(), "tenant-a", "")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("node_id is required"))
			})
		})
	})

	Describe("GetEdge", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var err error
			client, err = agedriver.New(nil)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when validating input parameters", func() {
			It("rejects empty tenant", func() {
				_, err := client.GetEdge(context.Background(), "", "edge-1")
				Expect(err).To(MatchError(graphdb.ErrTenantRequired))
			})

			It("rejects empty edgeID", func() {
				_, err := client.GetEdge(context.Background(), "tenant-a", "")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("edge_id is required"))
			})
		})
	})

	Describe("BulkCreateEdges", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var err error
			client, err = agedriver.New(nil)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when handling empty input", func() {
			It("returns nil for empty edge list", func() {
				ids, err := client.BulkCreateEdges(context.Background(), "tenant-a", nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(ids).To(BeNil())
			})
		})

		Context("when validating edge fields", func() {
			DescribeTable("reports the index of an invalid edge, alone or after a valid one",
				func(mutate func(*graphdb.BulkEdge), sentinel error, wantSuffix string) {
					valid := graphdb.BulkEdge{SourceID: "src-1", TargetID: "tgt-1", Edge: graphdb.Edge{ID: "edge-1", Label: "REL", ValidFrom: time.Now()}}
					invalid := valid
					invalid.Edge.ID = "edge-2"
					mutate(&invalid)

					expectRejected := func(batch []graphdb.BulkEdge, prefix string) {
						ids, err := client.BulkCreateEdges(context.Background(), "tenant-a", batch)
						Expect(err).To(HaveOccurred())
						Expect(ids).To(BeNil())
						Expect(err.Error()).To(HavePrefix(prefix))
						Expect(err.Error()).To(ContainSubstring(wantSuffix))
						if sentinel != nil {
							Expect(err).To(MatchError(sentinel))
						}
					}
					expectRejected([]graphdb.BulkEdge{invalid}, "bulk create edges [0]: ")
					expectRejected([]graphdb.BulkEdge{valid, invalid}, "bulk create edges [1]: ")
				},
				Entry("a label that is not an identifier",
					func(be *graphdb.BulkEdge) { be.Edge.Label = "MAPS TO" },
					graphdb.ErrInvalidCypher, `label "MAPS TO" must be an identifier`),
				Entry("a reserved property key",
					func(be *graphdb.BulkEdge) { be.Edge.Properties = map[string]any{"confidence": 0.5} },
					graphdb.ErrInvalidCypher, `property key "confidence" is reserved`),
				Entry("a property key that is not an identifier",
					func(be *graphdb.BulkEdge) { be.Edge.Properties = map[string]any{"bad key": 1} },
					graphdb.ErrInvalidCypher, `property key "bad key" must be an identifier`),
				Entry("an empty label",
					func(be *graphdb.BulkEdge) { be.Edge.Label = "" },
					nil, "label is required"),
				Entry("a missing source",
					func(be *graphdb.BulkEdge) { be.SourceID = "" },
					nil, "source and target are required"),
				Entry("a missing target",
					func(be *graphdb.BulkEdge) { be.TargetID = "" },
					nil, "source and target are required"),
				Entry("a zero valid_from",
					func(be *graphdb.BulkEdge) { be.Edge.ValidFrom = time.Time{} },
					nil, "valid_from is required"),
			)
		})
	})

	Describe("ExecuteQuery", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var err error
			client, err = agedriver.New(nil)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when validating input parameters", func() {
			It("rejects empty cypher", func() {
				_, err := client.ExecuteQuery(context.Background(), "tenant-a", "", nil)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cypher is required"))
			})

			It("rejects empty tenant", func() {
				_, err := client.ExecuteQuery(context.Background(), "", "MATCH (n) RETURN n", nil)
				Expect(err).To(MatchError(graphdb.ErrTenantRequired))
			})

			// client wraps a nil *sql.DB, so reaching the database would panic:
			// a returned error proves the check runs before any SQL is sent.
			DescribeTable("rejects cypher that contains the dollar-quote tag",
				func(cypher string) {
					_, err := client.ExecuteQuery(context.Background(), "tenant-a", cypher, nil)
					Expect(err).To(MatchError(graphdb.ErrInvalidCypher))
					Expect(err.Error()).To(ContainSubstring(agedriver.ExportCypherDollarTag))
					Expect(err.Error()).To(ContainSubstring("Remove it from the query"))
				},
				Entry("tag written directly", "RETURN 1 "+agedriver.ExportCypherDollarTag+") AS (v agtype); SELECT 1; --"),
				Entry("tag split around a second copy", "RETURN 1 $cyp"+agedriver.ExportCypherDollarTag+"her$) AS (v agtype); --"),
			)
		})

		DescribeTable("substitutes whole $name tokens only",
			func(cypher string, params map[string]string, want string) {
				Expect(agedriver.SubstituteParams(cypher, params)).To(Equal(want))
			},
			Entry("single parameter",
				"MATCH (n {id: $id}) RETURN n", map[string]string{"id": "a"},
				"MATCH (n {id: 'a'}) RETURN n"),
			Entry("prefix-sharing names stay distinct",
				"MATCH (n {id: $id2}) RETURN $id", map[string]string{"id": "a", "id2": "b"},
				"MATCH (n {id: 'b'}) RETURN 'a'"),
			Entry("a token without a parameter is left unchanged",
				"MATCH (n {id: $id2}) RETURN n", map[string]string{"id": "a"},
				"MATCH (n {id: $id2}) RETURN n"),
			Entry("values are escaped",
				"RETURN $v", map[string]string{"v": `it's \ odd`},
				`RETURN 'it\'s \\ odd'`),
			Entry("a substituted value is not substituted again",
				"RETURN $a, $b", map[string]string{"a": "$b", "b": "x"},
				"RETURN '$b', 'x'"),
			Entry("nil params leave the query unchanged",
				"RETURN $a", nil,
				"RETURN $a"),
		)
	})

	Describe("SupersedeFact", func() {
		var client graphdb.GraphDB

		BeforeEach(func() {
			var err error
			client, err = agedriver.New(nil)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when validating input parameters", func() {
			It("rejects request with both NodeID and EdgeID empty", func() {
				req := graphdb.SupersedeRequest{
					SupersededAt: time.Now(),
				}
				_, err := client.SupersedeFact(context.Background(), "tenant-a", req)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("node_id or edge_id is required"))
			})

			It("rejects request with both NodeID and EdgeID set", func() {
				req := graphdb.SupersedeRequest{
					NodeID:       "node-1",
					EdgeID:       "edge-1",
					SupersededAt: time.Now(),
				}
				_, err := client.SupersedeFact(context.Background(), "tenant-a", req)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("set node_id or edge_id, not both"))
			})

			It("rejects request with zero SupersededAt", func() {
				req := graphdb.SupersedeRequest{
					NodeID: "node-1",
				}
				_, err := client.SupersedeFact(context.Background(), "tenant-a", req)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("superseded_at is required"))
			})

			It("rejects empty tenant", func() {
				req := graphdb.SupersedeRequest{
					NodeID:       "node-1",
					SupersededAt: time.Now(),
				}
				_, err := client.SupersedeFact(context.Background(), "", req)
				Expect(err).To(MatchError(graphdb.ErrTenantRequired))
			})
		})
	})

	Describe("advisory lock keys", func() {
		It("pins the node lock key, so driver versions in a rolling deploy agree on it", func() {
			key := agedriver.AdvisoryKey("node", "crosscodex_acme", "cat/ac-2")
			Expect(agedriver.AdvisoryKey("node", "crosscodex_acme", "cat/ac-2")).To(Equal(key))
			Expect(key).To(Equal(int64(-1839689787764898047)), "FNV-1a of the NUL-terminated parts")
		})

		DescribeTable("gives a different node key when the graph or ID differs",
			func(gn, id string) {
				Expect(agedriver.AdvisoryKey("node", gn, id)).NotTo(Equal(agedriver.AdvisoryKey("node", "crosscodex_acme", "cat/ac-2")))
			},
			Entry("graph", "crosscodex_other", "cat/ac-2"),
			Entry("id", "crosscodex_acme", "cat/ac-3"),
		)

		It("separates parts, so moving a boundary changes the key", func() {
			Expect(agedriver.AdvisoryKey("node", "ab", "c")).NotTo(Equal(agedriver.AdvisoryKey("node", "a", "bc")))
			Expect(agedriver.AdvisoryKey("nodeg", "c")).NotTo(Equal(agedriver.AdvisoryKey("node", "gc")))
		})

		It("pins the graph lock key, so driver versions in a rolling deploy agree on it", func() {
			Expect(agedriver.AdvisoryKey("graph", "crosscodex_acme")).To(Equal(int64(3793828271057324715)), "FNV-1a of the NUL-terminated parts")
		})

		It("keeps graph keys in a separate domain and distinct per graph", func() {
			Expect(agedriver.AdvisoryKey("graph", "crosscodex_acme")).NotTo(Equal(agedriver.AdvisoryKey("graph", "crosscodex_other")))
			Expect(agedriver.AdvisoryKey("graph", "crosscodex_acme")).NotTo(Equal(agedriver.AdvisoryKey("node", "crosscodex_acme", "")))
		})
	})

	Describe("classifyErr", func() {
		It("wraps AGE's missing-graph error with ErrGraphNotFound, names the tenant, and records AGE's text only on the span", func() {
			tp, tpErr := telemetrytest.NewTestProvider()
			Expect(tpErr).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			ctx, span := tp.TracerProvider().Tracer("test").Start(context.Background(), "op")

			cause := errors.New(`ERROR: graph "crosscodex_tenant-x" does not exist (SQLSTATE 3F000)`)
			err := agedriver.ClassifyErr(ctx, "tenant-x", cause)
			span.End()

			Expect(err).To(MatchError(graphdb.ErrGraphNotFound))
			Expect(err.Error()).To(Equal(`graph for tenant "tenant-x" does not exist; create the tenant (or call CreateGraph) before using its graph: graph not found`))
			Expect(errors.Is(err, cause)).To(BeFalse(), "AGE's error names the internal graph and a SQLSTATE; it must not reach the caller")

			got := telemetrytest.FindSpan(tp.GetSpans(), "op")
			Expect(got).NotTo(BeNil())
			var recorded []string
			for _, ev := range got.Events() {
				for _, kv := range ev.Attributes {
					if kv.Key == "exception.message" {
						recorded = append(recorded, kv.Value.AsString())
					}
				}
			}
			Expect(recorded).To(ConsistOf(cause.Error()), "operators find AGE's text on the span")
		})

		It("passes other errors through unchanged", func() {
			cause := errors.New(`ERROR: relation "x" does not exist (SQLSTATE 42P01)`)
			Expect(agedriver.ClassifyErr(context.Background(), "tenant-x", cause)).To(BeIdenticalTo(cause))
		})

		It("does not classify an error that merely contains a quoted graph name near 'does not exist' for an unrelated object", func() {
			cause := errors.New(`ERROR: graph "g" exists but label "foo" does not exist`)
			Expect(agedriver.ClassifyErr(context.Background(), "tenant-x", cause)).To(BeIdenticalTo(cause))
		})

		It("returns nil for nil", func() {
			Expect(agedriver.ClassifyErr(context.Background(), "tenant-x", nil)).To(BeNil())
		})
	})

	Describe("parseQueryValue", func() {
		Context("when parsing AGE typed values", func() {
			It("parses vertex type", func() {
				raw := `{"id": 1, "label": "Node", "properties": {"id": "n-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
				val := agedriver.ParseQueryValue(raw)
				Expect(val.Type).To(Equal(graphdb.QueryValueNode))
				Expect(val.NodeVal).NotTo(BeNil())
				Expect(val.NodeVal.ID).To(Equal("n-1"))
			})

			It("parses edge type", func() {
				raw := `{"id": 10, "label": "REL", "start_id": 1, "end_id": 2, "properties": {"id": "e-1", "valid_from": "2025-01-01T00:00:00Z"}}::edge`
				val := agedriver.ParseQueryValue(raw)
				Expect(val.Type).To(Equal(graphdb.QueryValueEdge))
				Expect(val.EdgeVal).NotTo(BeNil())
				Expect(val.EdgeVal.Edge.ID).To(Equal("e-1"))
			})

			It("parses scalar as fallback", func() {
				raw := `"some string"`
				val := agedriver.ParseQueryValue(raw)
				Expect(val.Type).To(Equal(graphdb.QueryValueScalar))
				Expect(val.ScalarVal).To(Equal(`"some string"`))
			})
		})
	})

	Describe("Path.ValidAt", func() {
		Context("when checking temporal validity of paths", func() {
			It("returns true when all nodes and edges are valid at the given time", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
						},
						{
							ID:        "n2",
							Label:     "Requirement",
							ValidFrom: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
						},
					},
					Edges: []graphdb.Edge{
						{
							ID:        "e1",
							Label:     "SATISFIES",
							ValidFrom: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
						},
					},
				}
				Expect(path.ValidAt(queryTime)).To(BeTrue())
			})

			It("returns false when a node was created after the query time", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
						},
						{
							ID:        "n2",
							Label:     "Requirement",
							ValidFrom: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC),
						},
					},
					Edges: []graphdb.Edge{},
				}
				Expect(path.ValidAt(queryTime)).To(BeFalse())
			})

			It("returns false when a node was superseded before the query time", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				validTo := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
							ValidTo:   &validTo,
						},
					},
					Edges: []graphdb.Edge{},
				}
				Expect(path.ValidAt(queryTime)).To(BeFalse())
			})

			It("returns false when an edge was created after the query time", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
						},
					},
					Edges: []graphdb.Edge{
						{
							ID:        "e1",
							Label:     "SATISFIES",
							ValidFrom: time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC),
						},
					},
				}
				Expect(path.ValidAt(queryTime)).To(BeFalse())
			})

			It("returns false when an edge was superseded before the query time", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				validTo := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
						},
					},
					Edges: []graphdb.Edge{
						{
							ID:        "e1",
							Label:     "SATISFIES",
							ValidFrom: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
							ValidTo:   &validTo,
						},
					},
				}
				Expect(path.ValidAt(queryTime)).To(BeFalse())
			})

			It("returns true when ValidTo is nil (current facts)", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
							ValidTo:   nil,
						},
					},
					Edges: []graphdb.Edge{
						{
							ID:        "e1",
							Label:     "SATISFIES",
							ValidFrom: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
							ValidTo:   nil,
						},
					},
				}
				Expect(path.ValidAt(queryTime)).To(BeTrue())
			})

			It("returns true for empty path", func() {
				queryTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				path := graphdb.Path{
					Nodes: []graphdb.Node{},
					Edges: []graphdb.Edge{},
				}
				Expect(path.ValidAt(queryTime)).To(BeTrue())
			})

			It("returns false when query time equals ValidTo boundary", func() {
				validTo := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
				queryTime := validTo
				path := graphdb.Path{
					Nodes: []graphdb.Node{
						{
							ID:        "n1",
							Label:     "Control",
							ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
							ValidTo:   &validTo,
						},
					},
				}
				Expect(path.ValidAt(queryTime)).To(BeFalse())
			})
		})
	})
})
