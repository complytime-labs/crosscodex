package graph_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

var _ = Describe("Edge temporal attribute conversion", func() {
	validFrom := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	validTo := validFrom.Add(time.Hour)

	Describe("protoToEdge (write path)", func() {
		DescribeTable("maps TemporalAttributes onto the Edge fields, not Properties",
			func(confidence float32, want float64) {
				e := graph.ExportProtoToEdge(&pb.CreateEdgeRequest{
					Label: "MAPS",
					Temporal: &pb.TemporalAttributes{
						ValidFrom:    timestamppb.New(validFrom),
						ValidTo:      timestamppb.New(validTo),
						DeterminedBy: "job-1",
						Confidence:   confidence,
					},
				})
				Expect(e.Confidence).To(Equal(want))
				Expect(e.DeterminedBy).To(Equal("job-1"))
				Expect(e.ValidFrom).To(Equal(validFrom))
				Expect(e.ValidTo).To(HaveValue(Equal(validTo)))
				Expect(e.Properties).NotTo(HaveKey("confidence"))
				Expect(e.Properties).NotTo(HaveKey("determined_by"))
			},
			Entry("a decimal stored as its shortest float32 form, not 0.949999988", float32(0.95), 0.95),
			Entry("one", float32(1), 1.0),
			Entry("zero", float32(0), 0.0),
		)

		It("leaves Confidence and DeterminedBy zero without TemporalAttributes", func() {
			e := graph.ExportProtoToEdge(&pb.CreateEdgeRequest{Label: "MAPS"})
			Expect(e.Confidence).To(BeZero())
			Expect(e.DeterminedBy).To(BeEmpty())
		})
	})

	Describe("edgeToProto (read path)", func() {
		It("maps Confidence and DeterminedBy back into TemporalAttributes", func() {
			pe := graph.ExportEdgeToProto(graphdb.EdgeWithEndpoints{
				Edge: graphdb.Edge{ID: "e1", Label: "MAPS", ValidFrom: validFrom, ValidTo: &validTo, DeterminedBy: "job-1", Confidence: 0.95},
			}, nil)
			Expect(pe.GetTemporal().GetConfidence()).To(Equal(float32(0.95)))
			Expect(pe.GetTemporal().GetDeterminedBy()).To(Equal("job-1"))
			Expect(pe.GetTemporal().GetValidFrom().AsTime()).To(Equal(validFrom))
			Expect(pe.GetTemporal().GetValidTo().AsTime()).To(Equal(validTo))
		})

		DescribeTable("sets TemporalAttributes when only one temporal field is set, leaving the others empty",
			func(e graphdb.Edge, wantValidTo *time.Time, wantDeterminedBy string, wantConfidence float32) {
				temporal := graph.ExportEdgeToProto(graphdb.EdgeWithEndpoints{Edge: e}, nil).GetTemporal()
				Expect(temporal).NotTo(BeNil())
				Expect(temporal.GetValidFrom()).To(BeNil())
				if wantValidTo == nil {
					Expect(temporal.GetValidTo()).To(BeNil())
				} else {
					Expect(temporal.GetValidTo().AsTime()).To(Equal(*wantValidTo))
				}
				Expect(temporal.GetDeterminedBy()).To(Equal(wantDeterminedBy))
				Expect(temporal.GetConfidence()).To(Equal(wantConfidence))
			},
			Entry("Confidence", graphdb.Edge{ID: "e1", Confidence: 0.5}, nil, "", float32(0.5)),
			Entry("ValidTo", graphdb.Edge{ID: "e1", ValidTo: &validTo}, &validTo, "", float32(0)),
			Entry("DeterminedBy", graphdb.Edge{ID: "e1", DeterminedBy: "job-1"}, nil, "job-1", float32(0)),
		)

		It("omits TemporalAttributes for an edge with no temporal data", func() {
			pe := graph.ExportEdgeToProto(graphdb.EdgeWithEndpoints{Edge: graphdb.Edge{ID: "e1"}}, nil)
			Expect(pe.GetTemporal()).To(BeNil())
		})
	})

	It("round-trips confidence and determined_by from request to stored edge to response", func() {
		in := &pb.TemporalAttributes{ValidFrom: timestamppb.New(validFrom), DeterminedBy: "job-2", Confidence: 0.85}
		stored := graph.ExportProtoToEdge(&pb.CreateEdgeRequest{Label: "MAPS", Temporal: in})
		out := graph.ExportEdgeToProto(graphdb.EdgeWithEndpoints{Edge: stored}, nil).GetTemporal()
		Expect(out.GetConfidence()).To(Equal(in.GetConfidence()))
		Expect(out.GetDeterminedBy()).To(Equal(in.GetDeterminedBy()))
		Expect(out.GetValidFrom().AsTime()).To(Equal(validFrom))
	})
})

var _ = Describe("graphErrorMessage", func() {
	It("has a proto-field hint for every reserved key", func() {
		hints := graph.ExportReservedKeyHints
		for _, reserved := range []map[string]string{graphdb.ReservedNodeKeys(), graphdb.ReservedEdgeKeys()} {
			for key := range reserved {
				Expect(hints).To(HaveKey(key))
			}
		}
	})

	DescribeTable("names no Go field for any reserved key",
		func(check func(map[string]any) error, reserved map[string]string) {
			for key := range reserved {
				err := check(map[string]any{key: "v"})
				for _, tc := range []struct {
					err    error
					prefix string
				}{
					{fmt.Errorf("create edge: %w", err), ""},
					{&graphdb.BulkEdgeError{Index: 3, Err: err}, "bulk create edges [3]: "},
					{fmt.Errorf("outer: %w", &graphdb.BulkEdgeError{Index: 4, Err: err}), "bulk create edges [4]: "},
				} {
					msg := graph.ExportGraphErrorMessage(tc.err)
					Expect(msg).To(Equal(fmt.Sprintf("%sproperties[%q] is reserved: %s", tc.prefix, key, graph.ExportReservedKeyHints[key])))
					Expect(msg).NotTo(MatchRegexp(`\b(Node|Edge|SupersedeRequest)\.`))
				}
			}
		},
		Entry("nodes", graphdb.CheckNodeProperties, graphdb.ReservedNodeKeys()),
		Entry("edges", graphdb.CheckEdgeProperties, graphdb.ReservedEdgeKeys()),
	)

	It("passes other errors through unchanged", func() {
		err := fmt.Errorf("create edge: %w", graphdb.ErrNodeNotFound)
		Expect(graph.ExportGraphErrorMessage(err)).To(Equal(err.Error()))
	})

	It("takes the bulk index from *graphdb.BulkEdgeError, never from message text", func() {
		rk := graphdb.CheckEdgeProperties(map[string]any{"valid_from": "x"})
		msg := graph.ExportGraphErrorMessage(fmt.Errorf("bulk create edges [9]: %w", rk))
		Expect(msg).To(Equal(`properties["valid_from"] is reserved: set temporal.valid_from instead`))
	})
})
