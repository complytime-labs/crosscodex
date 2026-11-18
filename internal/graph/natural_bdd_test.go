package graph_test

import (
	"cmp"
	"math/rand"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
)

var _ = Describe("naturalCompare", func() {
	DescribeTable("orders identifiers with digit runs numerically",
		func(a, b string, sign int) {
			Expect(cmp.Compare(graph.ExportNaturalCompare(a, b), 0)).To(Equal(sign))
			Expect(cmp.Compare(graph.ExportNaturalCompare(b, a), 0)).To(Equal(-sign))
		},
		Entry("a shorter digit run before a longer one", "ac-2", "ac-10", -1),
		Entry("words followed by numbers", "Article 2", "Article 13", -1),
		Entry("enhancements in parentheses", "ac-2(1)", "ac-2(10)", -1),
		Entry("dotted outline numbers", "PO.1.1", "PO.1.10", -1),
		Entry("digit runs longer than any integer", "x-123456789012345678901234567890", "x-123456789012345678901234567891", -1),
		Entry("digit runs of different length beyond any integer", "x-99999999999999999999999999999", "x-100000000000000000000000000000", -1),
		Entry("a prefix before its extension", "ac-2", "ac-2(1)", -1),
		Entry("fewer leading zeros before more on a numeric tie", "a1", "a01", -1),
		Entry("equal identifiers", "x", "x", 0),
		Entry("the empty string before any other", "", "a", -1),
	)
})

var _ = Describe("sortCrosswalkRows with sort ids", func() {
	row := func(srcID, srcIdent, tgtID string) *pb.CrosswalkRow {
		return &pb.CrosswalkRow{
			Source: &pb.CrosswalkSide{NodeId: srcID, Identifier: srcIdent, Framework: "F"},
			Target: &pb.CrosswalkSide{NodeId: tgtID, Identifier: "t", Framework: "G"},
		}
	}
	sources := func(rows []*pb.CrosswalkRow) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Source.Identifier
		}
		return out
	}

	It("orders rows by sort id when both sources carry one, against identifier order", func() {
		rows := []*pb.CrosswalkRow{row("n1", "a-1", "t1"), row("n2", "b-1", "t1")}
		graph.ExportSortCrosswalkRows(rows, map[string]string{"n1": "2", "n2": "1"})
		Expect(sources(rows)).To(Equal([]string{"b-1", "a-1"}))
	})

	It("falls back to natural identifier order when a source lacks a sort id", func() {
		rows := []*pb.CrosswalkRow{row("n1", "ac-10", "t1"), row("n2", "ac-2", "t1")}
		graph.ExportSortCrosswalkRows(rows, map[string]string{"n1": "1"})
		Expect(sources(rows)).To(Equal([]string{"ac-2", "ac-10"}))
	})

	It("orders a set mixing controls with and without sort ids naturally, the same for every input order", func() {
		ids := map[string]string{"n1": "2", "n3": "1"}
		for seed := range 20 {
			rows := []*pb.CrosswalkRow{row("n1", "a", "t1"), row("n2", "b", "t1"), row("n3", "c", "t1")}
			rand.New(rand.NewSource(int64(seed))).Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
			graph.ExportSortCrosswalkRows(rows, ids)
			Expect(sources(rows)).To(Equal([]string{"a", "b", "c"}))
		}
	})
})
